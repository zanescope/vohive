package device

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/config"
)

func TestMissingWorkerRetryUsesPerDeviceCappedBackoff(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	wantDelays := []time.Duration{
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		5 * time.Minute,
		5 * time.Minute,
	}
	for attempt, wantDelay := range wantDelays {
		due, gotAttempt, delay := p.reserveMissingWorkerRetry("wwan6", now)
		if !due || gotAttempt != attempt+1 || delay != wantDelay {
			t.Fatalf("wwan6 attempt %d: due=%v attempt=%d delay=%s, want true/%d/%s",
				attempt+1, due, gotAttempt, delay, attempt+1, wantDelay)
		}
		now = now.Add(delay)
	}

	due, attempt, delay := p.reserveMissingWorkerRetry("wwan7", time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC))
	if !due || attempt != 1 || delay != time.Minute {
		t.Fatalf("wwan7 inherited wwan6 backoff: due=%v attempt=%d delay=%s", due, attempt, delay)
	}

	p.clearMissingWorkerRetry("wwan6")
	due, attempt, delay = p.reserveMissingWorkerRetry("wwan6", now)
	if !due || attempt != 1 || delay != time.Minute {
		t.Fatalf("wwan6 retry state was not reset: due=%v attempt=%d delay=%s", due, attempt, delay)
	}
}

func TestMissingWorkerRecoveryDoesNotStarveLaterDevice(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	raw := "free_device_limit: 0\ndevices:\n" +
		"- id: wwan5\n  device_backend: at\n  modem_imei: \"111111111111111\"\n" +
		"- id: wwan6\n  device_backend: at\n  modem_imei: \"222222222222222\"\n"
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.InitGlobalManager(configPath); err != nil {
		t.Fatalf("InitGlobalManager() error = %v", err)
	}

	p := NewPool(&config.Config{FreeDeviceLimit: 0})
	defer p.cancel()
	p.missingWorkerRetries = map[string]missingWorkerRetryState{
		"wwan5": {Attempts: 1, NextAttempt: time.Now().Add(time.Hour)},
	}

	if !p.recoverMissingConfiguredWorkers(0) {
		t.Fatal("later due device did not request a rescan")
	}

	p.missingWorkerRetryMu.Lock()
	wwan6 := p.missingWorkerRetries["wwan6"]
	p.missingWorkerRetryMu.Unlock()
	if wwan6.Attempts != 1 {
		t.Fatalf("wwan6 retry state = %+v, want first attempt despite wwan5 backoff", wwan6)
	}
}
