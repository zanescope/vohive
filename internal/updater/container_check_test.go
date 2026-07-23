package updater

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type captureCheckResolver struct {
	request CheckRequest
	called  bool
}

func (r *captureCheckResolver) Check(_ context.Context, request CheckRequest) (Candidate, error) {
	r.called = true
	r.request = request
	return Candidate{CurrentVer: request.CurrentVersion, LatestVer: "v1.7.0", HasUpdate: true}, nil
}

func TestContainerCheckUsesCompiledReleaseVersionWithoutEnablingSelfUpdate(t *testing.T) {
	root := t.TempDir()
	deploymentFile := filepath.Join(root, "etc", "deployment.json")
	deployment := DefaultDeployment()
	deployment.InstallRoot = filepath.Join(root, "opt", "vohive")
	deployment.ConfigPath = filepath.Join(root, "etc", "config.yaml")
	deployment.DataPath = filepath.Join(root, "var", "data")
	deployment.StateRoot = filepath.Join(root, "var", "update")
	deployment.CurrentVersion = ""
	if err := SaveDeployment(deploymentFile, deployment); err != nil {
		t.Fatal(err)
	}

	resolver := &captureCheckResolver{}
	coordinator := &LocalCoordinator{
		DeploymentFile: deploymentFile,
		Resolver:       resolver,
		validateScope:  func(RuntimePaths) error { return nil },
		containerized:  func() bool { return true },
		buildVersion:   func() string { return "v1.6.0" },
	}
	candidate, err := coordinator.Check(context.Background(), CheckRequest{Channel: ChannelStable})
	if err != nil {
		t.Fatal(err)
	}
	if resolver.request.CurrentVersion != "v1.6.0" || candidate.CurrentVer != "v1.6.0" {
		t.Fatalf("container check used %#v and candidate %#v", resolver.request, candidate)
	}

	previousContainerProbe := coordinator.containerized
	coordinator.containerized = func() bool { return false }
	resolver.called = false
	candidate, err = coordinator.Check(context.Background(), CheckRequest{Channel: ChannelStable})
	if err != nil {
		t.Fatal(err)
	}
	if resolver.called || candidate.HasUpdate {
		t.Fatal("native deployment without release metadata did not fail closed")
	}
	coordinator.containerized = previousContainerProbe

	// Capabilities remain host-managed even though the signed candidate can be
	// displayed in a container. This assertion is environment independent.
	if _, err := os.Stat("/.dockerenv"); err == nil {
		capabilities := DetectCapabilities(deployment)
		if capabilities.CanUpdate || capabilities.CanRollback {
			t.Fatalf("container self-update was enabled: %#v", capabilities)
		}
	}
}
