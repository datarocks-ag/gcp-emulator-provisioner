package provisioner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gcp-emulator-provisioner/internal/client"
	"gcp-emulator-provisioner/internal/config"
)

// emulatorUnsupportedErr reproduces the InvalidArgument the Pub/Sub emulator
// returns for an update mask naming a field it does not model. Verified against
// cloud-sdk:emulators, which returns this same text whether the path is sent
// alone or batched with others.
func emulatorUnsupportedErr() error {
	return status.Error(codes.InvalidArgument,
		"Invalid update_mask provided in the UpdateSubscriptionRequest: labels is not a known Subscription field.")
}

// emulatorUnsupportedTopicErr is the UpdateTopic equivalent.
func emulatorUnsupportedTopicErr() error {
	return status.Error(codes.InvalidArgument,
		"Invalid update_mask provided in the UpdateTopicRequest: labels is not a known Topic field.")
}

// mockPubSub implements PubSubAdmin, recording the calls the provisioner makes.
type mockPubSub struct {
	topics map[string]*client.Topic
	subs   map[string]*client.Subscription

	createdTopics []client.Topic
	updatedTopics []maskedTopic
	createdSubs   []client.Subscription
	updatedSubs   []maskedSub

	updateTopicErr error
	updateSubErr   error

	// unsupported*Paths make this endpoint refuse specific mask paths, the way
	// the Pub/Sub emulator refuses `labels`. sub/topicUpdateCalls record every
	// mask attempted, including the ones that failed, so a test can tell a
	// batch-then-split apart from a single call.
	unsupportedSubPaths   map[string]bool
	unsupportedTopicPaths map[string]bool
	subUpdateCalls        [][]string
	topicUpdateCalls      [][]string

	// updateSubErrPaths returns a non-emulator error for a specific path, to
	// check a real failure during the split still fails the run.
	updateSubErrPaths map[string]error
}

// refuses reports whether any requested path is one this endpoint does not model.
func refuses(unsupported map[string]bool, paths []string) bool {
	for _, p := range paths {
		if unsupported[p] {
			return true
		}
	}
	return false
}

// captureLogs redirects slog to a buffer for the duration of the test, so a
// test can assert on warnings the provisioner emits instead of returning.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

type maskedTopic struct {
	topic client.Topic
	paths []string
}

type maskedSub struct {
	sub   client.Subscription
	paths []string
}

func newMockPubSub() *mockPubSub {
	return &mockPubSub{
		topics: map[string]*client.Topic{},
		subs:   map[string]*client.Subscription{},
	}
}

func (m *mockPubSub) GetTopic(_ context.Context, name string) (*client.Topic, error) {
	if t, ok := m.topics[name]; ok {
		return t, nil
	}
	return nil, client.ErrNotFound
}

func (m *mockPubSub) CreateTopic(_ context.Context, t client.Topic) error {
	m.createdTopics = append(m.createdTopics, t)
	copied := t
	m.topics[t.Name] = &copied
	return nil
}

func (m *mockPubSub) UpdateTopic(_ context.Context, t client.Topic, paths []string) error {
	m.topicUpdateCalls = append(m.topicUpdateCalls, paths)
	if m.updateTopicErr != nil {
		return m.updateTopicErr
	}
	if refuses(m.unsupportedTopicPaths, paths) {
		return emulatorUnsupportedTopicErr()
	}
	m.updatedTopics = append(m.updatedTopics, maskedTopic{t, paths})
	return nil
}

func (m *mockPubSub) GetSubscription(_ context.Context, name string) (*client.Subscription, error) {
	if s, ok := m.subs[name]; ok {
		return s, nil
	}
	return nil, client.ErrNotFound
}

func (m *mockPubSub) CreateSubscription(_ context.Context, s client.Subscription) error {
	m.createdSubs = append(m.createdSubs, s)
	copied := s
	m.subs[s.Name] = &copied
	return nil
}

