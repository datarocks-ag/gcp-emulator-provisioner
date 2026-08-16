package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestStorageEndpointNormalisation(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// STORAGE_EMULATOR_HOST is conventionally a bare host:port.
		{"localhost:4443", "http://localhost:4443/storage/v1/"},
		{"http://fake-gcs:4443", "http://fake-gcs:4443/storage/v1/"},
		{"https://fake-gcs:4443/", "https://fake-gcs:4443/storage/v1/"},
		{"http://fake-gcs:4443/storage/v1", "http://fake-gcs:4443/storage/v1/"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := storageEndpoint(tt.in); got != tt.want {
				t.Errorf("storageEndpoint(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestShortName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"projects/p/topics/orders", "orders"},
		{"projects/p/subscriptions/processor", "processor"},
		{"orders", "orders"},
		{"", ""},
	}

	for _, tt := range tests {
		if got := shortName(tt.in); got != tt.want {
			t.Errorf("shortName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsUnsupportedField(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "emulator rejects an unmodelled field",
			err: status.Error(codes.InvalidArgument,
				"Invalid update_mask provided in the UpdateTopicRequest: labels is not a known Topic field."),
			want: true,
		},
		{
			name: "emulator rejects an unimplemented update",
			err: status.Error(codes.InvalidArgument,
				"Updating the expiration_policy field is currently unsupported in the Pub/Sub Emulator."),
			want: true,
		},
		{
			name: "immutable field is a real error",
			err: status.Error(codes.InvalidArgument,
				"the topic field in the Subscription is not mutable."),
			want: false,
		},
		{
			name: "other codes are real errors",
			err:  status.Error(codes.PermissionDenied, "labels is not a known Topic field"),
			want: false,
		},
		{
			name: "plain errors are real errors",
			err:  errors.New("connection refused"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUnsupportedField(tt.err); got != tt.want {
				t.Errorf("IsUnsupportedField(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsVersioningUnsupported(t *testing.T) {
	err := errors.New("googleapi: Error 500: not implemented: fs storage type does not support versioning yet")
	if !IsVersioningUnsupported(err) {
		t.Error("expected the fake-gcs-server filesystem backend error to be recognised")
	}
	if IsVersioningUnsupported(errors.New("connection refused")) {
		t.Error("unrelated errors must not be classified as a versioning gap")
	}
	if IsVersioningUnsupported(nil) {
		t.Error("nil must not be classified as a versioning gap")
	}
}

func TestIsBucketExistsError(t *testing.T) {
	if !isBucketExistsError(errors.New("googleapi: Error 409: A Cloud Storage bucket named 'x' already exists")) {
		t.Error("expected a 409 to be recognised as an existing bucket")
	}
	if isBucketExistsError(errors.New("connection refused")) {
		t.Error("unrelated errors must not be classified as an existing bucket")
	}
}

func TestSubscriptionProtoRoundTrip(t *testing.T) {
	c := &PubSubClient{projectID: "local-dev"}

	in := Subscription{
		Name:                      "processor",
		Topic:                     "orders",
		AckDeadline:               30 * time.Second,
		MessageRetention:          7 * 24 * time.Hour,
		RetainAckedMessages:       BoolPtr(true),
		EnableMessageOrdering:     BoolPtr(true),
		EnableExactlyOnceDelivery: BoolPtr(true),
		Filter:                    `attributes.type = "order"`,
		ExpirationSet:             true,
		ExpirationTTL:             48 * time.Hour,
		Labels:                    map[string]string{"team": "platform"},
		Push: &PushConfig{
			Endpoint:      "http://consumer:8080/events",
			Attributes:    map[string]string{"x-goog-version": "v1"},
			OIDCToken:     &OIDCToken{ServiceAccountEmail: "pusher@local-dev.iam.gserviceaccount.com", Audience: "http://consumer:8080"},
			Wrapper:       WrapperNone,
			WriteMetadata: true,
		},
		DeadLetter: &DeadLetterPolicy{Topic: "orders-dlq", MaxDeliveryAttempts: 5},
		Retry:      &RetryPolicy{MinimumBackoff: 10 * time.Second, MaximumBackoff: 600 * time.Second},
	}

	out := fromProtoSubscription(c.toProtoSubscription(in))

	if out.Name != in.Name || out.Topic != in.Topic {
		t.Errorf("names not round-tripped: got %q/%q", out.Name, out.Topic)
	}
	if out.AckDeadline != in.AckDeadline || out.MessageRetention != in.MessageRetention {
		t.Errorf("durations not round-tripped: %v / %v", out.AckDeadline, out.MessageRetention)
	}
	if !DerefBool(out.RetainAckedMessages) ||
		!DerefBool(out.EnableMessageOrdering) ||
		!DerefBool(out.EnableExactlyOnceDelivery) {
		t.Errorf("booleans not round-tripped: %+v", out)
	}
	if out.Filter != in.Filter {
		t.Errorf("filter = %q, want %q", out.Filter, in.Filter)
	}
	if !out.ExpirationSet || out.ExpirationTTL != in.ExpirationTTL {
		t.Errorf("expiration = %v/%v, want true/%v", out.ExpirationSet, out.ExpirationTTL, in.ExpirationTTL)
	}
	if out.Push == nil {
		t.Fatalf("push config not round-tripped: %+v", out)
	}
	if out.Push.Endpoint != in.Push.Endpoint || out.Push.Attributes["x-goog-version"] != "v1" {
		t.Errorf("push endpoint/attributes not round-tripped: %+v", out.Push)
	}
	if out.Push.OIDCToken == nil || *out.Push.OIDCToken != *in.Push.OIDCToken {
		t.Errorf("push oidc token = %+v, want %+v", out.Push.OIDCToken, in.Push.OIDCToken)
	}
	if out.Push.Wrapper != WrapperNone || !out.Push.WriteMetadata {
		t.Errorf("push wrapper = %v/%v, want WrapperNone/true", out.Push.Wrapper, out.Push.WriteMetadata)
	}
	if out.DeadLetter == nil || *out.DeadLetter != *in.DeadLetter {
		t.Errorf("dead letter = %+v, want %+v", out.DeadLetter, in.DeadLetter)
	}
	if out.Retry == nil || *out.Retry != *in.Retry {
		t.Errorf("retry = %+v, want %+v", out.Retry, in.Retry)
	}
}

func TestPushConfigWrapperOneof(t *testing.T) {
	c := &PubSubClient{projectID: "local-dev"}

	build := func(w Wrapper) *PushConfig {
		return &PushConfig{Endpoint: "http://consumer:8080", Wrapper: w}
	}

	// Unset must leave the oneof nil, so an update does not overwrite whatever
	// wrapper the subscription already has.
	unset := c.toProtoSubscription(Subscription{Name: "s", Topic: "t", Push: build(WrapperUnset)})
	if unset.GetPushConfig().GetWrapper() != nil {
		t.Errorf("WrapperUnset must leave the oneof nil, got %+v", unset.GetPushConfig().GetWrapper())
	}

	wrapped := c.toProtoSubscription(Subscription{Name: "s", Topic: "t", Push: build(WrapperPubSub)})
	if wrapped.GetPushConfig().GetPubsubWrapper() == nil {
		t.Error("WrapperPubSub must set the pubsub_wrapper oneof")
	}

	none := c.toProtoSubscription(Subscription{Name: "s", Topic: "t", Push: &PushConfig{
		Endpoint: "http://consumer:8080", Wrapper: WrapperNone, WriteMetadata: true,
	}})
	if !none.GetPushConfig().GetNoWrapper().GetWriteMetadata() {
		t.Error("WrapperNone must set no_wrapper with write_metadata")
	}

	// And back again.
	if got := fromProtoPushConfig(unset.GetPushConfig()); got.Wrapper != WrapperUnset {
		t.Errorf("unset wrapper round-tripped to %v", got.Wrapper)
	}
	if got := fromProtoPushConfig(wrapped.GetPushConfig()); got.Wrapper != WrapperPubSub {
		t.Errorf("pubsub wrapper round-tripped to %v", got.Wrapper)
	}
}

func TestPushConfigNilForPullSubscription(t *testing.T) {
	c := &PubSubClient{projectID: "local-dev"}

	got := c.toProtoSubscription(Subscription{Name: "s", Topic: "t"})
	if got.GetPushConfig() != nil {
		t.Errorf("a pull subscription must send no push_config, got %+v", got.GetPushConfig())
	}
	if out := fromProtoPushConfig(nil); out != nil {
		t.Errorf("a nil push config must stay nil, got %+v", out)
	}
}

func TestSubscriptionProtoNilBoolsSendTheDefault(t *testing.T) {
	c := &PubSubClient{projectID: "local-dev"}

	// A config that omits all three tri-states still has to produce a valid
	// create request; false is the Pub/Sub default for each.
	got := c.toProtoSubscription(Subscription{Name: "processor", Topic: "orders"})

	if got.GetRetainAckedMessages() || got.GetEnableMessageOrdering() || got.GetEnableExactlyOnceDelivery() {
		t.Errorf("nil tri-states must send false, got %+v", got)
	}

	// A subscription read back from the API never carries a nil, so the diff
	// always has a concrete value to compare against.
	out := fromProtoSubscription(got)
	if out.RetainAckedMessages == nil ||
		out.EnableMessageOrdering == nil ||
		out.EnableExactlyOnceDelivery == nil {
		t.Errorf("fromProtoSubscription must always populate the tri-states, got %+v", out)
	}
}

func TestSubscriptionProtoUsesQualifiedPaths(t *testing.T) {
	c := &PubSubClient{projectID: "local-dev"}

	got := c.toProtoSubscription(Subscription{
		Name:       "processor",
		Topic:      "orders",
		DeadLetter: &DeadLetterPolicy{Topic: "orders-dlq", MaxDeliveryAttempts: 5},
	})

	if got.GetName() != "projects/local-dev/subscriptions/processor" {
		t.Errorf("subscription name = %q", got.GetName())
	}
	if got.GetTopic() != "projects/local-dev/topics/orders" {
		t.Errorf("topic = %q", got.GetTopic())
	}
	// A short dead letter topic id has to be expanded too, or Pub/Sub rejects it.
	if got.GetDeadLetterPolicy().GetDeadLetterTopic() != "projects/local-dev/topics/orders-dlq" {
		t.Errorf("dead letter topic = %q", got.GetDeadLetterPolicy().GetDeadLetterTopic())
	}
}

func TestExpirationNeverSendsEmptyPolicy(t *testing.T) {
	c := &PubSubClient{projectID: "local-dev"}

	got := c.toProtoSubscription(Subscription{Name: "s", Topic: "t", ExpirationSet: true})
	if got.GetExpirationPolicy() == nil {
		t.Fatal("expected an expiration policy to be sent")
	}
	// A policy present but with no TTL is how Pub/Sub expresses "never expires".
	if got.GetExpirationPolicy().GetTtl() != nil {
		t.Errorf("ttl = %v, want nil for never", got.GetExpirationPolicy().GetTtl())
	}

	unset := c.toProtoSubscription(Subscription{Name: "s", Topic: "t"})
	if unset.GetExpirationPolicy() != nil {
		t.Error("an unset expiration must not send a policy at all")
	}
}

func TestRetryUntilReadySucceedsAfterTransientFailures(t *testing.T) {
	attempts := 0
	err := retryUntilReady(context.Background(), "test", func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("connection refused")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryUntilReady: %v", err)
	}
	if attempts != 3 {
		t.Errorf("probed %d times, want 3", attempts)
	}
}

func TestRetryUntilReadyStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := retryUntilReady(ctx, "test", func(context.Context) error {
		return errors.New("connection refused")
	})
	if err == nil {
		t.Fatal("expected an error once the context is cancelled")
	}
}

func TestTopicProtoConversion(t *testing.T) {
	c := &PubSubClient{projectID: "local-dev"}

	if got := c.TopicPath("orders"); got != "projects/local-dev/topics/orders" {
		t.Errorf("TopicPath = %q", got)
	}
	if got := c.SubscriptionPath("processor"); got != "projects/local-dev/subscriptions/processor" {
		t.Errorf("SubscriptionPath = %q", got)
	}
	if got := c.projectPath(); got != "projects/local-dev" {
		t.Errorf("projectPath = %q", got)
	}
}
