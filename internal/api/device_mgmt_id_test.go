package api

import (
	"testing"

	"github.com/zanescope/vohive/internal/config"
)

func TestNextDefaultDeviceID(t *testing.T) {
	tests := []struct {
		name    string
		devices []config.DeviceConfig
		want    string
	}{
		{
			name: "first device",
			want: "device01",
		},
		{
			name: "legacy interface id does not affect sequence",
			devices: []config.DeviceConfig{
				{ID: "wwan0"},
			},
			want: "device01",
		},
		{
			name: "increments",
			devices: []config.DeviceConfig{
				{ID: "device01"},
				{ID: "device02"},
			},
			want: "device03",
		},
		{
			name: "keeps sequence monotonic across gaps",
			devices: []config.DeviceConfig{
				{ID: "device01"},
				{ID: "device03"},
			},
			want: "device04",
		},
		{
			name: "widens after two digits",
			devices: []config.DeviceConfig{
				{ID: "device09"},
				{ID: "device99"},
			},
			want: "device100",
		},
		{
			name: "avoids case insensitive collision",
			devices: []config.DeviceConfig{
				{ID: " Device01 "},
			},
			want: "device02",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nextDefaultDeviceID(test.devices); got != test.want {
				t.Fatalf("nextDefaultDeviceID()=%q, want %q", got, test.want)
			}
		})
	}
}

func TestDeviceConfigFromDTODoesNotUseNetworkInterfaceAsID(t *testing.T) {
	got := deviceConfigFromDTO(deviceConfigDTO{Interface: "wwan9"})
	if got.ID != "" {
		t.Fatalf("ID=%q, want empty so add flow can allocate deviceNN", got.ID)
	}
	if got.Interface != "wwan9" {
		t.Fatalf("Interface=%q, want wwan9", got.Interface)
	}

	got = deviceConfigFromDTO(deviceConfigDTO{ID: " custom-id ", Interface: "wwan9"})
	if got.ID != "custom-id" {
		t.Fatalf("explicit ID=%q, want custom-id", got.ID)
	}
}