func (m *mockPubSub) UpdateSubscription(_ context.Context, s client.Subscription, paths []string) error {
	m.subUpdateCalls = append(m.subUpdateCalls, paths)
	if m.updateSubErr != nil {
		return m.updateSubErr
	}
	for _, p := range paths {
		if err, ok := m.updateSubErrPaths[p]; ok {
			return err
		}
	}
	if refuses(m.unsupportedSubPaths, paths) {
		return emulatorUnsupportedErr()
	}
	m.updatedSubs = append(m.updatedSubs, maskedSub{s, paths})
	return nil
}

// mockStorage implements StorageAdmin.
type mockStorage struct {
	buckets map[string]*client.Bucket

	created         []client.Bucket
	versioningCalls []versioningCall

	setVersioningErr error
}

type versioningCall struct {
	name    string
	enabled bool
}

func newMockStorage() *mockStorage {
	return &mockStorage{buckets: map[string]*client.Bucket{}}
}

func (m *mockStorage) GetBucket(_ context.Context, name string) (*client.Bucket, error) {
	if b, ok := m.buckets[name]; ok {
		return b, nil
	}
	return nil, client.ErrBucketNotFound
}

func (m *mockStorage) CreateBucket(_ context.Context, b client.Bucket) error {
	m.created = append(m.created, b)
	copied := b
	m.buckets[b.Name] = &copied
	return nil
}

func (m *mockStorage) SetVersioning(_ context.Context, name string, enabled bool) error {
	if m.setVersioningErr != nil {
		return m.setVersioningErr
	}
	m.versioningCalls = append(m.versioningCalls, versioningCall{name, enabled})
	if b, ok := m.buckets[name]; ok {
		b.VersioningEnabled = enabled
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }

func TestRunCreatesTopicsBeforeSubscriptions(t *testing.T) {
	ps := newMockPubSub()
	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{
		{
			Name: "orders",
			Subscriptions: []config.Subscription{
				{Name: "processor", AckDeadline: "30s", DeadLetter: &config.DeadLetter{
					Topic: "orders-dlq", MaxDeliveryAttempts: 5,
				}},
			},
		},
		{Name: "orders-dlq"},
	}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.createdTopics) != 2 {
		t.Fatalf("created %d topics, want 2", len(ps.createdTopics))
	}
	if len(ps.createdSubs) != 1 {
		t.Fatalf("created %d subscriptions, want 1", len(ps.createdSubs))
	}
	// The dead letter topic is declared after the subscription that uses it, so
	// the whole topic pass has to finish before any subscription is created.
	if ps.createdTopics[1].Name != "orders-dlq" {
		t.Errorf("second topic = %q, want orders-dlq", ps.createdTopics[1].Name)
	}
	if got := ps.createdSubs[0].DeadLetter; got == nil || got.Topic != "orders-dlq" {
		t.Errorf("subscription dead letter = %+v, want orders-dlq", got)
	}
}

func TestEnsureTopicIsIdempotent(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{
		Name:             "orders",
		Labels:           map[string]string{"domain": "commerce"},
		MessageRetention: 24 * time.Hour,
	}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name:             "orders",
		Labels:           map[string]string{"domain": "commerce"},
		MessageRetention: "24h",
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.createdTopics) != 0 || len(ps.updatedTopics) != 0 {
		t.Errorf("an up-to-date topic was mutated: created=%v updated=%v", ps.createdTopics, ps.updatedTopics)
	}
}

func TestEnsureTopicUpdatesOnlyDriftedFields(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{
		Name:             "orders",
		Labels:           map[string]string{"domain": "commerce"},
		MessageRetention: time.Hour,
	}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name:             "orders",
		Labels:           map[string]string{"domain": "commerce"},
		MessageRetention: "24h",
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.updatedTopics) != 1 {
		t.Fatalf("got %d updates, want 1", len(ps.updatedTopics))
	}
	paths := ps.updatedTopics[0].paths
	if len(paths) != 1 || paths[0] != pathTopicMessageRetention {
		t.Errorf("update mask = %v, want only %q", paths, pathTopicMessageRetention)
	}
}

