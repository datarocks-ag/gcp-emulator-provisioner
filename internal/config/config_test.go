package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes contents to a temp file and returns its path.
func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestLoadParsesFullConfig(t *testing.T) {
	t.Setenv("DLQ_TOPIC", "orders-dlq")
	t.Setenv("BUCKET_SUFFIX", "prod")

	path := writeConfig(t, `
project_id: local-dev
strategy: update
pubsub:
  topics:
    - name: orders
      labels:
        domain: commerce
      message_retention: 24h
      subscriptions:
        - name: order-processor
          ack_deadline: 30s
          message_retention: 7d
          enable_exactly_once_delivery: true
          retain_acked_messages: false
          filter: 'attributes.type = "order"'
          expiration_ttl: never
          dead_letter:
            topic: ${DLQ_TOPIC}
            max_delivery_attempts: 5
          retry:
            minimum_backoff: 10s
            maximum_backoff: 600s
    - name: orders-dlq
storage:
  buckets:
    - name: documents-${BUCKET_SUFFIX}
      versioning: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ProjectID != "local-dev" {
		t.Errorf("project_id = %q, want local-dev", cfg.ProjectID)
	}
	if len(cfg.PubSub.Topics) != 2 {
		t.Fatalf("got %d topics, want 2", len(cfg.PubSub.Topics))
	}

	topic := cfg.PubSub.Topics[0]
	if topic.Labels["domain"] != "commerce" {
		t.Errorf("labels = %v, want domain=commerce", topic.Labels)
	}

	sub := topic.Subscriptions[0]
	if sub.DeadLetter == nil || sub.DeadLetter.Topic != "orders-dlq" {
		t.Errorf("dead_letter.topic not expanded from ${DLQ_TOPIC}: %+v", sub.DeadLetter)
	}
	if sub.ExpirationTTL != ExpirationNever {
		t.Errorf("expiration_ttl = %q, want %q", sub.ExpirationTTL, ExpirationNever)
	}
	if sub.RetainAckedMessages == nil || *sub.RetainAckedMessages {
		t.Errorf("retain_acked_messages should be an explicit false, got %v", sub.RetainAckedMessages)
	}
	if sub.EnableMessageOrdering != nil {
		t.Errorf("enable_message_ordering should stay nil when omitted, got %v", *sub.EnableMessageOrdering)
	}

	if cfg.Storage.Buckets[0].Name != "documents-prod" {
		t.Errorf("bucket name = %q, want documents-prod", cfg.Storage.Buckets[0].Name)
	}
}

func TestLoadParsesPushConfig(t *testing.T) {
	t.Setenv("PUSH_SA_EMAIL", "pusher@local-dev.iam.gserviceaccount.com")

	path := writeConfig(t, `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: order-webhook
          ack_deadline: 10s
          push_endpoint: https://consumer.example.com/events
          push_attributes:
            x-goog-version: v1
          push_oidc_token:
            service_account_email: ${PUSH_SA_EMAIL}
            audience: https://consumer.example.com
          push_wrapper: none
          push_write_metadata: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	sub := cfg.PubSub.Topics[0].Subscriptions[0]
	if sub.PushOIDCToken == nil {
		t.Fatal("push_oidc_token not parsed")
	}
	// Guards the expandConfig landmine: a service account email is exactly the
	// kind of field people template, and a missing line here fails silently.
	if sub.PushOIDCToken.ServiceAccountEmail != "pusher@local-dev.iam.gserviceaccount.com" {
		t.Errorf("service_account_email not expanded from ${PUSH_SA_EMAIL}: %q",
			sub.PushOIDCToken.ServiceAccountEmail)
	}
	if sub.PushWrapper != PushWrapperNone || !sub.PushWriteMetadata {
		t.Errorf("wrapper = %q/%v, want %q/true", sub.PushWrapper, sub.PushWriteMetadata, PushWrapperNone)
	}
}

func TestLoadLeavesUnsetEnvVarReferenceIntact(t *testing.T) {
	path := writeConfig(t, `
project_id: local-dev
pubsub:
  topics:
    - name: orders
      labels:
        secret: ${DEFINITELY_NOT_SET_XYZ}
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.PubSub.Topics[0].Labels["secret"]; got != "${DEFINITELY_NOT_SET_XYZ}" {
		t.Errorf("unset var was rewritten to %q, want the reference left as is", got)
	}
}

func TestLoadRejectsInvalidConfigs(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "missing topic name",
			yaml:    "pubsub:\n  topics:\n    - labels: {a: b}\n",
			wantErr: "pubsub.topics[0].name: is required",
		},
		{
			name:    "topic name starting with goog",
			yaml:    "pubsub:\n  topics:\n    - name: googtopic\n",
			wantErr: `must not start with "goog"`,
		},
		{
			name:    "topic name too short",
			yaml:    "pubsub:\n  topics:\n    - name: ab\n",
			wantErr: "invalid topic name",
		},
		{
			name:    "duplicate topic",
			yaml:    "pubsub:\n  topics:\n    - name: orders\n    - name: orders\n",
			wantErr: "duplicate topic name",
		},
		{
			name: "duplicate subscription across topics",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: shared-sub
    - name: events
      subscriptions:
        - name: shared-sub
`,
			wantErr: "duplicate subscription name",
		},
		{
			name: "undeclared dead letter topic",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          dead_letter:
            topic: nowhere
            max_delivery_attempts: 5
`,
			wantErr: `"nowhere" is not declared in pubsub.topics`,
		},
		{
			name: "dead letter attempts out of range",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          dead_letter:
            topic: orders
            max_delivery_attempts: 2
`,
			wantErr: "must be between 5 and 100",
		},
		{
			name: "retry backoff inverted",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          retry:
            minimum_backoff: 60s
            maximum_backoff: 10s
`,
			wantErr: "must not exceed maximum_backoff",
		},
		{
			name: "push attributes without endpoint",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          push_attributes: {a: b}
`,
			wantErr: "requires push_endpoint",
		},
		{
			name: "invalid ack deadline duration",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          ack_deadline: soon
`,
			wantErr: "invalid duration",
		},
		{
			name: "push oidc token without endpoint",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          push_oidc_token:
            service_account_email: pusher@local-dev.iam.gserviceaccount.com
`,
			wantErr: "push_oidc_token: requires push_endpoint",
		},
		{
			name: "push oidc token without a service account",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          push_endpoint: https://consumer.example.com
          push_oidc_token:
            audience: https://consumer.example.com
`,
			wantErr: "service_account_email: is required",
		},
		{
			name: "invalid push wrapper",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          push_endpoint: https://consumer.example.com
          push_wrapper: raw
`,
			wantErr: "invalid push wrapper",
		},
		{
			name: "push wrapper without endpoint",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          push_wrapper: none
`,
			wantErr: "push_wrapper: requires push_endpoint",
		},
		{
			name: "push write metadata under the default wrapper",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          push_endpoint: https://consumer.example.com
          push_wrapper: pubsub
          push_write_metadata: true
`,
			wantErr: `push_write_metadata: requires push_wrapper: "none"`,
		},
		{
			name: "ack deadline below the Pub/Sub minimum",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          ack_deadline: 5s
`,
			wantErr: "outside the range Google Cloud accepts (10s-600s)",
		},
		{
			name: "ack deadline above the Pub/Sub maximum",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          ack_deadline: 700s
`,
			wantErr: "outside the range Google Cloud accepts (10s-600s)",
		},
		{
			name: "ack deadline with sub-second precision",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          ack_deadline: 10s500ms
`,
			wantErr: "must be a whole number of seconds",
		},
		{
			name:    "topic message retention below the minimum",
			yaml:    "pubsub:\n  topics:\n    - name: orders\n      message_retention: 1m\n",
			wantErr: "outside the range Google Cloud accepts (10m-31d)",
		},
		{
			name: "subscription message retention above the maximum",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          message_retention: 40d
`,
			wantErr: "outside the range Google Cloud accepts (10m-31d)",
		},
		{
			name: "retry backoff above the maximum",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          retry:
            minimum_backoff: 10s
            maximum_backoff: 700s
`,
			wantErr: "outside the range Google Cloud accepts (0s-600s)",
		},
		{
			name: "expiration ttl below the minimum",
			yaml: `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: processor
          expiration_ttl: 1h
`,
			wantErr: "outside the range Google Cloud accepts",
		},
		{
			name:    "credential without a path",
			yaml:    "credentials:\n  - account: local-emulator\n",
			wantErr: "credentials[0].path: is required",
		},
		{
			name:    "duplicate credential path",
			yaml:    "credentials:\n  - path: /tmp/a.json\n  - path: /tmp/a.json\n",
			wantErr: "duplicate path",
		},
		{
			name:    "invalid service account id",
			yaml:    "credentials:\n  - path: /tmp/a.json\n    account: Not_Valid\n",
			wantErr: "invalid service account id",
		},
		{
			name:    "relative token uri",
			yaml:    "credentials:\n  - path: /tmp/a.json\n    token_uri: /token\n",
			wantErr: "must be an absolute url",
		},
		{
			name:    "invalid credential strategy",
			yaml:    "credentials:\n  - path: /tmp/a.json\n    strategy: rotate\n",
			wantErr: "invalid strategy",
		},
		{
			name:    "invalid strategy",
			yaml:    "strategy: replace\n",
			wantErr: "invalid strategy",
		},
		{
			name:    "invalid project id",
			yaml:    "project_id: X\n",
			wantErr: "invalid project id",
		},
		{
			name:    "invalid bucket name",
			yaml:    "storage:\n  buckets:\n    - name: Not_Valid\n",
			wantErr: "invalid bucket name",
		},
		{
			name:    "duplicate bucket",
			yaml:    "storage:\n  buckets:\n    - name: docs-one\n    - name: docs-one\n",
			wantErr: "duplicate bucket name",
		},
		{
			name:    "invalid storage class",
			yaml:    "storage:\n  buckets:\n    - name: docs-one\n      storage_class: GLACIER\n",
			wantErr: "invalid storage class",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.yaml))
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadAcceptsExpirationNeverAndDurations(t *testing.T) {
	path := writeConfig(t, `
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: a-sub
          expiration_ttl: never
        - name: b-sub
          expiration_ttl: 48h
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// TestLoadAcceptsBoundaryDurations pins that the range checks are inclusive.
// Off-by-one at a boundary is the likeliest way to get these wrong, and it
// would reject a config Google Cloud accepts.
func TestLoadAcceptsBoundaryDurations(t *testing.T) {
	path := writeConfig(t, `
pubsub:
  topics:
    - name: orders
      message_retention: 10m
      subscriptions:
        - name: at-lower-bound
          ack_deadline: 10s
          message_retention: 10m
          expiration_ttl: 1d
          retry:
            minimum_backoff: 0s
            maximum_backoff: 0s
        - name: at-upper-bound
          ack_deadline: 600s
          message_retention: 31d
          expiration_ttl: 365d
          retry:
            minimum_backoff: 600s
            maximum_backoff: 600s
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("boundary values must be accepted: %v", err)
	}
}

func TestLoadParsesCredentials(t *testing.T) {
	t.Setenv("KEY_DIR", "/secrets")

	path := writeConfig(t, `
project_id: local-dev
credentials:
  - path: ${KEY_DIR}/fake-sa.json
    account: orders-service
    token_uri: http://oauth2-stub:8080/token
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Credentials) != 1 {
		t.Fatalf("got %d credentials, want 1", len(cfg.Credentials))
	}

	cred := cfg.Credentials[0]
	// Guards the expandConfig landmine — a key path is exactly the sort of
	// field people template.
	if cred.Path != "/secrets/fake-sa.json" {
		t.Errorf("path not expanded from ${KEY_DIR}: %q", cred.Path)
	}
	if cred.Account != "orders-service" || cred.TokenURI != "http://oauth2-stub:8080/token" {
		t.Errorf("credential = %+v", cred)
	}
}

func TestEffectiveCredentialStrategyIgnoresTheGlobalDefault(t *testing.T) {
	// A key file cannot converge under "update", so it never inherits it.
	if got := EffectiveCredentialStrategy(""); got != "create" {
		t.Errorf("EffectiveCredentialStrategy(\"\") = %q, want create", got)
	}
	if got := EffectiveCredentialStrategy("update"); got != "update" {
		t.Errorf("an explicit update must be honoured, got %q", got)
	}
}

func TestLoadRejectsMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	if _, err := Load(writeConfig(t, "pubsub: [oops\n")); err == nil {
		t.Fatal("expected an error for malformed YAML")
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"30s", 30 * time.Second, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"31d", 31 * 24 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{"soon", 0, true},
		{"xd", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseDuration(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseDuration(%q) = %v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDuration(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseDuration(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestEffectiveStrategy(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"all empty defaults to update", []string{"", ""}, "update"},
		{"resource wins over global", []string{"create", "update"}, "create"},
		{"falls through to global", []string{"", "create"}, "create"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveStrategy(tt.in...); got != tt.want {
				t.Errorf("EffectiveStrategy(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
