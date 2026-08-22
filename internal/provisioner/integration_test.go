//go:build integration

package provisioner_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"gcp-emulator-provisioner/internal/client"
	"gcp-emulator-provisioner/internal/config"
	"gcp-emulator-provisioner/internal/provisioner"
)

const testProject = "local-dev"

// Pinned, because a floating tag lets an upstream release break CI with no local
// commit to blame. Keep in step with docker-compose.yaml and
// docker-compose.emulators.yaml.
//
// google-cloud-cli is the renamed cloud-sdk repository and the one that publishes
// arm64, so the emulator runs natively on an Apple Silicon host instead of under
// emulation.
const (
	pubSubEmulatorImage = "gcr.io/google.com/cloudsdktool/google-cloud-cli:581.0.0-emulators"
	fakeGCSImage        = "fsouza/fake-gcs-server:1.55.1"
)

// startPubSubEmulator boots the gcloud Pub/Sub emulator and returns its host:port.
//
// The startup timeout stays generous: the image is roughly a gigabyte, so a cold
// cache dominates the first run.
func startPubSubEmulator(t *testing.T) string {
	t.Helper()

	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        pubSubEmulatorImage,
		ExposedPorts: []string{"8085/tcp"},
		Cmd: []string{
			"gcloud", "beta", "emulators", "pubsub", "start",
			"--host-port=0.0.0.0:8085", "--project=" + testProject,
		},
		WaitingFor: wait.ForLog("Server started, listening on").WithStartupTimeout(5 * time.Minute),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("starting Pub/Sub emulator: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminating Pub/Sub emulator: %v", err)
		}
	})

	endpoint, err := container.PortEndpoint(ctx, "8085/tcp", "")
	if err != nil {
		t.Fatalf("resolving Pub/Sub endpoint: %v", err)
	}
	return endpoint
}

// startFakeGCS boots fake-gcs-server on its in-memory backend, which is the only
// one that implements object versioning.
func startFakeGCS(t *testing.T) string {
	t.Helper()

	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        fakeGCSImage,
		ExposedPorts: []string{"4443/tcp"},
		Cmd:          []string{"-scheme", "http", "-host", "0.0.0.0", "-port", "4443", "-backend", "memory"},
		WaitingFor:   wait.ForListeningPort("4443/tcp").WithStartupTimeout(2 * time.Minute),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("starting fake-gcs-server: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminating fake-gcs-server: %v", err)
		}
	})

	endpoint, err := container.PortEndpoint(ctx, "4443/tcp", "")
	if err != nil {
		t.Fatalf("resolving Cloud Storage endpoint: %v", err)
	}
	return endpoint
}

