package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"

	"gcp-emulator-provisioner/internal/client"
	"gcp-emulator-provisioner/internal/config"
)

// Topic field paths accepted in an UpdateTopic mask.
const (
	pathTopicLabels           = "labels"
	pathTopicMessageRetention = "message_retention_duration"
)

func (p *Provisioner) ensureTopic(ctx context.Context, topic config.Topic, strategy string) error {
	desired, err := buildTopic(topic)
	if err != nil {
		return err
	}

	current, err := p.pubsub.GetTopic(ctx, topic.Name)
	if err != nil && !errors.Is(err, client.ErrNotFound) {
		return fmt.Errorf("checking topic: %w", err)
	}

	if errors.Is(err, client.ErrNotFound) {
		if p.opts.DryRun {
			slog.Info("Would create topic", "topic", topic.Name,
				"labels", len(desired.Labels), "message_retention", desired.MessageRetention)
			return nil
		}

		slog.Info("Creating topic", "topic", topic.Name)
		if err := p.pubsub.CreateTopic(ctx, desired); err != nil {
			return err
		}
		return nil
	}

	if strategy == "create" {
		slog.Info("Skipping existing topic (strategy=create)", "topic", topic.Name)
		return nil
	}

	paths := diffTopic(current, desired)
	if len(paths) == 0 {
		slog.Debug("Topic up to date", "topic", topic.Name)
		return nil
	}

	if p.opts.DryRun {
		slog.Info("Would update topic", "topic", topic.Name, "fields", paths)
		return nil
	}

	// The mask replaces the whole label map, so labels already on the topic have
	// to be carried across or the update would delete them.
	desired.Labels = mergeLabels(current.Labels, desired.Labels)

	slog.Info("Updating topic", "topic", topic.Name, "fields", paths)
	// The emulator rejects update masks Google Cloud accepts. Warn per refused
	// path rather than fail, so a config valid against Google Cloud still runs
	// locally and the paths this endpoint does support are still applied.
	return applyPaths(ctx, paths,
		func(ctx context.Context, mask []string) error {
			return p.pubsub.UpdateTopic(ctx, desired, mask)
		},
		func(path string, err error) {
			slog.Warn("Topic field not supported by this Pub/Sub endpoint, leaving it unchanged",
				"topic", topic.Name, "field", path, "error", err)
		})
}

// buildTopic converts a config.Topic into the transport-neutral form.
func buildTopic(t config.Topic) (client.Topic, error) {
	retention, err := config.ParseDuration(t.MessageRetention)
	if err != nil {
		return client.Topic{}, fmt.Errorf("parsing message_retention: %w", err)
	}

	return client.Topic{
		Name:             t.Name,
		Labels:           t.Labels,
		MessageRetention: retention,
	}, nil
}

// diffTopic returns the field paths on which current differs from desired.
//
// Only fields the config actually sets are compared: an omitted retention or an
// absent label map means "leave it alone", not "clear it". The provisioner is
// additive, so labels present on the topic but not in the config are preserved.
func diffTopic(current *client.Topic, desired client.Topic) []string {
	var paths []string

	if desired.MessageRetention > 0 && current.MessageRetention != desired.MessageRetention {
		paths = append(paths, pathTopicMessageRetention)
	}
	if len(desired.Labels) > 0 && !labelsSubset(desired.Labels, current.Labels) {
		paths = append(paths, pathTopicLabels)
	}

	return paths
}

// labelsSubset reports whether every key in want is present in got with the
// same value. Extra labels on the resource are not drift — the provisioner never
// removes what it did not declare.
func labelsSubset(want, got map[string]string) bool {
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// mergeLabels overlays the configured labels onto the ones already on the
// resource. An update mask replaces the whole label map, so labels the
// provisioner does not manage have to be carried across or they would be lost.
func mergeLabels(current, configured map[string]string) map[string]string {
	if len(configured) == 0 {
		return current
	}

	merged := make(map[string]string, len(current)+len(configured))
	maps.Copy(merged, current)
	maps.Copy(merged, configured)
	return merged
}
