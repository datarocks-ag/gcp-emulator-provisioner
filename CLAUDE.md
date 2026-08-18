# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Goal — GCP Emulator Provisioner

A Go binary (`gcp-emulator-provisioner`) that idempotently provisions Google Cloud Pub/Sub topics and
subscriptions and Cloud Storage buckets from a YAML config file. Runs as a one-shot Kubernetes
`Job` / init container or Docker Compose init service, then exits. No long-running process, no
health endpoint.

That description is about **the provisioner**, and still holds exactly. The repository also ships
a second, much smaller binary — `cmd/gcp-token-stub` — which is a long-lived helper rather than a
provisioner, and does not change the rule above. See "The token stub is a helper, not a second
provisioner" below.

The same binary targets the local emulators (`cloud-sdk:emulators`, `fake-gcs-server`) and real
Google Cloud — the client libraries switch on `PUBSUB_EMULATOR_HOST` / `STORAGE_EMULATOR_HOST`
and otherwise use Application Default Credentials. That dual targeting is the reason the project
exists; do not add anything that only works against one of them.

See [README.md](README.md) for the user-facing contract. This file covers what the README does
not — the internal structure and the constraints that are easy to break.

## Build & Test Commands

```sh
make build             # go build -o gcp-emulator-provisioner ./cmd/gcp-emulator-provisioner
make test              # go test -race ./...
make test-integration  # go test -race -tags=integration -v -timeout 30m ./...  (requires Docker)
make lint              # go tool golangci-lint run
make vet               # go vet ./...
make fmt               # go fmt ./...
make mod-tidy          # go mod tidy
make docker            # docker build -t gcp-emulator-provisioner .
make clean

go test -race ./internal/config/                                  # single package
go test -race -run TestEnsureTopic ./internal/provisioner/        # single test
```

- Go 1.25+, `CGO_ENABLED=0` (pure Go, no C dependencies)
- golangci-lint is a `tool` dependency in `go.mod` — always `go tool golangci-lint run`, never a
  globally installed binary
- Integration tests use `testcontainers-go` and the `//go:build integration` tag — plain
  `go test ./...` skips them
- **CI lints without the integration tag**, so `go tool golangci-lint run` alone will not check
  `integration_test.go`. Use `--build-tags=integration` when touching it
- Coverage gate: **two of them**, and the stricter one decides. `ci.yaml` runs
  `vladopajic/go-test-coverage` twice — once with `config: ./.testcoverage.yml` (`total: 60`) and
  again in the badge step with a hardcoded `threshold-total: 75`. A run at, say, 70% passes the
  first and fails the second. Change both together or they drift apart
- The `client` package talks to a live endpoint, so its connect and CRUD paths look
  integration-only — but the gate does not count integration tests. They are covered instead by
  in-process fakes in `internal/client`: `httptest` for the Cloud Storage JSON API
  (`storage_test.go`) and a real gRPC server implementing `PublisherServer` / `SubscriberServer`
  (`pubsub_server_test.go`). Both mirror emulator behaviour deliberately — fabricated
  `US-CENTRAL1`/`STANDARD`, and a mask validated in full before anything is applied. Extend the
  fakes rather than dropping back to integration-only coverage
- The Pub/Sub emulator image is **amd64-only**. On an arm64 host each integration test spends
  ~15s booting it under emulation; the full Pub/Sub suite takes around 80s

## Architecture

```
gcp-emulator-provisioner/
├── cmd/gcp-emulator-provisioner/
│   └── main.go              # entry point, flag/env parsing, section dispatch, exit codes
├── internal/
│   ├── config/              # YAML load, ${VAR} expansion, validation
│   ├── credentials/         # fake service account key generation (stdlib crypto only)
│   ├── client/              # transport-neutral types over the GCP SDKs
│   │   ├── client.go        # shared retry/backoff
│   │   ├── pubsub.go        # topic + subscription admin, proto conversion
│   │   └── storage.go       # bucket admin
│   └── provisioner/         # idempotent reconciliation, one file per resource kind
│       ├── provisioner.go   # PubSubAdmin / StorageAdmin interfaces, section methods
│       ├── topics.go
│       ├── subscriptions.go
│       ├── buckets.go
│       └── credentials.go
├── Dockerfile
├── docker-compose.yaml
├── Makefile
├── go.mod / go.sum
└── config.example.yaml
```

