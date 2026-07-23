package netprobe

import (
	"context"
	"sync"
	"time"
)

const minHostRequestInterval = 100 * time.Millisecond

// Coordinator bounds aggregate traffic and per-host request rate across every
// Prober that shares it.
type Coordinator struct {
	semaphore chan struct{}

	mu          sync.Mutex
	cooldowns   map[string]time.Time
	nextRequest map[string]time.Time
	hostGates   map[string]chan struct{}
}

func NewCoordinator(maxConcurrent int) *Coordinator {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	return &Coordinator{
		semaphore:   make(chan struct{}, maxConcurrent),
		cooldowns:   make(map[string]time.Time),
		nextRequest: make(map[string]time.Time),
		hostGates:   make(map[string]chan struct{}),
	}
}

func (c *Coordinator) acquire(ctx context.Context, host string) (func(), error) {
	gate := c.hostGate(host)
	select {
	case <-gate:
		defer func() { gate <- struct{}{} }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if err := c.waitForHost(ctx, host); err != nil {
		return nil, err
	}
	select {
	case c.semaphore <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := c.markHostRequest(host); err != nil {
		<-c.semaphore
		return nil, err
	}
	return func() { <-c.semaphore }, nil
}

func (c *Coordinator) hostGate(host string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	gate := c.hostGates[host]
	if gate == nil {
		gate = make(chan struct{}, 1)
		gate <- struct{}{}
		c.hostGates[host] = gate
	}
	return gate
}

func (c *Coordinator) waitForHost(ctx context.Context, host string) error {
	for {
		c.mu.Lock()
		now := time.Now()
		if until := c.cooldowns[host]; now.Before(until) {
			c.mu.Unlock()
			return ErrHostCooling
		}
		delete(c.cooldowns, host)
		next := c.nextRequest[host]
		if !now.Before(next) {
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()

		timer := time.NewTimer(time.Until(next))
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		}
	}
}

func (c *Coordinator) markHostRequest(host string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if until := c.cooldowns[host]; now.Before(until) {
		return ErrHostCooling
	}
	delete(c.cooldowns, host)
	c.nextRequest[host] = now.Add(minHostRequestInterval)
	return nil
}

func (c *Coordinator) cooldown(host string, delay time.Duration) {
	if host == "" || delay <= 0 {
		return
	}
	until := time.Now().Add(delay)
	c.mu.Lock()
	if current := c.cooldowns[host]; until.After(current) {
		c.cooldowns[host] = until
	}
	c.mu.Unlock()
}
