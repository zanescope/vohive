package hostconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
	"github.com/zanescope/vohive/internal/updater"
)

const (
	workerSchema                   = 1
	workerRequestName              = "request.json"
	workerResultName               = "result.json"
	maxWorkerFileBytes             = 256 << 10
	maxWorkerTargets               = 256
	workerWarningCleanupIncomplete = "cleanup_incomplete"
)

var (
	workerRequestIDPattern    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	workerPlanRevisionPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type WorkerRequest struct {
	Schema               int                  `json:"schema"`
	ID                   string               `json:"id"`
	Action               Action               `json:"action"`
	BootID               string               `json:"boot_id"`
	Targets              []mmisolation.Target `json:"targets,omitempty"`
	ExpectedRevision     string               `json:"expected_revision"`
	ExpectedPlanRevision string               `json:"expected_plan_revision"`
}

type WorkerResult struct {
	Schema          int                `json:"schema"`
	ID              string             `json:"id"`
	Action          Action             `json:"action"`
	BootID          string             `json:"boot_id,omitempty"`
	ManualAttention bool               `json:"manual_attention,omitempty"`
	Result          mmisolation.Result `json:"result"`
	Code            string             `json:"code,omitempty"`
	Error           string             `json:"error,omitempty"`
	WarningCode     string             `json:"warning_code,omitempty"`
}

func workerRequestPath(stateDir string) string {
	return filepath.Join(stateDir, workerRequestName)
}

func workerResultPath(stateDir string) string {
	return filepath.Join(stateDir, workerResultName)
}

func RunWorker(ctx context.Context, requestPath string, manager *mmisolation.Manager) error {
	requestPath = filepath.Clean(strings.TrimSpace(requestPath))
	if !filepath.IsAbs(requestPath) || filepath.Base(requestPath) != workerRequestName {
		return fmt.Errorf("%w: worker request path must be an absolute request.json path", mmisolation.ErrInvalidRequest)
	}
	lockPath, err := workerUpdateLockPath(requestPath)
	if err != nil {
		return err
	}
	request, err := loadWorkerRequest(requestPath)
	if err != nil {
		return err
	}
	currentBootID, err := readHostBootID()
	if err != nil {
		return persistWorkerResult(requestPath, request, mmisolation.Result{}, fmt.Errorf("read host boot ID: %w", err))
	}
	if currentBootID != request.BootID {
		return persistWorkerResult(requestPath, request, mmisolation.Result{}, fmt.Errorf("%w: request belongs to a different host boot", mmisolation.ErrInvalidRequest))
	}
	var result mmisolation.Result
	lock, err := updater.AcquireUpdateLock(lockPath)
	if errors.Is(err, updater.ErrUpdateLocked) {
		err = ErrOperationBusy
		return persistWorkerResult(requestPath, request, result, err)
	}
	if err != nil {
		return persistWorkerResult(requestPath, request, result, fmt.Errorf("acquire shared update lock: %w", err))
	}
	finish := func(result mmisolation.Result, operationErr error) error {
		if releaseErr := lock.Release(); releaseErr != nil {
			operationErr = errors.Join(operationErr, fmt.Errorf("release shared update lock: %w", releaseErr))
		}
		return persistWorkerResult(requestPath, request, result, operationErr)
	}
	if manager == nil {
		manager, err = mmisolation.New(mmisolation.Options{})
		if err != nil {
			return finish(result, err)
		}
	}

	plan, err := buildPlanSnapshot(manager, planInputsFromWorkerTargets(request.Targets))
	if err != nil {
		return finish(result, err)
	}
	if plan.Revision != request.ExpectedPlanRevision {
		err = fmt.Errorf("%w: expected %q, found %q", ErrPlanConflict, request.ExpectedPlanRevision, plan.Revision)
		return finish(result, err)
	}
	var expectedEntries []mmisolation.Entry
	if request.Action == ActionInstall {
		expectedEntries, err = expectedEntriesFromPlan(plan)
		if err != nil {
			return finish(result, err)
		}
	}
	result = mmisolation.Result{}
	switch request.Action {
	case ActionInstall:
		result, err = manager.Install(ctx, mmisolation.Request{
			Targets:          request.Targets,
			ExpectedRevision: request.ExpectedRevision,
			ExpectedEntries:  expectedEntries,
		})
	case ActionUninstall:
		result, err = manager.Uninstall(ctx, mmisolation.Request{
			ExpectedRevision: request.ExpectedRevision,
		})
	default:
		err = fmt.Errorf("%w: unsupported host configuration action %q", mmisolation.ErrInvalidRequest, request.Action)
	}

	return finish(result, err)
}

func loadWorkerRequest(path string) (WorkerRequest, error) {
	var request WorkerRequest
	file, err := openBoundedRegularFile(path, maxWorkerFileBytes)
	if err != nil {
		return request, fmt.Errorf("open host configuration request: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxWorkerFileBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("%w: decode worker request: %v", mmisolation.ErrInvalidRequest, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return request, fmt.Errorf("%w: %v", mmisolation.ErrInvalidRequest, err)
	}
	if request.Schema != workerSchema || !workerRequestIDPattern.MatchString(request.ID) {
		return request, fmt.Errorf("%w: invalid worker request identity", mmisolation.ErrInvalidRequest)
	}
	if request.Action != ActionInstall && request.Action != ActionUninstall {
		return request, fmt.Errorf("%w: invalid worker request action", mmisolation.ErrInvalidRequest)
	}
	if !bootIDPattern.MatchString(request.BootID) {
		return request, fmt.Errorf("%w: invalid worker request boot ID", mmisolation.ErrInvalidRequest)
	}
	if len(request.Targets) > maxWorkerTargets {
		return request, fmt.Errorf("%w: too many targets", mmisolation.ErrInvalidRequest)
	}
	if request.ExpectedRevision == "" || strings.TrimSpace(request.ExpectedRevision) != request.ExpectedRevision {
		return request, fmt.Errorf("%w: expected revision is required", mmisolation.ErrInvalidRequest)
	}
	if !workerPlanRevisionPattern.MatchString(request.ExpectedPlanRevision) {
		return request, fmt.Errorf("%w: expected plan revision is invalid", mmisolation.ErrInvalidRequest)
	}
	return request, nil
}

func loadWorkerResult(path string) (WorkerResult, error) {
	var result WorkerResult
	file, err := openBoundedRegularFile(path, maxWorkerFileBytes)
	if err != nil {
		return result, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxWorkerFileBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return result, err
	}
	if result.Schema != workerSchema || !workerRequestIDPattern.MatchString(result.ID) {
		return result, ErrWorkerResult
	}
	if result.Action != ActionInstall && result.Action != ActionUninstall {
		return result, ErrWorkerResult
	}
	if !bootIDPattern.MatchString(result.BootID) {
		return result, ErrWorkerResult
	}
	if (result.Error == "") != (result.Code == "") {
		return result, ErrWorkerResult
	}
	if result.Result.Reloaded && !result.Result.Changed {
		return result, ErrWorkerResult
	}
	if result.Result.ReloadIndeterminate &&
		(!result.ManualAttention || result.Result.Reloaded || result.Error == "") {
		return result, ErrWorkerResult
	}
	if result.Result.Changed && !result.Result.Reloaded && !result.Result.ReloadIndeterminate {
		return result, ErrWorkerResult
	}
	if result.WarningCode != "" &&
		(result.WarningCode != workerWarningCleanupIncomplete || result.Error == "" ||
			!result.Result.Changed || !result.Result.Reloaded || result.ManualAttention ||
			result.Result.ReloadIndeterminate) {
		return result, ErrWorkerResult
	}
	if result.Result.Changed && result.Result.Reloaded {
		switch result.Action {
		case ActionInstall:
			if result.Result.State != mmisolation.StateManaged || result.Result.Revision == "" ||
				result.Result.Revision == mmisolation.AbsentRevision {
				return result, ErrWorkerResult
			}
		case ActionUninstall:
			if result.Result.State != mmisolation.StateAbsent || result.Result.Revision != mmisolation.AbsentRevision {
				return result, ErrWorkerResult
			}
		}
	}
	return result, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON contains more than one value")
		}
		return err
	}
	return nil
}

