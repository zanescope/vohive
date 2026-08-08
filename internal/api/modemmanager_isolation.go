package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/internal/hostconfig"
	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
	"github.com/zanescope/vohive/pkg/logger"
)

const (
	hostConfigAuthorizationWindow = 5 * time.Minute
	hostConfigAuthorizationLimit  = 5
	hostConfigActionBodyLimit     = 16 << 10
)

type modemManagerIsolationActionRequest struct {
	CurrentPassword      string `json:"current_password"`
	ExpectedRevision     string `json:"expected_revision"`
	ExpectedPlanRevision string `json:"expected_plan_revision"`
}

type modemManagerIsolationActionResponse struct {
	hostconfig.Status
	Message string `json:"message"`
}

func (s *Server) SetHostConfigCoordinator(coordinator hostconfig.Coordinator) {
	s.hostConfig = coordinator
}

func (s *Server) SetHostConfigTargetProvider(provider func() []hostconfig.Target) {
	s.hostConfigTargetProvider = provider
}

func (s *Server) hostConfigCoordinator() hostconfig.Coordinator {
	if s.hostConfig != nil {
		return s.hostConfig
	}
	return hostconfig.NewLocalCoordinator()
}

func (s *Server) modemManagerIsolationTargets() []hostconfig.Target {
	if s.hostConfigTargetProvider != nil {
		return append([]hostconfig.Target(nil), s.hostConfigTargetProvider()...)
	}

	targets := make(map[string]hostconfig.Target)
	for _, cfg := range config.ListDevices() {
		if !strings.EqualFold(strings.TrimSpace(cfg.DeviceBackend), "qmi") {
			continue
		}
		id := strings.TrimSpace(cfg.ID)
		if id == "" {
			continue
		}
		targets[id] = hostconfig.Target{
			ID:            id,
			Name:          strings.TrimSpace(cfg.Name),
			USBPath:       strings.TrimSpace(cfg.USBPath),
			ControlDevice: strings.TrimSpace(cfg.ControlDevice),
		}
	}

	if s.pool != nil {
		for _, worker := range s.pool.GetAllWorkers() {
			if worker == nil || !strings.EqualFold(strings.TrimSpace(worker.Config.DeviceBackend), "qmi") {
				continue
			}
			id := strings.TrimSpace(worker.ID)
			if id == "" {
				id = strings.TrimSpace(worker.Config.ID)
			}
			if id == "" {
				continue
			}
			target := targets[id]
			target.ID = id
			if target.Name == "" {
				target.Name = strings.TrimSpace(worker.Config.Name)
			}
			target.USBPath = strings.TrimSpace(worker.Config.USBPath)
			target.ControlDevice = strings.TrimSpace(worker.Config.ControlDevice)
			targets[id] = target
		}
	}

	result := make([]hostconfig.Target, 0, len(targets))
	for _, target := range targets {
		result = append(result, target)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (s *Server) hostConfigAuthorizationAttemptKey(c *gin.Context, action hostconfig.Action) string {
	return string(action) + ":" + s.updateAuthorizationAttemptKey(c)
}

func (s *Server) allowHostConfigAuthorizationAttempt(key string, now time.Time) bool {
	s.hostConfigAuthMu.Lock()
	defer s.hostConfigAuthMu.Unlock()
	if s.hostConfigAuthAttempts == nil {
		s.hostConfigAuthAttempts = make(map[string]loginAttempt)
	}
	attempt := s.hostConfigAuthAttempts[key]
	if attempt.ResetAt.IsZero() || now.After(attempt.ResetAt) {
		attempt = loginAttempt{ResetAt: now.Add(hostConfigAuthorizationWindow)}
	}
	if attempt.Count >= hostConfigAuthorizationLimit {
		s.hostConfigAuthAttempts[key] = attempt
		return false
	}
	attempt.Count++
	s.hostConfigAuthAttempts[key] = attempt
	return true
}

func (s *Server) clearHostConfigAuthorizationAttempts(key string) {
	s.hostConfigAuthMu.Lock()
	delete(s.hostConfigAuthAttempts, key)
	s.hostConfigAuthMu.Unlock()
}

func (s *Server) handleModemManagerIsolationStatus(c *gin.Context) {
	status, err := s.hostConfigCoordinator().Status(c.Request.Context(), s.modemManagerIsolationTargets())
	if err != nil {
		writeHostConfigError(c, err)
		return
	}
	c.JSON(http.StatusOK, status)
}

func (s *Server) handleInstallModemManagerIsolation(c *gin.Context) {
	s.handleModemManagerIsolationAction(c, hostconfig.ActionInstall)
}

func (s *Server) handleUninstallModemManagerIsolation(c *gin.Context) {
	s.handleModemManagerIsolationAction(c, hostconfig.ActionUninstall)
}

func (s *Server) handleModemManagerIsolationAction(c *gin.Context, action hostconfig.Action) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, hostConfigActionBodyLimit)
	var body modemManagerIsolationActionRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || !jsonBodyEndsAtEOF(decoder) {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":  "invalid_host_config_request",
			"error": "current_password, expected_revision, and expected_plan_revision must be valid JSON fields",
		})
		return
	}
	expectedRevision := strings.TrimSpace(body.ExpectedRevision)
	expectedPlanRevision := strings.TrimSpace(body.ExpectedPlanRevision)
	if expectedRevision == "" || expectedPlanRevision == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":  "invalid_host_config_request",
			"error": "expected_revision and expected_plan_revision are required",
		})
		return
	}

	clientIP := c.ClientIP()
	authorizationKey := s.hostConfigAuthorizationAttemptKey(c, action)
	if !s.allowHostConfigAuthorizationAttempt(authorizationKey, time.Now()) {
		logger.Warn("ModemManager 隔离配置二次认证已限速",
			"action", action,
			"username", s.auth.Username,
			"ip", clientIP,
			"request_id", requestID(c))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"code":  "host_config_reauthentication_rate_limited",
			"error": "too many host configuration authorization attempts; try again later",
		})
		return
	}
	if body.CurrentPassword == "" || !checkPassword(s.auth.Password, body.CurrentPassword) {
		logger.Warn("ModemManager 隔离配置二次认证失败",
			"action", action,
			"username", s.auth.Username,
			"ip", clientIP,
			"request_id", requestID(c))
		c.JSON(http.StatusForbidden, gin.H{
			"code":  "host_config_reauthentication_required",
			"error": "the current administrator password is required for this host configuration action",
		})
		return
	}
	s.clearHostConfigAuthorizationAttempts(authorizationKey)

	targets := s.modemManagerIsolationTargets()
	auditFields := []any{
		"action", action,
		"username", s.auth.Username,
		"ip", clientIP,
		"request_id", requestID(c),
		"expected_revision", expectedRevision,
		"target_count", len(targets),
		"expected_plan_revision", expectedPlanRevision,
	}

	logger.Info("ModemManager 隔离配置请求已通过二次认证", auditFields...)
	status, err := s.hostConfigCoordinator().Apply(
		c.Request.Context(),
		action,
		targets,
		expectedRevision,
		expectedPlanRevision,
	)
	if err != nil {
		logger.Warn("ModemManager 隔离配置操作失败", append(auditFields, "err", err)...)

		writeHostConfigError(c, err)
		return
	}
	logger.Info("ModemManager 隔离配置操作完成",
		append(auditFields, "revision", status.Revision, "requires_replug", status.RequiresReplug)...)

	message := "ModemManager 隔离配置未发生变化"
	if status.RequiresReplug {
		if action == hostconfig.ActionInstall {
			message = "隔离规则已写入并重新加载，请在维护窗口逐台重插目标设备"
		} else {
			message = "隔离规则已卸载并重新加载；下次重插后 ModemManager 可能重新接管设备"
		}
	}
	if status.Warning != "" {
		message += " " + status.Warning
	}
	c.JSON(http.StatusOK, modemManagerIsolationActionResponse{Status: status, Message: message})
}

