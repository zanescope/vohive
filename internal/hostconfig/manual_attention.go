package hostconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	manualAttentionSchema = 1
	manualAttentionName   = "manual-attention.json"
	manualAttentionCode   = "changed_without_confirmed_reload"
)

var bootIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type manualAttentionMarker struct {
	Schema    int    `json:"schema"`
	Code      string `json:"code"`
	BootID    string `json:"boot_id,omitempty"`
	Action    Action `json:"action"`
	RequestID string `json:"request_id"`
}

func manualAttentionPath(stateDir string) string {
	return filepath.Join(stateDir, manualAttentionName)
}
func (c *LocalCoordinator) setManualAttentionFallback() {
	if c == nil {
		return
	}
	c.manualAttentionMu.Lock()
	c.manualAttentionFallback = true
	c.manualAttentionMu.Unlock()
}

func (c *LocalCoordinator) clearManualAttentionFallback() {
	if c == nil {
		return
	}
	c.manualAttentionMu.Lock()
	c.manualAttentionFallback = false
	c.manualAttentionMu.Unlock()
}

func (c *LocalCoordinator) manualAttentionFallbackActive() bool {
	if c == nil {
		return false
	}
	c.manualAttentionMu.RLock()
	defer c.manualAttentionMu.RUnlock()
	return c.manualAttentionFallback
}

func readHostBootID() (string, error) {
	file, err := os.Open("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 130))
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if !bootIDPattern.MatchString(value) {
		return "", errors.New("host boot ID is invalid")
	}
	return value, nil
}

func (c *LocalCoordinator) recordManualAttention(action Action, requestID string, evidence WorkerResult) error {
	if c == nil {
		return ErrOperationIndeterminate
	}
	bootID, bootErr := c.currentBootID()
	evidence.Schema = workerSchema
	evidence.ID = requestID
	evidence.Action = action
	evidence.BootID = bootID
	evidence.ManualAttention = true
	evidence.WarningCode = ""
	evidenceErr := atomicWriteJSON(workerResultPath(c.stateDir), evidence, 0o600)

	marker := manualAttentionMarker{
		Schema: manualAttentionSchema, Code: manualAttentionCode,
		BootID: bootID, Action: action, RequestID: requestID,
	}
	markerErr := atomicWriteJSON(manualAttentionPath(c.stateDir), marker, 0o600)
	if markerErr != nil {
		if evidenceErr != nil || bootErr != nil {
			c.setManualAttentionFallback()
		}
		return errors.Join(
			fmt.Errorf("persist manual-attention marker: %w", markerErr),
			wrapManualAttentionPersistenceError("persist fallback result", evidenceErr),
			wrapManualAttentionPersistenceError("read host boot ID", bootErr),
		)
	}

	c.clearManualAttentionFallback()
	if consumeErr := c.consumeWorkerEvidence(requestID, action, bootID); consumeErr != nil {
		return fmt.Errorf("consume fallback result after persisting manual-attention marker: %w", consumeErr)
	}
	return nil
}

func (c *LocalCoordinator) currentBootID() (string, error) {
	if c == nil || c.bootIDProvider == nil {
		return "", errors.New("host boot ID provider is unavailable")
	}
	value, err := c.bootIDProvider()
	if err != nil {
		return "", err
	}
	value = strings.TrimSpace(value)
	if !bootIDPattern.MatchString(value) {
		return "", errors.New("host boot ID is invalid")
	}
	return value, nil
}

func wrapManualAttentionPersistenceError(context string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", context, err)
}

func (c *LocalCoordinator) consumeWorkerEvidence(requestID string, action Action, bootID string) error {
	path := workerResultPath(c.stateDir)
	result, exists, err := loadOptionalWorkerResult(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if result.ID != requestID || result.Action != action || result.BootID != bootID || !result.ManualAttention {
		return errors.New("fallback result changed before it could be consumed")
	}
	return removeRegularMetadata(path)
}

func loadManualAttention(path string) (manualAttentionMarker, bool, error) {
	var marker manualAttentionMarker
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return marker, false, nil
	} else if err != nil {
		return marker, true, err
	}
	file, err := openBoundedRegularFile(path, maxWorkerFileBytes)
	if err != nil {
		return marker, true, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxWorkerFileBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return marker, true, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return marker, true, err
	}
	if marker.Schema != manualAttentionSchema || marker.Code != manualAttentionCode ||
		(marker.Action != ActionInstall && marker.Action != ActionUninstall) ||
		!workerRequestIDPattern.MatchString(marker.RequestID) ||
		(marker.BootID != "" && !bootIDPattern.MatchString(marker.BootID)) {
		return marker, true, errors.New("manual-attention metadata is invalid")
	}
	return marker, true, nil
}

func loadOptionalWorkerResult(path string) (WorkerResult, bool, error) {
	var result WorkerResult
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return result, false, nil
	} else if err != nil {
		return result, true, err
	}
	result, err := loadWorkerResult(path)
	return result, true, err
}

func loadOptionalWorkerRequest(path string) (WorkerRequest, bool, error) {
	var request WorkerRequest
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return request, false, nil
	} else if err != nil {
		return request, true, err
	}
	request, err := loadWorkerRequest(path)
	return request, true, err
}