### The token stub is a helper, not a second provisioner

`cmd/gcp-token-stub` serves a static OAuth2 token endpoint (`/token`) and stays up for the life of
the stack. It exists because some client libraries insist on obtaining a token before issuing any
request: `google-cloud-storage` for Java has no `STORAGE_EMULATOR_HOST` equivalent, so a JVM
application reaches fake-gcs-server through `spring.cloud.gcp.storage.host` while still holding
real `ServiceAccountCredentials`, signs a JWT, and exchanges it at the `token_uri` from its key
file. Point that at the stub and the exchange stays on the machine.

- **It does not verify the assertion, on purpose.** Nothing downstream verifies signatures either,
  so checking here would be theatre. Do not add verification.
- **It is a separate binary and a separate image** (`Dockerfile.token-stub`), which is what keeps
  the provisioner's one-shot contract intact. Do not fold the server into the provisioner.
- **`scratch` has no shell, `wget` or `curl`**, so the compose healthcheck runs the binary with
  `--ping`, which probes a running stub over HTTP and exits non-zero if it does not answer. Any
  healthcheck written as `CMD-SHELL` will fail in this image.
- It replaces what would otherwise be an nginx container serving 12 lines of static JSON, and its
  response is byte-identical to that.

### Credentials are a third section, and the only one that touches no endpoint

`ProvisionCredentials` writes local service account key files so libraries that demand
`GOOGLE_APPLICATION_CREDENTIALS` will start — Spring Cloud GCP parses the private key eagerly at
startup, so a placeholder string fails where a real keypair succeeds. `internal/credentials`
generates an RSA keypair with stdlib crypto only; Google holds no matching public key, so the
file authenticates to nothing. Verified accepted by `google.CredentialsFromJSON`.

Two rules that are easy to break:

- **It is create-only, and deliberately ignores the global strategy.** Every other resource falls
  back to `cfg.Strategy`; `EffectiveCredentialStrategy` does not, because the contents are a
  freshly generated keypair. Under a global `update` the file would be rewritten with a *different
  private key* on every run, rotating the credential underneath whatever already loaded it and
  never converging — the same hazard that keeps bucket labels out of the schema. Only an explicit
  `strategy: update` on the credential regenerates.
- **`token_uri` sets all four OAuth URLs**, not just `token_uri`. That is the point: a library
  that does try to mint a token then reaches a local stub instead of `accounts.google.com`.

This is the one feature that only makes sense against an emulator, which the project name now
reflects. It does not violate the dual-target rule — it writes a local file and calls no API, so
nothing behaves differently between endpoints. Do not extend it into anything that does.

`main.go` writes the resolved project id back into `cfg.ProjectID` before constructing the
provisioner, so a section reading it sees the same value whether it came from `GCP_PROJECT_ID` or
the config file.

### Two independent sections, not ordered phases

Unlike `seaweedfs-provisioner`'s phases, the two sections here have no ordering relationship —
Pub/Sub and Cloud Storage are separate services on separate endpoints. `--section` exists so a
deployment whose emulators live in different stacks can run one section per container, not
because one must precede the other.

`main.go` constructs only the client each section needs, and only when that section actually has
resources configured. It declares them as the `provisioner.PubSubAdmin` / `StorageAdmin`
interfaces rather than concrete pointers, so a skipped section yields a nil interface rather than
a non-nil interface wrapping a nil pointer.

### Ordering *within* the Pub/Sub section is load-bearing

`ProvisionPubSub` runs a full topic pass before any subscription pass, rather than walking each
topic with its subscriptions. A subscription referencing a topic that does not exist yet — its
own, or its dead letter target — fails with `NotFound`, and dead letter targets are routinely
declared after the subscription that uses them. `TestRunCreatesTopicsBeforeSubscriptions` pins
this; do not "simplify" it into a single loop.

`validateDeadLetterTargets` rejects a dead letter topic that is not declared anywhere in the
config. The provisioner is additive and never creates a topic it was not asked for, so without
that check the failure surfaces as an opaque `NotFound` from the API instead of a config error.

### The client layer is transport-neutral on purpose

