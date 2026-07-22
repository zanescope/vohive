package device

import (
	"testing"
	"time"
)

func TestMissingWorkerRecoveryBackoffAndReset(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	backoff := missingWorkerRecoveryBackoff{}

	allowed, delay := backoff.allow(now)
	if !allowed || delay != time.Minute {
		t.Fatalf("first allow = %v, delay = %s; want true, 1m", allowed, delay)
	}
	allowed, retryAfter := backoff.allow(now.Add(30 * time.Second))
	if allowed || retryAfter != 30*time.Second {
		t.Fatalf("early allow = %v, retry = %s; want false, 30s", allowed, retryAfter)
	}

	tests := []struct {
		at    time.Duration
		delay time.Duration
	}{
		{at: time.Minute, delay: 2 * time.Minute},
		{at: 3 * time.Minute, delay: 4 * time.Minute},
		{at: 7 * time.Minute, delay: 5 * time.Minute},
		{at: 12 * time.Minute, delay: 5 * time.Minute},
	}
	for _, tt := range tests {
		allowed, delay = backoff.allow(now.Add(tt.at))
		if !allowed || delay != tt.delay {
			t.Fatalf("allow at %s = %v, delay = %s; want true, %s", tt.at, allowed, delay, tt.delay)
		}
	}

	backoff.reset()
	allowed, delay = backoff.allow(now.Add(12 * time.Minute))
	if !allowed || delay != time.Minute {
		t.Fatalf("allow after reset = %v, delay = %s; want true, 1m", allowed, delay)
	}
}
