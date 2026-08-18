// Package config loads, expands and validates the provisioner's YAML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// validStrategies is the allowlist of update strategy values.
var validStrategies = map[string]bool{
	"":       true, // inherits from parent/default
	"create": true, // only create if missing, skip if exists
	"update": true, // create or update (default behavior)
}

// EffectiveStrategy returns the first non-empty strategy from the given list,
// defaulting to "update" if all are empty.
func EffectiveStrategy(strategies ...string) string {
	for _, s := range strategies {
		if s != "" {
			return s
		}
	}
	return "update"
}

// Config is the top-level YAML configuration.
type Config struct {
	Strategy string `yaml:"strategy"`
	// ProjectID is the Google Cloud project every resource is created under.
	// Overridden by GCP_PROJECT_ID when that is set.
	ProjectID string  `yaml:"project_id"`
	PubSub    PubSub  `yaml:"pubsub"`
	Storage   Storage `yaml:"storage"`
	// Credentials are local key files, not a Google Cloud resource. They are
	// listed here so one config describes everything a dev stack needs.
	Credentials []Credential `yaml:"credentials"`
}

// Credential is a service account key file to write locally.
//
// The key never authenticates to anything: it exists so libraries that refuse
// to start without GOOGLE_APPLICATION_CREDENTIALS have a parseable file.
type Credential struct {
	// Path is where the key file is written, absolute or relative to the
	// working directory.
	Path string `yaml:"path"`
	// Strategy is "create" (the default here) or "update". Unlike every other
	// resource, this does not fall back to the global strategy — see
	// EffectiveCredentialStrategy.
	Strategy string `yaml:"strategy"`
	// Account is the service account id in the generated address. Defaults to
	// "local-emulator".
	Account string `yaml:"account"`
	// TokenURI points every OAuth URL in the file at one address, so a library
	// that does try to mint a token reaches a local stub rather than Google.
	// Empty uses Google's real endpoints.
	TokenURI string `yaml:"token_uri"`
}

// PubSub groups the Pub/Sub resources to provision.
type PubSub struct {
	Topics []Topic `yaml:"topics"`
}

// Storage groups the Cloud Storage resources to provision.
type Storage struct {
	Buckets []Bucket `yaml:"buckets"`
}

// Topic defines a Pub/Sub topic and the subscriptions attached to it.
type Topic struct {
	Name     string            `yaml:"name"`
	Strategy string            `yaml:"strategy"`
	Labels   map[string]string `yaml:"labels"`
	// MessageRetention is how long the topic itself retains published messages.
	// Empty leaves retention to the individual subscriptions.
	MessageRetention string         `yaml:"message_retention"`
	Subscriptions    []Subscription `yaml:"subscriptions"`
}

// Subscription defines a Pub/Sub subscription on its enclosing topic.
type Subscription struct {
	Name     string `yaml:"name"`
	Strategy string `yaml:"strategy"`
	// AckDeadline is how long a subscriber has to acknowledge a message.
	// Google Cloud accepts 10s-600s; the emulator is more permissive.
	AckDeadline string `yaml:"ack_deadline"`
	// MessageRetention is how long unacknowledged messages stay in the backlog.
	MessageRetention string `yaml:"message_retention"`
	// RetainAckedMessages is a pointer so an omitted field leaves the
	// subscription's current setting untouched rather than reading as false.
	RetainAckedMessages *bool `yaml:"retain_acked_messages"`
	// EnableMessageOrdering is immutable after creation, in Google Cloud and in
	// the emulator alike. Drift is reported as a warning, never applied.
	EnableMessageOrdering *bool `yaml:"enable_message_ordering"`
	// EnableExactlyOnceDelivery is honoured by the emulator on create and update.
	EnableExactlyOnceDelivery *bool `yaml:"enable_exactly_once_delivery"`
	// Filter is immutable after creation, in Google Cloud and in the emulator
	// alike. Drift is reported as a warning, never applied.
	Filter string `yaml:"filter"`
	// ExpirationTTL is how long the subscription may sit idle before Pub/Sub
	// deletes it. The literal "never" disables expiry; empty leaves the default.
	ExpirationTTL  string            `yaml:"expiration_ttl"`
	Labels         map[string]string `yaml:"labels"`
	PushEndpoint   string            `yaml:"push_endpoint"`
	PushAttributes map[string]string `yaml:"push_attributes"`
	// PushOIDCToken makes Pub/Sub authenticate to the push endpoint with an OIDC
	// token. It is the only authentication method Pub/Sub models.
	PushOIDCToken *PushOIDCToken `yaml:"push_oidc_token"`
	// PushWrapper selects the delivered payload shape: PushWrapperPubSub, the
	// Pub/Sub default that wraps the message in an envelope, or PushWrapperNone,
	// which posts the raw payload. Empty leaves the current wrapper alone.
	PushWrapper string `yaml:"push_wrapper"`
	// PushWriteMetadata writes the message metadata into X-Goog-Pubsub-* headers.
	// Only meaningful with push_wrapper: none, which is the only wrapper that
	// carries the setting.
	PushWriteMetadata bool        `yaml:"push_write_metadata"`
	DeadLetter        *DeadLetter `yaml:"dead_letter"`
	Retry             *Retry      `yaml:"retry"`
}

