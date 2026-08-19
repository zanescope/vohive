package qmicore

import (
	"errors"
	"time"

	qmimanager "github.com/zanescope/quectel-qmi-go/pkg/manager"
	"github.com/zanescope/quectel-qmi-go/pkg/qmi"
	"github.com/zanescope/vohive/pkg/logger"
)

const (
	// QMI WDS verbose call-end reason type 3 is Call Manager (CM), and
	// reason 2504 is CM_INVALID_SIM_STATE.
	qmiCallEndReasonTypeCallManager   uint16 = 3
	qmiCallEndReasonCMInvalidSIMState uint16 = 2504
	qmiInvalidSIMDialFailureThreshold        = 5
	qmiInvalidSIMDialFailureWindow           = 3 * time.Minute
	qmiInvalidSIMRecoveryReason              = "qmi_invalid_sim_state"
)

type invalidSIMDialFailureState struct {
	generation  uint64
	consecutive int
	lastFailure time.Time
	escalated   bool
}

func (s *invalidSIMDialFailureState) reset() {
	*s = invalidSIMDialFailureState{}
}

func qmiErrorIsInvalidSIMState(err error) bool {
	if err == nil {
		return false
	}
	var startErr *qmi.StartNetworkError
	if !errors.As(err, &startErr) || startErr == nil || startErr.Reason == nil {
		return false
	}
	return startErr.Reason.Type == qmiCallEndReasonTypeCallManager &&
		startErr.Reason.Code == qmiCallEndReasonCMInvalidSIMState
}

func (s *invalidSIMDialFailureState) observe(generation uint64, err error, now time.Time) (int, bool) {
	if s == nil {
		return 0, false
	}
	if !qmiErrorIsInvalidSIMState(err) {
		s.reset()
		return 0, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	if (s.generation != 0 && generation != 0 && s.generation != generation) ||
		(!s.lastFailure.IsZero() && (now.Before(s.lastFailure) || now.Sub(s.lastFailure) > qmiInvalidSIMDialFailureWindow)) {
		s.reset()
	}
	if generation != 0 {
		s.generation = generation
	}
	s.lastFailure = now
	s.consecutive++
	if s.escalated || s.consecutive < qmiInvalidSIMDialFailureThreshold {
		return s.consecutive, false
	}
	s.escalated = true
	return s.consecutive, true
}

// handleInvalidSIMDialRecovery prevents CM_INVALID_SIM_STATE from remaining in
// the library's reconnect loop forever. The lower layer already performs a
// radio power cycle after repeated dial failures; if the same structured error
// survives five attempts, rebuild the Worker so all QMI clients and SIM state
// are acquired again. The pool's existing recovery controller rate-limits that
// rebuild across Worker generations.
func (m *Manager) handleInvalidSIMDialRecovery(event qmimanager.Event) {
	if m == nil {
		return
	}
	if event.Type == qmimanager.EventConnected {
		m.dialRecoveryMu.Lock()
		m.invalidSIMDialFailures.reset()
		m.dialRecoveryMu.Unlock()
		return
	}
	if event.Type != qmimanager.EventDialFailed {
		return
	}

	m.dialRecoveryMu.Lock()
	count, escalate := m.invalidSIMDialFailures.observe(event.Generation, event.Error, time.Now())
	m.dialRecoveryMu.Unlock()
	if !escalate {
		return
	}

	logger.Warn("QMI 连续报告 SIM 状态无效，升级为受限 Worker 重建",
		"device", m.cfg.ID,
		"failures", count,
		"threshold", qmiInvalidSIMDialFailureThreshold,
		"window", qmiInvalidSIMDialFailureWindow.String(),
		"reason", qmiInvalidSIMRecoveryReason,
		"err", event.Error)
	m.dispatchRecoveryExhausted(qmiInvalidSIMRecoveryReason, event.Error)
}
