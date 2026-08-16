package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Topic is a transport-neutral Pub/Sub topic.
type Topic struct {
	// Name is the short topic id, not the projects/*/topics/* path.
	Name             string
	Labels           map[string]string
	MessageRetention time.Duration
}

// DeadLetterPolicy mirrors the Pub/Sub dead letter settings.
type DeadLetterPolicy struct {
	// Topic is the short topic id of the dead letter target.
	Topic               string
	MaxDeliveryAttempts int32
}

// RetryPolicy mirrors the Pub/Sub redelivery backoff settings.
type RetryPolicy struct {
	MinimumBackoff time.Duration
	MaximumBackoff time.Duration
}

// Subscription is a transport-neutral Pub/Sub subscription.
type Subscription struct {
	// Name and Topic are short ids, not fully qualified resource paths.
	Name             string
	Topic            string
	AckDeadline      time.Duration
	MessageRetention time.Duration
	// RetainAckedMessages, EnableMessageOrdering and EnableExactlyOnceDelivery
	// are pointers for the same reason their config counterparts are: nil means
	// the config said nothing and the live setting must be left alone. A plain
	// bool would read as false and turn off something enabled out of band.
	//
	// fromProtoSubscription always populates them, so a subscription read back
	// from the API never carries a nil; only a desired one built from config can.
	RetainAckedMessages       *bool
	EnableMessageOrdering     *bool
	EnableExactlyOnceDelivery *bool
	Filter                    string
	// ExpirationSet distinguishes "leave expiry alone" from an explicit value.
	// When it is true, an ExpirationTTL of 0 means the subscription never expires.
	ExpirationSet bool
	ExpirationTTL time.Duration
	Labels        map[string]string
	// Push is nil when the config declared no push delivery, matching DeadLetter
	// and Retry. Pub/Sub returns an empty PushConfig for a pull subscription, so
	// a live subscription may carry a non-nil Push with an empty Endpoint.
	Push       *PushConfig
	DeadLetter *DeadLetterPolicy
	Retry      *RetryPolicy
}

// PushConfig mirrors the Pub/Sub push delivery settings.
type PushConfig struct {
	Endpoint   string
	Attributes map[string]string
	// OIDCToken is the authentication_method oneof; nil means unauthenticated.
	OIDCToken *OIDCToken
	// Wrapper is the wrapper oneof. WrapperUnset on a desired subscription means
	// the config did not ask for a wrapper; on a live one it means the endpoint
	// reported none, which Pub/Sub treats as WrapperPubSub.
	Wrapper Wrapper
	// WriteMetadata only applies to WrapperNone.
	WriteMetadata bool
}

// OIDCToken is the token Pub/Sub mints when calling an authenticated push endpoint.
type OIDCToken struct {
	ServiceAccountEmail string
	Audience            string
}

// Wrapper distinguishes an unstated push wrapper from an explicit choice.
type Wrapper int

// Push wrapper states.
const (
	WrapperUnset Wrapper = iota
	WrapperPubSub
	WrapperNone
)

// PubSubClient wraps the Pub/Sub admin APIs for a single project.
type PubSubClient struct {
	projectID string
	topics    *vkit.TopicAdminClient
	subs      *vkit.SubscriptionAdminClient
}

// ErrNotFound reports that a topic or subscription does not exist.
var ErrNotFound = errors.New("resource not found")

