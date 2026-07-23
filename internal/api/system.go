package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/internal/updater"
	"github.com/zanescope/vohive/pkg/logger"
)

var updateJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type updateCheckResponse struct {
	Capabilities updater.Capabilities `json:"capabilities"`
	Candidate    *updater.Candidate   `json:"candidate,omitempty"`
}

type startUpdateRequest struct {
	Channel         updater.Channel `json:"channel"`
	Version         string          `json:"version"`
	CurrentPassword string          `json:"current_password"`
}

const (
	updateAuthorizationWindow = 5 * time.Minute
	updateAuthorizationLimit  = 5
)

func newDefaultUpdateCoordinator() updater.Coordinator {
	verifier, err := updater.DefaultSignatureVerifier()
	if err != nil {
		return updater.NewLocalCoordinator("", nil, nil)
	}
	resolver, err := updater.NewGitHubResolver(&http.Client{Timeout: 30 * time.Second}, verifier)
	if err != nil {
		return updater.NewLocalCoordinator("", nil, nil)
	}
	return updater.NewLocalCoordinator("", resolver, nil)
}

// SetUpdateCoordinator replaces the host update boundary. It is intended for
// tests and embedding; production uses a signed GitHub resolver and an
// independent system service worker.
func (s *Server) SetUpdateCoordinator(coordinator updater.Coordinator) {
	s.updates = coordinator
}

func (s *Server) updateCoordinator() updater.Coordinator {
	if s.updates != nil {
		return s.updates
	}
	return newDefaultUpdateCoordinator()
}

func (s *Server) handleUpdateCapabilities(c *gin.Context) {
	capabilities, err := s.updateCoordinator().Capabilities(c.Request.Context())
	if err != nil {
		writeUpdateError(c, err)
		return
	}
	c.JSON(http.StatusOK, capabilities)
}

func (s *Server) handleUpdateCheck(c *gin.Context) {
	coordinator := s.updateCoordinator()
	capabilities, err := coordinator.Capabilities(c.Request.Context())
	if err != nil {
		writeUpdateError(c, err)
		return
	}
	response := updateCheckResponse{Capabilities: capabilities}
	if !capabilities.CanCheck {
		c.JSON(http.StatusOK, response)
		return
	}

	var request updater.CheckRequest
	if rawChannel := strings.TrimSpace(c.Query("channel")); rawChannel != "" {
		channel, parseErr := updater.ParseChannel(rawChannel)
		if parseErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_channel", "error": parseErr.Error()})
			return
		}
		request.Channel = channel
	}
	candidate, err := coordinator.Check(c.Request.Context(), request)
	if err != nil {
		writeUpdateError(c, err)
		return
	}
	response.Candidate = &candidate
	c.JSON(http.StatusOK, response)
}

func (s *Server) updateAuthorizationAttemptKey(c *gin.Context) string {
	token := s.requestSessionToken(c)
	if token == "" {
		return "remote:" + c.Request.RemoteAddr
	}
	digest := sha256.Sum256([]byte(token))
	return "session:" + hex.EncodeToString(digest[:])
}

func (s *Server) allowUpdateAuthorizationAttempt(key string, now time.Time) bool {
	s.updateAuthMu.Lock()
	defer s.updateAuthMu.Unlock()
	if s.updateAuthAttempts == nil {
		s.updateAuthAttempts = make(map[string]loginAttempt)
	}
	attempt := s.updateAuthAttempts[key]
	if attempt.ResetAt.IsZero() || now.After(attempt.ResetAt) {
		attempt = loginAttempt{ResetAt: now.Add(updateAuthorizationWindow)}
	}
	if attempt.Count >= updateAuthorizationLimit {
		s.updateAuthAttempts[key] = attempt
		return false
	}
	attempt.Count++
	s.updateAuthAttempts[key] = attempt
	return true
}

func (s *Server) clearUpdateAuthorizationAttempts(key string) {
	s.updateAuthMu.Lock()
	delete(s.updateAuthAttempts, key)
	s.updateAuthMu.Unlock()
}