func TestEnsureTopicPreservesUnmanagedLabels(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{
		Name:   "orders",
		Labels: map[string]string{"managed-elsewhere": "keep"},
	}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name:   "orders",
		Labels: map[string]string{"domain": "commerce"},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.updatedTopics) != 1 {
		t.Fatalf("got %d updates, want 1", len(ps.updatedTopics))
	}
	sent := ps.updatedTopics[0].topic.Labels
	if sent["managed-elsewhere"] != "keep" {
		t.Errorf("label set out of band was dropped: %v", sent)
	}
	if sent["domain"] != "commerce" {
		t.Errorf("configured label missing: %v", sent)
	}
}

func TestStrategyCreateSkipsExistingResources(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders", MessageRetention: time.Hour}
	ps.subs["processor"] = &client.Subscription{Name: "processor", Topic: "orders", AckDeadline: 10 * time.Second}

	cfg := &config.Config{
		Strategy: "create",
		PubSub: config.PubSub{Topics: []config.Topic{{
			Name:             "orders",
			MessageRetention: "24h",
			Subscriptions:    []config.Subscription{{Name: "processor", AckDeadline: "60s"}},
		}}},
	}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.updatedTopics) != 0 || len(ps.updatedSubs) != 0 {
		t.Errorf("strategy=create mutated existing resources: topics=%v subs=%v", ps.updatedTopics, ps.updatedSubs)
	}
}

func TestDryRunAppliesNothing(t *testing.T) {
	ps := newMockPubSub()
	st := newMockStorage()
	ps.topics["orders"] = &client.Topic{Name: "orders", MessageRetention: time.Hour}
	st.buckets["documents"] = &client.Bucket{Name: "documents", VersioningEnabled: false}

	cfg := &config.Config{
		PubSub: config.PubSub{Topics: []config.Topic{{
			Name:             "orders",
			MessageRetention: "24h",
			Subscriptions:    []config.Subscription{{Name: "processor", AckDeadline: "30s"}},
		}}},
		Storage: config.Storage{Buckets: []config.Bucket{
			{Name: "documents", Versioning: boolPtr(true)},
			{Name: "new-bucket", Versioning: boolPtr(true)},
		}},
	}

	p := New(ps, st, cfg, Options{DryRun: true})
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(ps.createdTopics)+len(ps.updatedTopics)+len(ps.createdSubs)+len(ps.updatedSubs) != 0 {
		t.Error("dry run mutated Pub/Sub")
	}
	if len(st.created)+len(st.versioningCalls) != 0 {
		t.Error("dry run mutated Cloud Storage")
	}
}

func TestEnsureSubscriptionUpdatesMutableFieldsOnly(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	ps.subs["processor"] = &client.Subscription{
		Name:        "processor",
		Topic:       "orders",
		AckDeadline: 10 * time.Second,
		// Immutable fields that the config below disagrees with.
		Filter:                `attributes.type = "old"`,
		EnableMessageOrdering: boolPtr(false),
	}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name: "orders",
		Subscriptions: []config.Subscription{{
			Name:                  "processor",
			AckDeadline:           "60s",
			Filter:                `attributes.type = "new"`,
			EnableMessageOrdering: boolPtr(true),
		}},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.updatedSubs) != 1 {
		t.Fatalf("got %d updates, want 1", len(ps.updatedSubs))
	}
	for _, path := range ps.updatedSubs[0].paths {
		if path == "filter" || path == "enable_message_ordering" {
			t.Errorf("update mask names immutable field %q: %v", path, ps.updatedSubs[0].paths)
		}
	}
	if len(ps.updatedSubs[0].paths) != 1 || ps.updatedSubs[0].paths[0] != pathSubAckDeadline {
		t.Errorf("update mask = %v, want only %q", ps.updatedSubs[0].paths, pathSubAckDeadline)
	}
}