`internal/client` exposes plain structs (`Topic`, `Subscription`, `Bucket`) and converts to and
from protobuf internally, following `seaweedfs-provisioner`'s `LifecycleRule`. This is what makes
the diff logic and the immutable-field rules unit-testable without a live endpoint or a protobuf
fixture. Keep `pubsubpb` out of `internal/provisioner`.

Short ids in, short ids out: config and the provisioner deal in `orders`, and only the client
expands to `projects/{p}/topics/orders`. **Dead letter topics need the same expansion** — Pub/Sub
rejects a bare id there.

### Immutable fields versus emulator gaps — two different mechanisms

Both end in a warning rather than a failed run, and it is easy to conflate them.

**Immutable fields** are detected *before* the call, by `immutableDrift`, and never enter the
update mask. Verified against the emulator: `filter` and `enable_message_ordering` are rejected
(`filter` is immutable on Google Cloud too), as is the subscription's `topic`. Recreating the
subscription to apply them would discard its backlog and ack state, so the provisioner reports
the difference and moves on.

**Emulator gaps** are detected *after* the call, by `client.IsUnsupportedField`, matching the
emulator's `InvalidArgument` text. Verified against `cloud-sdk:emulators`:

| Update mask path | Emulator | Google Cloud |
|---|---|---|
| `topic.labels` | `labels is not a known Topic field` | works |
| `subscription.labels` | `labels is not a known Subscription field` | works |
| `subscription.expiration_policy` | `currently unsupported in the Pub/Sub Emulator` | works |

All three work at *creation* time in the emulator — only the update path is missing. Downgrading
them keeps a config that is correct against Google Cloud runnable locally. Every other
`InvalidArgument` stays fatal, which is what keeps `enable_message_ordering`'s "not mutable"
error from being swallowed. Do not broaden `IsUnsupportedField` to match on the code alone.

The emulator update masks that **do** work: `ack_deadline_seconds`,
`message_retention_duration`, `retain_acked_messages`, `enable_exactly_once_delivery`,
`retry_policy`, `dead_letter_policy`, `push_config`, and `topic.message_retention_duration`.
`push_config` works with the `oidc_token` and `no_wrapper` oneofs populated, not just the
endpoint and attributes.

**A refused path must not take the batch down with it.** The emulator validates the whole mask
before touching the resource, so `UpdateSubscription(["ack_deadline_seconds", "labels"])` fails
in its entirety and applies nothing — measured, not assumed. `applyPaths` in
`internal/provisioner/update.go` therefore sends the batch first and, on `IsUnsupportedField`,
re-sends each path alone. One RPC in the common case, `1 + N` only when a gap is actually hit.

Do not replace this by parsing the field name out of the error text: the two known messages have
different shapes, only one names the field in a fixed position, a batch can contain two refused
paths, and both emulator images are unpinned. Do not partition by a hardcoded unsupported set
either — that bakes emulator knowledge into the reconciler and misses any gap not yet catalogued.
The emulator returns the *same* text for a single-path mask as for a batch, which is what makes
the retry safe; if that ever changes, the retry would turn a warning into a fatal error.

### fake-gcs-server is far more limited than it looks

Verified against `fsouza/fake-gcs-server:latest`:

- **Versioning only exists on the in-memory backend.** The default filesystem backend answers
  `POST`/`PATCH` carrying `versioning` with a 500, `not implemented: fs storage type does not
  support versioning yet`. Compose and the integration tests both pass `-backend memory`.
- **That 500 carries reason `internalError`, which the Cloud Storage client treats as
  retryable** — it retried for over five minutes in testing before anyone noticed. Every bucket
  call is therefore wrapped in a 30s `operationTimeout`. Do not remove it.
- **Labels, lifecycle rules and CORS are accepted and silently discarded**, on both backends: the
  request returns 200 and the bucket never reports them back. Managing them would mean every run
  sees drift and rewrites forever without converging, which is why the config schema deliberately
  has no `labels`, `lifecycle` or `cors` for buckets. Do not add them.
- **`location` and `storage_class` are ignored**, always reported as `US-CENTRAL1` / `STANDARD`.
  They are safe to support because they are create-time-only and never reconciled. They *are*
  compared, by `createOnlyDrift` in `buckets.go`, but only when
  `Options.CompareCreateOnlyBucketAttrs` is set — `main.go` turns it on exactly when
  `STORAGE_EMULATOR_HOST` is empty. Comparing against the fabricated values would warn on every
  local run about a difference that is not real, and the sentinels cannot simply be ignored
  because real buckets genuinely are US-CENTRAL1/STANDARD.

