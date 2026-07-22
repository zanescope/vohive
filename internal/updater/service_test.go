package updater

import (
	"context"
	"errors"
	"testing"
)

type serviceRunnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f serviceRunnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func TestServiceActiveOnlyAcceptsDocumentedInactiveStatus(t *testing.T) {
	tests := []struct {
		name        string
		installType InstallType
		exitCode    int
		wantActive  bool
		wantError   bool
	}{
		{name: "systemd active", installType: InstallSystemd, exitCode: 0, wantActive: true},
		{name: "systemd inactive", installType: InstallSystemd, exitCode: 3},
		{name: "systemd command failure", installType: InstallSystemd, exitCode: 4, wantError: true},
		{name: "openwrt active", installType: InstallOpenWrt, exitCode: 0, wantActive: true},
		{name: "openwrt inactive", installType: InstallOpenWrt, exitCode: 1},
		{name: "openwrt command failure", installType: InstallOpenWrt, exitCode: 2, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := serviceRunnerFunc(func(context.Context, string, ...string) ([]byte, error) {
				if test.exitCode == 0 {
					return nil, nil
				}
				return nil, &CommandError{Name: "status", ExitCode: test.exitCode, Err: errors.New("exit")}
			})
			active, err := NewServiceController(test.installType, runner).Active(context.Background())
			if active != test.wantActive {
				t.Fatalf("active=%v want %v", active, test.wantActive)
			}
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v wantError=%v", err, test.wantError)
			}
		})
	}
}

func TestServiceActiveFailsClosedForOpaqueRunnerError(t *testing.T) {
	runner := serviceRunnerFunc(func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("runner unavailable")
	})
	active, err := NewServiceController(InstallSystemd, runner).Active(context.Background())
	if active || err == nil {
		t.Fatalf("active=%v error=%v; want inactive with an explicit error", active, err)
	}
}