func TestEnsureSubscriptionIsIdempotent(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	ps.subs["processor"] = &client.Subscription{
		Name:                      "processor",
		Topic:                     "orders",
		AckDeadline:               30 * time.Second,
		MessageRetention:          7 * 24 * time.Hour,
		EnableExactlyOnceDelivery: boolPtr(true),
		ExpirationSet:             true,
		DeadLetter:                &client.DeadLetterPolicy{Topic: "orders-dlq", MaxDeliveryAttempts: 5},
		Retry:                     &client.RetryPolicy{MinimumBackoff: 10 * time.Second, MaximumBackoff: 600 * time.Second},
	}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{
		{
			Name: "orders",
			Subscriptions: []config.Subscription{{
				Name:                      "processor",
				AckDeadline:               "30s",
				MessageRetention:          "7d",
				EnableExactlyOnceDelivery: boolPtr(true),
				ExpirationTTL:             config.ExpirationNever,
				DeadLetter:                &config.DeadLetter{Topic: "orders-dlq", MaxDeliveryAttempts: 5},
				Retry:                     &config.Retry{MinimumBackoff: "10s", MaximumBackoff: "600s"},
			}},
		},
		{Name: "orders-dlq"},
	}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.updatedSubs) != 0 {
		t.Errorf("an up-to-date subscription was updated with %v", ps.updatedSubs[0].paths)
	}
}

func TestEnsureSubscriptionTolerateEmulatorUnsupportedField(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	ps.subs["processor"] = &client.Subscription{Name: "processor", Topic: "orders", AckDeadline: 10 * time.Second}
	ps.updateSubErr = emulatorUnsupportedErr()

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name:          "orders",
		Subscriptions: []config.Subscription{{Name: "processor", AckDeadline: "60s"}},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	// The emulator rejecting a field Google Cloud accepts is a warning, not a
	// failed run.
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}
}

func TestEnsureSubscriptionPropagatesRealErrors(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	ps.subs["processor"] = &client.Subscription{Name: "processor", Topic: "orders", AckDeadline: 10 * time.Second}
	ps.updateSubErr = errors.New("permission denied")

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name:          "orders",
		Subscriptions: []config.Subscription{{Name: "processor", AckDeadline: "60s"}},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err == nil {
		t.Fatal("expected the update error to fail the run")
	}
}

func TestOmittedTriStateBoolsAreNotDrift(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	// Both were enabled out of band. The config below says nothing about either.
	ps.subs["processor"] = &client.Subscription{
		Name:                      "processor",
		Topic:                     "orders",
		AckDeadline:               10 * time.Second,
		RetainAckedMessages:       boolPtr(true),
		EnableExactlyOnceDelivery: boolPtr(true),
	}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name:          "orders",
		Subscriptions: []config.Subscription{{Name: "processor", AckDeadline: "60s"}},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.updatedSubs) != 1 {
		t.Fatalf("got %d updates, want 1", len(ps.updatedSubs))
	}
	paths := ps.updatedSubs[0].paths
	if len(paths) != 1 || paths[0] != pathSubAckDeadline {
		t.Errorf("update mask = %v, want only %q — an omitted tri-state must not turn a live setting off",
			paths, pathSubAckDeadline)
	}
}

func TestExplicitFalseTurnsTriStateOff(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	ps.subs["processor"] = &client.Subscription{
		Name:                "processor",
		Topic:               "orders",
		AckDeadline:         30 * time.Second,
		RetainAckedMessages: boolPtr(true),
	}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name: "orders",
		Subscriptions: []config.Subscription{{
			Name: "processor", AckDeadline: "30s", RetainAckedMessages: boolPtr(false),
		}},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.updatedSubs) != 1 {
		t.Fatalf("got %d updates, want 1", len(ps.updatedSubs))
	}
	if paths := ps.updatedSubs[0].paths; len(paths) != 1 || paths[0] != pathSubRetainAcked {
		t.Errorf("update mask = %v, want %q — an explicit false must still be applied", paths, pathSubRetainAcked)
	}
}