// PushOIDCToken configures the OIDC token Pub/Sub mints when calling a push
// endpoint.
type PushOIDCToken struct {
	ServiceAccountEmail string `yaml:"service_account_email"`
	// Audience defaults to the push endpoint URL when empty.
	Audience string `yaml:"audience"`
}

// Push wrapper values. Pub/Sub models these as a oneof, so they are mutually
// exclusive and an empty string means "leave whatever is there alone".
const (
	PushWrapperPubSub = "pubsub"
	PushWrapperNone   = "none"
)

// validPushWrappers is the allowlist of push wrapper values.
var validPushWrappers = map[string]bool{
	"":                true,
	PushWrapperPubSub: true,
	PushWrapperNone:   true,
}

// DeadLetter configures where a subscription forwards messages it fails to
// deliver, and after how many attempts.
type DeadLetter struct {
	// Topic is a topic name in the same project, not a fully qualified path.
	Topic               string `yaml:"topic"`
	MaxDeliveryAttempts int    `yaml:"max_delivery_attempts"`
}

// Retry configures the backoff Pub/Sub applies between redelivery attempts.
type Retry struct {
	MinimumBackoff string `yaml:"minimum_backoff"`
	MaximumBackoff string `yaml:"maximum_backoff"`
}

// Bucket defines a Cloud Storage bucket to provision.
type Bucket struct {
	Name     string `yaml:"name"`
	Strategy string `yaml:"strategy"`
	// Versioning is a pointer so that an omitted field leaves the bucket's
	// current setting untouched, rather than reading as an explicit "false"
	// and suspending versioning someone enabled out of band.
	Versioning *bool `yaml:"versioning"`
	// Location is set at creation and never reconciled — Cloud Storage does not
	// allow moving a bucket, and fake-gcs-server ignores the field entirely.
	Location string `yaml:"location"`
	// StorageClass is set at creation and never reconciled, for the same reason.
	StorageClass string `yaml:"storage_class"`
}

// ExpirationNever is the ExpirationTTL value that disables subscription expiry.
const ExpirationNever = "never"

// containsNullByte returns true if s contains a null byte (\x00).
func containsNullByte(s string) bool {
	return strings.ContainsRune(s, '\x00')
}

var envVarPattern = regexp.MustCompile(`\$\{([^}]+)}`)

// expandEnvVars replaces ${VAR} references with their environment variable values.
func expandEnvVars(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		varName := envVarPattern.FindStringSubmatch(match)[1]
		if val, ok := os.LookupEnv(varName); ok {
			return val
		}
		return match
	})
}

// expandMap expands env vars in both the keys and values of a string map.
func expandMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[expandEnvVars(k)] = expandEnvVars(v)
	}
	return out
}

