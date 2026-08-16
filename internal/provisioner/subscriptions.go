package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"gcp-emulator-provisioner/internal/client"
	"gcp-emulator-provisioner/internal/config"
)

// Subscription field paths accepted in an UpdateSubscription mask.
const (
	pathSubAckDeadline      = "ack_deadline_seconds"
	pathSubMessageRetention = "message_retention_duration"
	pathSubRetainAcked      = "retain_acked_messages"
	pathSubExactlyOnce      = "enable_exactly_once_delivery"
	pathSubExpiration       = "expiration_policy"
	pathSubLabels           = "labels"
	pathSubPushConfig       = "push_config"
	pathSubDeadLetter       = "dead_letter_policy"
	pathSubRetry            = "retry_policy"
)

func (p *Provisioner) ensureSubscription(ctx context.Context, topicName string, sub config.Subscription, strategy string) error {
	desired, err := buildSubscription(topicName, sub)
	if err != nil {
		return err
	}

	current, err := p.pubsub.GetSubscription(ctx, sub.Name)
	if err != nil && !errors.Is(err, client.ErrNotFound) {
		return fmt.Errorf("checking subscription: %w", err)
	}

	if errors.Is(err, client.ErrNotFound) {
		if p.opts.DryRun {
			slog.Info("Would create subscription",
				"subscription", sub.Name, "topic", topicName,
				"ack_deadline", desired.AckDeadline, "push", desired.Push != nil)
			return nil
		}

		slog.Info("Creating subscription", "subscription", sub.Name, "topic", topicName)
		return p.pubsub.CreateSubscription(ctx, desired)
	}

	if strategy == "create" {
		slog.Info("Skipping existing subscription (strategy=create)", "subscription", sub.Name)
		return nil
	}

	// Immutable drift is reported and then dropped: Pub/Sub rejects an update
	// mask naming these fields, and recreating the subscription would discard
	// its backlog and acknowledgement state.
	for _, d := range immutableDrift(current, desired) {
		slog.Warn("Subscription field cannot be changed after creation, leaving it as is",
			"subscription", sub.Name, "field", d.field, "current", d.current, "configured", d.desired,
			"remedy", "delete the subscription by hand if the new value is required")
	}

	paths := diffSubscription(current, desired)
	if len(paths) == 0 {
		slog.Debug("Subscription up to date", "subscription", sub.Name)
		return nil
	}

	if p.opts.DryRun {
		slog.Info("Would update subscription", "subscription", sub.Name, "fields", paths)
		return nil
	}

	desired.Labels = mergeLabels(current.Labels, desired.Labels)
	// The mask replaces the whole push_config, so push attributes set out of band
	// have to be carried across for the same reason labels do.
	if desired.Push != nil && current.Push != nil {
		desired.Push.Attributes = mergeLabels(current.Push.Attributes, desired.Push.Attributes)
	}

	slog.Info("Updating subscription", "subscription", sub.Name, "fields", paths)
	return applyPaths(ctx, paths,
		func(ctx context.Context, mask []string) error {
			return p.pubsub.UpdateSubscription(ctx, desired, mask)
		},
		func(path string, err error) {
			slog.Warn("Subscription field not supported by this Pub/Sub endpoint, leaving it unchanged",
				"subscription", sub.Name, "field", path, "error", err)
		})
}

