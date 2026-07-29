package qmicore

import (
	"errors"
	"strings"
	"testing"

	"github.com/zanescope/vohive/internal/config"
)

func TestValidateQMITransportOwnership(t *testing.T) {
	base := config.DeviceConfig{
		DeviceBackend: "qmi",
		ControlDevice: "/dev/cdc-wdm0",
	}

	tests := []struct {
		name        string
		cfg         config.DeviceConfig
		scan        qmiControlDeviceHolders
		scanErr     error
		wantErrPart string
	}{
		{
			name: "direct unused control device",
			cfg:  base,
		},
		{
			name: "direct held control device",
			cfg:  base,
			scan: qmiControlDeviceHolders{
				Holders: []qmiControlDeviceHolder{{PID: 101, Command: "vohive"}},
			},
			wantErrPart: "direct mode requires an unowned control device",
		},
		{
			name: "proxy held only by qmi proxy",
			cfg: func() config.DeviceConfig {
				cfg := base
				cfg.QMIUseProxy = true
				return cfg
			}(),
			scan: qmiControlDeviceHolders{
				Holders: []qmiControlDeviceHolder{{PID: 202, Command: "/usr/libexec/qmi-proxy"}},
			},
		},
		{
			name: "proxy held by non proxy process",
			cfg: func() config.DeviceConfig {
				cfg := base
				cfg.QMIUseProxy = true
				return cfg
			}(),
			scan: qmiControlDeviceHolders{
				Holders: []qmiControlDeviceHolder{{PID: 303, Command: "ModemWorker"}},
			},
			wantErrPart: "proxy mode requires qmi-proxy-only ownership",
		},
		{
			name: "modem manager owned proxy",
			cfg: func() config.DeviceConfig {
				cfg := base
				cfg.QMIUseProxy = true
				return cfg
			}(),
			scan: qmiControlDeviceHolders{
				Holders: []qmiControlDeviceHolder{{
					PID:               404,
					Command:           "/usr/libexec/qmi-proxy",
					ModemManagerOwned: true,
				}},
			},
			wantErrPart: "owned by ModemManager",
		},
		{
			name:        "holder scan unknown",
			cfg:         base,
			scan:        qmiControlDeviceHolders{Unknown: true},
			wantErrPart: "holder scan incomplete",
		},
		{
			name:        "holder scan failure",
			cfg:         base,
			scanErr:     errors.New("permission denied"),
			wantErrPart: "holder scan failed",
		},
		{
			name: "non qmi backend",
			cfg: config.DeviceConfig{
				DeviceBackend: "at",
				ControlDevice: "/dev/cdc-wdm0",
			},
		},
		{
			name: "unresolved control device",
			cfg:  config.DeviceConfig{DeviceBackend: "qmi"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := detectQMIControlDeviceHolders
			detectQMIControlDeviceHolders = func(path string) (qmiControlDeviceHolders, error) {
				if path != "/dev/cdc-wdm0" {
					t.Fatalf("holder scan path=%q, want /dev/cdc-wdm0", path)
				}
				return tt.scan, tt.scanErr
			}
			defer func() {
				detectQMIControlDeviceHolders = original
			}()

			err := validateQMITransportOwnership(tt.cfg)
			if tt.wantErrPart == "" {
				if err != nil {
					t.Fatalf("validateQMITransportOwnership() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Fatalf("validateQMITransportOwnership() error = %v, want substring %q", err, tt.wantErrPart)
			}
		})
	}
}
