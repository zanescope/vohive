package hostconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
)

const planSchema = 1

type planTargetInput struct {
	ID      string
	USBPath string
}

type planTargetResolution struct {
	ID              string
	USBPath         string
	Resolved        bool
	Matcher         mmisolation.Matcher
	ResolutionError string
}

type planSnapshot struct {
	RuleStatus  mmisolation.Status
	Resolutions []planTargetResolution
	Revision    string
}

type canonicalPlan struct {
	Schema       int                   `json:"schema"`
	RuleState    mmisolation.RuleState `json:"rule_state"`
	RuleRevision string                `json:"rule_revision"`
	Targets      []canonicalPlanTarget `json:"targets"`
}

type canonicalPlanTarget struct {
	ID              string                  `json:"id"`
	USBPath         string                  `json:"usb_path"`
	Resolved        bool                    `json:"resolved"`
	Kind            mmisolation.MatcherKind `json:"kind"`
	VendorID        string                  `json:"vendor_id"`
	Serial          string                  `json:"serial"`
	KernelPath      string                  `json:"kernel_path"`
	ResolutionError string                  `json:"resolution_error"`
}

func planInputsFromTargets(targets []Target) []planTargetInput {
	inputs := make([]planTargetInput, 0, len(targets))
	for _, target := range targets {
		inputs = append(inputs, planTargetInput{ID: target.ID, USBPath: target.USBPath})
	}
	return inputs
}

func planInputsFromWorkerTargets(targets []mmisolation.Target) []planTargetInput {
	inputs := make([]planTargetInput, 0, len(targets))
	for _, target := range targets {
		inputs = append(inputs, planTargetInput{ID: target.ID, USBPath: target.USBPath})
	}
	return inputs
}

func buildPlanSnapshot(manager *mmisolation.Manager, inputs []planTargetInput) (planSnapshot, error) {
	if manager == nil {
		return planSnapshot{}, errors.New("ModemManager isolation manager is nil")
	}
	ruleStatus, err := manager.Inspect()
	if err != nil {
		return planSnapshot{}, err
	}
	sorted := append([]planTargetInput(nil), inputs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ID != sorted[j].ID {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].USBPath < sorted[j].USBPath
	})
	canonical := canonicalPlan{
		Schema: planSchema, RuleState: ruleStatus.State, RuleRevision: ruleStatus.Revision,
		Targets: make([]canonicalPlanTarget, 0, len(sorted)),
	}
	resolutions := make([]planTargetResolution, 0, len(sorted))
	for _, input := range sorted {
		matcher, resolveErr := manager.ResolveMatcher(mmisolation.Target{ID: input.ID, USBPath: input.USBPath})
		resolution := planTargetResolution{ID: input.ID, USBPath: input.USBPath, Matcher: matcher}
		if resolveErr == nil {
			resolution.Resolved = true
		} else {
			resolution.ResolutionError = resolveErr.Error()
		}
		resolutions = append(resolutions, resolution)
		canonical.Targets = append(canonical.Targets, canonicalPlanTarget{
			ID: input.ID, USBPath: input.USBPath, Resolved: resolution.Resolved,
			Kind: matcher.Kind, VendorID: matcher.VendorID, Serial: matcher.Serial,
			KernelPath: matcher.KernelPath, ResolutionError: resolution.ResolutionError,
		})
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return planSnapshot{}, fmt.Errorf("encode ModemManager isolation plan: %w", err)
	}
	digest := sha256.Sum256(payload)
	return planSnapshot{
		RuleStatus: ruleStatus, Resolutions: resolutions,
		Revision: "sha256:" + hex.EncodeToString(digest[:]),
	}, nil
}

func expectedEntriesFromPlan(plan planSnapshot) ([]mmisolation.Entry, error) {
	entries := make([]mmisolation.Entry, 0, len(plan.Resolutions))
	for _, resolution := range plan.Resolutions {
		if !resolution.Resolved {
			return nil, fmt.Errorf(
				"%w: target %q was not resolved in the validated plan",
				ErrPlanConflict, resolution.ID,
			)
		}
		entries = append(entries, mmisolation.Entry{TargetID: resolution.ID, Matcher: resolution.Matcher})
	}
	return entries, nil
}