// expandConfig walks the config and expands env vars in string fields.
//
// Adding a new string field to a config struct requires adding it here too, or
// ${VAR} references in that field will never expand.
func expandConfig(cfg *Config) {
	cfg.Strategy = expandEnvVars(cfg.Strategy)
	cfg.ProjectID = expandEnvVars(cfg.ProjectID)

	for i := range cfg.PubSub.Topics {
		t := &cfg.PubSub.Topics[i]
		t.Name = expandEnvVars(t.Name)
		t.Strategy = expandEnvVars(t.Strategy)
		t.MessageRetention = expandEnvVars(t.MessageRetention)
		t.Labels = expandMap(t.Labels)

		for j := range t.Subscriptions {
			s := &t.Subscriptions[j]
			s.Name = expandEnvVars(s.Name)
			s.Strategy = expandEnvVars(s.Strategy)
			s.AckDeadline = expandEnvVars(s.AckDeadline)
			s.MessageRetention = expandEnvVars(s.MessageRetention)
			s.Filter = expandEnvVars(s.Filter)
			s.ExpirationTTL = expandEnvVars(s.ExpirationTTL)
			s.PushEndpoint = expandEnvVars(s.PushEndpoint)
			s.PushWrapper = expandEnvVars(s.PushWrapper)
			s.Labels = expandMap(s.Labels)
			s.PushAttributes = expandMap(s.PushAttributes)

			if s.PushOIDCToken != nil {
				s.PushOIDCToken.ServiceAccountEmail = expandEnvVars(s.PushOIDCToken.ServiceAccountEmail)
				s.PushOIDCToken.Audience = expandEnvVars(s.PushOIDCToken.Audience)
			}
			if s.DeadLetter != nil {
				s.DeadLetter.Topic = expandEnvVars(s.DeadLetter.Topic)
			}
			if s.Retry != nil {
				s.Retry.MinimumBackoff = expandEnvVars(s.Retry.MinimumBackoff)
				s.Retry.MaximumBackoff = expandEnvVars(s.Retry.MaximumBackoff)
			}
		}
	}

	for i := range cfg.Credentials {
		c := &cfg.Credentials[i]
		c.Path = expandEnvVars(c.Path)
		c.Strategy = expandEnvVars(c.Strategy)
		c.Account = expandEnvVars(c.Account)
		c.TokenURI = expandEnvVars(c.TokenURI)
	}

	for i := range cfg.Storage.Buckets {
		b := &cfg.Storage.Buckets[i]
		b.Name = expandEnvVars(b.Name)
		b.Strategy = expandEnvVars(b.Strategy)
		b.Location = expandEnvVars(b.Location)
		b.StorageClass = expandEnvVars(b.StorageClass)
	}
}

// Load reads and parses a YAML config file, expanding env vars and validating.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config YAML: %w", err)
	}

	expandConfig(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

// ParseDuration parses a duration string supporting Go durations and "Nd" day notation.
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}

	// Day notation like "7d" or "31d", which Go's parser does not accept.
	if strings.HasSuffix(s, "d") {
		prefix := strings.TrimSuffix(s, "d")
		days, err := strconv.Atoi(prefix)
		if err == nil {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}

	return time.ParseDuration(s)
}

