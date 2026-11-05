// SPDX-License-Identifier: GPL-3.0-or-later

// Package limits holds the small rate limiter and TTL cache used by the server.
package limits

import (
	"context"
	"sync"
	"time"
)

// Limiter is a token bucket allowing perMinute requests per minute with a
// small burst. A zero or negative rate disables limiting.
type Limiter struct {
	mu       sync.Mutex
	interval time.Duration
	burst    float64
	tokens   float64
	last     time.Time
	now      func() time.Time
}

// NewLimiter returns a limiter for perMinute requests per minute.
func NewLimiter(perMinute int) *Limiter {
	l := &Limiter{now: time.Now}
	if perMinute > 0 {
		l.interval = time.Minute / time.Duration(perMinute)
		l.burst = float64(min(perMinute, 5))
		l.tokens = l.burst
		l.last = l.now()
	}
	return l
}

// Wait blocks until a request may proceed or ctx ends.
func (l *Limiter) Wait(ctx context.Context) error {
	if l.interval == 0 {
		return nil
	}
	for {
		l.mu.Lock()
		now := l.now()
		l.tokens = min(l.burst, l.tokens+float64(now.Sub(l.last))/float64(l.interval))
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - l.tokens) * float64(l.interval))
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// Cache is a small TTL cache with a fixed entry cap (oldest entry evicted).
type Cache[V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	items   map[string]entry[V]
	order   []string
	nowFunc func() time.Time
}

type entry[V any] struct {
	v   V
	exp time.Time
}

// NewCache returns a cache; ttl <= 0 disables it.
func NewCache[V any](ttl time.Duration, maxEntries int) *Cache[V] {
	return &Cache[V]{ttl: ttl, max: max(maxEntries, 1), items: map[string]entry[V]{}, nowFunc: time.Now}
}

// Get returns a live entry.
func (c *Cache[V]) Get(key string) (V, bool) {
	var zero V
	if c.ttl <= 0 {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || !c.nowFunc().Before(e.exp) {
		delete(c.items, key)
		return zero, false
	}
	return e.v, true
}

// Set stores v under key.
func (c *Cache[V]) Set(key string, v V) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; !exists {
		c.order = append(c.order, key)
		for len(c.items) >= c.max && len(c.order) > 0 {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.items, oldest)
		}
	}
	c.items[key] = entry[V]{v: v, exp: c.nowFunc().Add(c.ttl)}
}
