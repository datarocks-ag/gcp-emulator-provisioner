# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [2.0.1] - 2026-08-18

Documentation and dependencies. No change to the provisioner's or the stub's
behaviour.

### Added

- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)**, describing how the
  components interact, with diagrams: the local stack topology, the credential
  and token-exchange sequence, dual targeting, package layering, section
  ordering, the reconciliation loop, and what deliberately does not exist.

  The stack diagram makes one thing explicit that the Compose file does not: the
  token stub has no outgoing arrows. It reads no files and holds no state, which
  is why it does not mount the secrets volume — it never needs the key it hands
  out tokens for.

- **Dependabot version updates** for Go modules, GitHub Actions and the
  Dockerfile builder images, on top of the security updates that were already
  running. Minor and patch updates are grouped into one pull request per
  ecosystem; majors arrive individually.

  No `target-branch` is set, because specifying it would stop Dependabot raising
  security updates, and the branch prefix stays at the default because `ci.yaml`
  triggers on `dependabot/**`.

### Changed

- **The token stub's Compose service now declares its configuration.** It
  previously set nothing, which hid both the available settings and a trap: the
  healthcheck runs the binary with no flags, so it resolves the port from
  `TOKEN_STUB_ADDR`. Moving the server with `--addr` instead leaves the probe
  checking `:8099`, and the container reports unhealthy while serving perfectly
  well. All three variables are now shown at their defaults, and the reason is
  stated in the `--ping` flag help and the README.

- Dependency updates:
  - `google.golang.org/api` 0.287.1 to 0.293.0
  - `google.golang.org/grpc` 1.82.1 to 1.83.0
  - `google.golang.org/protobuf` 1.36.11 to 1.36.12
  - `golang` builder image 1.25-alpine to 1.26-alpine
  - `actions/checkout`, `actions/setup-go` and `actions/upload-artifact` 6 to 7,
    `codecov/codecov-action` 5 to 7, `docker/setup-buildx-action` 3 to 4

## [2.0.0] - 2026-08-18

Local stacks can now satisfy client libraries that insist on a credential.
Spring Cloud GCP parses `GOOGLE_APPLICATION_CREDENTIALS` eagerly at startup, and
`google-cloud-storage` for Java has no `STORAGE_EMULATOR_HOST` equivalent — it
authenticates for real even against an emulator that checks nothing. This
release covers both halves locally: a generated key file, and a token endpoint
for it to talk to. Together they replace the OpenSSL and nginx sidecars such a
stack would otherwise need.

The provisioner's contract is unchanged: still one-shot, still no long-running
process, still no health endpoint. The token stub is a second, separate binary
with its own lifecycle.

### Added

- **Fake service account keys.** A `credentials:` section writes local key files
  for libraries that refuse to start without `GOOGLE_APPLICATION_CREDENTIALS` —
  Spring Cloud GCP parses the private key eagerly at startup, so a placeholder
  string fails where a real keypair succeeds. The RSA keypair is generated
  locally with stdlib crypto, so Google holds no matching public key and the
  file authenticates to nothing; it is the credential equivalent of a
  self-signed certificate for localhost, and is verified as accepted by Google's
  own auth library. `private_key_id` and `client_id` are fixed, obviously
  non-Google values so the file cannot be mistaken for a real credential.

  `token_uri` points every OAuth URL in the file at one address, so a library
  that does try to mint a token reaches a local stub rather than
  `accounts.google.com`.

  The file is never rewritten once it exists — its contents are a fresh keypair,
  so rewriting would rotate the credential underneath whatever already loaded it
  and could never converge. This is the one setting that does not inherit the
  global strategy; `strategy: update` on the entry forces a new key.

  The key is written `0644` into a `0755` directory. That is deliberate: it
  exists to be read by other containers, which routinely run as a different UID
  than the provisioner, and `0600` would deny the only consumer it has.

- **`gcp-token-stub`**, a second binary serving a static OAuth2 token endpoint
  for local stacks. Some Google client libraries insist on obtaining a token
  before issuing any request, even against an emulator that ignores
  authentication: `google-cloud-storage` for Java has no
  `STORAGE_EMULATOR_HOST` equivalent, so a JVM application reaches
  fake-gcs-server through `spring.cloud.gcp.storage.host` while still holding
  real `ServiceAccountCredentials`, signs a JWT, and exchanges it at the
  `token_uri` from its key file. Pointing that at the stub keeps the exchange on
  the machine.

  The assertion is deliberately not verified, because nothing downstream
  verifies signatures either. It ships as its own `scratch` image — a few
  megabytes in place of an nginx container serving static JSON — and, since
  `scratch` has no shell, probes itself with `--ping` for container
  healthchecks. The provisioner remains one-shot; the stub is a separate binary
  with its own lifecycle.

- The release pipeline publishes **two images**:
  `ghcr.io/datarocks-ag/gcp-emulator-provisioner` and
  `ghcr.io/datarocks-ag/gcp-token-stub`, each scanned by Trivy before it is
  pushed. GoReleaser emits a separate archive per binary.

- **`--section=credentials`**, which touches no endpoint and so runs with no
  emulator up. In a full run credentials are written first, because the
  applications that need the key file usually start alongside the provisioner.

## [1.0.0] - 2026-08-16

Initial release.

### Added

- Idempotent provisioning of Google Cloud Pub/Sub topics and subscriptions and
  Cloud Storage buckets from a YAML config file. Additive only — resources absent
  from the config are never deleted.
- **Two provisioning sections** (`--section` / `GCP_SECTION`): `pubsub`,
  `storage`, or `all` (default). A section with no configured resources is
  skipped and its client is never constructed, so a stack running only one
  emulator needs only that endpoint.