// ConnectPubSub dials the Pub/Sub admin APIs and blocks until they answer.
//
// An empty emulatorHost means Google Cloud, reached with Application Default
// Credentials; a host:port means the emulator, reached without credentials over
// an insecure connection.
func ConnectPubSub(ctx context.Context, projectID, emulatorHost string) (*PubSubClient, error) {
	var opts []option.ClientOption
	if emulatorHost != "" {
		opts = append(opts,
			option.WithEndpoint(emulatorHost),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		)
	}

	topics, err := vkit.NewTopicAdminClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating topic admin client: %w", err)
	}

	subs, err := vkit.NewSubscriptionAdminClient(ctx, opts...)
	if err != nil {
		// The topic client owns a gRPC connection that would otherwise leak.
		_ = topics.Close()
		return nil, fmt.Errorf("creating subscription admin client: %w", err)
	}

	c := &PubSubClient{projectID: projectID, topics: topics, subs: subs}

	// ListTopics is the cheapest call that proves the service is answering.
	probe := func(ctx context.Context) error {
		it := c.topics.ListTopics(ctx, &pubsubpb.ListTopicsRequest{
			Project:  c.projectPath(),
			PageSize: 1,
		})
		_, err := it.Next()
		if err != nil && !errors.Is(err, iterator.Done) {
			return err
		}
		return nil
	}

	if err := retryUntilReady(ctx, "Pub/Sub", probe); err != nil {
		_ = c.Close()
		return nil, err
	}

	slog.Info("Connected to Pub/Sub", "project", projectID, "emulator", emulatorHost != "")
	return c, nil
}

// Close releases both admin clients.
func (c *PubSubClient) Close() error {
	return errors.Join(c.topics.Close(), c.subs.Close())
}

func (c *PubSubClient) projectPath() string {
	return "projects/" + c.projectID
}

// TopicPath returns the fully qualified resource name for a topic id.
func (c *PubSubClient) TopicPath(name string) string {
	return c.projectPath() + "/topics/" + name
}

// SubscriptionPath returns the fully qualified resource name for a subscription id.
func (c *PubSubClient) SubscriptionPath(name string) string {
	return c.projectPath() + "/subscriptions/" + name
}

// BoolPtr returns a pointer to b, for building a tri-state field from a known value.
func BoolPtr(b bool) *bool { return &b }

// DerefBool reads a tri-state bool as the concrete value the proto needs. A nil
// pointer means the config left the field alone, which on the wire is the
// Pub/Sub default, false.
func DerefBool(b *bool) bool { return b != nil && *b }

// shortName returns the trailing id of a fully qualified resource path.
func shortName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// GetTopic returns the topic, or ErrNotFound if it does not exist.
func (c *PubSubClient) GetTopic(ctx context.Context, name string) (*Topic, error) {
	t, err := c.topics.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: c.TopicPath(name)})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("getting topic %q: %w", name, err)
	}
	return &Topic{
		Name:             shortName(t.GetName()),
		Labels:           t.GetLabels(),
		MessageRetention: t.GetMessageRetentionDuration().AsDuration(),
	}, nil
}

// CreateTopic creates a topic. An already existing topic is not an error, so
// concurrent provisioner runs do not fight each other.
func (c *PubSubClient) CreateTopic(ctx context.Context, t Topic) error {
	req := &pubsubpb.Topic{
		Name:   c.TopicPath(t.Name),
		Labels: t.Labels,
	}
	if t.MessageRetention > 0 {
		req.MessageRetentionDuration = durationpb.New(t.MessageRetention)
	}

	if _, err := c.topics.CreateTopic(ctx, req); err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil
		}
		return fmt.Errorf("creating topic %q: %w", t.Name, err)
	}
	return nil
}

// UpdateTopic applies the named field paths to an existing topic.
func (c *PubSubClient) UpdateTopic(ctx context.Context, t Topic, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	req := &pubsubpb.Topic{
		Name:   c.TopicPath(t.Name),
		Labels: t.Labels,
	}
	if t.MessageRetention > 0 {
		req.MessageRetentionDuration = durationpb.New(t.MessageRetention)
	}

	_, err := c.topics.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic:      req,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
	})
	if err != nil {
		return fmt.Errorf("updating topic %q (%s): %w", t.Name, strings.Join(paths, ","), err)
	}
	return nil
}