func TestOmittedImmutableFieldsDoNotWarn(t *testing.T) {
	logs := captureLogs(t)

	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	// Both immutable fields are set live; the config mentions neither.
	ps.subs["processor"] = &client.Subscription{
		Name:                  "processor",
		Topic:                 "orders",
		AckDeadline:           30 * time.Second,
		EnableMessageOrdering: boolPtr(true),
		Filter:                `attributes.type = "order"`,
	}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name:          "orders",
		Subscriptions: []config.Subscription{{Name: "processor", AckDeadline: "30s"}},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if strings.Contains(logs.String(), "cannot be changed after creation") {
		t.Errorf("omitted immutable fields warned on a run that asked for no change:\n%s", logs.String())
	}
}

func TestEnsureSubscriptionAppliesSupportedPathsWhenOneIsRefused(t *testing.T) {
	logs := captureLogs(t)

	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	ps.subs["processor"] = &client.Subscription{
		Name: "processor", Topic: "orders", AckDeadline: 10 * time.Second,
	}
	ps.unsupportedSubPaths = map[string]bool{pathSubLabels: true}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name: "orders",
		Subscriptions: []config.Subscription{{
			Name: "processor", AckDeadline: "60s", Labels: map[string]string{"team": "platform"},
		}},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	// Batch, then one call per path.
	if len(ps.subUpdateCalls) != 3 {
		t.Fatalf("attempted %d update calls, want 3 (batch + one per path): %v",
			len(ps.subUpdateCalls), ps.subUpdateCalls)
	}
	if len(ps.updatedSubs) != 1 {
		t.Fatalf("applied %d updates, want 1", len(ps.updatedSubs))
	}
	if paths := ps.updatedSubs[0].paths; len(paths) != 1 || paths[0] != pathSubAckDeadline {
		t.Errorf("applied mask = %v, want %q — a refused path must not discard the rest of the batch",
			paths, pathSubAckDeadline)
	}
	if !strings.Contains(logs.String(), `"field":"labels"`) {
		t.Errorf("warning should name the refused path, got:\n%s", logs.String())
	}
}

func TestEnsureSubscriptionSinglePathRefusedIsNotRetried(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	ps.subs["processor"] = &client.Subscription{Name: "processor", Topic: "orders", AckDeadline: 30 * time.Second}
	ps.unsupportedSubPaths = map[string]bool{pathSubLabels: true}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name: "orders",
		Subscriptions: []config.Subscription{{
			Name: "processor", AckDeadline: "30s", Labels: map[string]string{"team": "platform"},
		}},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.subUpdateCalls) != 1 {
		t.Errorf("attempted %d calls, want 1 — a one-path batch has nothing to split: %v",
			len(ps.subUpdateCalls), ps.subUpdateCalls)
	}
}

func TestEnsureSubscriptionRealErrorDuringSplitFailsRun(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	ps.subs["processor"] = &client.Subscription{Name: "processor", Topic: "orders", AckDeadline: 10 * time.Second}
	ps.unsupportedSubPaths = map[string]bool{pathSubLabels: true}
	ps.updateSubErrPaths = map[string]error{pathSubAckDeadline: errors.New("permission denied")}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name: "orders",
		Subscriptions: []config.Subscription{{
			Name: "processor", AckDeadline: "60s", Labels: map[string]string{"team": "platform"},
		}},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err == nil {
		t.Fatal("a real error during the per-path retry must fail the run")
	}
}

