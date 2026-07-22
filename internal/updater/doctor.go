package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Diagnostic struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type DoctorReport struct {
	Healthy     bool         `json:"healthy"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

func Doctor(ctx context.Context, deploymentFile string, checker ReadyChecker) DoctorReport {
	report := DoctorReport{Healthy: true}
	add := func(name string, err error, success string) {
		diagnostic := Diagnostic{Name: name, OK: err == nil, Message: success}
		if err != nil {
			diagnostic.Message = err.Error()
			report.Healthy = false
		}
		report.Diagnostics = append(report.Diagnostics, diagnostic)
	}
	deployment, err := DiscoverDeployment(deploymentFile)
	add("deployment", err, "deployment metadata is valid")
	if err != nil {
		return report
	}
	if deploymentFile == "" {
		deploymentFile = DefaultDeploymentPath
	}
	paths := PathsFor(deploymentFile, deployment)
	add("paths", paths.Validate(), "managed paths are absolute and scoped")
	capabilities := DetectCapabilities(deployment)
	if capabilities.CanUpdate {
		add("update-capability", nil, "independent updates are supported")
	} else {
		add("update-capability", errors.New(capabilities.Reason), "")
	}
	_, verifierErr := DefaultSignatureVerifier()
	add("signature", verifierErr, "a minisign trust root is configured")
	if _, _, pointerErr := currentVersionTarget(paths); pointerErr != nil {
		add("current-version", pointerErr, "")
	} else {
		add("current-version", nil, "current points to a managed release")
	}
	if stale, lockErr := IsLockStale(paths.LockFile(), 2*time.Hour); lockErr != nil {
		add("update-lock", lockErr, "")
	} else if stale {
		add("update-lock", fmt.Errorf("stale update lock at %s; run recover during a maintenance window", paths.LockFile()), "")
	} else if _, statErr := os.Stat(paths.LockFile()); statErr == nil {
		add("update-lock", ErrUpdateLocked, "")
	} else if os.IsNotExist(statErr) {
		add("update-lock", nil, "no update transaction is active")
	} else {
		add("update-lock", statErr, "")
	}
	for name, path := range map[string]string{
		"releases-directory": paths.ReleasesDir(),
		"state-directory":    paths.StateRoot,
		"backup-directory":   paths.BackupsDir(),
	} {
		info, statErr := os.Stat(path)
		if statErr == nil && !info.IsDir() {
			statErr = fmt.Errorf("%s is not a directory", path)
		}
		if os.IsNotExist(statErr) {
			parent := filepath.Dir(path)
			if parentInfo, parentErr := os.Stat(parent); parentErr == nil && parentInfo.IsDir() {
				statErr = nil
			}
		}
		add(name, statErr, path+" is available")
	}
	if checker != nil && deployment.ReadyURL != "" {
		readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		readyErr := checker.Ready(readyCtx, deployment.ReadyURL)
		cancel()
		add("readiness", readyErr, "service readiness endpoint is healthy")
	}
	return report
}
