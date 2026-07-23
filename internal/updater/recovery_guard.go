package updater

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func guardManualRecovery(paths RuntimePaths) error {
	state, err := LoadState(paths.StateFile())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Phase == PhaseManualRecovery {
		return ErrManualRecoveryRequired
	}
	return nil
}
func guardEngineUpdateStart(paths RuntimePaths, requestedJobID string) error {
	state, err := LoadState(paths.StateFile())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Phase == PhaseManualRecovery {
		return ErrManualRecoveryRequired
	}
	if !state.Phase.Terminal() && !(state.Phase == PhaseChecking && requestedJobID != "" && state.ID == requestedJobID) {
		return ErrUpdateLocked
	}
	return nil
}

// GuardStart is called by the managed service immediately before starting
// VoHive. It permits the intentional start performed by a live update worker,
// while refusing to boot a half-switched or manually recoverable transaction.
func GuardStart(deploymentFile string) error {
	deployment, err := DiscoverDeployment(deploymentFile)
	if err != nil {
		return err
	}
	paths := PathsFor(deploymentFile, deployment)
	if err := paths.Validate(); err != nil {
		return err
	}
	if err := ValidateProductionScope(paths); err != nil {
		return err
	}
	return guardStartPaths(paths)
}

func guardStartPaths(paths RuntimePaths) error {
	state, err := LoadState(paths.StateFile())
	if os.IsNotExist(err) {
		if _, lockErr := os.Lstat(paths.LockFile()); os.IsNotExist(lockErr) {
			return nil
		} else if lockErr != nil {
			return lockErr
		}
		return fmt.Errorf("%w: update lock exists without transaction state", ErrInterruptedUpdate)
	}
	if err != nil {
		return err
	}
	if state.Phase == PhaseManualRecovery {
		return ErrManualRecoveryRequired
	}
	if state.Phase.Terminal() {
		return nil
	}
	active, err := updateLockProcessActive(paths.LockFile())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInterruptedUpdate, err)
	}
	if !active {
		return fmt.Errorf("%w: transaction %s is in phase %s without a live updater", ErrInterruptedUpdate, state.ID, state.Phase)
	}
	return nil
}

func updateLockProcessActive(lockPath string) (bool, error) {
	data, err := os.ReadFile(lockPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fields := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found && key != "" {
			fields[key] = strings.TrimSpace(value)
		}
	}
	pid, err := strconv.Atoi(fields["pid"])
	if err != nil || pid <= 0 {
		return false, errors.New("update lock has an invalid pid")
	}
	if runtime.GOOS != "linux" {
		return false, errors.New("live updater verification is supported only on Linux")
	}
	lockedBootID := fields["boot_id"]
	lockedStartTicks := fields["process_start_ticks"]
	if lockedBootID == "" || lockedStartTicks == "" {
		return false, errors.New("update lock has no Linux process identity")
	}
	if _, err := strconv.ParseUint(lockedStartTicks, 10, 64); err != nil {
		return false, errors.New("update lock has invalid process start ticks")
	}
	currentBootID, currentStartTicks, err := currentUpdateProcessIdentity(pid)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if currentBootID != lockedBootID || currentStartTicks != lockedStartTicks {
		return false, nil
	}
	return true, nil
}

func currentUpdateProcessIdentity(pid int) (string, string, error) {
	if runtime.GOOS != "linux" {
		return "", "", nil
	}
	bootData, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", "", fmt.Errorf("read Linux boot id: %w", err)
	}
	bootID := strings.TrimSpace(string(bootData))
	if bootID == "" || strings.ContainsAny(bootID, " \t\r\n") {
		return "", "", errors.New("Linux boot id is invalid")
	}
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	statData, err := os.ReadFile(statPath)
	if err != nil {
		return "", "", err
	}
	// /proc/PID/stat field 2 is parenthesized and may contain spaces. Strip
	// through its final ')' so index 19 below is the documented field 22.
	closing := strings.LastIndexByte(string(statData), ')')
	if closing < 0 {
		return "", "", errors.New("Linux process stat is malformed")
	}
	statFields := strings.Fields(string(statData)[closing+1:])
	if len(statFields) <= 19 {
		return "", "", errors.New("Linux process stat has no starttime")
	}
	startTicks := statFields[19]
	if value, err := strconv.ParseUint(startTicks, 10, 64); err != nil || value == 0 {
		return "", "", errors.New("Linux process starttime is invalid")
	}
	return bootID, startTicks, nil
}
