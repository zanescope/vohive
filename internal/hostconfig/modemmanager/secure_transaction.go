package modemmanager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type ruleWriteKind uint8

const (
	ruleWriteCreate ruleWriteKind = iota + 1
	ruleWriteExchange
)

type ruleWriteTransaction struct {
	manager        *Manager
	kind           ruleWriteKind
	backupPath     string
	backupInfo     os.FileInfo
	backupRevision string
	newInfo        os.FileInfo
	newRevision    string
	oldInfo        os.FileInfo
	oldRevision    string
	finished       bool
}

func (m *Manager) beginRuleWrite(
	ctx context.Context,
	data []byte,
	expected Status,
) (*ruleWriteTransaction, error) {
	dir := filepath.Dir(m.rulePath)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(m.rulePath)+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create rule temporary file: %w", err)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		_ = temp.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o644); err != nil {
		return nil, fmt.Errorf("set rule permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return nil, fmt.Errorf("write rule temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return nil, fmt.Errorf("sync rule temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return nil, fmt.Errorf("close rule temporary file: %w", err)
	}
	newInfo, err := os.Lstat(tempPath)
	if err != nil {
		return nil, fmt.Errorf("inspect rule temporary file: %w", err)
	}
	newRevision := revisionForBytes(data)
	if err := verifyExactRegularFile(tempPath, newInfo, newRevision, true); err != nil {
		return nil, fmt.Errorf("validate rule temporary file: %w", err)
	}

	switch expected.State {
	case StateAbsent:
		latest, _, inspectErr := m.inspectRule()
		if inspectErr != nil {
			return nil, inspectErr
		}
		if latest.State != StateAbsent || latest.Revision != expected.Revision {
			return nil, fmt.Errorf("%w: rule changed before creation", ErrRevisionConflict)
		}
		if err := installNoReplace(tempPath, m.rulePath); err != nil {
			return nil, fmt.Errorf("install managed rule: %w", err)
		}
		removeTemp = false
		txn := &ruleWriteTransaction{
			manager: m, kind: ruleWriteCreate, newInfo: newInfo, newRevision: newRevision,
		}
		if err := verifyExactRegularFile(m.rulePath, newInfo, newRevision, true); err != nil {
			_, rollbackErr := txn.rollback(ctx, false)
			if rollbackErr != nil {
				return nil, fmt.Errorf(
					"validate installed managed rule: %v; rollback failed: %w",
					err, rollbackErr,
				)
			}
			return nil, fmt.Errorf("validate installed managed rule: %w", err)
		}
		if err := m.fileOps.syncDirectory(dir); err != nil {
			_, rollbackErr := txn.rollback(ctx, true)
			if rollbackErr != nil {
				return nil, fmt.Errorf(
					"sync rule directory: %v; rollback failed: %w",
					err, rollbackErr,
				)
			}
			return nil, fmt.Errorf("sync rule directory: %w; rule change was rolled back", err)
		}
		return txn, nil

	case StateManaged:
		latest, _, inspectErr := m.inspectRule()
		if inspectErr != nil {
			return nil, inspectErr
		}
		if latest.State != StateManaged || latest.Revision != expected.Revision {
			return nil, fmt.Errorf("%w: rule changed before exchange", ErrRevisionConflict)
		}
		oldInfo, statErr := os.Lstat(m.rulePath)
		if statErr != nil {
			return nil, fmt.Errorf("inspect managed rule before exchange: %w", statErr)
		}
		if err := verifyExactRegularFile(
			m.rulePath, oldInfo, expected.Revision, true,
		); err != nil {
			return nil, fmt.Errorf(
				"%w: managed rule changed before exchange: %v",
				ErrRevisionConflict, err,
			)
		}
		if err := m.fileOps.exchange(tempPath, m.rulePath); err != nil {
			return nil, fmt.Errorf("atomically exchange managed rule: %w", err)
		}
		removeTemp = false
		txn := &ruleWriteTransaction{
			manager: m, kind: ruleWriteExchange, backupPath: tempPath,
			newInfo: newInfo, newRevision: newRevision,
			oldInfo: oldInfo, oldRevision: expected.Revision,
		}
		backupInfo, backupRevision, backupSnapshotErr := snapshotStableRegularFile(tempPath)
		if backupSnapshotErr == nil {
			txn.backupInfo = backupInfo
			txn.backupRevision = backupRevision
		}
		validationErr := backupSnapshotErr
		if validationErr == nil {
			validationErr = txn.validateExchange()
		}
		if validationErr == nil {
			if m.fileOps.afterExchange != nil {
				validationErr = m.fileOps.afterExchange()
			}
		}
		if err := validationErr; err != nil {
			_, rollbackErr := txn.rollback(ctx, false)
			if rollbackErr != nil {
				return nil, fmt.Errorf(
					"validate exchanged managed rule: %v; rollback failed: %w",
					err, rollbackErr,
				)
			}
			return nil, fmt.Errorf("validate exchanged managed rule: %w", err)
		}
		if err := m.fileOps.syncDirectory(dir); err != nil {
			_, rollbackErr := txn.rollback(ctx, true)
			if rollbackErr != nil {
				return nil, fmt.Errorf(
					"sync exchanged rule directory: %v; rollback failed: %w",
					err, rollbackErr,
				)
			}
			return nil, fmt.Errorf(
				"sync exchanged rule directory: %w; rule change was rolled back",
				err,
			)
		}
		return txn, nil

	default:
		return nil, fmt.Errorf(
			"%w: cannot replace rule in state %q",
			ErrInvalidRequest, expected.State,
		)
	}
}

func (txn *ruleWriteTransaction) validateExchange() error {
	if err := verifyExactRegularFile(
		txn.manager.rulePath, txn.newInfo, txn.newRevision, true,
	); err != nil {
		return fmt.Errorf(
			"%w: canonical rule is not the prepared replacement: %v", ErrRevisionConflict, err,
		)
	}
	if txn.backupInfo == nil || txn.backupRevision == "" {
		return fmt.Errorf("%w: exchanged backup has no stable snapshot", ErrRevisionConflict)
	}
	if err := verifyExactRegularFile(
		txn.backupPath, txn.backupInfo, txn.backupRevision, false,
	); err != nil {
		return fmt.Errorf(
			"%w: exchanged backup changed after capture: %v", ErrRevisionConflict, err,
		)
	}
	if err := verifyExactRegularFile(
		txn.backupPath, txn.oldInfo, txn.oldRevision, true,
	); err != nil {
		return fmt.Errorf(
			"%w: exchanged backup is not the expected managed rule: %v", ErrRevisionConflict, err,
		)
	}
	return nil
}

func (txn *ruleWriteTransaction) rollback(ctx context.Context, reload bool) (bool, error) {
	if txn == nil || txn.manager == nil {
		return false, errors.New("rule write transaction is nil")
	}
	if txn.finished {
		return false, errors.New("rule write transaction is already finished")
	}
	if txn.kind == ruleWriteCreate {
		txn.finished = true
		return txn.manager.rollbackCreatedRule(
			txn.newRevision,
			ctx,
			reload,
		)
	}
	if txn.kind != ruleWriteExchange {
		return false, errors.New("unknown rule write transaction kind")
	}
	return txn.rollbackExchange(ctx, reload)
}

func (txn *ruleWriteTransaction) rollbackExchange(ctx context.Context, reload bool) (bool, error) {
	m := txn.manager
	if err := verifyExactRegularFile(
		m.rulePath, txn.newInfo, txn.newRevision, true,
	); err != nil {
		return false, fmt.Errorf(
			"%w: replacement changed before exchange rollback: %v",
			ErrRevisionConflict, err,
		)
	}
	if txn.backupInfo == nil || txn.backupRevision == "" {
		return false, fmt.Errorf("%w: exchanged backup has no stable snapshot", ErrRevisionConflict)
	}
	if err := verifyExactRegularFile(
		txn.backupPath, txn.backupInfo, txn.backupRevision, false,
	); err != nil {
		return false, fmt.Errorf(
			"%w: captured rule changed before exchange rollback: %v",
			ErrRevisionConflict, err,
		)
	}
	if err := m.fileOps.exchange(txn.backupPath, m.rulePath); err != nil {
		return false, fmt.Errorf("exchange previous managed rule back into place: %w", err)
	}
	txn.finished = true

	canonicalErr := verifyExactRegularFile(
		m.rulePath, txn.backupInfo, txn.backupRevision, false,
	)
	displacedErr := verifyExactRegularFile(
		txn.backupPath, txn.newInfo, txn.newRevision, true,
	)
	if canonicalErr != nil || displacedErr != nil {
		return false, fmt.Errorf(
			"%w: exchange rollback validation failed: canonical=%v, displaced=%v",
			ErrRevisionConflict, canonicalErr, displacedErr,
		)
	}

	var rollbackErrs []error
	reloadConfirmed := false
	if err := m.fileOps.syncDirectory(filepath.Dir(m.rulePath)); err != nil {
		rollbackErrs = append(
			rollbackErrs,
			fmt.Errorf("sync exchanged rollback: %w", err),
		)
	}
	if reload {
		if err := reloadUdevRules(ctx, m.runner); err != nil {
			rollbackErrs = append(
				rollbackErrs,
				fmt.Errorf("reload restored udev rules: %w", err),
			)
		} else {
			reloadConfirmed = true
		}
	}
	if err := verifyExactRegularFile(
		txn.backupPath, txn.newInfo, txn.newRevision, true,
	); err != nil {
		rollbackErrs = append(
			rollbackErrs,
			fmt.Errorf("validate rolled-back replacement before cleanup: %w", err),
		)
	} else if err := m.fileOps.remove(txn.backupPath); err != nil {
		rollbackErrs = append(
			rollbackErrs,
			fmt.Errorf("remove rolled-back replacement: %w", err),
		)
	} else if err := m.fileOps.syncDirectory(filepath.Dir(m.rulePath)); err != nil {
		rollbackErrs = append(
			rollbackErrs,
			fmt.Errorf("sync rolled-back replacement cleanup: %w", err),
		)
	}
	return reloadConfirmed, errors.Join(rollbackErrs...)
}

func (txn *ruleWriteTransaction) commit() error {
	if txn == nil || txn.manager == nil {
		return errors.New("rule write transaction is nil")
	}
	if txn.finished {
		return errors.New("rule write transaction is already finished")
	}
	if txn.kind == ruleWriteCreate {
		txn.finished = true
		return nil
	}
	if txn.kind != ruleWriteExchange {
		return errors.New("unknown rule write transaction kind")
	}
	if err := txn.validateExchange(); err != nil {
		return fmt.Errorf(
			"%w: exchanged rule changed before backup cleanup: %v",
			ErrRevisionConflict, err,
		)
	}
	if err := txn.manager.fileOps.remove(txn.backupPath); err != nil {
		return fmt.Errorf("%w: remove replaced managed rule backup: %w", ErrCleanupIncomplete, err)
	}
	txn.finished = true
	if err := txn.manager.fileOps.syncDirectory(
		filepath.Dir(txn.manager.rulePath),
	); err != nil {
		return fmt.Errorf("%w: sync replaced rule backup cleanup: %w", ErrCleanupIncomplete, err)
	}
	return nil
}

func verifyExactRegularFile(
	path string,
	identity os.FileInfo,
	revision string,
	managed bool,
) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	if identity != nil && !os.SameFile(identity, info) {
		return errors.New("file identity changed")
	}
	data, err := readStableRegularFile(path, info)
	if err != nil {
		return err
	}
	if revisionForBytes(data) != revision {
		return errors.New("file revision changed")
	}
	if managed {
		if _, err := parseManagedRule(data); err != nil {
			return fmt.Errorf("file is not a canonical managed rule: %w", err)
		}
	}
	return nil
}

func snapshotStableRegularFile(path string) (os.FileInfo, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, "", errors.New("path is not a regular file")
	}
	data, err := readStableRegularFile(path, info)
	if err != nil {
		return nil, "", err
	}
	return info, revisionForBytes(data), nil
}
