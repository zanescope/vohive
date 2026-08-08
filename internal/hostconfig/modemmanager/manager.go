package modemmanager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxRuleFileSize = 1 << 20

type Manager struct {
	rulePath   string
	sysfsRoot  string
	sysfsMount string
	runner     Runner
	fileOps    atomicFileOps
	mu         sync.Mutex
}

type atomicFileOps struct {
	exchange      func(string, string) error
	remove        func(string) error
	syncDirectory func(string) error
	afterExchange func() error
}

type stagedRemoval struct {
	path     string
	info     os.FileInfo
	revision string
}

func (staged stagedRemoval) validate() error {
	if staged.path == "" || staged.info == nil || staged.revision == "" {
		return fmt.Errorf("%w: staged rule has no stable snapshot", ErrRevisionConflict)
	}
	if err := verifyExactRegularFile(staged.path, staged.info, staged.revision, false); err != nil {
		return fmt.Errorf("%w: staged rule changed after capture: %v", ErrRevisionConflict, err)
	}
	return nil
}

func New(options Options) (*Manager, error) {
	if strings.TrimSpace(options.RulePath) == "" {
		options.RulePath = DefaultRulePath
	}
	if strings.TrimSpace(options.SysfsRoot) == "" {
		options.SysfsRoot = DefaultSysfsRoot
	}
	if strings.TrimSpace(options.SysfsMount) == "" {
		options.SysfsMount = DefaultSysfsMount
	}
	if options.Runner == nil {
		options.Runner = osRunner{}
	}

	rulePath, err := cleanAbsolutePath(options.RulePath)
	if err != nil {
		return nil, fmt.Errorf("rule path: %w", err)
	}
	sysfsRoot, err := cleanAbsolutePath(options.SysfsRoot)
	if err != nil {
		return nil, fmt.Errorf("USB sysfs root: %w", err)
	}
	sysfsMount, err := cleanAbsolutePath(options.SysfsMount)
	if err != nil {
		return nil, fmt.Errorf("sysfs mount: %w", err)
	}
	if within, err := pathWithin(sysfsMount, sysfsRoot); err != nil || !within {
		return nil, fmt.Errorf("USB sysfs root must be within the sysfs mount")
	}

	return &Manager{
		rulePath:   rulePath,
		sysfsRoot:  sysfsRoot,
		sysfsMount: sysfsMount,
		runner:     options.Runner,
		fileOps: atomicFileOps{
			exchange:      exchangePaths,
			remove:        os.Remove,
			syncDirectory: syncDirectory,
		},
	}, nil
}

func cleanAbsolutePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	return filepath.Clean(path), nil
}

func (m *Manager) Inspect() (Status, error) {
	if m == nil {
		return Status{}, fmt.Errorf("%w: manager is nil", ErrInvalidRequest)
	}
	status, _, err := m.inspectRule()
	return status, err
}

