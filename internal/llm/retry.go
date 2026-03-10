package llm

import (
	"context"
	"math"
	"time"
)

// RetryConfig holds retry behavior.
type RetryConfig struct {
	Enabled         bool
	MaxRetries      int
	InitialDelay    time.Duration
	MaxDelay        time.Duration
	ExponentialBase float64
}

// CalculateDelay returns delay for the given attempt (0-based).
func (c *RetryConfig) CalculateDelay(attempt int) time.Duration {
	sec := c.InitialDelay.Seconds() * math.Pow(c.ExponentialBase, float64(attempt))
	d := time.Duration(sec * float64(time.Second))
	if d > c.MaxDelay {
		return c.MaxDelay
	}
	return d
}

// RetryExhaustedError is returned when all retries failed.
type RetryExhaustedError struct {
	Attempts int
	LastErr  error
}

func (e *RetryExhaustedError) Error() string {
	return e.LastErr.Error()
}

// DoWithRetry runs fn up to 1 + MaxRetries times with exponential backoff.
// If onRetry is non-nil it is called before each retry with the error and attempt (1-based).
func DoWithRetry(ctx context.Context, cfg RetryConfig, fn func() error, onRetry func(err error, attempt int)) error {
	var lastErr error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if attempt == cfg.MaxRetries {
			break
		}
		if onRetry != nil {
			onRetry(lastErr, attempt+1)
		}
		delay := cfg.CalculateDelay(attempt)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return &RetryExhaustedError{Attempts: cfg.MaxRetries + 1, LastErr: lastErr}
}