func TestEnsureTopicAppliesSupportedPathsWhenOneIsRefused(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders", MessageRetention: time.Hour}
	ps.unsupportedTopicPaths = map[string]bool{pathTopicLabels: true}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name:             "orders",
		MessageRetention: "24h",
		Labels:           map[string]string{"domain": "commerce"},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.updatedTopics) != 1 {
		t.Fatalf("applied %d updates, want 1", len(ps.updatedTopics))
	}
	if paths := ps.updatedTopics[0].paths; len(paths) != 1 || paths[0] != pathTopicMessageRetention {
		t.Errorf("applied mask = %v, want %q", paths, pathTopicMessageRetention)
	}
}

func TestPushConfigDriftFoldsIntoOnePath(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	ps.subs["webhook"] = &client.Subscription{
		Name: "webhook", Topic: "orders", AckDeadline: 10 * time.Second,
		Push: &client.PushConfig{
			Endpoint:  "http://consumer:8080/events",
			OIDCToken: &client.OIDCToken{ServiceAccountEmail: "pusher@local-dev.iam.gserviceaccount.com", Audience: "http://old"},
		},
	}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name: "orders",
		Subscriptions: []config.Subscription{{
			Name: "webhook", AckDeadline: "10s",
			PushEndpoint: "http://consumer:8080/events",
			PushOIDCToken: &config.PushOIDCToken{
				ServiceAccountEmail: "pusher@local-dev.iam.gserviceaccount.com",
				Audience:            "http://new",
			},
		}},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.updatedSubs) != 1 {
		t.Fatalf("got %d updates, want 1", len(ps.updatedSubs))
	}
	if paths := ps.updatedSubs[0].paths; len(paths) != 1 || paths[0] != pathSubPushConfig {
		t.Errorf("update mask = %v, want only %q", paths, pathSubPushConfig)
	}
}

func TestPushWrapperUnsetIsNotDrift(t *testing.T) {
	tests := []struct {
		name        string
		live        client.Wrapper
		configured  string
		wantUpdated bool
	}{
		{"config omits the wrapper", client.WrapperNone, "", false},
		{"config states the default the endpoint does not echo", client.WrapperUnset, config.PushWrapperPubSub, false},
		{"config states the default the endpoint echoes", client.WrapperPubSub, config.PushWrapperPubSub, false},
		{"config genuinely changes the wrapper", client.WrapperPubSub, config.PushWrapperNone, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := newMockPubSub()
			ps.topics["orders"] = &client.Topic{Name: "orders"}
			ps.subs["webhook"] = &client.Subscription{
				Name: "webhook", Topic: "orders", AckDeadline: 10 * time.Second,
				Push: &client.PushConfig{Endpoint: "http://consumer:8080", Wrapper: tt.live},
			}

			cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
				Name: "orders",
				Subscriptions: []config.Subscription{{
					Name: "webhook", AckDeadline: "10s",
					PushEndpoint: "http://consumer:8080",
					PushWrapper:  tt.configured,
				}},
			}}}}

			p := New(ps, nil, cfg, DefaultOptions())
			if err := p.ProvisionPubSub(context.Background()); err != nil {
				t.Fatalf("ProvisionPubSub: %v", err)
			}

			if updated := len(ps.updatedSubs) > 0; updated != tt.wantUpdated {
				t.Errorf("updated = %v, want %v (masks: %v)", updated, tt.wantUpdated, ps.updatedSubs)
			}
		})
	}
}

