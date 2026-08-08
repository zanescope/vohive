package hostconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
)

func persistWorkerResult(requestPath string, request WorkerRequest, result mmisolation.Result, operationErr error) error {
	warningCode := ""
	if errors.Is(operationErr, mmisolation.ErrCleanupIncomplete) {
		warningCode = workerWarningCleanupIncomplete
	}
	return persistWorkerResultWithWarning(requestPath, request, result, operationErr, warningCode)
}

func persistWorkerResultWithWarning(
	requestPath string, request WorkerRequest, result mmisolation.Result,
	operationErr error, warningCode string,
) error {
	workerResult := WorkerResult{
		Schema:          workerSchema,
		ID:              request.ID,
		Action:          request.Action,
		BootID:          request.BootID,
		ManualAttention: result.ReloadIndeterminate,
		Result:          result,
		WarningCode:     warningCode,
	}
	if operationErr != nil {
		workerResult.Code = workerErrorCode(operationErr)
		workerResult.Error = operationErr.Error()
	}
	if writeErr := atomicWriteJSON(workerResultPath(filepath.Dir(requestPath)), workerResult, 0o600); writeErr != nil {
		if operationErr == nil {
			return writeErr
		}
		return errors.Join(operationErr, fmt.Errorf("persist host configuration worker result: %w", writeErr))
	}
	return operationErr
}

func workerUpdateLockPath(requestPath string) (string, error) {
	hostConfigDir := filepath.Dir(requestPath)
	if filepath.Base(hostConfigDir) != "host-config" {
		return "", fmt.Errorf("%w: request.json must be inside a host-config directory", mmisolation.ErrInvalidRequest)
	}
	info, err := os.Lstat(hostConfigDir)
	if err != nil {
		return "", fmt.Errorf("%w: inspect host-config directory: %v", mmisolation.ErrInvalidRequest, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: host-config directory is unsafe", mmisolation.ErrInvalidRequest)
	}
	stateRoot := filepath.Dir(hostConfigDir)
	if stateRoot == hostConfigDir || filepath.Dir(stateRoot) == stateRoot {
		return "", fmt.Errorf("%w: host-config directory has no safe state root", mmisolation.ErrInvalidRequest)
	}
	lockPath := filepath.Join(stateRoot, "update", "update.lock")
	if !filepath.IsAbs(lockPath) {
		return "", fmt.Errorf("%w: derived update lock path is not absolute", mmisolation.ErrInvalidRequest)
	}
	return lockPath, nil
}
