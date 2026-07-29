package device

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zanescope/vohive/pkg/logger"
)

const (
	qmiCoreStartupInlineBudget      = 1500 * time.Millisecond
	qmiCoreRetryAttemptBudget       = 15 * time.Second
	qmiCoreStartupRetryMaxAttempts  = 5
	qmiCoreStartupRetryInitialDelay = 2 * time.Second
	qmiCoreStartupRetryMaximumDelay = 32 * time.Second
)

type qmiCoreStartResult struct {
	err   error
	retry bool
	abort bool
}

type qmiCoreRetryState uint8

const (
	qmiCoreRetryStopped qmiCoreRetryState = iota
	qmiCoreRetryRecovered
	qmiCoreRetryExhausted
)

type qmiCoreRetryOutcome struct {
	state    qmiCoreRetryState
	attempts int
	err      error
}

type qmiCoreRetryPolicy struct {
	maxAttempts   int
	attemptBudget time.Duration
	initialDelay  time.Duration
	maximumDelay  time.Duration
}

func defaultQMICoreStartupRetryPolicy() qmiCoreRetryPolicy {
	return qmiCoreRetryPolicy{
		maxAttempts:   qmiCoreStartupRetryMaxAttempts,
		attemptBudget: qmiCoreRetryAttemptBudget,
		initialDelay:  qmiCoreStartupRetryInitialDelay,
		maximumDelay:  qmiCoreStartupRetryMaximumDelay,
	}
}