func TestPushAttributesPreservedOnEndpointChange(t *testing.T) {
	ps := newMockPubSub()
	ps.topics["orders"] = &client.Topic{Name: "orders"}
	ps.subs["webhook"] = &client.Subscription{
		Name: "webhook", Topic: "orders", AckDeadline: 10 * time.Second,
		Push: &client.PushConfig{
			Endpoint:   "http://old:8080/events",
			Attributes: map[string]string{"set-out-of-band": "keep"},
		},
	}

	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{
		Name: "orders",
		Subscriptions: []config.Subscription{{
			Name: "webhook", AckDeadline: "10s",
			PushEndpoint:   "http://new:8080/events",
			PushAttributes: map[string]string{"x-goog-version": "v1"},
		}},
	}}}}

	p := New(ps, nil, cfg, DefaultOptions())
	if err := p.ProvisionPubSub(context.Background()); err != nil {
		t.Fatalf("ProvisionPubSub: %v", err)
	}

	if len(ps.updatedSubs) != 1 {
		t.Fatalf("got %d updates, want 1", len(ps.updatedSubs))
	}
	// The mask replaces the whole push_config, so anything not carried across is lost.
	sent := ps.updatedSubs[0].sub.Push.Attributes
	if sent["set-out-of-band"] != "keep" {
		t.Errorf("push attribute set out of band was dropped: %v", sent)
	}
	if sent["x-goog-version"] != "v1" {
		t.Errorf("configured push attribute missing: %v", sent)
	}
}

func TestEnsureBucketCreatesAndReconcilesVersioning(t *testing.T) {
	st := newMockStorage()
	st.buckets["existing"] = &client.Bucket{Name: "existing", VersioningEnabled: false}

	cfg := &config.Config{Storage: config.Storage{Buckets: []config.Bucket{
		{Name: "fresh", Versioning: boolPtr(true), Location: "EU", StorageClass: "STANDARD"},
		{Name: "existing", Versioning: boolPtr(true)},
	}}}

	p := New(nil, st, cfg, DefaultOptions())
	if err := p.ProvisionStorage(context.Background()); err != nil {
		t.Fatalf("ProvisionStorage: %v", err)
	}

	if len(st.created) != 1 || st.created[0].Name != "fresh" {
		t.Fatalf("created = %+v, want just fresh", st.created)
	}
	if !st.created[0].VersioningEnabled || st.created[0].Location != "EU" {
		t.Errorf("create-time attributes not passed through: %+v", st.created[0])
	}
	if len(st.versioningCalls) != 1 || st.versioningCalls[0] != (versioningCall{"existing", true}) {
		t.Errorf("versioning calls = %+v, want one enabling it on existing", st.versioningCalls)
	}
}

func TestEnsureBucketLeavesVersioningAloneWhenOmitted(t *testing.T) {
	st := newMockStorage()
	st.buckets["docs"] = &client.Bucket{Name: "docs", VersioningEnabled: true}

	cfg := &config.Config{Storage: config.Storage{Buckets: []config.Bucket{{Name: "docs"}}}}

	p := New(nil, st, cfg, DefaultOptions())
	if err := p.ProvisionStorage(context.Background()); err != nil {
		t.Fatalf("ProvisionStorage: %v", err)
	}

	if len(st.versioningCalls) != 0 {
		t.Errorf("omitted versioning was reconciled anyway: %+v", st.versioningCalls)
	}
}

func TestEnsureBucketAnnotatesUnsupportedVersioning(t *testing.T) {
	st := newMockStorage()
	st.buckets["docs"] = &client.Bucket{Name: "docs"}
	st.setVersioningErr = errors.New("googleapi: Error 500: not implemented: fs storage type does not support versioning yet")

	cfg := &config.Config{Storage: config.Storage{Buckets: []config.Bucket{
		{Name: "docs", Versioning: boolPtr(true)},
	}}}

	p := New(nil, st, cfg, DefaultOptions())
	err := p.ProvisionStorage(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "-backend memory") {
		t.Errorf("error should name the fix, got: %v", err)
	}
}

