// SPDX-License-Identifier: GPL-3.0-or-later

package limits

import (
	"context"
	"testing"
	"time"
)

func TestLimiterAllowsBurstThenPaces(t *testing.T) {
	l := NewLimiter(6000) // one token per 10ms, burst 5
	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 8*time.Millisecond {
		t.Fatalf("burst of 5 took %v, want ~instant", d)
	}
	start = time.Now()
	for i := 0; i < 3; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 15*time.Millisecond {
		t.Fatalf("3 paced waits took only %v, limiter is not pacing", d)
	}
}

func TestLimiterWaitHonoursContext(t *testing.T) {
	l := NewLimiter(1) // one per minute, burst 1
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("second wait should not be granted within the timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatal("Wait ignored the context deadline")
	}
}

func TestLimiterDisabledWhenRateIsZero(t *testing.T) {
	l := NewLimiter(0)
	for i := 0; i < 1000; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCacheExpiresAndEvicts(t *testing.T) {
	now := time.Unix(1000, 0)
	c := NewCache[string](time.Minute, 2)
	c.nowFunc = func() time.Time { return now }

	c.Set("a", "1")
	c.Set("b", "2")
	if v, ok := c.Get("a"); !ok || v != "1" {
		t.Fatalf("a = %q, %v", v, ok)
	}
	c.Set("c", "3") // evicts the oldest entry, "a"
	if _, ok := c.Get("a"); ok {
		t.Fatal("oldest entry was not evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b evicted too early")
	}
	now = now.Add(61 * time.Second)
	if _, ok := c.Get("b"); ok {
		t.Fatal("expired entry returned")
	}
}

func TestCacheDisabledWithZeroTTL(t *testing.T) {
	c := NewCache[int](0, 10)
	c.Set("x", 1)
	if _, ok := c.Get("x"); ok {
		t.Fatal("disabled cache returned a value")
	}
}
