package client

import (
	"context"
	"net"
	"sync"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// fakePubSub is a minimal in-process stand-in for the Pub/Sub admin services.
//
// The real emulator is exercised by the integration tests; this covers the
// client's request building, proto conversion and error classification without
// Docker — the amd64-only emulator image costs ~15s a boot, so these paths would
// otherwise go unmeasured. It deliberately mirrors two emulator behaviours the
// provisioner depends on: an update mask is validated in full before anything is
// applied, and unmodelled fields are refused with InvalidArgument.
type fakePubSub struct {
	pubsubpb.UnimplementedPublisherServer
	pubsubpb.UnimplementedSubscriberServer

	mu     sync.Mutex
	topics map[string]*pubsubpb.Topic
	subs   map[string]*pubsubpb.Subscription

	// unsupportedPaths are refused the way the emulator refuses `labels`.
	unsupportedPaths map[string]bool
	// listErr fails the readiness probe until cleared.
	listErr error
	// appliedMasks records every mask that reached the server.
	appliedMasks [][]string
}

func startFakePubSub(t *testing.T) (*fakePubSub, string) {
	t.Helper()

	f := &fakePubSub{
		topics:           map[string]*pubsubpb.Topic{},
		subs:             map[string]*pubsubpb.Subscription{},
		unsupportedPaths: map[string]bool{},
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	srv := grpc.NewServer()
	pubsubpb.RegisterPublisherServer(srv, f)
	pubsubpb.RegisterSubscriberServer(srv, f)

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return f, lis.Addr().String()
}

func (f *fakePubSub) ListTopics(context.Context, *pubsubpb.ListTopicsRequest) (*pubsubpb.ListTopicsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &pubsubpb.ListTopicsResponse{}, nil
}

func (f *fakePubSub) CreateTopic(_ context.Context, t *pubsubpb.Topic) (*pubsubpb.Topic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.topics[t.GetName()]; ok {
		return nil, status.Error(codes.AlreadyExists, "topic already exists")
	}
	f.topics[t.GetName()] = proto.Clone(t).(*pubsubpb.Topic)
	return t, nil
}

func (f *fakePubSub) GetTopic(_ context.Context, r *pubsubpb.GetTopicRequest) (*pubsubpb.Topic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.topics[r.GetTopic()]
	if !ok {
		return nil, status.Error(codes.NotFound, "topic not found")
	}
	return t, nil
}

func (f *fakePubSub) UpdateTopic(_ context.Context, r *pubsubpb.UpdateTopicRequest) (*pubsubpb.Topic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	paths := r.GetUpdateMask().GetPaths()
	f.appliedMasks = append(f.appliedMasks, paths)

	// The emulator validates the whole mask before touching anything.
	for _, p := range paths {
		if f.unsupportedPaths[p] {
			return nil, status.Errorf(codes.InvalidArgument,
				"Invalid update_mask provided in the UpdateTopicRequest: %s is not a known Topic field.", p)
		}
	}

	existing, ok := f.topics[r.GetTopic().GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "topic not found")
	}
	for _, p := range paths {
		switch p {
		case "labels":
			existing.Labels = r.GetTopic().GetLabels()
		case "message_retention_duration":
			existing.MessageRetentionDuration = r.GetTopic().GetMessageRetentionDuration()
		}
	}
	return existing, nil
}

func (f *fakePubSub) CreateSubscription(_ context.Context, s *pubsubpb.Subscription) (*pubsubpb.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.subs[s.GetName()]; ok {
		return nil, status.Error(codes.AlreadyExists, "subscription already exists")
	}
	f.subs[s.GetName()] = proto.Clone(s).(*pubsubpb.Subscription)
	return s, nil
}

func (f *fakePubSub) GetSubscription(_ context.Context, r *pubsubpb.GetSubscriptionRequest) (*pubsubpb.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subs[r.GetSubscription()]
	if !ok {
		return nil, status.Error(codes.NotFound, "subscription not found")
	}
	return s, nil
}

func (f *fakePubSub) UpdateSubscription(_ context.Context, r *pubsubpb.UpdateSubscriptionRequest) (*pubsubpb.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	paths := r.GetUpdateMask().GetPaths()
	f.appliedMasks = append(f.appliedMasks, paths)

	for _, p := range paths {
		if f.unsupportedPaths[p] {
			return nil, status.Errorf(codes.InvalidArgument,
				"Invalid update_mask provided in the UpdateSubscriptionRequest: %s is not a known Subscription field.", p)
		}
	}

	existing, ok := f.subs[r.GetSubscription().GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "subscription not found")
	}
	in := r.GetSubscription()
	for _, p := range paths {
		switch p {
		case "ack_deadline_seconds":
			existing.AckDeadlineSeconds = in.GetAckDeadlineSeconds()
		case "message_retention_duration":
			existing.MessageRetentionDuration = in.GetMessageRetentionDuration()
		case "retain_acked_messages":
			existing.RetainAckedMessages = in.GetRetainAckedMessages()
		case "enable_exactly_once_delivery":
			existing.EnableExactlyOnceDelivery = in.GetEnableExactlyOnceDelivery()
		case "expiration_policy":
			existing.ExpirationPolicy = in.GetExpirationPolicy()
		case "labels":
			existing.Labels = in.GetLabels()
		case "push_config":
			existing.PushConfig = in.GetPushConfig()
		case "dead_letter_policy":
			existing.DeadLetterPolicy = in.GetDeadLetterPolicy()
		case "retry_policy":
			existing.RetryPolicy = in.GetRetryPolicy()
		}
	}
	return existing, nil
}

func (f *fakePubSub) masks() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appliedMasks
}

func (f *fakePubSub) refuse(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsupportedPaths[path] = true
}

func connectFakePubSub(t *testing.T, host string) *PubSubClient {
	t.Helper()

	c, err := ConnectPubSub(context.Background(), "local-dev", host)
	if err != nil {
		t.Fatalf("ConnectPubSub: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
