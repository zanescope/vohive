package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type CommandError struct {
	Name     string
	Output   string
	ExitCode int
	Err      error
}

func (e *CommandError) Error() string {
	detail := strings.TrimSpace(e.Output)
	if detail == "" {
		return fmt.Sprintf("%s failed: %v", e.Name, e.Err)
	}
	return fmt.Sprintf("%s failed: %v: %s", e.Name, e.Err, detail)
}

func (e *CommandError) Unwrap() error { return e.Err }

type OSCommandRunner struct{}

func (OSCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err == nil {
		return output, nil
	}
	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	return output, &CommandError{Name: name, Output: string(output), ExitCode: exitCode, Err: err}
}

type ServiceController interface {
	Stop(context.Context) error
	Start(context.Context) error
	Active(context.Context) (bool, error)
}

type managedService struct {
	typeName InstallType
	runner   CommandRunner
}

func NewServiceController(installType InstallType, runner CommandRunner) ServiceController {
	if runner == nil {
		runner = OSCommandRunner{}
	}
	return &managedService{typeName: installType, runner: runner}
}

func (s *managedService) Stop(ctx context.Context) error {
	switch s.typeName {
	case InstallSystemd:
		_, err := s.runner.Run(ctx, "systemctl", "stop", "vohive.service")
		return err
	case InstallOpenWrt:
		_, err := s.runner.Run(ctx, "/etc/init.d/vohive", "stop")
		return err
	default:
		return ErrPortableUnsupported
	}
}

func (s *managedService) Start(ctx context.Context) error {
	switch s.typeName {
	case InstallSystemd:
		_, err := s.runner.Run(ctx, "systemctl", "start", "vohive.service")
		return err
	case InstallOpenWrt:
		_, err := s.runner.Run(ctx, "/etc/init.d/vohive", "start")
		return err
	default:
		return ErrPortableUnsupported
	}
}

func (s *managedService) Active(ctx context.Context) (bool, error) {
	var name string
	var args []string
	var inactiveCode int
	switch s.typeName {
	case InstallSystemd:
		name, args, inactiveCode = "systemctl", []string{"is-active", "--quiet", "vohive.service"}, 3
	case InstallOpenWrt:
		name, args, inactiveCode = "/etc/init.d/vohive", []string{"running"}, 1
	default:
		return false, ErrPortableUnsupported
	}
	_, err := s.runner.Run(ctx, name, args...)
	if err == nil {
		return true, nil
	}
	var commandErr *CommandError
	if errors.As(err, &commandErr) && commandErr.ExitCode == inactiveCode {
		return false, nil
	}
	return false, fmt.Errorf("determine VoHive service activity: %w", err)
}

type ReadyChecker interface {
	Ready(context.Context, string) error
}

type HTTPReadyChecker struct {
	Client *http.Client
}

func (h HTTPReadyChecker) Ready(ctx context.Context, endpoint string) error {
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("readiness endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}

type JobLauncher interface {
	Launch(context.Context, InstallType) error
}

type ServiceJobLauncher struct {
	Runner CommandRunner
}

func (l ServiceJobLauncher) Launch(ctx context.Context, installType InstallType) error {
	runner := l.Runner
	if runner == nil {
		runner = OSCommandRunner{}
	}
	switch installType {
	case InstallSystemd:
		_, err := runner.Run(ctx, "systemctl", "start", "--no-block", "vohive-update.service")
		return err
	case InstallOpenWrt:
		_, err := runner.Run(ctx, "/etc/init.d/vohive-update", "start")
		return err
	default:
		return ErrPortableUnsupported
	}
}

type Capabilities struct {
	Channel     Channel     `json:"channel"`
	InstallType InstallType `json:"install_type"`
	Layout      Layout      `json:"layout"`
	CanCheck    bool        `json:"can_check"`
	CanUpdate   bool        `json:"can_update"`
	CanRollback bool        `json:"can_rollback"`
	Reason      string      `json:"reason,omitempty"`
}

func DetectCapabilities(deployment Deployment) Capabilities {
	capabilities := Capabilities{
		Channel:     deployment.Channel,
		InstallType: deployment.InstallType,
		Layout:      deployment.Layout,
		CanCheck:    true,
		CanUpdate:   true,
		CanRollback: deployment.LastGoodVersion != "",
	}
	if deployment.Channel == ChannelPinned {
		capabilities.CanCheck = false
		capabilities.CanUpdate = false
		capabilities.Reason = "pinned deployments do not check moving release channels; select stable or beta explicitly"
		return capabilities
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		capabilities.CanUpdate = false
		capabilities.CanRollback = false
		capabilities.Reason = "container updates require the host-side vohivectl agent"
		return capabilities
	}
	if deployment.InstallType == InstallPortable {
		capabilities.CanUpdate = false
		capabilities.CanRollback = false
		capabilities.Reason = ErrPortableUnsupported.Error()
	}
	return capabilities
}

func waitReady(ctx context.Context, checker ReadyChecker, endpoint string, timeout, interval, stableFor time.Duration) error {
	if checker == nil {
		return errors.New("readiness checker is required")
	}
	if endpoint == "" {
		endpoint = DefaultReadyURL
	}
	deadline := time.Now().Add(timeout)
	consecutive := 0
	for {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := checker.Ready(checkCtx, endpoint)
		cancel()
		if err == nil {
			consecutive++
			if consecutive >= 3 {
				break
			}
		} else {
			consecutive = 0
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service did not become ready within %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	if stableFor <= 0 {
		return nil
	}
	stableDeadline := time.Now().Add(stableFor)
	for time.Now().Before(stableDeadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := checker.Ready(checkCtx, endpoint)
		cancel()
		if err != nil {
			return fmt.Errorf("service lost readiness during stabilization: %w", err)
		}
	}
	return nil
}