// buildSubscription converts a config.Subscription into the transport-neutral form.
func buildSubscription(topicName string, s config.Subscription) (client.Subscription, error) {
	ackDeadline, err := config.ParseDuration(s.AckDeadline)
	if err != nil {
		return client.Subscription{}, fmt.Errorf("parsing ack_deadline: %w", err)
	}
	retention, err := config.ParseDuration(s.MessageRetention)
	if err != nil {
		return client.Subscription{}, fmt.Errorf("parsing message_retention: %w", err)
	}

	out := client.Subscription{
		Name:             s.Name,
		Topic:            topicName,
		AckDeadline:      ackDeadline,
		MessageRetention: retention,
		// Carried across as pointers: nil has to survive this far, or an omitted
		// key would read as an explicit false further down.
		RetainAckedMessages:       s.RetainAckedMessages,
		EnableMessageOrdering:     s.EnableMessageOrdering,
		EnableExactlyOnceDelivery: s.EnableExactlyOnceDelivery,
		Filter:                    s.Filter,
		Labels:                    s.Labels,
		Push:                      buildPushConfig(s),
	}

	switch s.ExpirationTTL {
	case "":
		// Leave whatever policy the subscription already has.
	case config.ExpirationNever:
		out.ExpirationSet = true
	default:
		ttl, err := config.ParseDuration(s.ExpirationTTL)
		if err != nil {
			return client.Subscription{}, fmt.Errorf("parsing expiration_ttl: %w", err)
		}
		out.ExpirationSet = true
		out.ExpirationTTL = ttl
	}

	if s.DeadLetter != nil {
		out.DeadLetter = &client.DeadLetterPolicy{
			Topic:               s.DeadLetter.Topic,
			MaxDeliveryAttempts: int32(s.DeadLetter.MaxDeliveryAttempts),
		}
	}

	if s.Retry != nil {
		minBackoff, err := config.ParseDuration(s.Retry.MinimumBackoff)
		if err != nil {
			return client.Subscription{}, fmt.Errorf("parsing retry.minimum_backoff: %w", err)
		}
		maxBackoff, err := config.ParseDuration(s.Retry.MaximumBackoff)
		if err != nil {
			return client.Subscription{}, fmt.Errorf("parsing retry.maximum_backoff: %w", err)
		}
		out.Retry = &client.RetryPolicy{MinimumBackoff: minBackoff, MaximumBackoff: maxBackoff}
	}

	return out, nil
}

// buildPushConfig converts the push settings, returning nil when the config
// declared no push delivery. Every other push field requires an endpoint, which
// validation enforces, so the endpoint alone decides whether there is one.
func buildPushConfig(s config.Subscription) *client.PushConfig {
	if s.PushEndpoint == "" {
		return nil
	}

	out := &client.PushConfig{
		Endpoint:      s.PushEndpoint,
		Attributes:    s.PushAttributes,
		WriteMetadata: s.PushWriteMetadata,
	}

	if s.PushOIDCToken != nil {
		out.OIDCToken = &client.OIDCToken{
			ServiceAccountEmail: s.PushOIDCToken.ServiceAccountEmail,
			Audience:            s.PushOIDCToken.Audience,
		}
	}

	switch s.PushWrapper {
	case config.PushWrapperPubSub:
		out.Wrapper = client.WrapperPubSub
	case config.PushWrapperNone:
		out.Wrapper = client.WrapperNone
	}

	return out
}

// boolDrift reports whether a tri-state bool the config actually set disagrees
// with the live value. A nil desired means the config omitted the key, which is
// never drift. A nil current reads as the Pub/Sub default, false.
func boolDrift(current, desired *bool) bool {
	return desired != nil && client.DerefBool(current) != *desired
}

// immutableDrift returns the fields Pub/Sub fixes at creation time that the
// config now disagrees with.
//
// `filter` and `enable_message_ordering` are immutable on Google Cloud, and the
// emulator rejects an update naming either. `topic` is immutable everywhere.
//
// Fields the config leaves unset are not compared, so an omitted key does not
// warn on every run about a value it never asked to change. `topic` is the
// exception: buildSubscription always populates it from the enclosing topic.
func immutableDrift(current *client.Subscription, desired client.Subscription) []drift {
	var drifts []drift

	if desired.Filter != "" && desired.Filter != current.Filter {
		drifts = append(drifts, drift{"filter", current.Filter, desired.Filter})
	}
	if boolDrift(current.EnableMessageOrdering, desired.EnableMessageOrdering) {
		drifts = append(drifts, drift{
			"enable_message_ordering",
			client.DerefBool(current.EnableMessageOrdering),
			client.DerefBool(desired.EnableMessageOrdering),
		})
	}
	if desired.Topic != current.Topic {
		drifts = append(drifts, drift{"topic", current.Topic, desired.Topic})
	}

	return drifts
}

