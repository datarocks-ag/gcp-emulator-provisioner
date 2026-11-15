// Package client wraps the Google Cloud SDKs behind transport-neutral types, so
// the provisioner reconciles plain structs rather than protobuf messages.
package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"
)

const (
	maxRetries   = 15
	initialDelay = 1 * time.Second
	maxDelay     = 30 * time.Second
	totalTimeout = 5 * time.Minute
)

// retryUntilReady calls probe with exponential backoff until it succeeds, the
// attempts run out, or the context expires. Emulators and real endpoints alike
// may refuse connections for a while after the process starts.
func retryUntilReady(ctx context.Context, service string, probe func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()
	start := time.Now()

	for attempt := 0; attempt <= maxRetries; attempt++ {
		err := probe(ctx)
		if err == nil {
			return nil
		}

		if ctx.Err() != nil {
			return gaveUp(ctx, start)
		}

		delay := time.Duration(float64(initialDelay) * math.Pow(2, float64(attempt)))
		if delay > maxDelay {
			delay = maxDelay
		}

		slog.Warn(service+" not ready, retrying",
			"attempt", attempt+1,
			"max_retries", maxRetries,
			"delay", delay,
			"error", err,
		)

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return gaveUp(ctx, start)
		}
	}

	return fmt.Errorf("failed to connect to %s after %d attempts", service, maxRetries+1)
}

// gaveUp describes why retryUntilReady stopped once its context ended. The
// elapsed time is measured rather than assumed: a caller's own deadline or a
// cancellation can end the wait long before totalTimeout.
func gaveUp(ctx context.Context, start time.Time) error {
	elapsed := time.Since(start)
	if elapsed >= time.Second {
		elapsed = elapsed.Round(time.Second)
	} else {
		elapsed = elapsed.Round(time.Millisecond)
	}

	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("connection cancelled after %s: %w", elapsed, ctx.Err())
	}
	return fmt.Errorf("connection timeout after %s: %w", elapsed, ctx.Err())
}
