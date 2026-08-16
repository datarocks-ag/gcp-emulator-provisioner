package client

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPubSubTopicLifecycle(t *testing.T) {
	_, host := startFakePubSub(t)
	c := connectFakePubSub(t, host)
	ctx := context.Background()

	if _, err := c.GetTopic(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTopic on a missing topic = %v, want ErrNotFound", err)
	}

	if err := c.CreateTopic(ctx, Topic{
		Name:             "orders",
		Labels:           map[string]string{"domain": "commerce"},
		MessageRetention: 24 * time.Hour,
	}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	got, err := c.GetTopic(ctx, "orders")
	if err != nil {
		t.Fatalf("GetTopic: %v", err)
	}
	// Short ids in, short ids out — the qualified path stays inside the client.
	if got.Name != "orders" {
		t.Errorf("name = %q, want the short id", got.Name)
	}
	if got.Labels["domain"] != "commerce" || got.MessageRetention != 24*time.Hour {
		t.Errorf("topic = %+v", got)
	}

	if err := c.UpdateTopic(ctx, Topic{Name: "orders", MessageRetention: 48 * time.Hour},
		[]string{"message_retention_duration"}); err != nil {
		t.Fatalf("UpdateTopic: %v", err)
	}
	after, err := c.GetTopic(ctx, "orders")
	if err != nil {
		t.Fatalf("re-reading topic: %v", err)
	}
	if after.MessageRetention != 48*time.Hour {
		t.Errorf("retention = %v, want 48h", after.MessageRetention)
	}
}

func TestCreateTopicSwallowsAlreadyExists(t *testing.T) {
	_, host := startFakePubSub(t)
	c := connectFakePubSub(t, host)
	ctx := context.Background()

	if err := c.CreateTopic(ctx, Topic{Name: "orders"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := c.CreateTopic(ctx, Topic{Name: "orders"}); err != nil {
		t.Errorf("creating an existing topic must not error, got %v", err)
	}
}

func TestUpdateWithEmptyMaskIsANoOp(t *testing.T) {
	f, host := startFakePubSub(t)
	c := connectFakePubSub(t, host)
	ctx := context.Background()

	if err := c.UpdateTopic(ctx, Topic{Name: "orders"}, nil); err != nil {
		t.Errorf("UpdateTopic with no paths: %v", err)
	}
	if err := c.UpdateSubscription(ctx, Subscription{Name: "sub", Topic: "orders"}, nil); err != nil {
		t.Errorf("UpdateSubscription with no paths: %v", err)
	}
	if len(f.masks()) != 0 {
		t.Errorf("an empty mask must not reach the server, got %v", f.masks())
	}
}

func TestUpdateTopicSurfacesTheEmulatorFieldGap(t *testing.T) {
	f, host := startFakePubSub(t)
	c := connectFakePubSub(t, host)
	ctx := context.Background()

	if err := c.CreateTopic(ctx, Topic{Name: "orders"}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	f.refuse("labels")

	err := c.UpdateTopic(ctx, Topic{Name: "orders", Labels: map[string]string{"a": "b"}}, []string{"labels"})
	if err == nil {
		t.Fatal("expected the refused path to error")
	}
	if !IsUnsupportedField(err) {
		t.Errorf("error should be classified as an endpoint gap, got %v", err)
	}
}

func TestPubSubSubscriptionLifecycle(t *testing.T) {
	_, host := startFakePubSub(t)
	c := connectFakePubSub(t, host)
	ctx := context.Background()

	if _, err := c.GetSubscription(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSubscription on a missing subscription = %v, want ErrNotFound", err)
	}

	desired := Subscription{
		Name:                      "processor",
		Topic:                     "orders",
		AckDeadline:               30 * time.Second,
		MessageRetention:          7 * 24 * time.Hour,
		RetainAckedMessages:       BoolPtr(true),
		EnableExactlyOnceDelivery: BoolPtr(true),
		ExpirationSet:             true,
		Labels:                    map[string]string{"team": "platform"},
		Push: &PushConfig{
			Endpoint:      "http://consumer:8080/events",
			OIDCToken:     &OIDCToken{ServiceAccountEmail: "pusher@local-dev.iam.gserviceaccount.com"},
			Wrapper:       WrapperNone,
			WriteMetadata: true,
		},
		DeadLetter: &DeadLetterPolicy{Topic: "orders-dlq", MaxDeliveryAttempts: 5},
		Retry:      &RetryPolicy{MinimumBackoff: 10 * time.Second, MaximumBackoff: 600 * time.Second},
	}
	if err := c.CreateSubscription(ctx, desired); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}

	got, err := c.GetSubscription(ctx, "processor")
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if got.Name != "processor" || got.Topic != "orders" {
		t.Errorf("ids not shortened on the way out: %+v", got)
	}
	if got.AckDeadline != 30*time.Second {
		t.Errorf("ack deadline = %v", got.AckDeadline)
	}
	if !DerefBool(got.RetainAckedMessages) || !DerefBool(got.EnableExactlyOnceDelivery) {
		t.Errorf("tri-states not applied: %+v", got)
	}
	// "never" is an expiration policy carrying no TTL.
	if !got.ExpirationSet || got.ExpirationTTL != 0 {
		t.Errorf("expiration = %v/%v", got.ExpirationSet, got.ExpirationTTL)
	}
	if got.Push == nil || got.Push.OIDCToken == nil || got.Push.Wrapper != WrapperNone {
		t.Errorf("push config not round-tripped: %+v", got.Push)
	}
	// The dead letter topic has to be a qualified path on the wire and a short
	// id coming back.
	if got.DeadLetter == nil || got.DeadLetter.Topic != "orders-dlq" {
		t.Errorf("dead letter = %+v", got.DeadLetter)
	}
	if got.Retry == nil || got.Retry.MinimumBackoff != 10*time.Second {
		t.Errorf("retry = %+v", got.Retry)
	}

	if err := c.UpdateSubscription(ctx, Subscription{
		Name: "processor", Topic: "orders", AckDeadline: 45 * time.Second,
	}, []string{"ack_deadline_seconds"}); err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	after, err := c.GetSubscription(ctx, "processor")
	if err != nil {
		t.Fatalf("re-reading subscription: %v", err)
	}
	if after.AckDeadline != 45*time.Second {
		t.Errorf("ack deadline = %v, want 45s", after.AckDeadline)
	}
}

func TestCreateSubscriptionSwallowsAlreadyExists(t *testing.T) {
	_, host := startFakePubSub(t)
	c := connectFakePubSub(t, host)
	ctx := context.Background()

	sub := Subscription{Name: "processor", Topic: "orders"}
	if err := c.CreateSubscription(ctx, sub); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := c.CreateSubscription(ctx, sub); err != nil {
		t.Errorf("creating an existing subscription must not error, got %v", err)
	}
}

func TestSubscriptionExpirationNeverVersusTTL(t *testing.T) {
	_, host := startFakePubSub(t)
	c := connectFakePubSub(t, host)
	ctx := context.Background()

	if err := c.CreateSubscription(ctx, Subscription{
		Name: "ttl", Topic: "orders", ExpirationSet: true, ExpirationTTL: 48 * time.Hour,
	}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	got, err := c.GetSubscription(ctx, "ttl")
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if !got.ExpirationSet || got.ExpirationTTL != 48*time.Hour {
		t.Errorf("expiration = %v/%v, want set with a 48h ttl", got.ExpirationSet, got.ExpirationTTL)
	}

	// An omitted expiration must send no policy at all, leaving the default.
	if err := c.CreateSubscription(ctx, Subscription{Name: "default", Topic: "orders"}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	unset, err := c.GetSubscription(ctx, "default")
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if unset.ExpirationSet {
		t.Error("an omitted expiration_ttl must not send an expiration policy")
	}
}

func TestConnectPubSubRetriesUntilReady(t *testing.T) {
	f, host := startFakePubSub(t)

	// The probe fails once, then succeeds — the emulator refusing connections
	// briefly after start is the case retryUntilReady exists for.
	f.mu.Lock()
	f.listErr = errors.New("connection refused")
	f.mu.Unlock()

	go func() {
		time.Sleep(200 * time.Millisecond)
		f.mu.Lock()
		f.listErr = nil
		f.mu.Unlock()
	}()

	c, err := ConnectPubSub(context.Background(), "local-dev", host)
	if err != nil {
		t.Fatalf("ConnectPubSub should retry past a transient failure: %v", err)
	}
	_ = c.Close()
}