func (c *LocalCoordinator) applyManualAttention(status Status, activeRequest *WorkerRequest) Status {
	if c == nil {
		return status
	}
	bootID, bootErr := c.currentBootID()
	requestPath := workerRequestPath(c.stateDir)
	markerPath := manualAttentionPath(c.stateDir)
	resultPath := workerResultPath(c.stateDir)
	request, requestExists, requestErr := loadOptionalWorkerRequest(requestPath)
	activeRequestMatched := false
	if requestExists {
		if requestErr != nil {
			return lockManualAttentionStatus(status,
				"宿主机配置操作日志无法验证。请由 root 核对 udev 规则并删除 "+requestPath+"、"+resultPath+" 和 "+markerPath+"，再重启 VoHive。")
		}
		if bootErr != nil {
			return lockManualAttentionStatus(status,
				"无法读取当前启动标识，不能确认未完成的宿主机配置操作。请由 root 核对 udev 规则并清理 "+requestPath+"、"+resultPath+" 和 "+markerPath+"，再重启 VoHive。")
		}
		if activeRequest != nil && workerRequestsEqual(request, *activeRequest) && request.BootID == bootID {
			activeRequestMatched = true
			requestExists = false
		} else if request.BootID != bootID {
			if err := removeRegularMetadata(resultPath); err != nil {
				return lockManualAttentionStatus(status,
					"旧启动周期的宿主机配置结果无法清理。请由 root 核对并删除 "+requestPath+"、"+resultPath+" 和 "+markerPath+"，再重启 VoHive。")
			}
			if err := c.consumeWorkerRequest(request); err != nil {
				return lockManualAttentionStatus(status,
					"旧启动周期的宿主机配置操作日志无法清理。请由 root 核对并删除 "+requestPath+"、"+resultPath+" 和 "+markerPath+"，再重启 VoHive。")
			}
			requestExists = false
			c.clearManualAttentionFallback()
		}
	}
	if activeRequest != nil && !activeRequestMatched {
		return lockManualAttentionStatus(status,
			"当前宿主机配置操作的日志已缺失或被替换。请重启宿主机后重新检查；若暂时无法重启，请由 root 重新加载并核对 udev 规则后清理 "+requestPath+"、"+resultPath+" 和 "+markerPath+"，再重启 VoHive。")
	}
	if requestExists {
		return lockManualAttentionStatus(status,
			"宿主机存在同一启动周期内未完成确认的配置操作。请重启宿主机后重新检查；若暂时无法重启，请由 root 执行 'udevadm control --reload-rules'，核对受管规则后删除 "+requestPath+"、"+resultPath+" 和 "+markerPath+"，再重启 VoHive。")
	}

	marker, markerExists, markerErr := loadManualAttention(markerPath)
	if markerExists && markerErr == nil && marker.BootID != "" && bootErr == nil && marker.BootID != bootID {
		if err := c.consumeWorkerEvidence(marker.RequestID, marker.Action, marker.BootID); err != nil {
			markerErr = fmt.Errorf("clear stale fallback result after host reboot: %w", err)
		} else if err := removeRegularMetadata(markerPath); err != nil {
			markerErr = fmt.Errorf("clear stale manual-attention marker after host reboot: %w", err)
		} else {
			markerExists = false
			c.clearManualAttentionFallback()
		}
	}
	if markerExists {
		reason := "宿主机配置结果需要人工确认。请重启宿主机后重新检查；若暂时无法重启，请由 root 执行 'udevadm control --reload-rules'，核对受管规则后删除 " + markerPath + " 和 " + resultPath + "，再重启 VoHive。"
		if markerErr != nil {
			reason = "人工确认状态元数据无法验证。" + reason
		}
		return lockManualAttentionStatus(status, reason)
	}

	result, resultExists, resultErr := loadOptionalWorkerResult(resultPath)
	if resultExists {
		if resultErr != nil {
			return lockManualAttentionStatus(status,
				"人工确认结果元数据无法验证。请由 root 核对 udev 规则并删除 "+resultPath+"，再重启 VoHive。")
		}
		if bootErr != nil || result.BootID == "" {
			return lockManualAttentionStatus(status,
				"人工确认结果缺少可验证的启动周期。请由 root 核对 udev 规则并删除 "+resultPath+"，再重启 VoHive。")
		}
		if result.BootID != bootID {
			if err := removeRegularMetadata(resultPath); err != nil {
				return lockManualAttentionStatus(status,
					"旧启动周期的人工确认结果无法清理。请由 root 核对并删除 "+resultPath+"，再重启 VoHive。")
			}
			resultExists = false
			c.clearManualAttentionFallback()
		}
	}
	if resultExists {
		needsAttention, checkErr := c.workerResultNeedsManualAttention(result)
		if needsAttention {
			reason := "宿主机配置结果需要人工确认。请重启宿主机后重新检查；若暂时无法重启，请由 root 执行 'udevadm control --reload-rules'，核对受管规则后删除 " + resultPath + "，再重启 VoHive。"
			if bootErr != nil || checkErr != nil {
				reason = "人工确认结果无法完整验证。" + reason
			}
			return lockManualAttentionStatus(status, reason)
		}
	}
	if c.manualAttentionFallbackActive() {
		return lockManualAttentionStatus(status,
			"宿主机配置结果需要人工确认，且恢复锁未能持久保存。请由 root 重新加载并核对 udev 规则，再重启 VoHive。")
	}
	return status
}

func (c *LocalCoordinator) workerResultNeedsManualAttention(result WorkerResult) (bool, error) {
	if result.ManualAttention || result.Result.ReloadIndeterminate {
		return true, nil
	}
	if result.Result.Changed && result.Result.Reloaded && result.Error != "" &&
		result.WarningCode != workerWarningCleanupIncomplete {
		return true, nil
	}
	if !result.Result.Changed || !result.Result.Reloaded {
		return false, nil
	}
	if err := c.verifyCommittedPostcondition(result.Action, result.Result); err != nil {
		return true, err
	}
	return false, nil
}

func lockManualAttentionStatus(status Status, reason string) Status {
	status.Status = StateModified
	status.ManualAttention = true
	status.CanInstall = false
	status.CanUninstall = false
	status.RequiresReplug = false
	status.Warning = ""
	status.Reason = reason
	return status
}
