package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zanescope/vohive/internal/updater"
)

var updateJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type updateCheckResponse struct {
	Capabilities updater.Capabilities `json:"capabilities"`
	Candidate    *updater.Candidate   `json:"candidate,omitempty"`
}

type startUpdateRequest struct {
	Channel updater.Channel `json:"channel"`
	Version string          `json:"version"`
}

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

func (s *Server) handleStartUpdateJob(c *gin.Context) {
	var body startUpdateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_request", "error": "channel and exact version must be valid JSON fields"})
		return
	}
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
	state, err := s.updateCoordinator().Start(c.Request.Context(), updater.UpdateRequest{
		Schema:  1,
		Channel: body.Channel,
		Version: strings.TrimSpace(body.Version),
	})
	if err != nil {
		writeUpdateError(c, err)
		return
	}
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