// resourceNamePattern matches the Pub/Sub naming rules for topics and
// subscriptions: 3-255 characters, starting with a letter, made up of letters,
// digits and - _ . ~ + %.
var resourceNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9\-_.~+%]{2,254}$`)

// bucketNamePattern validates Cloud Storage bucket names: 3-63 characters,
// lowercase alphanumeric plus hyphens, starting and ending alphanumeric.
// Cloud Storage also permits dots in domain-named buckets, which this rejects.
var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// projectIDPattern validates a Google Cloud project ID: 6-30 characters,
// starting with a lowercase letter, made up of lowercase letters, digits and
// hyphens, and not ending in a hyphen. Emulator projects are conventionally
// looser, so this is deliberately the only place project IDs are checked.
var projectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

func validate(cfg *Config) error {
	if err := validateStrategy("strategy", cfg.Strategy); err != nil {
		return err
	}

	if cfg.ProjectID != "" && !projectIDPattern.MatchString(cfg.ProjectID) {
		return fmt.Errorf("project_id: invalid project id %q (must be 6-30 chars, lowercase letters, digits and hyphens, starting with a letter)", cfg.ProjectID)
	}

	if err := validateTopics(cfg.PubSub.Topics); err != nil {
		return err
	}
	if err := validateBuckets(cfg.Storage.Buckets); err != nil {
		return err
	}
	return validateCredentials(cfg.Credentials)
}

// validateStrategy returns an error if the strategy value is invalid.
func validateStrategy(path, value string) error {
	if !validStrategies[value] {
		return fmt.Errorf("%s: invalid strategy %q (must be \"create\" or \"update\")", path, value)
	}
	return nil
}

// validateDuration checks an optional duration field, rejecting null bytes,
// unparseable values and negative durations.
func validateDuration(path, value string) error {
	if value == "" {
		return nil
	}
	if containsNullByte(value) {
		return fmt.Errorf("%s: contains null byte", path)
	}
	d, err := ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s: invalid duration %q: %w", path, value, err)
	}
	if d < 0 {
		return fmt.Errorf("%s: negative duration %q is not allowed", path, value)
	}
	return nil
}

// Pub/Sub API limits.
//
// Validating these locally is what keeps the dual-target promise honest: the
// emulator accepts values Google Cloud rejects, so a config exercised only
// against the emulator would otherwise pass here and fail on the first real run
// with an opaque InvalidArgument.
const (
	minAckDeadline      = 10 * time.Second
	maxAckDeadline      = 600 * time.Second
	minMessageRetention = 10 * time.Minute
	maxMessageRetention = 31 * 24 * time.Hour
	maxRetryBackoff     = 600 * time.Second
	minExpirationTTL    = 24 * time.Hour
	maxExpirationTTL    = 365 * 24 * time.Hour
)

// validateDurationRange checks an optional duration lies within [lo, hi] after
// the shared syntax checks. An empty value is unconstrained — omitted means
// "leave it alone", not "use zero".
//
// bounds is the human form used in the error ("10s-600s"); a formatted
// time.Duration reads as "744h0m0s", which helps nobody.
func validateDurationRange(path, value string, lo, hi time.Duration, bounds string) error {
	if err := validateDuration(path, value); err != nil {
		return err
	}
	if value == "" {
		return nil
	}

	d, err := ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s: invalid duration %q: %w", path, value, err)
	}
	if d < lo || d > hi {
		return fmt.Errorf("%s: %q is outside the range Google Cloud accepts (%s)", path, value, bounds)
	}
	return nil
}

// validateLabels rejects null bytes in label keys and values.
func validateLabels(path string, labels map[string]string) error {
	for k, v := range labels {
		if containsNullByte(k) || containsNullByte(v) {
			return fmt.Errorf("%s: label %q contains null byte", path, k)
		}
	}
	return nil
}

func validateTopics(topics []Topic) error {
	names := make(map[string]bool)
	// Subscription names are unique per project, not per topic, so they are
	// tracked across the whole list rather than within each topic.
	subNames := make(map[string]bool)

	for i, t := range topics {
		prefix := fmt.Sprintf("pubsub.topics[%d]", i)

		if err := validateStrategy(prefix+".strategy", t.Strategy); err != nil {
			return err
		}

		if t.Name == "" {
			return fmt.Errorf("%s.name: is required", prefix)
		}
		if containsNullByte(t.Name) {
			return fmt.Errorf("%s.name: contains null byte", prefix)
		}
		if !resourceNamePattern.MatchString(t.Name) {
			return fmt.Errorf("%s.name: invalid topic name %q (must be 3-255 chars, start with a letter, and contain only letters, digits and -_.~+%%)", prefix, t.Name)
		}
		if strings.HasPrefix(t.Name, "goog") {
			return fmt.Errorf("%s.name: topic name %q must not start with \"goog\"", prefix, t.Name)
		}
		if names[t.Name] {
			return fmt.Errorf("%s.name: duplicate topic name %q", prefix, t.Name)
		}
		names[t.Name] = true

		if err := validateLabels(prefix+".labels", t.Labels); err != nil {
			return err
		}
		if err := validateDurationRange(prefix+".message_retention", t.MessageRetention,
			minMessageRetention, maxMessageRetention, "10m-31d"); err != nil {
			return err
		}

		if err := validateSubscriptions(prefix, t.Subscriptions, subNames); err != nil {
			return err
		}
	}

	// Dead letter topics must be provisioned too, otherwise the subscription
	// referencing them fails at create time with a bare NotFound.
	return validateDeadLetterTargets(topics, names)
}

func validateSubscriptions(topicPrefix string, subs []Subscription, seen map[string]bool) error {
	for i, s := range subs {
		prefix := fmt.Sprintf("%s.subscriptions[%d]", topicPrefix, i)

		if err := validateStrategy(prefix+".strategy", s.Strategy); err != nil {
			return err
		}

		if s.Name == "" {
			return fmt.Errorf("%s.name: is required", prefix)
		}
		if containsNullByte(s.Name) {
			return fmt.Errorf("%s.name: contains null byte", prefix)
		}
		if !resourceNamePattern.MatchString(s.Name) {
			return fmt.Errorf("%s.name: invalid subscription name %q (must be 3-255 chars, start with a letter, and contain only letters, digits and -_.~+%%)", prefix, s.Name)
		}
		if strings.HasPrefix(s.Name, "goog") {
			return fmt.Errorf("%s.name: subscription name %q must not start with \"goog\"", prefix, s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("%s.name: duplicate subscription name %q (subscription names are unique per project)", prefix, s.Name)
		}
		seen[s.Name] = true

		if err := validateLabels(prefix+".labels", s.Labels); err != nil {
			return err
		}
		if err := validateLabels(prefix+".push_attributes", s.PushAttributes); err != nil {
			return err
		}

		if err := validateAckDeadline(prefix, s.AckDeadline); err != nil {
			return err
		}
		if err := validateDurationRange(prefix+".message_retention", s.MessageRetention,
			minMessageRetention, maxMessageRetention, "10m-31d"); err != nil {
			return err
		}
		if s.ExpirationTTL != "" && s.ExpirationTTL != ExpirationNever {
			if err := validateDurationRange(prefix+".expiration_ttl", s.ExpirationTTL,
				minExpirationTTL, maxExpirationTTL, "1d-365d, or \"never\""); err != nil {
				return err
			}
		}

		if containsNullByte(s.Filter) {
			return fmt.Errorf("%s.filter: contains null byte", prefix)
		}
		if containsNullByte(s.PushEndpoint) {
			return fmt.Errorf("%s.push_endpoint: contains null byte", prefix)
		}
		if s.PushEndpoint == "" && len(s.PushAttributes) > 0 {
			return fmt.Errorf("%s.push_attributes: requires push_endpoint to be set", prefix)
		}
		if err := validatePush(prefix, s); err != nil {
			return err
		}

		if err := validateDeadLetter(prefix, s.DeadLetter); err != nil {
			return err
		}
		if err := validateRetry(prefix, s.Retry); err != nil {
			return err
		}
	}

	return nil
}

// validatePush checks the push delivery settings that sit alongside
// push_endpoint. Each of them is part of the push_config message, so none of
// them means anything without an endpoint.
func validatePush(subPrefix string, s Subscription) error {
	if containsNullByte(s.PushWrapper) {
		return fmt.Errorf("%s.push_wrapper: contains null byte", subPrefix)
	}
	if !validPushWrappers[s.PushWrapper] {
		return fmt.Errorf("%s.push_wrapper: invalid push wrapper %q (must be %q or %q)",
			subPrefix, s.PushWrapper, PushWrapperPubSub, PushWrapperNone)
	}
	if s.PushEndpoint == "" && s.PushWrapper != "" {
		return fmt.Errorf("%s.push_wrapper: requires push_endpoint to be set", subPrefix)
	}
	// write_metadata only exists inside the no-wrapper message, so asking for it
	// under the default wrapper is a config mistake rather than a no-op.
	if s.PushWriteMetadata && s.PushWrapper != PushWrapperNone {
		return fmt.Errorf("%s.push_write_metadata: requires push_wrapper: %q", subPrefix, PushWrapperNone)
	}

	if s.PushOIDCToken == nil {
		return nil
	}
	prefix := subPrefix + ".push_oidc_token"

	if s.PushEndpoint == "" {
		return fmt.Errorf("%s: requires push_endpoint to be set", prefix)
	}
	if s.PushOIDCToken.ServiceAccountEmail == "" {
		return fmt.Errorf("%s.service_account_email: is required", prefix)
	}
	if containsNullByte(s.PushOIDCToken.ServiceAccountEmail) {
		return fmt.Errorf("%s.service_account_email: contains null byte", prefix)
	}
	if containsNullByte(s.PushOIDCToken.Audience) {
		return fmt.Errorf("%s.audience: contains null byte", prefix)
	}
	return nil
}

// validateAckDeadline checks the ack deadline against the Pub/Sub range and
// rejects sub-second precision.
//
// Pub/Sub stores this as ack_deadline_seconds, so "10s500ms" is truncated to 10
// on the wire, read back as "10s", and reported as drift on every subsequent
// run — an update that can never converge. Rejecting it up front is the same
// call the provisioner makes for bucket labels and lifecycle rules.
func validateAckDeadline(subPrefix, value string) error {
	path := subPrefix + ".ack_deadline"
	if err := validateDurationRange(path, value, minAckDeadline, maxAckDeadline, "10s-600s"); err != nil {
		return err
	}
	if value == "" {
		return nil
	}

	d, err := ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s: invalid duration %q: %w", path, value, err)
	}
	if d%time.Second != 0 {
		return fmt.Errorf(
			"%s: %q must be a whole number of seconds (Pub/Sub stores it as ack_deadline_seconds, "+
				"so a fractional value is truncated and then reported as drift on every run)", path, value)
	}
	return nil
}

func validateDeadLetter(subPrefix string, dl *DeadLetter) error {
	if dl == nil {
		return nil
	}
	prefix := subPrefix + ".dead_letter"

	if dl.Topic == "" {
		return fmt.Errorf("%s.topic: is required", prefix)
	}
	if containsNullByte(dl.Topic) {
		return fmt.Errorf("%s.topic: contains null byte", prefix)
	}
	// Google Cloud accepts 5-100; anything outside is rejected at create time.
	if dl.MaxDeliveryAttempts != 0 && (dl.MaxDeliveryAttempts < 5 || dl.MaxDeliveryAttempts > 100) {
		return fmt.Errorf("%s.max_delivery_attempts: must be between 5 and 100, got %d", prefix, dl.MaxDeliveryAttempts)
	}
	return nil
}

func validateRetry(subPrefix string, r *Retry) error {
	if r == nil {
		return nil
	}
	prefix := subPrefix + ".retry"

	if err := validateDurationRange(prefix+".minimum_backoff", r.MinimumBackoff,
		0, maxRetryBackoff, "0s-600s"); err != nil {
		return err
	}
	if err := validateDurationRange(prefix+".maximum_backoff", r.MaximumBackoff,
		0, maxRetryBackoff, "0s-600s"); err != nil {
		return err
	}

	minBackoff, err := ParseDuration(r.MinimumBackoff)
	if err != nil {
		return fmt.Errorf("%s.minimum_backoff: invalid duration %q: %w", prefix, r.MinimumBackoff, err)
	}
	maxBackoff, err := ParseDuration(r.MaximumBackoff)
	if err != nil {
		return fmt.Errorf("%s.maximum_backoff: invalid duration %q: %w", prefix, r.MaximumBackoff, err)
	}
	if minBackoff > 0 && maxBackoff > 0 && minBackoff > maxBackoff {
		return fmt.Errorf("%s: minimum_backoff %q must not exceed maximum_backoff %q", prefix, r.MinimumBackoff, r.MaximumBackoff)
	}
	return nil
}

// validateDeadLetterTargets checks that every dead letter topic is itself
// declared in the config. The provisioner is additive and never creates a topic
// it was not asked for, so an undeclared target would only surface as a
// NotFound from the API at create time.
func validateDeadLetterTargets(topics []Topic, declared map[string]bool) error {
	for i, t := range topics {
		for j, s := range t.Subscriptions {
			if s.DeadLetter == nil || declared[s.DeadLetter.Topic] {
				continue
			}
			return fmt.Errorf(
				"pubsub.topics[%d].subscriptions[%d].dead_letter.topic: %q is not declared in pubsub.topics (the provisioner only creates topics listed in the config)",
				i, j, s.DeadLetter.Topic)
		}
	}
	return nil
}

// validStorageClasses is the allowlist of Cloud Storage bucket storage classes.
var validStorageClasses = map[string]bool{
	"":                             true,
	"STANDARD":                     true,
	"NEARLINE":                     true,
	"COLDLINE":                     true,
	"ARCHIVE":                      true,
	"MULTI_REGIONAL":               true,
	"REGIONAL":                     true,
	"DURABLE_REDUCED_AVAILABILITY": true,
}

func validateBuckets(buckets []Bucket) error {
	names := make(map[string]bool)

	for i, b := range buckets {
		prefix := fmt.Sprintf("storage.buckets[%d]", i)

		if err := validateStrategy(prefix+".strategy", b.Strategy); err != nil {
			return err
		}

		if b.Name == "" {
			return fmt.Errorf("%s.name: is required", prefix)
		}
		if containsNullByte(b.Name) {
			return fmt.Errorf("%s.name: contains null byte", prefix)
		}
		if !bucketNamePattern.MatchString(b.Name) {
			return fmt.Errorf("%s.name: invalid bucket name %q (must be 3-63 chars, lowercase alphanumeric and hyphens)", prefix, b.Name)
		}
		if names[b.Name] {
			return fmt.Errorf("%s.name: duplicate bucket name %q", prefix, b.Name)
		}
		names[b.Name] = true

		if containsNullByte(b.Location) {
			return fmt.Errorf("%s.location: contains null byte", prefix)
		}
		if !validStorageClasses[strings.ToUpper(b.StorageClass)] {
			return fmt.Errorf("%s.storage_class: invalid storage class %q", prefix, b.StorageClass)
		}
	}

	return nil
}

// accountPattern matches a Google service account id: 6-30 characters, starting
// with a lowercase letter, made up of lowercase letters, digits and hyphens.
var accountPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// EffectiveCredentialStrategy resolves a credential's strategy.
//
// Unlike every other resource, this deliberately does not fall back to the
// global strategy. A key file's contents are a freshly generated keypair, so
// "update" cannot converge — it would hand running applications a different
// private key on every run. Regenerating therefore has to be asked for on the
// credential itself.
func EffectiveCredentialStrategy(strategy string) string {
	if strategy == "" {
		return "create"
	}
	return strategy
}

func validateCredentials(creds []Credential) error {
	paths := make(map[string]bool)

	for i, c := range creds {
		prefix := fmt.Sprintf("credentials[%d]", i)

		if err := validateStrategy(prefix+".strategy", c.Strategy); err != nil {
			return err
		}

		if c.Path == "" {
			return fmt.Errorf("%s.path: is required", prefix)
		}
		if containsNullByte(c.Path) {
			return fmt.Errorf("%s.path: contains null byte", prefix)
		}
		if paths[c.Path] {
			return fmt.Errorf("%s.path: duplicate path %q", prefix, c.Path)
		}
		paths[c.Path] = true

		if c.Account != "" && !accountPattern.MatchString(c.Account) {
			return fmt.Errorf(
				"%s.account: invalid service account id %q (must be 6-30 chars, lowercase letters, digits and hyphens, starting with a letter)",
				prefix, c.Account)
		}

		if c.TokenURI != "" {
			if containsNullByte(c.TokenURI) {
				return fmt.Errorf("%s.token_uri: contains null byte", prefix)
			}
			parsed, err := url.Parse(c.TokenURI)
			if err != nil {
				return fmt.Errorf("%s.token_uri: invalid url %q: %w", prefix, c.TokenURI, err)
			}
			if parsed.Scheme == "" || parsed.Host == "" {
				return fmt.Errorf("%s.token_uri: %q must be an absolute url with a scheme and host", prefix, c.TokenURI)
			}
		}
	}

	return nil
}
