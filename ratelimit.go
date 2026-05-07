package client

import (
	"context"
	"sync"
	"time"
)

type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	rate     float64 // tokens per nanosecond
	lastTick int64   // UnixNano
}

func newTokenBucket(rps float64, burst int) *tokenBucket {
	if rps <= 0 || burst <= 0 {
		return nil
	}
	return &tokenBucket{
		tokens:   float64(burst),
		capacity: float64(burst),
		rate:     rps / 1e9,
		lastTick: time.Now().UnixNano(),
	}
}

func (tb *tokenBucket) Wait(ctx context.Context) error {
	for {
		tb.mu.Lock()
		now := time.Now().UnixNano()
		elapsed := now - tb.lastTick
		if elapsed > 0 {
			tb.lastTick = now
			tb.tokens += float64(elapsed) * tb.rate
			if tb.tokens > tb.capacity {
				tb.tokens = tb.capacity
			}
		}
		if tb.tokens >= 1 {
			tb.tokens--
			tb.mu.Unlock()
			return nil
		}
		waitNs := int64((1 - tb.tokens) / tb.rate)
		tb.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(waitNs)):
		}
	}
}
