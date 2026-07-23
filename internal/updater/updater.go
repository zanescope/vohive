package updater

import (
	"context"
	"errors"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/zanescope/vohive/internal/global"
)

type UpdateInfo struct {
	HasUpdate   bool   `json:"has_update"`
	CurrentVer  string `json:"current_version"`
	LatestVer   string `json:"latest_version"`
	ReleaseNote string `json:"release_note"`
	IsDocker    bool   `json:"is_docker"`
}

// CheckUpdate is the compatibility wrapper for the legacy API. Candidate
// metadata and artifact hashes are accepted only after manifest signature
// verification. Non-SemVer development builds deliberately report no update.
func CheckUpdate() (*UpdateInfo, error) {
	deployment, err := DiscoverDeployment(DefaultDeploymentPath)
	if err != nil {
		return nil, err
	}
	currentVersion := deployment.CurrentVersion
	if currentVersion == "" {
		currentVersion = global.Version
	}
	verifier, err := DefaultSignatureVerifier()
	if err != nil {
		return nil, err
	}
	resolver, err := NewGitHubResolver(&http.Client{Timeout: 30 * time.Second}, verifier)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	candidate, err := resolver.Check(ctx, CheckRequest{
		Channel: deployment.Channel, CurrentVersion: currentVersion,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	})
	if err != nil {
		return nil, err
	}
	_, dockerErr := os.Stat("/.dockerenv")
	return &UpdateInfo{
		HasUpdate: candidate.HasUpdate, CurrentVer: candidate.CurrentVer,
		LatestVer: candidate.LatestVer, ReleaseNote: candidate.ReleaseNote,
		IsDocker: dockerErr == nil,
	}, nil
}

// ApplyUpdate preserves the old handler entry point but dispatches the signed,
// exact target to the independent update unit. It never overwrites or signals
// the running VoHive process.
func ApplyUpdate() error {
	if _, err := os.Stat(DefaultDeploymentPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("transactional update metadata is missing; run the signed installer with --repair")
		}
		return err
	}
	deployment, err := LoadDeployment(DefaultDeploymentPath)
	if err != nil {
		return err
	}
	verifier, err := DefaultSignatureVerifier()
	if err != nil {
		return err
	}
	resolver, err := NewGitHubResolver(&http.Client{Timeout: 30 * time.Second}, verifier)
	if err != nil {
		return err
	}
	coordinator := NewLocalCoordinator(DefaultDeploymentPath, resolver, ServiceJobLauncher{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	candidate, err := coordinator.Check(ctx, CheckRequest{Channel: deployment.Channel})
	if err != nil {
		return err
	}
	if !candidate.HasUpdate {
		return errors.New("no applicable signed update is available")
	}
	_, err = coordinator.Start(ctx, UpdateRequest{
		Schema: 1, Channel: deployment.Channel, Version: candidate.LatestVer,
	})
	return err
}