func connectPubSub(t *testing.T, host string) *client.PubSubClient {
	t.Helper()

	c, err := client.ConnectPubSub(context.Background(), testProject, host)
	if err != nil {
		t.Fatalf("connecting to Pub/Sub: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func connectStorage(t *testing.T, host string) *client.StorageClient {
	t.Helper()

	c, err := client.ConnectStorage(context.Background(), testProject, host)
	if err != nil {
		t.Fatalf("connecting to Cloud Storage: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// pubsubConfig is the configuration every Pub/Sub integration test provisions.
func pubsubConfig() *config.Config {
	retainAcked := true
	exactlyOnce := true
	ordering := true

	return &config.Config{
		ProjectID: testProject,
		PubSub: config.PubSub{Topics: []config.Topic{
			{
				Name:             "orders",
				Labels:           map[string]string{"domain": "commerce"},
				MessageRetention: "24h",
				Subscriptions: []config.Subscription{
					{
						Name:                      "order-processor",
						AckDeadline:               "30s",
						MessageRetention:          "7d",
						RetainAckedMessages:       &retainAcked,
						EnableExactlyOnceDelivery: &exactlyOnce,
						Labels:                    map[string]string{"team": "platform"},
						DeadLetter: &config.DeadLetter{
							Topic:               "orders-dlq",
							MaxDeliveryAttempts: 5,
						},
						Retry: &config.Retry{MinimumBackoff: "10s", MaximumBackoff: "600s"},
					},
					{
						Name:                  "order-audit",
						AckDeadline:           "60s",
						Filter:                `attributes.type = "order.created"`,
						EnableMessageOrdering: &ordering,
						ExpirationTTL:         config.ExpirationNever,
					},
					{
						Name:           "order-webhook",
						AckDeadline:    "10s",
						PushEndpoint:   "http://consumer:8080/events",
						PushAttributes: map[string]string{"x-goog-version": "v1"},
					},
				},
			},
			{Name: "orders-dlq", MessageRetention: "7d"},
		}},
	}
}

func TestIntegrationPubSubProvisionsAndIsIdempotent(t *testing.T) {
	host := startPubSubEmulator(t)
	c := connectPubSub(t, host)
	ctx := context.Background()
	cfg := pubsubConfig()

	p := provisioner.New(c, nil, cfg, provisioner.DefaultOptions())
	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Every declared resource exists, with the configured settings.
	topic, err := c.GetTopic(ctx, "orders")
	if err != nil {
		t.Fatalf("getting topic: %v", err)
	}
	if topic.MessageRetention != 24*time.Hour {
		t.Errorf("topic retention = %v, want 24h", topic.MessageRetention)
	}
	if topic.Labels["domain"] != "commerce" {
		t.Errorf("topic labels = %v, want domain=commerce", topic.Labels)
	}

	sub, err := c.GetSubscription(ctx, "order-processor")
	if err != nil {
		t.Fatalf("getting subscription: %v", err)
	}
	if sub.AckDeadline != 30*time.Second {
		t.Errorf("ack deadline = %v, want 30s", sub.AckDeadline)
	}
	if sub.MessageRetention != 7*24*time.Hour {
		t.Errorf("message retention = %v, want 7d", sub.MessageRetention)
	}
	if !client.DerefBool(sub.RetainAckedMessages) || !client.DerefBool(sub.EnableExactlyOnceDelivery) {
		t.Errorf("booleans not applied: %+v", sub)
	}
	if sub.DeadLetter == nil || sub.DeadLetter.Topic != "orders-dlq" || sub.DeadLetter.MaxDeliveryAttempts != 5 {
		t.Errorf("dead letter policy = %+v", sub.DeadLetter)
	}
	if sub.Retry == nil || sub.Retry.MinimumBackoff != 10*time.Second {
		t.Errorf("retry policy = %+v", sub.Retry)
	}

	audit, err := c.GetSubscription(ctx, "order-audit")
	if err != nil {
		t.Fatalf("getting audit subscription: %v", err)
	}
	if audit.Filter != `attributes.type = "order.created"` {
		t.Errorf("filter = %q", audit.Filter)
	}
	if !client.DerefBool(audit.EnableMessageOrdering) {
		t.Error("message ordering was not applied at creation")
	}
	// expiration_ttl: never means a policy with no TTL.
	if !audit.ExpirationSet || audit.ExpirationTTL != 0 {
		t.Errorf("expiration = set:%v ttl:%v, want set with no ttl", audit.ExpirationSet, audit.ExpirationTTL)
	}

	push, err := c.GetSubscription(ctx, "order-webhook")
	if err != nil {
		t.Fatalf("getting push subscription: %v", err)
	}
	if push.Push == nil || push.Push.Endpoint != "http://consumer:8080/events" {
		t.Errorf("push config = %+v", push.Push)
	}

	// A second run against the provisioned state must be a no-op that succeeds.
	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}

	after, err := c.GetSubscription(ctx, "order-processor")
	if err != nil {
		t.Fatalf("re-reading subscription: %v", err)
	}
	if after.AckDeadline != sub.AckDeadline || after.MessageRetention != sub.MessageRetention {
		t.Errorf("second run changed the subscription: %+v -> %+v", sub, after)
	}
}

func TestIntegrationPubSubUpdatesDriftedFields(t *testing.T) {
	host := startPubSubEmulator(t)
	c := connectPubSub(t, host)
	ctx := context.Background()

	cfg := pubsubConfig()
	p := provisioner.New(c, nil, cfg, provisioner.DefaultOptions())
	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Change the mutable settings and re-provision.
	cfg.PubSub.Topics[0].MessageRetention = "48h"
	cfg.PubSub.Topics[0].Subscriptions[0].AckDeadline = "45s"
	cfg.PubSub.Topics[0].Subscriptions[0].DeadLetter.MaxDeliveryAttempts = 10
	cfg.PubSub.Topics[0].Subscriptions[0].Retry.MinimumBackoff = "20s"

	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}

	topic, err := c.GetTopic(ctx, "orders")
	if err != nil {
		t.Fatalf("getting topic: %v", err)
	}
	if topic.MessageRetention != 48*time.Hour {
		t.Errorf("topic retention = %v, want 48h", topic.MessageRetention)
	}

	sub, err := c.GetSubscription(ctx, "order-processor")
	if err != nil {
		t.Fatalf("getting subscription: %v", err)
	}
	if sub.AckDeadline != 45*time.Second {
		t.Errorf("ack deadline = %v, want 45s", sub.AckDeadline)
	}
	if sub.DeadLetter.MaxDeliveryAttempts != 10 {
		t.Errorf("max delivery attempts = %d, want 10", sub.DeadLetter.MaxDeliveryAttempts)
	}
	if sub.Retry.MinimumBackoff != 20*time.Second {
		t.Errorf("minimum backoff = %v, want 20s", sub.Retry.MinimumBackoff)
	}
}

// TestIntegrationPubSubImmutableDriftIsNotFatal pins the behaviour that matters
// most in practice: Pub/Sub rejects an update mask naming `filter`, so a config
// change there must warn and carry on rather than fail the whole run.
func TestIntegrationPubSubImmutableDriftIsNotFatal(t *testing.T) {
	host := startPubSubEmulator(t)
	c := connectPubSub(t, host)
	ctx := context.Background()

	cfg := pubsubConfig()
	p := provisioner.New(c, nil, cfg, provisioner.DefaultOptions())
	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}

	cfg.PubSub.Topics[0].Subscriptions[1].Filter = `attributes.type = "order.shipped"`

	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("changing an immutable field must not fail the run: %v", err)
	}

	sub, err := c.GetSubscription(ctx, "order-audit")
	if err != nil {
		t.Fatalf("getting subscription: %v", err)
	}
	if sub.Filter != `attributes.type = "order.created"` {
		t.Errorf("filter = %q, want the original value left in place", sub.Filter)
	}
}

// TestIntegrationPubSubEmulatorLabelGapIsNotFatal covers the emulator rejecting
// a `labels` update mask that Google Cloud accepts.
//
// It also pins the batching behaviour: the emulator validates the whole mask
// before touching the resource, so a refused `labels` path used to abandon
// every field batched with it while the run still reported success. The
// supported paths must be applied anyway.
func TestIntegrationPubSubEmulatorLabelGapIsNotFatal(t *testing.T) {
	host := startPubSubEmulator(t)
	c := connectPubSub(t, host)
	ctx := context.Background()

	cfg := &config.Config{
		ProjectID: testProject,
		PubSub: config.PubSub{Topics: []config.Topic{{
			Name:             "labelled",
			Labels:           map[string]string{"env": "local"},
			MessageRetention: "24h",
			Subscriptions: []config.Subscription{{
				Name:        "labelled-sub",
				AckDeadline: "30s",
				Labels:      map[string]string{"team": "platform"},
			}},
		}}},
	}

	p := provisioner.New(c, nil, cfg, provisioner.DefaultOptions())
	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Drift a refused path and a supported one together, on both resource kinds.
	cfg.PubSub.Topics[0].Labels["env"] = "changed"
	cfg.PubSub.Topics[0].MessageRetention = "48h"
	cfg.PubSub.Topics[0].Subscriptions[0].Labels["team"] = "changed"
	cfg.PubSub.Topics[0].Subscriptions[0].AckDeadline = "45s"

	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("a label update the emulator cannot apply must not fail the run: %v", err)
	}

	topic, err := c.GetTopic(ctx, "labelled")
	if err != nil {
		t.Fatalf("getting topic: %v", err)
	}
	if topic.MessageRetention != 48*time.Hour {
		t.Errorf("topic retention = %v, want 48h — a refused `labels` path must not discard the rest of the batch",
			topic.MessageRetention)
	}

	sub, err := c.GetSubscription(ctx, "labelled-sub")
	if err != nil {
		t.Fatalf("getting subscription: %v", err)
	}
	if sub.AckDeadline != 45*time.Second {
		t.Errorf("ack deadline = %v, want 45s — a refused `labels` path must not discard the rest of the batch",
			sub.AckDeadline)
	}
}