// diffSubscription returns the mutable field paths on which current differs
// from desired. Fields the config leaves unset are not compared, so an omitted
// value means "leave it alone" rather than "reset it to zero".
func diffSubscription(current *client.Subscription, desired client.Subscription) []string {
	var paths []string

	if desired.AckDeadline > 0 && current.AckDeadline != desired.AckDeadline {
		paths = append(paths, pathSubAckDeadline)
	}
	if desired.MessageRetention > 0 && current.MessageRetention != desired.MessageRetention {
		paths = append(paths, pathSubMessageRetention)
	}
	if boolDrift(current.RetainAckedMessages, desired.RetainAckedMessages) {
		paths = append(paths, pathSubRetainAcked)
	}
	if boolDrift(current.EnableExactlyOnceDelivery, desired.EnableExactlyOnceDelivery) {
		paths = append(paths, pathSubExactlyOnce)
	}
	if desired.ExpirationSet &&
		(!current.ExpirationSet || current.ExpirationTTL != desired.ExpirationTTL) {
		paths = append(paths, pathSubExpiration)
	}
	if len(desired.Labels) > 0 && !labelsSubset(desired.Labels, current.Labels) {
		paths = append(paths, pathSubLabels)
	}
	if desired.Push != nil && !pushConfigEqual(current.Push, desired.Push) {
		paths = append(paths, pathSubPushConfig)
	}
	if desired.DeadLetter != nil && !deadLetterEqual(current.DeadLetter, desired.DeadLetter) {
		paths = append(paths, pathSubDeadLetter)
	}
	if desired.Retry != nil && !retryEqual(current.Retry, desired.Retry) {
		paths = append(paths, pathSubRetry)
	}

	return paths
}

// pushConfigEqual compares only what the config declares. The update mask
// replaces the whole push_config message, so every push setting folds into the
// one path — but a field the config left unset must still not count as drift.
func pushConfigEqual(current, desired *client.PushConfig) bool {
	if current == nil {
		return false
	}

	if desired.Endpoint != "" && current.Endpoint != desired.Endpoint {
		return false
	}
	if !labelsSubset(desired.Attributes, current.Attributes) {
		return false
	}
	if !oidcEqual(current.OIDCToken, desired.OIDCToken) {
		return false
	}
	if desired.Wrapper != client.WrapperUnset {
		if effectiveWrapper(current.Wrapper) != desired.Wrapper {
			return false
		}
		// write_metadata only exists on the no-wrapper message.
		if desired.Wrapper == client.WrapperNone && current.WriteMetadata != desired.WriteMetadata {
			return false
		}
	}
	return true
}

// oidcEqual compares the push authentication. A config that declares no token
// leaves whatever is on the subscription alone, matching every other omitted field.
func oidcEqual(current, desired *client.OIDCToken) bool {
	if desired == nil {
		return true
	}
	return current != nil && *current == *desired
}

// effectiveWrapper resolves a live wrapper for comparison. Pub/Sub defaults an
// unset wrapper to the pubsub wrapper, so reading an unset live value as the
// default is what stops `push_wrapper: pubsub` from drifting forever against an
// endpoint that never echoes one back.
//
// It is applied to the live value only, never to the desired one, where unset
// keeps its own meaning of "leave it alone".
func effectiveWrapper(w client.Wrapper) client.Wrapper {
	if w == client.WrapperUnset {
		return client.WrapperPubSub
	}
	return w
}

func deadLetterEqual(a, b *client.DeadLetterPolicy) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func retryEqual(a, b *client.RetryPolicy) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
