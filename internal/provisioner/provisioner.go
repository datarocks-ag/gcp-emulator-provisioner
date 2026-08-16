// Package provisioner reconciles the declared configuration against a live
// Pub/Sub and Cloud Storage endpoint, idempotently and additively.
package provisioner

import (
	"context"
	"fmt"
	"log/slog"

	"gcp-emulator-provisioner/internal/client"
	"gcp-emulator-provisioner/internal/config"
)

// Options controls provisioner behaviour.
type Options struct {
	// DryRun, when true, logs every mutation as a preview instead of applying it.
	// Read-only calls still run so the preview reflects live state.
	DryRun bool

	// CompareCreateOnlyBucketAttrs enables the bucket location / storage class
	// drift warning.
	//
	// It is off against fake-gcs-server, which reports US-CENTRAL1 and STANDARD
	// for every bucket regardless of what was passed at creation — the
	// comparison there would warn on every run and say nothing true. Nothing
	// about what gets provisioned depends on this; only whether a difference the
	// provisioner cannot apply is reported.
	CompareCreateOnlyBucketAttrs bool
}

// DefaultOptions returns Options with sensible defaults.
func DefaultOptions() Options {
	return Options{}
}

// drift describes one field whose live value differs from the configured one.
// It is shared by the resource kinds that report a difference they cannot
// apply: a subscription's immutable fields and a bucket's create-time ones.
type drift struct {
	field   string
	current any
	desired any
}

// PubSubAdmin defines the Pub/Sub operations needed by the provisioner.
type PubSubAdmin interface {
	GetTopic(ctx context.Context, name string) (*client.Topic, error)
	CreateTopic(ctx context.Context, t client.Topic) error
	UpdateTopic(ctx context.Context, t client.Topic, paths []string) error
	GetSubscription(ctx context.Context, name string) (*client.Subscription, error)
	CreateSubscription(ctx context.Context, s client.Subscription) error
	UpdateSubscription(ctx context.Context, s client.Subscription, paths []string) error
}

// StorageAdmin defines the Cloud Storage operations needed by the provisioner.
type StorageAdmin interface {
	GetBucket(ctx context.Context, name string) (*client.Bucket, error)
	CreateBucket(ctx context.Context, b client.Bucket) error
	SetVersioning(ctx context.Context, name string, enabled bool) error
}

// Provisioner orchestrates idempotent Google Cloud resource provisioning.
//
// The two sections are independent: a config may declare only Pub/Sub topics or
// only buckets, and the corresponding client is nil when that section is not
// being run. Within the Pub/Sub section order matters — every topic is created
// before any subscription, because a subscription referencing a topic that does
// not exist yet (its own, or its dead letter target) fails with NotFound.
type Provisioner struct {
	pubsub  PubSubAdmin
	storage StorageAdmin
	cfg     *config.Config
	opts    Options
}

// New creates a new Provisioner. Either dependency may be nil when only the
// other section is going to be run.
func New(pubsub PubSubAdmin, storage StorageAdmin, cfg *config.Config, opts Options) *Provisioner {
	return &Provisioner{
		pubsub:  pubsub,
		storage: storage,
		cfg:     cfg,
		opts:    opts,
	}
}

// ProvisionPubSub reconciles every topic, then every subscription.
//
// Topics are provisioned first as a whole pass rather than topic-by-topic with
// its subscriptions, so that a subscription may name a dead letter topic
// declared later in the file.
func (p *Provisioner) ProvisionPubSub(ctx context.Context) error {
	topics := p.cfg.PubSub.Topics
	if len(topics) == 0 {
		slog.Debug("No topics configured, skipping Pub/Sub section")
		return nil
	}

	slog.Info("Provisioning Pub/Sub", "topics", len(topics), "subscriptions", countSubscriptions(topics))

	for _, topic := range topics {
		strategy := config.EffectiveStrategy(topic.Strategy, p.cfg.Strategy)
		if err := p.ensureTopic(ctx, topic, strategy); err != nil {
			return fmt.Errorf("provisioning topic %q: %w", topic.Name, err)
		}
	}

	for _, topic := range topics {
		for _, sub := range topic.Subscriptions {
			strategy := config.EffectiveStrategy(sub.Strategy, p.cfg.Strategy)
			if err := p.ensureSubscription(ctx, topic.Name, sub, strategy); err != nil {
				return fmt.Errorf("provisioning subscription %q on topic %q: %w", sub.Name, topic.Name, err)
			}
		}
	}

	return nil
}

// ProvisionStorage reconciles every bucket.
func (p *Provisioner) ProvisionStorage(ctx context.Context) error {
	buckets := p.cfg.Storage.Buckets
	if len(buckets) == 0 {
		slog.Debug("No buckets configured, skipping Cloud Storage section")
		return nil
	}

	slog.Info("Provisioning Cloud Storage", "buckets", len(buckets))

	for _, bucket := range buckets {
		strategy := config.EffectiveStrategy(bucket.Strategy, p.cfg.Strategy)
		if err := p.ensureBucket(ctx, bucket, strategy); err != nil {
			return fmt.Errorf("provisioning bucket %q: %w", bucket.Name, err)
		}
	}

	return nil
}

// Run executes both sections: Pub/Sub, then Cloud Storage.
func (p *Provisioner) Run(ctx context.Context) error {
	slog.Info("Starting provisioning")

	if err := p.ProvisionPubSub(ctx); err != nil {
		return err
	}
	if err := p.ProvisionStorage(ctx); err != nil {
		return err
	}

	slog.Info("Provisioning complete")
	return nil
}

func countSubscriptions(topics []config.Topic) int {
	n := 0
	for _, t := range topics {
		n += len(t.Subscriptions)
	}
	return n
}
