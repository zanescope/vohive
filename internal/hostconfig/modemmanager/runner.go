package modemmanager

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type osRunner struct{}

func (osRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func reloadUdevRules(ctx context.Context, runner Runner) error {
	output, err := runner.Run(ctx, "udevadm", "control", "--reload-rules")
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("reload udev rules: %w", err)
	}
	return fmt.Errorf("reload udev rules: %w: %s", err, detail)
}