func jsonBodyEndsAtEOF(decoder *json.Decoder) bool {
	var extra any
	err := decoder.Decode(&extra)
	return errors.Is(err, io.EOF)
}

func writeHostConfigError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	code := "host_config_error"
	switch {
	case errors.Is(err, hostconfig.ErrUnsupported):
		status, code = http.StatusUnprocessableEntity, "host_config_unsupported"
	case errors.Is(err, hostconfig.ErrNotInstallable):
		status, code = http.StatusConflict, "host_config_not_installable"
	case errors.Is(err, hostconfig.ErrNotUninstallable):
		status, code = http.StatusConflict, "host_config_not_uninstallable"
	case errors.Is(err, hostconfig.ErrPlanConflict):
		status, code = http.StatusConflict, "host_config_plan_conflict"
	case errors.Is(err, hostconfig.ErrOperationBusy):
		status, code = http.StatusConflict, "host_config_busy"
	case errors.Is(err, hostconfig.ErrOperationIndeterminate):
		status, code = http.StatusConflict, "host_config_indeterminate"
	case errors.Is(err, mmisolation.ErrRevisionConflict):
		status, code = http.StatusConflict, "host_config_revision_conflict"
	case errors.Is(err, mmisolation.ErrForeignRule):
		status, code = http.StatusConflict, "host_config_foreign_rule"
	case errors.Is(err, mmisolation.ErrTamperedRule):
		status, code = http.StatusConflict, "host_config_modified_rule"
	case errors.Is(err, mmisolation.ErrUnsafeUSBPath),
		errors.Is(err, mmisolation.ErrNoStableMatcher),
		errors.Is(err, mmisolation.ErrInvalidRequest):
		status, code = http.StatusUnprocessableEntity, "host_config_invalid_target"
	}
	c.JSON(status, gin.H{"code": code, "error": err.Error()})
}
