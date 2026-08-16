// Package client wraps the Google Cloud SDKs behind transport-neutral types, so
// the provisioner reconciles plain structs rather than protobuf messages.
package client

import (
	"context"
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

	for attempt := 0; attempt <= maxRetries; attempt++ {
		err := probe(ctx)
		if err == nil {
			return nil
		}

		if ctx.Err() != nil {
			return fmt.Errorf("connection timeout after %s: %w", totalTimeout, ctx.Err())
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
			return fmt.Errorf("connection timeout: %w", ctx.Err())
		}
	}

	return fmt.Errorf("failed to connect to %s after %d attempts", service, maxRetries+1)
}