// TestIntegrationPubSubPushConfigOneofs covers the two push_config oneofs the
// provisioner models beyond the endpoint.
//
// It carries its own config rather than extending pubsubConfig(), which five
// other tests share — each emulator boot costs around 15s on an arm64 host.
func TestIntegrationPubSubPushConfigOneofs(t *testing.T) {
	host := startPubSubEmulator(t)
	c := connectPubSub(t, host)
	ctx := context.Background()

	cfg := &config.Config{
		ProjectID: testProject,
		PubSub: config.PubSub{Topics: []config.Topic{{
			Name: "events",
			Subscriptions: []config.Subscription{{
				Name:         "webhook",
				AckDeadline:  "10s",
				PushEndpoint: "http://consumer:8080/events",
				PushOIDCToken: &config.PushOIDCToken{
					ServiceAccountEmail: "pusher@local-dev.iam.gserviceaccount.com",
					Audience:            "http://consumer:8080",
				},
				PushWrapper:       config.PushWrapperNone,
				PushWriteMetadata: true,
			}},
		}}},
	}

	p := provisioner.New(c, nil, cfg, provisioner.DefaultOptions())
	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}

	sub, err := c.GetSubscription(ctx, "webhook")
	if err != nil {
		t.Fatalf("getting subscription: %v", err)
	}
	if sub.Push == nil || sub.Push.OIDCToken == nil {
		t.Fatalf("push oidc token not applied at creation: %+v", sub.Push)
	}
	if sub.Push.OIDCToken.Audience != "http://consumer:8080" {
		t.Errorf("audience = %q", sub.Push.OIDCToken.Audience)
	}
	if sub.Push.Wrapper != client.WrapperNone || !sub.Push.WriteMetadata {
		t.Errorf("wrapper = %v/%v, want WrapperNone/true", sub.Push.Wrapper, sub.Push.WriteMetadata)
	}

	// A second run must see no drift: a field that is accepted but not echoed
	// back would update forever without converging.
	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}

	// And the oneofs must be updatable, not just settable at creation.
	cfg.PubSub.Topics[0].Subscriptions[0].PushOIDCToken.Audience = "http://consumer:9090"
	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("updating the push oidc token: %v", err)
	}

	updated, err := c.GetSubscription(ctx, "webhook")
	if err != nil {
		t.Fatalf("re-reading subscription: %v", err)
	}
	if updated.Push.OIDCToken.Audience != "http://consumer:9090" {
		t.Errorf("audience = %q, want the updated value", updated.Push.OIDCToken.Audience)
	}
}

