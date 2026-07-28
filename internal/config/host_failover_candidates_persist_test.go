package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestHostNetworkBackupSelectionPreservesClickOrder(t *testing.T) {
	path := writeTempConfig(t, `
config_schema: 1
web:
  username: alice
  password: secret
host_network_failover:
  primary_interface: eth0
devices:
  - id: modem-a
    future_device_option: keep
  - id: modem-b
`)

	setHostNetworkBackupForTest(t, path, "modem-b", true)
	setHostNetworkBackupForTest(t, path, "modem-a", true)
	assertHostNetworkCandidates(t, path, []string{"modem-b", "modem-a"})

	// Saving an already-selected device must not move it.
	setHostNetworkBackupForTest(t, path, "modem-b", true)
	assertHostNetworkCandidates(t, path, []string{"modem-b", "modem-a"})

	setHostNetworkBackupForTest(t, path, "modem-b", false)
	assertHostNetworkCandidates(t, path, []string{"modem-a"})
	setHostNetworkBackupForTest(t, path, "modem-b", true)
	assertHostNetworkCandidates(t, path, []string{"modem-a", "modem-b"})

	setHostNetworkBackupForTest(t, path, "modem-a", false)
	setHostNetworkBackupForTest(t, path, "modem-b", false)
	assertHostNetworkCandidates(t, path, nil)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "candidate_device_ids:") || strings.Contains(text, "enabled:") {
		t.Fatalf("empty selection did not disable failover cleanly:\n%s", text)
	}
	if !strings.Contains(text, "future_device_option: keep") {
		t.Fatal("device update lost an unknown field")
	}
}

func TestDeletingAndAddingDeviceUpdatesHostNetworkCandidates(t *testing.T) {
	path := writeTempConfig(t, `
config_schema: 1
web:
  username: alice
  password: secret
host_network_failover:
  primary_interface: eth0
  candidate_device_ids: [modem-a]
devices:
  - id: modem-a
`)

	if err := DeleteDeviceInFile(path, "modem-a"); err != nil {
		t.Fatal(err)
	}
	assertHostNetworkCandidates(t, path, nil)

	if err := AddDeviceInFileWithLimitAndHostNetworkBackup(path, DeviceConfig{ID: "modem-b"}, 0, true); err != nil {
		t.Fatal(err)
	}
	assertHostNetworkCandidates(t, path, []string{"modem-b"})
}

func TestHostNetworkBackupSelectionAllowsAutoDiscoveredPrimary(t *testing.T) {
	path := writeTempConfig(t, `
config_schema: 1
web:
  username: alice
  password: secret
devices:
  - id: modem-a
`)

	enabled := true
	err := UpdateDeviceInFileWithHostNetworkBackup(path, "modem-a", DeviceConfig{ID: "modem-a"}, &enabled)
	if err != nil {
		t.Fatalf("select backup without primary_interface: %v", err)
	}
	assertHostNetworkCandidates(t, path, []string{"modem-a"})
}

func setHostNetworkBackupForTest(t *testing.T, path, deviceID string, enabled bool) {
	t.Helper()
	if err := UpdateDeviceInFileWithHostNetworkBackup(path, deviceID, DeviceConfig{ID: deviceID}, &enabled); err != nil {
		t.Fatal(err)
	}
}

func assertHostNetworkCandidates(t *testing.T, path string, want []string) {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.HostFailover.CandidateDeviceIDs, want) {
		t.Fatalf("candidate IDs = %v, want %v", cfg.HostFailover.CandidateDeviceIDs, want)
	}
}