func openBoundedRegularFile(path string, maxBytes int64) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0o077 != 0 || info.Size() > maxBytes {
		return nil, errors.New("file is not a private bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 ||
		!os.SameFile(info, opened) {
		file.Close()
		return nil, errors.New("file changed while it was opened")
	}
	return file, nil
}

func workerErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrPlanConflict),
		errors.Is(err, mmisolation.ErrTargetSnapshotConflict):
		return "plan_conflict"
	case errors.Is(err, ErrOperationBusy):
		return "operation_busy"
	case errors.Is(err, mmisolation.ErrRevisionConflict):
		return "revision_conflict"
	case errors.Is(err, mmisolation.ErrForeignRule):
		return "foreign_rule"
	case errors.Is(err, mmisolation.ErrTamperedRule):
		return "tampered_rule"
	case errors.Is(err, mmisolation.ErrUnsafeUSBPath):
		return "unsafe_usb_path"
	case errors.Is(err, mmisolation.ErrNoStableMatcher):
		return "no_stable_matcher"
	case errors.Is(err, mmisolation.ErrInvalidRequest):
		return "invalid_request"
	default:
		return "worker_error"
	}
}

func workerResultError(result WorkerResult) error {
	if result.Error == "" {
		return nil
	}
	var sentinel error
	switch result.Code {
	case "plan_conflict":
		sentinel = ErrPlanConflict
	case "operation_busy":
		sentinel = ErrOperationBusy
	case "revision_conflict":
		sentinel = mmisolation.ErrRevisionConflict
	case "foreign_rule":
		sentinel = mmisolation.ErrForeignRule
	case "tampered_rule":
		sentinel = mmisolation.ErrTamperedRule
	case "unsafe_usb_path":
		sentinel = mmisolation.ErrUnsafeUSBPath
	case "no_stable_matcher":
		sentinel = mmisolation.ErrNoStableMatcher
	case "invalid_request":
		sentinel = mmisolation.ErrInvalidRequest
	default:
		sentinel = ErrWorkerResult
	}
	return fmt.Errorf("%w: %s", sentinel, result.Error)
}