- **Emulator and Google Cloud from one binary.** `PUBSUB_EMULATOR_HOST` and
  `STORAGE_EMULATOR_HOST` select the emulators; with neither set the Google
  client libraries fall back to Application Default Credentials.
- **Topics**: labels and message retention, reconciled through a minimal update
  mask so only drifted fields are sent. Labels already on a topic are merged
  rather than replaced.
- **Subscriptions**: ack deadline, message retention, retain-acked-messages,
  exactly-once delivery, expiration policy (including `never`), labels, dead
  letter policy and retry policy. `filter` and `enable_message_ordering` are
  applied at creation.
- **Push subscriptions**: endpoint and attributes, plus `push_oidc_token`
  (`service_account_email`, `audience`) for endpoints that require an OIDC
  token, and `push_wrapper` (`pubsub` | `none`) with `push_write_metadata` to
  select the delivered payload shape. All of them fold into the single
  `push_config` update mask path, and attributes already on the subscription are
  merged rather than replaced — the mask overwrites the whole message, so
  anything not carried across would be deleted.
- **Optional fields leave live settings alone.** `versioning`,
  `retain_acked_messages`, `enable_message_ordering`,
  `enable_exactly_once_delivery` and `push_wrapper` are all tri-state: omitting
  one leaves whatever the resource has, and only an explicit value changes it.
  A resource configured out of band is never silently reset, and never reported
  as drift the provisioner was not asked to fix.
- **Immutable field drift is a warning, not a failure.** Pub/Sub fixes `filter`,
  `enable_message_ordering` and the subscription's topic at creation; the
  provisioner reports the difference and leaves the resource intact rather than
  recreating it and discarding its backlog.
- **Emulator gaps are tolerated.** The Pub/Sub emulator rejects update masks
  naming `labels` or `expiration_policy` that Google Cloud accepts; those
  specific `InvalidArgument` responses are downgraded to a warning, while every
  other one stays fatal. Because the emulator validates the whole mask before
  touching the resource, a refused path would take every field batched with it
  down too — so an update is sent as one call and, if refused, retried path by
  path. The supported paths still apply, and the warning names the exact path
  that did not.
- **Buckets**: existence, versioning, and create-time location and storage class.
  A live bucket whose `location` or `storage_class` disagrees with the config is
  reported as a warning — Cloud Storage cannot move a bucket, so the difference
  is never applied. That comparison is off against an emulator, which reports
  `US-CENTRAL1` / `STANDARD` for every bucket whatever it was created with;
  `STORAGE_EMULATOR_HOST` decides. Bucket labels, lifecycle rules and CORS are
  deliberately not managed, because fake-gcs-server accepts and then discards
  them, which would produce drift that never converges.
- Topics are provisioned in a full pass before any subscription, so a
  subscription may reference a dead letter topic declared later in the file. A
  dead letter topic that is not declared anywhere is rejected at config load.
- **The config surface is deliberately a subset of the API.** IAM belongs in
  Terraform, and Pub/Sub schemas, BigQuery/Cloud Storage/Bigtable export
  subscriptions, message transforms and import topics are out of scope because
  the emulator implements none of them — a config using them could not be
  exercised locally, which is the point of one binary targeting both.
- Dry-run mode (`--dry-run` / `DRY_RUN`) that previews every mutation against
  live state without applying it.
- YAML config with `${VAR}` environment variable expansion, including inside
  label and push-attribute maps.
- Configurable strategy (`update` by default, or `create` to skip existing
  resources), settable globally or per topic, subscription and bucket.
- Path-prefixed config validation covering Pub/Sub and Cloud Storage naming
  rules, duration syntax, dead letter delivery-attempt bounds, retry backoff
  ordering, and push settings declared without a push endpoint.
- **Duration fields are checked against the Google Cloud limits at config load**,
  rather than failing on the first API call. The emulator is more permissive, so
  without this a config could pass locally and then fail in production:

  | Field | Range |
  |---|---|
  | `ack_deadline` | 10s – 600s, whole seconds only |
  | `message_retention` (topic and subscription) | 10m – 31d |
  | `expiration_ttl` | 1d – 365d, or `never` |
  | `retry.minimum_backoff`, `retry.maximum_backoff` | 0s – 600s |
  | `dead_letter.max_delivery_attempts` | 5 – 100 |

  `ack_deadline` is restricted to whole seconds because Pub/Sub stores it as
  `ack_deadline_seconds`: `10s500ms` would be truncated on the wire, read back
  as `10s`, and then reported as drift on every subsequent run.
- Exponential backoff retry (15 attempts, 1s doubling to 30s, 5 minute ceiling)
  while the emulators come up, plus a 30s deadline on every bucket call so
  fake-gcs-server's retryable 500 for an unsupported field cannot hang the run
  indefinitely.
- A clear error when `versioning` is requested against a fake-gcs-server started
  on its default filesystem backend, which does not implement it.
- Structured JSON logging via `log/slog`, with `LOG_LEVEL` control.
- Docker Compose stack running both emulators with healthchecks, and a
  multi-stage `scratch` container image.
- Integration tests that boot both emulators with `testcontainers-go` and
  exercise provisioning, idempotency, drift reconciliation, dry-run, immutable
  field drift, the push config oneofs, and the emulator label gap including that
  a refused path does not discard the rest of its batch.

[Unreleased]: https://github.com/datarocks-ag/gcp-emulator-provisioner/compare/v2.0.1...HEAD
[2.0.1]: https://github.com/datarocks-ag/gcp-emulator-provisioner/compare/v2.0.0...v2.0.1
[2.0.0]: https://github.com/datarocks-ag/gcp-emulator-provisioner/compare/v1.0.0...v2.0.0
[1.0.0]: https://github.com/datarocks-ag/gcp-emulator-provisioner/releases/tag/v1.0.0
