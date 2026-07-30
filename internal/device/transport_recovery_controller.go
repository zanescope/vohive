package device

import (
	"strings"
	"sync"
	"time"
)

const (
	rebuildWindow      = 30 * time.Minute
	rebuildMaxInWindow = 5
)

type TransportRecoveryEventKind string

const (
	TransportRecoveryEventRecoveryExhausted TransportRecoveryEventKind = "recovery_exhausted"
	TransportRecoveryEventHealthSuspect     TransportRecoveryEventKind = "health_suspect"
	TransportRecoveryEventMissingWorker     TransportRecoveryEventKind = "missing_worker"
	TransportRecoveryEventManualReboot      TransportRecoveryEventKind = "manual_reboot"
	TransportRecoveryEventUdevWake          TransportRecoveryEventKind = "udev_wake"
)

type TransportRecoveryEvent struct {
	DeviceID         string
	WorkerGeneration uint64
	Kind             TransportRecoveryEventKind
	Source           string
	Err              error
	At               time.Time
}

type TransportRecoveryBeginStatus string

const (
	TransportRecoveryBeginInvalid         TransportRecoveryBeginStatus = "invalid"
	TransportRecoveryBeginAccepted        TransportRecoveryBeginStatus = "accepted"
	TransportRecoveryBeginDuplicate       TransportRecoveryBeginStatus = "duplicate"
	TransportRecoveryBeginStaleGeneration TransportRecoveryBeginStatus = "stale_generation"
	TransportRecoveryBeginRateLimited     TransportRecoveryBeginStatus = "rate_limited"
)

// TransportRecoveryToken identifies one reserved recovery operation. Tokens are
// deliberately opaque to callers: a stale operation must not be able to finish
// a newer recovery for the same stable device ID.
type TransportRecoveryToken struct {
	deviceID string
	sequence uint64
}

func (token TransportRecoveryToken) valid() bool {
	return token.deviceID != "" && token.sequence != 0
}

type transportRecoveryActive struct {
	event          TransportRecoveryEvent
	token          TransportRecoveryToken
	budgeted       bool
	rebuildStarted bool
}

type TransportRecoveryController struct {
	pool *Pool

	mu                sync.Mutex
	active            map[string]transportRecoveryActive
	workerGenerations map[string]uint64
	rebuildTimes      map[string][]time.Time
	nextToken         uint64
}

func NewTransportRecoveryController(pool *Pool) *TransportRecoveryController {
	return &TransportRecoveryController{
		pool:              pool,
		active:            make(map[string]transportRecoveryActive),
		workerGenerations: make(map[string]uint64),
		rebuildTimes:      make(map[string][]time.Time),
	}
}

// Begin reserves a per-device recovery operation. For budgeted recoveries the
// sliding-window limit is checked here, but the attempt is not recorded until
// Commit confirms that the rebuild runner actually started.
func (c *TransportRecoveryController) Begin(event TransportRecoveryEvent, budgeted bool) (TransportRecoveryToken, TransportRecoveryBeginStatus) {
	return c.beginAt(event, budgeted, time.Now())
}

func (c *TransportRecoveryController) beginAt(event TransportRecoveryEvent, budgeted bool, now time.Time) (TransportRecoveryToken, TransportRecoveryBeginStatus) {
	if c == nil {
		return TransportRecoveryToken{}, TransportRecoveryBeginInvalid
	}
	event.DeviceID = strings.TrimSpace(event.DeviceID)
	if event.DeviceID == "" || !event.startsRecovery() {
		return TransportRecoveryToken{}, TransportRecoveryBeginInvalid
	}
	if event.At.IsZero() {
		event.At = now
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if currentGeneration := c.workerGenerations[event.DeviceID]; currentGeneration != 0 && event.WorkerGeneration != 0 && event.WorkerGeneration != currentGeneration {
		return TransportRecoveryToken{}, TransportRecoveryBeginStaleGeneration
	}
	if _, exists := c.active[event.DeviceID]; exists {
		return TransportRecoveryToken{}, TransportRecoveryBeginDuplicate
	}
	if budgeted && len(c.pruneRebuildTimesLocked(event.DeviceID, now)) >= rebuildMaxInWindow {
		return TransportRecoveryToken{}, TransportRecoveryBeginRateLimited
	}

	c.nextToken++
	if c.nextToken == 0 {
		c.nextToken++
	}
	token := TransportRecoveryToken{deviceID: event.DeviceID, sequence: c.nextToken}
	c.active[event.DeviceID] = transportRecoveryActive{
		event:    event,
		token:    token,
		budgeted: budgeted,
	}
	return token, TransportRecoveryBeginAccepted
}

// Commit records a budgeted rebuild attempt only after the runner has acquired
// its execution slot. It revalidates the generation and window under the same
// lock so a delayed reservation cannot start after becoming stale.
func (c *TransportRecoveryController) Commit(token TransportRecoveryToken) bool {
	return c.commitAt(token, time.Now())
}

func (c *TransportRecoveryController) commitAt(token TransportRecoveryToken, now time.Time) bool {
	if c == nil || !token.valid() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	active, exists := c.active[token.deviceID]
	if !exists || active.token != token || active.rebuildStarted {
		return false
	}
	if currentGeneration := c.workerGenerations[token.deviceID]; currentGeneration != 0 &&
		active.event.WorkerGeneration != 0 &&
		active.event.WorkerGeneration != currentGeneration {
		delete(c.active, token.deviceID)
		return false
	}
	if active.budgeted {
		kept := c.pruneRebuildTimesLocked(token.deviceID, now)
		if len(kept) >= rebuildMaxInWindow {
			delete(c.active, token.deviceID)
			return false
		}
		c.rebuildTimes[token.deviceID] = append(kept, now)
	}
	active.rebuildStarted = true
	c.active[token.deviceID] = active
	return true
}

// Finish releases only the operation represented by token. A late defer from
// an older recovery therefore cannot clear a newer operation's active slot.
func (c *TransportRecoveryController) Finish(token TransportRecoveryToken) bool {
	if c == nil || !token.valid() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	active, exists := c.active[token.deviceID]
	if !exists || active.token != token {
		return false
	}
	delete(c.active, token.deviceID)
	return true
}

func (c *TransportRecoveryController) SetWorkerGeneration(deviceID string, generation uint64) {
	if c == nil {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return
	}
	c.mu.Lock()
	// Rebuild history is keyed by the stable device ID and intentionally
	// survives Worker generations. Generation only gates stale callbacks.
	c.workerGenerations[deviceID] = generation
	c.mu.Unlock()
}

func (c *TransportRecoveryController) SetWorkerGenerationForTest(deviceID string, generation uint64) {
	c.SetWorkerGeneration(deviceID, generation)
}

func (c *TransportRecoveryController) pruneRebuildTimesLocked(deviceID string, now time.Time) []time.Time {
	cutoff := now.Add(-rebuildWindow)
	kept := c.rebuildTimes[deviceID][:0]
	for _, ts := range c.rebuildTimes[deviceID] {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	c.rebuildTimes[deviceID] = kept
	return kept
}

func (event TransportRecoveryEvent) startsRecovery() bool {
	switch event.Kind {
	case TransportRecoveryEventRecoveryExhausted, TransportRecoveryEventHealthSuspect,
		TransportRecoveryEventMissingWorker, TransportRecoveryEventManualReboot, TransportRecoveryEventUdevWake:
		return true
	default:
		return false
	}
}