func runQMIStartCoreAttempt(parent context.Context, startCore func(context.Context) error, budget time.Duration) qmiCoreStartResult {
	if parent == nil {
		parent = context.Background()
	}
	if budget <= 0 {
		budget = qmiCoreStartupInlineBudget
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	err := startCore(ctx)
	if err == nil {
		return qmiCoreStartResult{}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return qmiCoreStartResult{err: err, retry: true}
	}
	if qmiStartCoreFailureShouldAbortWorker(err.Error()) {
		return qmiCoreStartResult{err: err, abort: true}
	}
	return qmiCoreStartResult{err: err, retry: true}
}

func runQMIStartCoreRetryAttempt(parent context.Context, startCore func(context.Context) error, budget time.Duration) error {
	if parent == nil {
		parent = context.Background()
	}
	if budget <= 0 {
		budget = qmiCoreRetryAttemptBudget
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	return startCore(ctx)
}

func (policy qmiCoreRetryPolicy) delayBeforeAttempt(attempt int) time.Duration {
	if attempt <= 0 || policy.initialDelay <= 0 {
		return 0
	}
	delay := policy.initialDelay
	if policy.maximumDelay > 0 && delay > policy.maximumDelay {
		delay = policy.maximumDelay
	}
	for current := 1; current < attempt; current++ {
		if policy.maximumDelay > 0 && delay >= policy.maximumDelay {
			return policy.maximumDelay
		}
		delay *= 2
		if policy.maximumDelay > 0 && delay > policy.maximumDelay {
			delay = policy.maximumDelay
		}
	}
	return delay
}

func newQMIStartCoreRetryContext(parent context.Context, workerStop <-chan struct{}) (context.Context, func()) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	if workerStop == nil {
		return ctx, cancel
	}

	bridgeDone := make(chan struct{})
	go func() {
		defer close(bridgeDone)
		select {
		case <-workerStop:
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, func() {
		cancel()
		<-bridgeDone
	}
}

func waitQMIStartCoreRetry(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func runQMIStartCoreRetryStateMachine(
	parent context.Context,
	startCore func(context.Context) error,
	policy qmiCoreRetryPolicy,
	onFailure func(attempt int, total int, err error, nextDelay time.Duration),
) qmiCoreRetryOutcome {
	if parent == nil {
		parent = context.Background()
	}
	if startCore == nil {
		return qmiCoreRetryOutcome{state: qmiCoreRetryExhausted, err: errors.New("qmi core start function is nil")}
	}
	if policy.maxAttempts <= 0 {
		return qmiCoreRetryOutcome{state: qmiCoreRetryExhausted, err: errors.New("qmi core startup retry policy has no attempts")}
	}

	var lastErr error
	for attempt := 1; attempt <= policy.maxAttempts; attempt++ {
		if !waitQMIStartCoreRetry(parent, policy.delayBeforeAttempt(attempt)) {
			return qmiCoreRetryOutcome{state: qmiCoreRetryStopped, attempts: attempt - 1, err: parent.Err()}
		}
		err := runQMIStartCoreRetryAttempt(parent, startCore, policy.attemptBudget)
		if err == nil {
			return qmiCoreRetryOutcome{state: qmiCoreRetryRecovered, attempts: attempt}
		}
		if parent.Err() != nil {
			return qmiCoreRetryOutcome{state: qmiCoreRetryStopped, attempts: attempt, err: err}
		}
		lastErr = err
		nextDelay := time.Duration(0)
		if attempt < policy.maxAttempts {
			nextDelay = policy.delayBeforeAttempt(attempt + 1)
		}
		if onFailure != nil {
			onFailure(attempt, policy.maxAttempts, err, nextDelay)
		}
	}
	return qmiCoreRetryOutcome{state: qmiCoreRetryExhausted, attempts: policy.maxAttempts, err: lastErr}
}

func (p *Pool) startQMICoreWithStartupBudget(worker *Worker, reason string) error {
	return p.startQMICoreWithStartupBudgetContext(p.ctx, worker, reason)
}

func (p *Pool) startQMICoreWithStartupBudgetContext(parent context.Context, worker *Worker, reason string) error {
	if worker == nil || worker.QMICore == nil {
		return nil
	}
	if p.lifecycle != nil {
		p.lifecycle.BeginRecovery(worker.ID, LifecyclePhaseQMIStarting, reason, qmiLifecycleRecoveryTTL)
	}

	result := runQMIStartCoreAttempt(parent, worker.QMICore.StartCoreContext, qmiCoreStartupInlineBudget)
	if result.err == nil {
		cleanupWorkerStartupSIMAuthLogicalChannels(worker)
		if _, resetErr := p.resetExistingQMIDataConnectionBeforePreference(worker, reason); resetErr != nil {
			logger.Warn(fmt.Sprintf("[%s] QMI Core 启动后清理已有数据连接失败，继续启动", worker.ID), "err", resetErr)
		}
		p.markQMIControlRecovered(worker, reason)
		logger.Debug(fmt.Sprintf("[%s] QMI Core 已启动，网络偏好将异步应用", worker.ID))
		return nil
	}
	if result.abort {
		return result.err
	}

	logger.Warn(fmt.Sprintf("[%s] 启动 QMI Core 未就绪，转入后台重试", worker.ID),
		"err", result.err,
		"startup_budget", qmiCoreStartupInlineBudget.String())
	p.startQMICoreRetryLoop(worker)
	return nil
}

func (p *Pool) startQMICoreRetryLoop(worker *Worker) {
	if p == nil || worker == nil || worker.QMICore == nil || !worker.qmiCoreStartupRetrying.CompareAndSwap(false, true) {
		return
	}
	generation := worker.generation
	go func() {
		defer worker.qmiCoreStartupRetrying.Store(false)
		retryCtx, stopRetry := newQMIStartCoreRetryContext(p.ctx, worker.stop)
		defer stopRetry()

		outcome := runQMIStartCoreRetryStateMachine(
			retryCtx,
			worker.QMICore.StartCoreContext,
			defaultQMICoreStartupRetryPolicy(),
			func(attempt int, total int, err error, nextDelay time.Duration) {
				if nextDelay <= 0 {
					return
				}
				logger.Warn(fmt.Sprintf("[%s] 启动 QMI Core 失败(重试中)", worker.ID),
					"err", err,
					"attempt", attempt,
					"max_attempts", total,
					"next_retry_in", nextDelay.String())
			},
		)
		if outcome.state == qmiCoreRetryStopped || !p.acceptsWorkerCallback(worker, generation) {
			return
		}

		switch outcome.state {
		case qmiCoreRetryRecovered:
			logger.Info(fmt.Sprintf("[%s] QMI Core 已恢复启动", worker.ID), "attempts", outcome.attempts)
			cleanupWorkerStartupSIMAuthLogicalChannels(worker)
			if _, resetErr := p.resetExistingQMIDataConnectionBeforePreference(worker, "qmi_core_recovered"); resetErr != nil {
				logger.Warn(fmt.Sprintf("[%s] QMI Core 恢复后清理既有数据连接失败，跳过自动应用网络偏好", worker.ID), "err", resetErr)
			} else {
				if applyErr := p.applyNetworkPreference(worker); applyErr != nil {
					logger.Warn(fmt.Sprintf("[%s] QMI Core 恢复后自动应用网络偏好失败", worker.ID), "err", applyErr)
				}
			}
			p.markQMIControlRecovered(worker, "qmi_core_recovered")
		case qmiCoreRetryExhausted:
			worker.markQMIControlUnavailable()
			lastErr := outcome.err
			if lastErr == nil {
				lastErr = errors.New("qmi core startup retries exhausted")
			}
			exhaustedErr := fmt.Errorf("QMI core startup retry exhausted after %d attempts: %w", outcome.attempts, lastErr)
			logger.Warn("QMI Core 冷启动重试已耗尽，转入受限 Worker 重建",
				"device", worker.ID,
				"attempts", outcome.attempts,
				"err", exhaustedErr)
			p.maybeScheduleTransportRebuild(worker, HealthLayerQMI, "qmi_start_core_retry_exhausted", exhaustedErr)
		}
	}()
}