func TestIntegrationPubSubDryRunAppliesNothing(t *testing.T) {
	host := startPubSubEmulator(t)
	c := connectPubSub(t, host)
	ctx := context.Background()

	cfg := pubsubConfig()
	p := provisioner.New(c, nil, cfg, provisioner.Options{DryRun: true})
	if err := p.ProvisionPubSub(ctx); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	if _, err := c.GetTopic(ctx, "orders"); err == nil {
		t.Error("dry run created a topic")
	}
	if _, err := c.GetSubscription(ctx, "order-processor"); err == nil {
		t.Error("dry run created a subscription")
	}
}

func TestIntegrationStorageProvisionsAndIsIdempotent(t *testing.T) {
	host := startFakeGCS(t)
	c := connectStorage(t, host)
	ctx := context.Background()

	versioning := true
	cfg := &config.Config{
		ProjectID: testProject,
		Storage: config.Storage{Buckets: []config.Bucket{
			{Name: "documents", Versioning: &versioning},
			{Name: "media-assets"},
			// fake-gcs-server reports US-CENTRAL1 for this whatever it was
			// created with. With CompareCreateOnlyBucketAttrs off — the default,
			// and what main.go picks when STORAGE_EMULATOR_HOST is set — that
			// fabricated value must produce no drift warning and no failure.
			// The option-on branch can only be exercised against real Cloud
			// Storage, so it has no CI coverage.
			{Name: "eu-bucket", Location: "EU"},
		}},
	}

	p := provisioner.New(nil, c, cfg, provisioner.DefaultOptions())
	if err := p.ProvisionStorage(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}

	bucket, err := c.GetBucket(ctx, "documents")
	if err != nil {
		t.Fatalf("getting bucket: %v", err)
	}
	if !bucket.VersioningEnabled {
		t.Error("versioning was not enabled")
	}

	if _, err := c.GetBucket(ctx, "media-assets"); err != nil {
		t.Fatalf("getting second bucket: %v", err)
	}

	if err := p.ProvisionStorage(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func TestIntegrationStorageReconcilesVersioningOnExistingBucket(t *testing.T) {
	host := startFakeGCS(t)
	c := connectStorage(t, host)
	ctx := context.Background()

	off, on := false, true
	cfg := &config.Config{
		ProjectID: testProject,
		Storage:   config.Storage{Buckets: []config.Bucket{{Name: "documents", Versioning: &off}}},
	}

	p := provisioner.New(nil, c, cfg, provisioner.DefaultOptions())
	if err := p.ProvisionStorage(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}

	cfg.Storage.Buckets[0].Versioning = &on
	if err := p.ProvisionStorage(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}

	bucket, err := c.GetBucket(ctx, "documents")
	if err != nil {
		t.Fatalf("getting bucket: %v", err)
	}
	if !bucket.VersioningEnabled {
		t.Error("versioning drift was not reconciled")
	}
}

func TestIntegrationStorageMissingBucketIsNotAnError(t *testing.T) {
	host := startFakeGCS(t)
	c := connectStorage(t, host)

	_, err := c.GetBucket(context.Background(), "never-created")
	if err == nil {
		t.Fatal("expected an error for a missing bucket")
	}
	if !errors.Is(err, client.ErrBucketNotFound) {
		t.Errorf("error = %v, want ErrBucketNotFound", err)
	}
}