func TestCreateOnlyDrift(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Bucket
		live client.Bucket
		want int
	}{
		{"location differs", config.Bucket{Location: "EU"}, client.Bucket{Location: "US-CENTRAL1"}, 1},
		{"location differs only in case", config.Bucket{Location: "eu"}, client.Bucket{Location: "EU"}, 0},
		{"endpoint reports no location", config.Bucket{Location: "EU"}, client.Bucket{}, 0},
		{"config omits location", config.Bucket{}, client.Bucket{Location: "US-CENTRAL1"}, 0},
		{"storage class differs", config.Bucket{StorageClass: "NEARLINE"}, client.Bucket{StorageClass: "STANDARD"}, 1},
		{
			"both differ",
			config.Bucket{Location: "EU", StorageClass: "NEARLINE"},
			client.Bucket{Location: "US-CENTRAL1", StorageClass: "STANDARD"},
			2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := createOnlyDrift(tt.cfg, &tt.live); len(got) != tt.want {
				t.Errorf("createOnlyDrift = %+v, want %d entries", got, tt.want)
			}
		})
	}
}

func TestEnsureBucketCreateOnlyDriftIsNotFatal(t *testing.T) {
	logs := captureLogs(t)

	st := newMockStorage()
	st.buckets["docs"] = &client.Bucket{Name: "docs", Location: "US-CENTRAL1", StorageClass: "STANDARD"}

	cfg := &config.Config{Storage: config.Storage{Buckets: []config.Bucket{
		{Name: "docs", Location: "EU", StorageClass: "NEARLINE"},
	}}}

	p := New(nil, st, cfg, Options{CompareCreateOnlyBucketAttrs: true})
	if err := p.ProvisionStorage(context.Background()); err != nil {
		t.Fatalf("create-only drift must be a warning, not a failure: %v", err)
	}

	if len(st.created) != 0 || len(st.versioningCalls) != 0 {
		t.Errorf("reporting drift must not mutate anything: created=%v versioning=%v",
			st.created, st.versioningCalls)
	}
	if !strings.Contains(logs.String(), `"field":"location"`) {
		t.Errorf("expected a location drift warning, got:\n%s", logs.String())
	}
}

func TestEnsureBucketCreateOnlyDriftIsOffByDefault(t *testing.T) {
	logs := captureLogs(t)

	st := newMockStorage()
	// What fake-gcs-server reports for every bucket, whatever it was created with.
	st.buckets["docs"] = &client.Bucket{Name: "docs", Location: "US-CENTRAL1", StorageClass: "STANDARD"}

	cfg := &config.Config{Storage: config.Storage{Buckets: []config.Bucket{{Name: "docs", Location: "EU"}}}}

	p := New(nil, st, cfg, DefaultOptions())
	if err := p.ProvisionStorage(context.Background()); err != nil {
		t.Fatalf("ProvisionStorage: %v", err)
	}

	if strings.Contains(logs.String(), "cannot be changed after creation") {
		t.Errorf("the emulator fabricates these values, so the default must not warn:\n%s", logs.String())
	}
}

func TestEnsureBucketNormalisesStorageClass(t *testing.T) {
	st := newMockStorage()

	cfg := &config.Config{Storage: config.Storage{Buckets: []config.Bucket{
		{Name: "docs", StorageClass: "standard"},
	}}}

	p := New(nil, st, cfg, DefaultOptions())
	if err := p.ProvisionStorage(context.Background()); err != nil {
		t.Fatalf("ProvisionStorage: %v", err)
	}

	if len(st.created) != 1 || st.created[0].StorageClass != "STANDARD" {
		t.Errorf("storage class = %q, want it upper-cased before it reaches the API", st.created[0].StorageClass)
	}
}

func TestSectionsSkipWhenNotConfigured(t *testing.T) {
	// A config with no buckets must not touch the (nil) storage client.
	cfg := &config.Config{PubSub: config.PubSub{Topics: []config.Topic{{Name: "orders"}}}}
	p := New(newMockPubSub(), nil, cfg, DefaultOptions())
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// And the reverse.
	cfg = &config.Config{Storage: config.Storage{Buckets: []config.Bucket{{Name: "docs"}}}}
	p = New(nil, newMockStorage(), cfg, DefaultOptions())
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
