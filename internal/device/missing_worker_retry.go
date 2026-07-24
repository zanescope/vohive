package device

import (
	"strings"
	"time"

	"github.com/zanescope/vohive/internal/config"
)

const missingWorkerLogInterval = 30 * time.Minute

type missingWorkerRetryState struct {
	Attempts    int
	NextAttempt time.Time
}

func missingWorkerRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 3 {
		shift = 3
	}
	delay := time.Minute * time.Duration(1<<shift)
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

// reserveMissingWorkerRetry 原子地判断单台设备本轮是否允许重试，并提前预留下次时间窗。
func (p *Pool) reserveMissingWorkerRetry(deviceID string, now time.Time) (bool, int, time.Duration) {
	if p == nil {
		return false, 0, 0
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false, 0, 0
	}
	if now.IsZero() {
		now = time.Now()
	}

	p.missingWorkerRetryMu.Lock()
	defer p.missingWorkerRetryMu.Unlock()
	if p.missingWorkerRetries == nil {
		p.missingWorkerRetries = make(map[string]missingWorkerRetryState)
	}
	state := p.missingWorkerRetries[deviceID]
	if !state.NextAttempt.IsZero() && now.Before(state.NextAttempt) {
		return false, state.Attempts, state.NextAttempt.Sub(now)
	}
	state.Attempts++
	delay := missingWorkerRetryDelay(state.Attempts)
	state.NextAttempt = now.Add(delay)
	p.missingWorkerRetries[deviceID] = state
	return true, state.Attempts, delay
}

func (p *Pool) clearMissingWorkerRetry(deviceID string) {
	if p == nil {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return
	}
	p.missingWorkerRetryMu.Lock()
	delete(p.missingWorkerRetries, deviceID)
	p.missingWorkerRetryMu.Unlock()
}

func (p *Pool) pruneMissingWorkerRetries(managed []config.DeviceConfig) {
	if p == nil {
		return
	}
	configured := make(map[string]struct{}, len(managed))
	for _, dev := range managed {
		if id := strings.TrimSpace(dev.ID); id != "" {
			configured[id] = struct{}{}
		}
	}

	p.missingWorkerRetryMu.Lock()
	for deviceID := range p.missingWorkerRetries {
		if _, ok := configured[deviceID]; !ok {
			delete(p.missingWorkerRetries, deviceID)
		}
	}
	p.missingWorkerRetryMu.Unlock()
}
