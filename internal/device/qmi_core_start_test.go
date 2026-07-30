package device

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/config"
)

func TestRunQMIStartCoreAttemptReturnsAfterStartupBudget(t *testing.T) {
	startCore := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	start := time.Now()
	result := runQMIStartCoreAttempt(context.Background(), startCore, 20*time.Millisecond)

	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline exceeded", result.err)
	}
	if !result.retry {
		t.Fatal("retry = false, want true for startup budget timeout")
	}
	if result.abort {
		t.Fatal("abort = true, want false for startup budget timeout")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("elapsed = %v, want bounded by startup budget", elapsed)
	}
}

func TestRunQMIStartCoreAttemptAbortsKnownFatalStartupError(t *testing.T) {
	result := runQMIStartCoreAttempt(context.Background(), func(context.Context) error {
		return errors.New("open /dev/cdc-wdm9: no such file or directory")
	}, time.Second)

	if !result.abort {
		t.Fatal("abort = false, want true for missing QMI control device")
	}
	if result.retry {
		t.Fatal("retry = true, want false for missing QMI control device")
	}
}

func TestRunQMIStartCoreRetryAttemptIsBounded(t *testing.T) {
	startCore := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	start := time.Now()
	err := runQMIStartCoreRetryAttempt(context.Background(), startCore, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("elapsed = %v, want bounded by retry budget", elapsed)
	}
}

func TestQMICoreRetryPolicyDelayIsCapped(t *testing.T) {
	policy := qmiCoreRetryPolicy{initialDelay: 2 * time.Millisecond, maximumDelay: 5 * time.Millisecond}
	want := []time.Duration{2 * time.Millisecond, 4 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	for i, expected := range want {
		if got := policy.delayBeforeAttempt(i + 1); got != expected {
			t.Fatalf("delayBeforeAttempt(%d) = %v, want %v", i+1, got, expected)
		}
	}
}

func TestRunQMIStartCoreRetryStateMachineExhaustsAtPolicyLimit(t *testing.T) {
	wantErr := errors.New("still unavailable")
	calls := 0
	failures := 0
	outcome := runQMIStartCoreRetryStateMachine(
		context.Background(),
		func(context.Context) error {
			calls++
			return wantErr
		},
		qmiCoreRetryPolicy{maxAttempts: 3, attemptBudget: time.Second},
		func(attempt int, total int, err error, nextDelay time.Duration) {
			failures++
			if attempt > total {
				t.Fatalf("attempt %d exceeds total %d", attempt, total)
			}
			if !errors.Is(err, wantErr) {
				t.Errorf("failure err = %v, want %v", err, wantErr)
			}
			if attempt == total && nextDelay != 0 {
				t.Errorf("final nextDelay = %v, want 0", nextDelay)
			}
		},
	)

	if outcome.state != qmiCoreRetryExhausted {
		t.Fatalf("state = %v, want exhausted", outcome.state)
	}
	if outcome.attempts != 3 || calls != 3 || failures != 3 {
		t.Fatalf("attempts=%d calls=%d failures=%d, want all 3", outcome.attempts, calls, failures)
	}
	if !errors.Is(outcome.err, wantErr) {
		t.Fatalf("err = %v, want %v", outcome.err, wantErr)
	}
}

func TestRunQMIStartCoreRetryStateMachineStopsAfterRecovery(t *testing.T) {
	calls := 0
	outcome := runQMIStartCoreRetryStateMachine(
		context.Background(),
		func(context.Context) error {
			calls++
			if calls == 2 {
				return nil
			}
			return errors.New("not ready")
		},
		qmiCoreRetryPolicy{maxAttempts: 5, attemptBudget: time.Second},
		nil,
	)

	if outcome.state != qmiCoreRetryRecovered {
		t.Fatalf("state = %v, want recovered", outcome.state)
	}
	if outcome.attempts != 2 || calls != 2 {
		t.Fatalf("attempts=%d calls=%d, want both 2", outcome.attempts, calls)
	}
}

func TestQMIStartCoreRetryStopsWithWorkerAndJoinsCancelBridge(t *testing.T) {
	workerStop := make(chan struct{})
	retryCtx, cleanup := newQMIStartCoreRetryContext(context.Background(), workerStop)
	started := make(chan struct{})
	done := make(chan qmiCoreRetryOutcome, 1)
	go func() {
		done <- runQMIStartCoreRetryStateMachine(
			retryCtx,
			func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			},
			qmiCoreRetryPolicy{maxAttempts: 5, attemptBudget: time.Second},
			nil,
		)
	}()

	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("retry attempt did not start")
	}
	close(workerStop)

	select {
	case outcome := <-done:
		if outcome.state != qmiCoreRetryStopped {
			t.Fatalf("state = %v, want stopped", outcome.state)
		}
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("err = %v, want context canceled", outcome.err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("retry state machine did not stop with Worker")
	}

	cleanupDone := make(chan struct{})
	go func() {
		cleanup()
		close(cleanupDone)
	}()
	select {
	case <-cleanupDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("retry cancellation bridge did not exit")
	}
}

func TestStartAllReturnsBeforeQMIDiscoveryCompletes(t *testing.T) {
	origDiscover := discoverQMIDevicesFn
	releaseDiscover := make(chan struct{})
	discoverEntered := make(chan struct{})
	discoverQMIDevicesFn = func() ([]QMIDevice, error) {
		close(discoverEntered)
		<-releaseDiscover
		return nil, nil
	}
	t.Cleanup(func() {
		close(releaseDiscover)
		discoverQMIDevicesFn = origDiscover
	})

	p := NewPool(&config.Config{Devices: []config.DeviceConfig{{
		ID:            "dev-qmi",
		ModemIMEI:     "860000000000001",
		ControlDevice: "/dev/cdc-wdm-test",
		Interface:     "wwan-test",
		DeviceBackend: "qmi",
	}}})
	t.Cleanup(func() { _ = p.Shutdown() })

	done := make(chan error, 1)
	go func() {
		done <- p.StartAll()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StartAll() error = %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("StartAll() blocked on QMI discovery; startup must not wait for device bootstrap")
	}

	select {
	case <-discoverEntered:
	case <-time.After(time.Second):
		t.Fatal("background QMI discovery did not start")
	}
}