### Tri-state fields

`config.Bucket.Versioning`, `config.Subscription.RetainAckedMessages`, `EnableMessageOrdering`
and `EnableExactlyOnceDelivery` are all `*bool`. An omitted field must leave the resource alone;
a plain `bool` would read as `false` and turn off something enabled out of band.

**The three subscription booleans stay `*bool` all the way through `client.Subscription`.** They
were once dereferenced at the client boundary, which silently threw the distinction away: an
omitted `retain_acked_messages` turned off a setting enabled out of band, and an omitted
`enable_message_ordering` warned on every run. `fromProtoSubscription` always populates them, so
a live value is never nil; `toProtoSubscription` uses `client.DerefBool` to send the proto
default; and `diffSubscription` / `immutableDrift` compare through `boolDrift`, which treats a
nil desired as "not drift". `immutableDrift` guards `filter` the same way, with `!= ""`.

`ExpirationTTL` is a third state again: empty leaves the policy alone, `never` means an
`ExpirationPolicy` with **no** TTL (that is how Pub/Sub spells "does not expire"), and a duration
means a TTL. `client.Subscription` carries `ExpirationSet` alongside `ExpirationTTL` to keep the
three apart.

### Diffing is subset-based, never exact

`diffTopic` / `diffSubscription` only compare fields the config actually sets, and `labelsSubset`
treats extra labels on a live resource as *not* drift. `mergeLabels` then carries them across
before an update, because the mask replaces the whole label map — writing only the configured
labels would delete every label the provisioner does not manage. Same hazard as
`seaweedfs-provisioner`'s lifecycle merge.

`push_config` has the same shape one level down. `pushConfigEqual` compares only what the config
declares, and the push attributes are merged in `ensureSubscription` alongside the labels —
the mask replaces the whole `push_config` message, so an update triggered by a drifted endpoint
would otherwise delete every attribute set out of band. The push wrapper needs one extra guard:
`effectiveWrapper` resolves an unset **live** wrapper to `WrapperPubSub` (the Pub/Sub default)
before comparing, never the desired one, or `push_wrapper: pubsub` would drift forever against an
endpoint that does not echo a wrapper back.

### Range validation belongs in the config layer

The emulator accepts durations Google Cloud rejects, so a config exercised only locally would
otherwise pass validation and fail on the first real run with an opaque `InvalidArgument`.
`validateDurationRange` enforces the documented API limits at load time — see the table in
`README.md`. This is the same dual-target reasoning as everything else here, applied before any
call is made.

`ack_deadline` additionally has to be a whole number of seconds. Pub/Sub stores it as
`ack_deadline_seconds`, so `10s500ms` truncates on the wire, reads back as `10s`, and is then
reported as drift on every run — a non-converging update, the same failure this repo refuses to
ship for bucket labels.

### Dry-run

`Options.DryRun` follows the sibling provisioners: read-only calls still execute so the preview
reflects live state, only mutations are skipped. The exception is a resource that does not exist
yet, whose settings cannot be read, so the create intent is logged instead. When adding a
mutation, guard it with `p.opts.DryRun` and log a "Would ..." line.

### Config expansion is field-by-field

`config.expandEnvVars` uses a custom regexp, **not** `os.ExpandEnv`, and `expandConfig` walks
every string field explicitly — including map keys and values via `expandMap`. **Adding a new
string field to a config struct requires adding it to `expandConfig` too**, or values in that
field will never expand.

### Logging

- `log/slog` with `slog.NewJSONHandler(os.Stdout, ...)`, key-value pairs — never `fmt.Print`
- `LOG_LEVEL` env var controls level (debug/info/warn/error, default: info)
- Log every action taken and every no-op skip

### Retry / Backoff (ecosystem standard)

```go
maxRetries   = 15
initialDelay = 1 * time.Second
maxDelay     = 30 * time.Second
totalTimeout = 5 * time.Minute
```

Exponential backoff in `retryUntilReady`, shared by both clients. Note the failure mode: an
authorization error looks identical to an outage for five minutes.