// GetSubscription returns the subscription, or ErrNotFound if it does not exist.
func (c *PubSubClient) GetSubscription(ctx context.Context, name string) (*Subscription, error) {
	s, err := c.subs.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{
		Subscription: c.SubscriptionPath(name),
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("getting subscription %q: %w", name, err)
	}
	return fromProtoSubscription(s), nil
}

// CreateSubscription creates a subscription. An already existing subscription is
// not an error, matching CreateTopic.
func (c *PubSubClient) CreateSubscription(ctx context.Context, s Subscription) error {
	if _, err := c.subs.CreateSubscription(ctx, c.toProtoSubscription(s)); err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil
		}
		return fmt.Errorf("creating subscription %q: %w", s.Name, err)
	}
	return nil
}

// UpdateSubscription applies the named field paths to an existing subscription.
func (c *PubSubClient) UpdateSubscription(ctx context.Context, s Subscription, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	_, err := c.subs.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: c.toProtoSubscription(s),
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: paths},
	})
	if err != nil {
		return fmt.Errorf("updating subscription %q (%s): %w", s.Name, strings.Join(paths, ","), err)
	}
	return nil
}

func (c *PubSubClient) toProtoSubscription(s Subscription) *pubsubpb.Subscription {
	out := &pubsubpb.Subscription{
		Name:  c.SubscriptionPath(s.Name),
		Topic: c.TopicPath(s.Topic),
		// A nil tri-state sends the Pub/Sub default. At create time that is what
		// an omitted key means anyway; at update time the field only enters the
		// mask when the config set it, so the value sent here is inert.
		RetainAckedMessages:       DerefBool(s.RetainAckedMessages),
		EnableMessageOrdering:     DerefBool(s.EnableMessageOrdering),
		EnableExactlyOnceDelivery: DerefBool(s.EnableExactlyOnceDelivery),
		Filter:                    s.Filter,
		Labels:                    s.Labels,
	}

	if s.AckDeadline > 0 {
		out.AckDeadlineSeconds = int32(s.AckDeadline.Seconds())
	}
	if s.MessageRetention > 0 {
		out.MessageRetentionDuration = durationpb.New(s.MessageRetention)
	}
	if s.ExpirationSet {
		// A policy with no TTL is how Pub/Sub expresses "never expires".
		out.ExpirationPolicy = &pubsubpb.ExpirationPolicy{}
		if s.ExpirationTTL > 0 {
			out.ExpirationPolicy.Ttl = durationpb.New(s.ExpirationTTL)
		}
	}
	if s.Push != nil {
		out.PushConfig = toProtoPushConfig(s.Push)
	}
	if s.DeadLetter != nil {
		out.DeadLetterPolicy = &pubsubpb.DeadLetterPolicy{
			DeadLetterTopic:     c.TopicPath(s.DeadLetter.Topic),
			MaxDeliveryAttempts: s.DeadLetter.MaxDeliveryAttempts,
		}
	}
	if s.Retry != nil {
		out.RetryPolicy = &pubsubpb.RetryPolicy{}
		if s.Retry.MinimumBackoff > 0 {
			out.RetryPolicy.MinimumBackoff = durationpb.New(s.Retry.MinimumBackoff)
		}
		if s.Retry.MaximumBackoff > 0 {
			out.RetryPolicy.MaximumBackoff = durationpb.New(s.Retry.MaximumBackoff)
		}
	}

	return out
}

