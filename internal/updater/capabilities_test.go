package updater

import "testing"

func TestPinnedDeploymentDoesNotCheckMovingChannels(t *testing.T) {
	deployment := DefaultDeployment()
	deployment.Channel = ChannelPinned

	capabilities := DetectCapabilities(deployment)
	if capabilities.CanCheck || capabilities.CanUpdate {
		t.Fatalf("pinned capabilities = %#v; want check and update disabled", capabilities)
	}
	if capabilities.Reason == "" {
		t.Fatal("pinned deployment has no operator guidance")
	}
}
