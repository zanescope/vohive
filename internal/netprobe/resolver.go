package netprobe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type lookupCacheEntry struct {
	addresses []string
	expiresAt time.Time
}

// LookupCache adds bounded-TTL caching and in-flight request coalescing to any
// family-aware resolver without prescribing a DNS transport or server policy.
type LookupCache struct {
	lookup LookupFunc
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]lookupCacheEntry
	group   singleflight.Group
}

func NewLookupCache(lookup LookupFunc, ttl time.Duration) *LookupCache {
	return &LookupCache{
		lookup:  lookup,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]lookupCacheEntry),
	}
}

func (c *LookupCache) Lookup(ctx context.Context, host string, family Family) ([]string, error) {
	if c == nil || c.lookup == nil {
		return nil, errors.New("public IP probe lookup is not configured")
	}
	key := fmt.Sprintf("%d:%s", family, strings.ToLower(strings.TrimSpace(host)))
	if addresses, ok := c.cached(key); ok {
		return addresses, nil
	}

	result := c.group.DoChan(key, func() (any, error) {
		if addresses, ok := c.cached(key); ok {
			return addresses, nil
		}
		addresses, err := c.lookup(ctx, host, family)
		if err != nil {
			return nil, err
		}
		addresses = append([]string(nil), addresses...)
		if c.ttl > 0 && len(addresses) > 0 {
			c.mu.Lock()
			c.entries[key] = lookupCacheEntry{addresses: addresses, expiresAt: c.now().Add(c.ttl)}
			c.mu.Unlock()
		}
		return addresses, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resolved := <-result:
		if resolved.Err != nil {
			return nil, resolved.Err
		}
		return append([]string(nil), resolved.Val.([]string)...), nil
	}
}

func (c *LookupCache) Forget(host string, family Family) {
	if c == nil {
		return
	}
	key := fmt.Sprintf("%d:%s", family, strings.ToLower(strings.TrimSpace(host)))
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
	c.group.Forget(key)
}

func (c *LookupCache) cached(key string) ([]string, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return append([]string(nil), entry.addresses...), true
}
