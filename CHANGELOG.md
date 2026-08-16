# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/datarocks-ag/gcp-emulator-provisioner/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/datarocks-ag/gcp-emulator-provisioner/releases/tag/v1.0.0
