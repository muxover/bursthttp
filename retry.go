package client

import (
	"context"
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

type Retryer struct {
	maxRetries  int
	baseDelay   time.Duration
	maxDelay    time.Duration
	multiplier  float64
	jitter      bool
	statusCodes map[int]bool
}

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

func (r *Retryer) WaitAdaptive(ctx context.Context, attempt, statusCode int, retryAfterHdr string, err error) error {
	d := r.adaptiveDelay(attempt, statusCode, retryAfterHdr, err)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Retryer) adaptiveDelay(attempt, statusCode int, retryAfterHdr string, err error) time.Duration {
	// 429 + Retry-After: honour the server's hint (including 0 = immediate).
	if statusCode == 429 && retryAfterHdr != "" {
		if d, ok := parseRetryAfter(retryAfterHdr); ok {
			if d > r.maxDelay {
				d = r.maxDelay
			}
			return d
		}
	}
	// Timeout or network error: retry immediately (connection blip, not server overload).
	if err != nil && (IsTimeout(err) || isConnectionLevelError(err)) {
		return 0
	}
	// Application-level error (5xx, etc.): exponential backoff.
	return r.Backoff(attempt)
}

func isConnectionLevelError(err error) bool {
	var de *DetailedError
	if errors.As(err, &de) {
		return de.Type == ErrorTypeNetwork
	}
	return errors.Is(err, ErrConnectFailed) || errors.Is(err, ErrConnectionClosed)
}

func parseRetryAfter(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	// Integer seconds — most common form.
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, true
	}
	// HTTP-date (IMF-fixdate, RFC 7231 §7.1.3).
	const httpDate = "Mon, 02 Jan 2006 15:04:05 GMT"
	if t, err := time.Parse(httpDate, s); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}
