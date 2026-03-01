package client

import (
	"context"
	"math/rand"
	"time"
)

// Retryer executes a request with configurable retry logic.
// It is created from the client's Config and reused across requests.
type Retryer struct {
	maxRetries  int
	baseDelay   time.Duration
	maxDelay    time.Duration
	multiplier  float64
	jitter      bool
	statusCodes map[int]bool
}

// NewRetryer creates a Retryer from configuration.
// Returns nil when retries are disabled (MaxRetries == 0).
func NewRetryer(config *Config) *Retryer {
	if config.MaxRetries <= 0 {
		return nil
	}
	codes := make(map[int]bool, len(config.RetryableStatus))
	for _, c := range config.RetryableStatus {
		codes[c] = true
	}
	return &Retryer{
		maxRetries:  config.MaxRetries,
		baseDelay:   config.RetryBaseDelay,
		maxDelay:    config.RetryMaxDelay,
		multiplier:  config.RetryMultiplier,
		jitter:      config.RetryJitter,
		statusCodes: codes,
	}
}

// ShouldRetry reports whether the request should be retried based on the
// response status code or error. attempt is 0-indexed (first retry = 0).
func (r *Retryer) ShouldRetry(attempt int, resp *Response, err error) bool {
	if attempt >= r.maxRetries {
		return false
	}
	if err != nil && IsRetryable(err) {
		return true
	}
	if resp != nil && r.statusCodes[resp.StatusCode] {
		return true
	}
	return false
}

// Backoff returns the duration to wait before the given retry attempt.
// Uses exponential backoff with optional jitter.
func (r *Retryer) Backoff(attempt int) time.Duration {
	delay := float64(r.baseDelay)
	for i := 0; i < attempt; i++ {
		delay *= r.multiplier
	}
	if delay > float64(r.maxDelay) {
		delay = float64(r.maxDelay)
	}
	if r.jitter {
		delay = delay/2 + rand.Float64()*(delay/2)
	}
	return time.Duration(delay)
}

// Wait sleeps for the backoff duration, returning early if ctx is cancelled.
// Returns ctx.Err() if the context fires before the backoff elapses.
func (r *Retryer) Wait(ctx context.Context, attempt int) error {
	d := r.Backoff(attempt)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