### Exit Codes

- `0` — provisioning completed (k8s Job compatible)
- `1` — invalid section, config load, connection, or provisioning failure

## Key Conventions

- **No shared libraries** across provisioners — each project is standalone
- `gopkg.in/yaml.v3` for config parsing
- Dockerfile: multi-stage `golang:1.25-alpine` builder → `scratch` final image, CA certs copied in
- `-ldflags="-s -w -X main.version=${VERSION}"` for minimal binary + version injection
- Errors always wrapped: `fmt.Errorf("<lowercase gerund phrase>: %w", err)`
- Validation errors are path-prefixed — `pubsub.topics[0].subscriptions[1].dead_letter.topic: ...`
- Additive only: never delete topics, subscriptions or buckets absent from the config
- `CreateTopic` / `CreateSubscription` / `CreateBucket` swallow `AlreadyExists` / 409, so
  concurrent runs do not fight each other
- Strategy applies to topics, subscriptions and buckets alike. Unlike `nats-provisioner`, where
  consumers are always reconciled, subscriptions take their own `strategy` field falling back to
  the global one — they are independent resources, not settings of their topic
- Keep `README.md` and `CHANGELOG.md` current — every provisioner in this ecosystem ships both,
  and the changelog follows Keep a Changelog + SemVer with an `## [Unreleased]` section at the top

## CI / Release

- `.github/workflows/ci.yaml` — runs on `feature/**`, `bugfix/**`, `hotfix/**`, `release/**`,
  `dependabot/**`: lint, unit tests + coverage badge, integration tests, Trivy scan, build
- `.github/workflows/release.yaml` — runs on `develop` and `v*` tags. The `docker` job is a matrix
  over the two images, publishing multi-arch to `ghcr.io/datarocks-ag/gcp-emulator-provisioner`
  and `ghcr.io/datarocks-ag/gcp-token-stub`. Adding a binary means adding a matrix entry *and* a
  GoReleaser build id — the image name comes from `matrix.image`, not `IMAGE_NAME`, which only
  ever names the repository. The buildx cache is scoped per image, or the legs evict each other
- GoReleaser runs on tags only, and emits one archive per binary so a stack that needs only the
  stub does not download the provisioner as well
- There is no plain-push CI on `develop`/`main` — work lands on prefixed branches via PR
- Both emulator images are unpinned in tests and compose, so upstream changes can break CI
  without any local change

## Project-Specific Notes

- **Do not add IAM.** Neither emulator enforces it, so a config exercising IAM locally would
  diverge silently from production. That layer belongs in Terraform, and the scope boundary is
  the same one every provisioner in this repository draws. `push_oidc_token` is not an exception:
  it is a field of the subscription, and no IAM API is called.
- **The config surface is deliberately a subset of the API.** A field-by-field sweep against
  `pubsub/v2 v2.6.1` and `storage v1.64.0` settled what stays out, and why. Do not add these back
  without a reason that survives the dual-target rule:

  | Excluded | Why |
  |---|---|
  | Pub/Sub `Schema`, `topic.schema_settings` | The emulator serves no `SchemaService` at all. Also awkward: there is no `UpdateSchema`, and `CommitSchema` appends a revision even for an identical definition, so idempotency is not free |
  | BigQuery / Cloud Storage / Bigtable export subscriptions | Not implemented by the emulator |
  | `message_transforms`, `ingestion_data_source_settings`, `kms_key_name`, `message_storage_policy`, `tags` | Google Cloud only |
  | GCS retention policy, autoclass, soft delete, RPO, encryption, HNS, custom placement, logging, website, requester pays | Google Cloud only |
  | GCS `uniform_bucket_level_access`, `public_access_prevention`, ACLs | The IAM boundary above |
  | GCS `labels`, `lifecycle`, `cors` | Accepted and discarded by fake-gcs-server — drift never converges |
  | `subscription.detached` | Driven by the `DetachSubscription` RPC, not a declarative setting |
- The Compose healthchecks are verified against the images: `cloud-sdk:emulators` ships `curl`
  and the emulator answers a plain HTTP `GET /` with 200 despite speaking gRPC;
  `fake-gcs-server` ships `wget` but **not** `curl`.
- `pubsub` in Compose needs a long `start_period` because of the amd64-only image.