func (s *Server) handleStartUpdateJob(c *gin.Context) {
	var body startUpdateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_request", "error": "channel, exact version, and current password must be valid JSON fields"})
		return
	}
	clientIP := c.ClientIP()
	authorizationKey := s.updateAuthorizationAttemptKey(c)
	if !s.allowUpdateAuthorizationAttempt(authorizationKey, time.Now()) {
		logger.Warn("系统更新二次认证已限速", "username", s.auth.Username, "ip", clientIP, "request_id", requestID(c))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"code":  "update_reauthentication_rate_limited",
			"error": "too many update authorization attempts; try again later",
		})
		return
	}
	if body.CurrentPassword == "" || !checkPassword(s.auth.Password, body.CurrentPassword) {
		logger.Warn("系统更新二次认证失败", "username", s.auth.Username, "ip", clientIP, "request_id", requestID(c))
		c.JSON(http.StatusForbidden, gin.H{
			"code":  "update_reauthentication_required",
			"error": "the current administrator password is required to start an update",
		})
		return
	}
	s.clearUpdateAuthorizationAttempts(authorizationKey)
	if body.Channel != "" {
		channel, err := updater.ParseChannel(string(body.Channel))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_channel", "error": err.Error()})
			return
		}
		body.Channel = channel
	}
	if strings.TrimSpace(body.Version) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "exact_version_required", "error": "an exact checked version is required"})
		return
	}
	version := strings.TrimSpace(body.Version)
	auditFields := []any{
		"username", s.auth.Username,
		"ip", clientIP,
		"request_id", requestID(c),
		"channel", body.Channel,
	}
	logger.Info("系统更新请求已通过二次认证", auditFields...)
	state, err := s.updateCoordinator().Start(c.Request.Context(), updater.UpdateRequest{
		Schema:  1,
		Channel: body.Channel,
		Version: version,
	})
	if err != nil {
		logger.Warn("系统更新任务启动失败", append(auditFields, "err", err)...)
		writeUpdateError(c, err)
		return
	}
	logger.Info("系统更新任务已接受", append(auditFields, "version", version, "job_id", state.ID)...)
	c.JSON(http.StatusAccepted, state)
}

func (s *Server) handleUpdateJobState(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("job_id"))
	if len(jobID) == 0 || len(jobID) > 128 || !updateJobIDPattern.MatchString(jobID) {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_job_id", "error": "invalid update job id"})
		return
	}
	state, err := s.updateCoordinator().State(c.Request.Context(), jobID)
	if err != nil {
		writeUpdateError(c, err)
		return
	}
	c.JSON(http.StatusOK, state)
}

func writeUpdateError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	code := "update_error"
	switch {
	case errors.Is(err, updater.ErrManualRecoveryRequired):
		status, code = http.StatusConflict, "manual_recovery_required"
	case errors.Is(err, updater.ErrUpdateLocked):
		status, code = http.StatusConflict, "update_locked"
	case errors.Is(err, updater.ErrJobNotFound):
		status, code = http.StatusNotFound, "job_not_found"
	case errors.Is(err, updater.ErrInvalidUpdateRequest):
		status, code = http.StatusBadRequest, "invalid_update_request"
	case errors.Is(err, updater.ErrTargetNotApplicable):
		status, code = http.StatusConflict, "target_not_applicable"
	case errors.Is(err, updater.ErrUpdateUnsupported), errors.Is(err, updater.ErrNonReleaseBuild), errors.Is(err, updater.ErrPortableUnsupported):
		status, code = http.StatusUnprocessableEntity, "update_unsupported"
	case errors.Is(err, updater.ErrSignatureUnavailable):
		status, code = http.StatusUnprocessableEntity, "signature_unavailable"
	case errors.Is(err, updater.ErrReleaseUpstream):
		status, code = http.StatusBadGateway, "release_upstream_unavailable"
	}
	c.JSON(status, gin.H{"code": code, "error": err.Error()})
}