func (m *Manager) inspectRule() (Status, []byte, error) {
	info, err := os.Lstat(m.rulePath)
	if os.IsNotExist(err) {
		return Status{State: StateAbsent, Revision: AbsentRevision}, nil, nil
	}
	if err != nil {
		return Status{}, nil, fmt.Errorf("inspect managed rule: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(m.rulePath)
		revision := revisionForBytes([]byte("symlink\x00" + target))
		return Status{
			State: StateTampered, Revision: revision,
			Reason: "managed rule path is a symbolic link",
		}, nil, nil
	}
	if !info.Mode().IsRegular() {
		revision := revisionForBytes([]byte("mode\x00" + info.Mode().String()))
		return Status{
			State: StateTampered, Revision: revision,
			Reason: "managed rule path is not a regular file",
		}, nil, nil
	}

	data, err := readStableRegularFile(m.rulePath, info)
	if err != nil {
		return Status{}, nil, fmt.Errorf("read managed rule: %w", err)
	}
	revision := revisionForBytes(data)
	if !bytes.HasPrefix(data, []byte(managedMarker)) {
		state := StateForeign
		reason := "rule path contains a file not managed by VoHive"
		if bytes.HasPrefix(data, []byte(managedMarkerPrefix)) {
			state = StateTampered
			reason = "managed rule header is invalid"
		}
		return Status{State: state, Revision: revision, Reason: reason}, data, nil
	}
	entries, err := parseManagedRule(data)
	if err != nil {
		return Status{
			State: StateTampered, Revision: revision, Reason: err.Error(),
		}, data, nil
	}
	return Status{
		State: StateManaged, Revision: revision, Entries: cloneEntries(entries),
	}, data, nil
}

func readStableRegularFile(path string, before os.FileInfo) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!opened.Mode().IsRegular() || !os.SameFile(before, after) || !os.SameFile(opened, after) {
		return nil, errors.New("rule path changed while it was being inspected")
	}
	if opened.Size() > maxRuleFileSize {
		return nil, errors.New("rule file is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRuleFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRuleFileSize {
		return nil, errors.New("rule file is too large")
	}
	return data, nil
}

func revisionForBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (m *Manager) Install(ctx context.Context, request Request) (Result, error) {
	if m == nil {
		return Result{}, fmt.Errorf("%w: manager is nil", ErrInvalidRequest)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	before, beforeData, err := m.inspectRule()
	if err != nil {
		return Result{}, err
	}
	if err := writableStateError(before); err != nil {
		return resultFromStatus(before, false, false), err
	}
	if err := validateInstallRevision(before, request.ExpectedRevision); err != nil {
		return resultFromStatus(before, false, false), err
	}
	if len(request.Targets) == 0 {
		return resultFromStatus(before, false, false), fmt.Errorf("%w: at least one target is required", ErrInvalidRequest)
	}

	entries := make([]Entry, 0, len(request.Targets))
	for _, target := range request.Targets {
		target.ID = strings.TrimSpace(target.ID)
		matcher, err := m.ResolveMatcher(target)
		if err != nil {
			return resultFromStatus(before, false, false), err
		}
		entries = append(entries, Entry{TargetID: target.ID, Matcher: matcher})
	}
	data, err := Render(entries)
	if err != nil {
		return resultFromStatus(before, false, false), err
	}
	if request.ExpectedEntries != nil {
		expectedData, expectedErr := Render(request.ExpectedEntries)
		if expectedErr != nil {
			return resultFromStatus(before, false, false), fmt.Errorf("validate expected target snapshot: %w", expectedErr)
		}
		if !bytes.Equal(data, expectedData) {
			return resultFromStatus(before, false, false), fmt.Errorf(
				"%w: resolved target matchers differ from the validated plan", ErrTargetSnapshotConflict)
		}
	}
	if before.State == StateManaged && bytes.Equal(beforeData, data) {
		return resultFromStatus(before, false, false), nil
	}

	latest, _, err := m.inspectRule()
	if err != nil {
		return resultFromStatus(before, false, false), err
	}
	if latest.State != before.State || latest.Revision != before.Revision {
		return resultFromStatus(latest, false, false), fmt.Errorf(
			"%w: expected %s, found %s", ErrRevisionConflict, before.Revision, latest.Revision)
	}
	after := Status{State: StateManaged, Revision: revisionForBytes(data), Entries: cloneEntries(entries)}
	if ctx == nil {
		ctx = context.Background()
	}
	transaction, err := m.beginRuleWrite(ctx, data, before)
	if err != nil {
		return m.resultAfterMutationError(before, before, false), err
	}
	if err := reloadUdevRules(ctx, m.runner); err != nil {
		reloadConfirmed, rollbackErr := transaction.rollback(ctx, true)
		if rollbackErr != nil {
			result := m.resultAfterMutationError(before, after, false)
			result.ReloadIndeterminate = !reloadConfirmed
			return result,
				fmt.Errorf("%v; rollback failed: %w", err, rollbackErr)
		}
		result := resultFromStatus(before, false, false)
		result.ReloadIndeterminate = !reloadConfirmed
		return result, fmt.Errorf("%w; rule change was rolled back", err)
	}
	if err := transaction.commit(); err != nil {
		return m.resultAfterMutationError(before, after, true),
			fmt.Errorf("rule was applied but backup cleanup failed: %w", err)
	}
	return resultFromStatus(after, true, true), nil
}

func (m *Manager) rollbackCreatedRule(
	installedRevision string,
	ctx context.Context,
	reload bool,
) (bool, error) {
	current, _, err := m.inspectRule()
	if err != nil {
		return false, err
	}
	if current.State != StateManaged || current.Revision != installedRevision {
		return false, fmt.Errorf("%w: installed rule changed before rollback", ErrRevisionConflict)
	}
	staged, err := m.stageOwnedRemoval(installedRevision)
	if err != nil {
		return false, err
	}
	reloadConfirmed := false
	if reload {
		if err := reloadUdevRules(ctx, m.runner); err != nil {
			if _, restoreErr := m.restoreStagedRemoval(staged, ctx, false); restoreErr != nil {
				return false, fmt.Errorf("reload rollback without new rule: %v; restore failed rule: %w", err, restoreErr)
			}
			return false, fmt.Errorf("reload restored udev rules: %w", err)
		}
		reloadConfirmed = true
	}
	if err := staged.validate(); err != nil {
		return reloadConfirmed, err
	}
	if err := m.fileOps.remove(staged.path); err != nil {
		return reloadConfirmed, fmt.Errorf("remove rolled-back rule: %w", err)
	}
	return reloadConfirmed, m.fileOps.syncDirectory(filepath.Dir(m.rulePath))
}

func (m *Manager) Uninstall(ctx context.Context, request Request) (Result, error) {
	if m == nil {
		return Result{}, fmt.Errorf("%w: manager is nil", ErrInvalidRequest)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	before, _, err := m.inspectRule()
	if err != nil {
		return Result{}, err
	}
	if err := writableStateError(before); err != nil {
		return resultFromStatus(before, false, false), err
	}
	if len(request.Targets) != 0 {
		return resultFromStatus(before, false, false), fmt.Errorf("%w: uninstall does not accept targets", ErrInvalidRequest)
	}
	if before.State == StateAbsent {
		expected := strings.TrimSpace(request.ExpectedRevision)
		if expected != "" && expected != AbsentRevision {
			return resultFromStatus(before, false, false), fmt.Errorf(
				"%w: expected %s, found %s", ErrRevisionConflict, expected, before.Revision)
		}
		return resultFromStatus(before, false, false), nil
	}
	if strings.TrimSpace(request.ExpectedRevision) == "" || request.ExpectedRevision != before.Revision {
		return resultFromStatus(before, false, false), fmt.Errorf(
			"%w: expected %s, found %s", ErrRevisionConflict, request.ExpectedRevision, before.Revision)
	}

	staged, err := m.stageOwnedRemoval(before.Revision)
	if err != nil {
		return m.resultAfterMutationError(before, before, false), err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := reloadUdevRules(ctx, m.runner); err != nil {
		reloadConfirmed, rollbackErr := m.restoreStagedRemoval(staged, ctx, true)
		if rollbackErr != nil {
			current, _ := m.Inspect()
			result := resultFromStatus(current, true, false)
			result.ReloadIndeterminate = !reloadConfirmed
			return result, fmt.Errorf("%v; rollback failed: %w", err, rollbackErr)
		}
		result := resultFromStatus(before, false, false)
		result.ReloadIndeterminate = !reloadConfirmed
		return result, fmt.Errorf("%w; rule removal was rolled back", err)
	}
	if err := staged.validate(); err != nil {
		return Result{State: StateAbsent, Revision: AbsentRevision, Changed: true, Reloaded: true}, err
	}
	if err := m.fileOps.remove(staged.path); err != nil {
		return Result{
			State: StateAbsent, Revision: AbsentRevision, Changed: true, Reloaded: true,
		}, fmt.Errorf("%w: remove staged managed rule: %w", ErrCleanupIncomplete, err)
	}
	if err := m.fileOps.syncDirectory(filepath.Dir(m.rulePath)); err != nil {
		return Result{
			State: StateAbsent, Revision: AbsentRevision, Changed: true, Reloaded: true,
		}, fmt.Errorf("%w: sync rule directory: %w", ErrCleanupIncomplete, err)
	}
	return Result{
		State: StateAbsent, Revision: AbsentRevision, Changed: true, Reloaded: true,
	}, nil
}

func (m *Manager) stageOwnedRemoval(expectedRevision string) (stagedRemoval, error) {
	latest, _, err := m.inspectRule()
	if err != nil {
		return stagedRemoval{}, err
	}
	if latest.State != StateManaged || latest.Revision != expectedRevision {
		return stagedRemoval{}, fmt.Errorf("%w: rule changed before removal", ErrRevisionConflict)
	}
	expectedInfo, err := os.Lstat(m.rulePath)
	if err != nil {
		return stagedRemoval{}, fmt.Errorf("inspect managed rule before removal: %w", err)
	}
	if err := verifyExactRegularFile(m.rulePath, expectedInfo, expectedRevision, true); err != nil {
		return stagedRemoval{}, fmt.Errorf("%w: rule changed before removal: %v", ErrRevisionConflict, err)
	}

	dir := filepath.Dir(m.rulePath)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(m.rulePath)+".removed-*")
	if err != nil {
		return stagedRemoval{}, fmt.Errorf("reserve removal path: %w", err)
	}
	backupPath := temp.Name()
	if closeErr := temp.Close(); closeErr != nil {
		_ = os.Remove(backupPath)
		return stagedRemoval{}, fmt.Errorf("close removal path: %w", closeErr)
	}
	if err := os.Remove(backupPath); err != nil {
		return stagedRemoval{}, fmt.Errorf("prepare removal path: %w", err)
	}
	if err := os.Rename(m.rulePath, backupPath); err != nil {
		return stagedRemoval{}, fmt.Errorf("stage managed rule removal: %w", err)
	}

	info, revision, snapshotErr := snapshotStableRegularFile(backupPath)
	if snapshotErr != nil {
		return stagedRemoval{}, fmt.Errorf(
			"%w: staged rule has no safe recovery snapshot: %v", ErrRevisionConflict, snapshotErr,
		)
	}
	staged := stagedRemoval{path: backupPath, info: info, revision: revision}
	restoreAfterFailure := func(operationErr error) (stagedRemoval, error) {
		if _, restoreErr := m.restoreStagedRemoval(staged, context.Background(), false); restoreErr != nil {
			return stagedRemoval{}, fmt.Errorf("%v; rollback failed: %w", operationErr, restoreErr)
		}
		return stagedRemoval{}, operationErr
	}
	if err := verifyExactRegularFile(staged.path, expectedInfo, expectedRevision, true); err != nil {
		return restoreAfterFailure(fmt.Errorf(
			"%w: staged rule differs from the expected managed rule: %v", ErrRevisionConflict, err,
		))
	}
	if err := m.fileOps.syncDirectory(dir); err != nil {
		return restoreAfterFailure(fmt.Errorf("sync staged rule removal: %w", err))
	}
	return staged, nil
}

func (m *Manager) restoreStagedRemoval(staged stagedRemoval, ctx context.Context, reload bool) (bool, error) {
	if err := staged.validate(); err != nil {
		return false, err
	}
	if _, err := os.Lstat(m.rulePath); err == nil {
		return false, fmt.Errorf("%w: rule path was recreated during rollback", ErrRevisionConflict)
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := restoreNoReplace(staged.path, m.rulePath); err != nil {
		return false, fmt.Errorf("restore removed rule: %w", err)
	}
	if err := verifyExactRegularFile(m.rulePath, staged.info, staged.revision, false); err != nil {
		return false, fmt.Errorf("%w: restored rule changed: %v", ErrRevisionConflict, err)
	}
	if !reload {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := reloadUdevRules(ctx, m.runner); err != nil {
		return false, fmt.Errorf("reload restored udev rules: %w", err)
	}
	return true, nil
}

func installNoReplace(source, destination string) error {
	if err := os.Link(source, destination); err != nil {
		return err
	}
	_ = os.Remove(source)
	return nil
}

func restoreNoReplace(source, destination string) error {
	if err := os.Link(source, destination); err != nil {
		return err
	}
	if err := os.Remove(source); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func writableStateError(status Status) error {
	switch status.State {
	case StateAbsent, StateManaged:
		return nil
	case StateForeign:
		return fmt.Errorf("%w: %s", ErrForeignRule, status.Reason)
	case StateTampered:
		return fmt.Errorf("%w: %s", ErrTamperedRule, status.Reason)
	default:
		return fmt.Errorf("%w: unknown rule state %q", ErrInvalidRequest, status.State)
	}
}

func validateInstallRevision(status Status, expected string) error {
	expected = strings.TrimSpace(expected)
	if status.State == StateAbsent {
		if expected == "" || expected == AbsentRevision {
			return nil
		}
		return fmt.Errorf("%w: expected %s, found %s", ErrRevisionConflict, expected, status.Revision)
	}
	if expected == "" || expected != status.Revision {
		return fmt.Errorf("%w: expected %s, found %s", ErrRevisionConflict, expected, status.Revision)
	}
	return nil
}

func resultFromStatus(status Status, changed, reloaded bool) Result {
	return Result{
		State: status.State, Revision: status.Revision,
		Changed: changed, Reloaded: reloaded, Entries: cloneEntries(status.Entries),
	}
}

func (m *Manager) resultAfterMutationError(
	before Status,
	fallback Status,
	reloaded bool,
) Result {
	current, err := m.Inspect()
	if err != nil {
		current = fallback
	}
	changed := current.State != before.State || current.Revision != before.Revision
	return resultFromStatus(current, changed, reloaded)
}

func cloneEntries(entries []Entry) []Entry {
	if len(entries) == 0 {
		return nil
	}
	return append([]Entry(nil), entries...)
}