// toProtoPushConfig converts the push settings, leaving each oneof unset when
// the config did not ask for it. Verified against cloud-sdk:emulators: both
// oneofs are accepted at create, accepted in a `push_config` update mask, and
// echoed back by GetSubscription.
func toProtoPushConfig(p *PushConfig) *pubsubpb.PushConfig {
	out := &pubsubpb.PushConfig{
		PushEndpoint: p.Endpoint,
		Attributes:   p.Attributes,
	}

	if p.OIDCToken != nil {
		out.AuthenticationMethod = &pubsubpb.PushConfig_OidcToken_{
			OidcToken: &pubsubpb.PushConfig_OidcToken{
				ServiceAccountEmail: p.OIDCToken.ServiceAccountEmail,
				Audience:            p.OIDCToken.Audience,
			},
		}
	}

	switch p.Wrapper {
	case WrapperPubSub:
		out.Wrapper = &pubsubpb.PushConfig_PubsubWrapper_{
			PubsubWrapper: &pubsubpb.PushConfig_PubsubWrapper{},
		}
	case WrapperNone:
		out.Wrapper = &pubsubpb.PushConfig_NoWrapper_{
			NoWrapper: &pubsubpb.PushConfig_NoWrapper{WriteMetadata: p.WriteMetadata},
		}
	case WrapperUnset:
		// Leave the oneof nil so Pub/Sub keeps whatever the subscription has.
	}

	return out
}

// fromProtoPushConfig converts the push settings back. A nil PushConfig, which
// is what a pull subscription reports, stays nil.
func fromProtoPushConfig(pc *pubsubpb.PushConfig) *PushConfig {
	if pc == nil {
		return nil
	}

	out := &PushConfig{
		Endpoint:   pc.GetPushEndpoint(),
		Attributes: pc.GetAttributes(),
	}

	if t := pc.GetOidcToken(); t != nil {
		out.OIDCToken = &OIDCToken{
			ServiceAccountEmail: t.GetServiceAccountEmail(),
			Audience:            t.GetAudience(),
		}
	}

	switch {
	case pc.GetNoWrapper() != nil:
		out.Wrapper = WrapperNone
		out.WriteMetadata = pc.GetNoWrapper().GetWriteMetadata()
	case pc.GetPubsubWrapper() != nil:
		out.Wrapper = WrapperPubSub
	}

	return out
}

func fromProtoSubscription(s *pubsubpb.Subscription) *Subscription {
	out := &Subscription{
		Name:                      shortName(s.GetName()),
		Topic:                     shortName(s.GetTopic()),
		AckDeadline:               time.Duration(s.GetAckDeadlineSeconds()) * time.Second,
		MessageRetention:          s.GetMessageRetentionDuration().AsDuration(),
		RetainAckedMessages:       BoolPtr(s.GetRetainAckedMessages()),
		EnableMessageOrdering:     BoolPtr(s.GetEnableMessageOrdering()),
		EnableExactlyOnceDelivery: BoolPtr(s.GetEnableExactlyOnceDelivery()),
		Filter:                    s.GetFilter(),
		Labels:                    s.GetLabels(),
		Push:                      fromProtoPushConfig(s.GetPushConfig()),
	}

	if p := s.GetExpirationPolicy(); p != nil {
		out.ExpirationSet = true
		out.ExpirationTTL = p.GetTtl().AsDuration()
	}
	if p := s.GetDeadLetterPolicy(); p != nil {
		out.DeadLetter = &DeadLetterPolicy{
			Topic:               shortName(p.GetDeadLetterTopic()),
			MaxDeliveryAttempts: p.GetMaxDeliveryAttempts(),
		}
	}
	if p := s.GetRetryPolicy(); p != nil {
		out.Retry = &RetryPolicy{
			MinimumBackoff: p.GetMinimumBackoff().AsDuration(),
			MaximumBackoff: p.GetMaximumBackoff().AsDuration(),
		}
	}

	return out
}

// IsUnsupportedField reports whether err is the emulator refusing a field it
// does not model, rather than a genuine problem with the request.
//
// The Pub/Sub emulator rejects update masks that Google Cloud accepts — `labels`
// on both topics and subscriptions, and `expiration_policy` on subscriptions —
// with InvalidArgument. Treating those as fatal would make a config that is
// perfectly valid against Google Cloud fail against the emulator, so the
// provisioner downgrades them to a warning. Anything else stays fatal.
func IsUnsupportedField(err error) bool {
	if status.Code(err) != codes.InvalidArgument {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "is not a known") ||
		strings.Contains(msg, "currently unsupported in the Pub/Sub Emulator")
}
