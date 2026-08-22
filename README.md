# gcp-emulator-provisioner

[![CI](https://github.com/datarocks-ag/gcp-emulator-provisioner/actions/workflows/ci.yaml/badge.svg)](https://github.com/datarocks-ag/gcp-emulator-provisioner/actions/workflows/ci.yaml)
![coverage](https://raw.githubusercontent.com/datarocks-ag/gcp-emulator-provisioner/badges/.badges/develop/coverage.svg)

A Go CLI tool that idempotently provisions Google Cloud Pub/Sub topics and
subscriptions and Cloud Storage buckets from a YAML config file. Designed to run
as a one-shot container — either as a Docker Compose init service (via
`service_completed_successfully`) or as a Kubernetes `Job` / init container.

The name says *emulator* because that is the primary use case: standing up the
Pub/Sub and Cloud Storage state a local dev stack expects, against
`gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators` and `fsouza/fake-gcs-server`.
Unlike the sibling provisioners in this ecosystem, which drive the genuine
article, there is no way to run Google Cloud locally — so the local target is a
reimplementation, and coping with its gaps is a first-class design concern here.
See [Emulator Caveats](#emulator-caveats).

**It is not emulator-only.** The same binary and the same config run against real
Google Cloud: the client libraries switch on `PUBSUB_EMULATOR_HOST` and
`STORAGE_EMULATOR_HOST`, and fall back to Application Default Credentials when
neither is set. That dual targeting is deliberate — a config you exercise locally
is the one you ship — and nothing is supported here that only works against one
side.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how the components fit
together, with diagrams.

## Features

- Idempotent provisioning of Pub/Sub topics, subscriptions and Cloud Storage buckets
- Drift reconciliation with a minimal update mask — only the fields that actually
  changed are sent
- Fields Pub/Sub fixes at creation (`filter`, `enable_message_ordering`) are
  reported as warnings instead of failing the run
- Emulator gaps that Google Cloud does not have are downgraded to warnings, so one
  config works against both
- Dry-run mode that previews every mutation against live state
- YAML config with `${VAR}` environment variable expansion
- Configurable strategy: `update` (default) or `create` (skip existing)
- Exponential backoff retry while the emulators come up
- Structured JSON logging via `log/slog`
- Never deletes topics, subscriptions or buckets that are not in the config

## Quick Start

```bash
docker compose up
```

This starts both emulators, waits for them to become healthy, provisions
everything in `config.example.yaml`, and exits 0.

## Reusing the emulator stack

`docker-compose.yaml` is the worked example for this repository. The two upstream
emulator containers inside it — their pinned images, arguments and healthchecks —
are also published on their own as
[docker-compose.emulators.yaml](docker-compose.emulators.yaml), so another project
does not have to re-derive them.

```yaml
include:
  - path: docker-compose.emulators.yaml

services:
  gcp-emulator-provisioner:
    image: ghcr.io/datarocks-ag/gcp-emulator-provisioner:latest
    depends_on:
      pubsub: { condition: service_healthy }
      gcs:    { condition: service_healthy }
    environment:
      GCP_PROJECT_ID: local-dev
      PUBSUB_EMULATOR_HOST: pubsub:8085
      STORAGE_EMULATOR_HOST: gcs:4443
    volumes:
      - ./gcp-config.yaml:/config.yaml:ro
```

`include:` (Compose v2.20+) resolves local paths only — there is no URL form — so
vendor the file by copy, submodule or subtree. Everything a consumer normally
changes is an environment variable with a default, so a vendored copy stays
byte-identical to the original and the next update diffs cleanly instead of
replaying local edits.

| Variable | Default | Sets |
|---|---|---|
| `GCP_PROJECT_ID` | `local-dev` | the emulator project, the notification project, and the storage healthcheck query |
| `PUBSUB_EMULATOR_PORT` | `8085` | the published host port |
| `STORAGE_EMULATOR_PORT` | `4443` | the published host port |
| `STORAGE_EMULATOR_PUBLIC_HOST` | `localhost:4443` | the `Host` that path-style signed URLs must be requested with |
| `STORAGE_NOTIFICATIONS_TOPIC` | `storage-notifications` | the topic object events are published to |
| `STORAGE_NOTIFICATIONS_EVENTS` | `finalize` | `finalize`, `delete`, `metadataUpdate`, `archive` |

The container-internal ports are fixed at 8085 and 4443, so `pubsub:8085` and
`gcs:4443` stay correct inside the network whatever the host publishes.

Both services are bound to `127.0.0.1`, and fake-gcs-server runs on its in-memory
backend — the only backend that implements versioning.

### Object notifications

fake-gcs-server publishes bucket notifications to the Pub/Sub emulator, which is
close enough to real GCS notifications to develop against. The topic has to exist:
declare it in your config like any other.

```yaml
pubsub:
  topics:
    - name: storage-notifications
```

If it does not, the upload still answers `200` and the event is dropped — the only
trace is `error publishing event: ... NotFound` in the `gcs` container log. Nothing
fails loudly; the notifications simply never arrive.

### Persisting objects

The in-memory backend loses every object on restart. Trading versioning away for
persistence means overriding the whole `gcs` command in your own file, because
Compose replaces list-valued `command` rather than merging it:

```yaml
services:
  gcs:
    command: ["-scheme", "http", "-host", "0.0.0.0", "-port", "4443",
              "-backend", "filesystem", "-public-host", "localhost:4443"]
    volumes:
      - ./run/mock-storage:/storage
    # Defaulted, because an unset variable collapses to ":" and the container
    # runs as root — leaving root-owned files in ./run/mock-storage that the next
    # run, as a real uid, cannot read. That surfaces as fake-gcs answering 500 to
    # everything. macOS is typically 501.
    user: "${STORAGE_EMULATOR_USERID:-1000}:${STORAGE_EMULATOR_GROUPID:-1000}"
```

A bucket declaring `versioning` then fails against it — see
[Emulator Caveats](#emulator-caveats).

### Why a fragment and not an image

A derived `gcp-pubsub-emulator` image would make this repository the vendor of an
artifact it does not own: every gcloud-SDK CVE would wait on a rebuild here before
reaching you, and the image scan in `release.yaml` would report a wall of findings
nothing in this repository can fix. The arguments that genuinely differ per stack —
`-public-host`, the notification topic, the backend — cannot be baked in anyway.

## Scope

The provisioner manages **application-level state** — the resources your services
expect to exist at startup. It deliberately does not touch IAM bindings, service
accounts or project configuration: the emulators do not enforce IAM at all, so a
config exercising it locally would diverge silently from production. That layer
belongs in Terraform.

| Resource | Managed |
|---|---|
| Service account keys | a local, non-authenticating key file for libraries that require `GOOGLE_APPLICATION_CREDENTIALS` |
| Pub/Sub topics | labels, message retention |
| Pub/Sub subscriptions | ack deadline, retention, retain-acked, exactly-once, expiration, labels, push config (endpoint, attributes, OIDC token, payload wrapper), dead letter policy, retry policy, filter and ordering (at creation) |
| Cloud Storage buckets | existence, versioning, location and storage class (at creation) |

Bucket labels, lifecycle rules and CORS are **not** managed: fake-gcs-server
accepts all three and then discards them, so every run would see drift and
rewrite them forever without ever converging.

Pub/Sub schemas, BigQuery/Cloud Storage/Bigtable export subscriptions, message
transforms and import topics are **not** managed either. The Pub/Sub emulator
implements none of them — it serves no `SchemaService` at all — so a config
using them could not be exercised locally, which is the whole point of this
binary targeting both.

## Fake service account keys

Many libraries refuse to start without `GOOGLE_APPLICATION_CREDENTIALS` pointing
at a parseable service account key — Spring Cloud GCP, for one, calls
`GoogleCredentials.fromStream` eagerly at startup and parses the private key
there and then. That happens even when every call the application goes on to
make is answered by an emulator that ignores authentication entirely.

```yaml
credentials:
  - path: ./secrets/fake-sa.json
    account: local-emulator                    # optional
    token_uri: http://oauth2-stub:8080/token   # optional
```

The generated file is **structurally a real key and cryptographically inert**.
The RSA keypair is generated locally, so Google holds no matching public key and
the file cannot authenticate to Google Cloud — it is the credential equivalent
of a self-signed certificate for localhost. Verified as accepted by Google's own
auth library, which is what makes it work where a hand-written placeholder fails.

Every identifying field is a fixed, obviously non-Google value, so the file
cannot be mistaken for a real credential:

| Field | Value |
|---|---|
| `private_key_id` | `local-emulator-key` (a real one is 40 hex characters) |
| `client_id` | `000000000000000000000` |
| `client_email` | `<account>@<project>.iam.gserviceaccount.com` |

`token_uri` points **every** OAuth URL in the file at one address, so a library
that does try to mint a token reaches a local stub rather than
`accounts.google.com`. Omit it to use Google's real endpoints.

**The file is written world-readable (`0644`, in a `0755` directory).** That is
deliberate: it exists to be read by *other* containers, which routinely run as a
different UID than whatever wrote it, and `0600` would deny the only consumer it
has at application startup. There is nothing to protect — the key authenticates
to nothing.

**The file is never rewritten once it exists.** Its contents are a fresh
keypair, so rewriting would rotate the credential underneath whatever already
loaded it, and could never converge. This is the one setting that does not
inherit the global `strategy` — put `strategy: update` on the entry itself to
force a new key.

## The token stub

A key file alone is not always enough. Some Google client libraries insist on
*obtaining* a token before they will issue any request, even against an emulator
that ignores authentication: `google-cloud-storage` for Java has no
`STORAGE_EMULATOR_HOST` equivalent, so a JVM application reaches fake-gcs-server
through `spring.cloud.gcp.storage.host` while still holding real
`ServiceAccountCredentials`. Those sign a JWT and exchange it at the `token_uri`
from their key file.

`gcp-token-stub` is a second, tiny binary in this repository that answers that
exchange locally, so it never leaves the machine:

```bash
gcp-token-stub                      # listens on :8099, serves /token
gcp-token-stub --addr :9000
gcp-token-stub --ping               # probe a running stub; for healthchecks
```

```
POST /token  ->  200 {"access_token":"local-emulator-token","token_type":"Bearer","expires_in":3600}
GET  /token  ->  the same, so a healthcheck can probe without forging a grant
anything else -> 404 {"error":"not_found"}
```

Point a credential at it and the whole loop closes locally:

```yaml
credentials:
  - path: /secrets/fake-sa.json
    token_uri: http://gcp-token-stub:8099/token
```

**The assertion is deliberately not verified.** Nothing downstream checks the
signatures either — fake-gcs-server does not validate them — so verifying here
would only be theatre. This is a local development stub and must never be
exposed to anything that matters.

It ships as its own image, `ghcr.io/datarocks-ag/gcp-token-stub` — 6MB on
`scratch`, against nginx:alpine's 62MB. Because `scratch` has no shell, `wget` or `curl`, the binary probes
itself for container healthchecks:

```yaml
healthcheck:
  test: ["CMD", "/gcp-token-stub", "--ping"]
```

Configure the address through the **environment** rather than `--addr` when you
change it. The healthcheck runs the binary with no flags, so it resolves the
port from `TOKEN_STUB_ADDR`; moving the server with `--addr` alone leaves the
probe checking `:8099` and the container reports unhealthy while serving
perfectly well.

| Setting | Flag | Env | Default |
|---|---|---|---|
| Listen address | `--addr` | `TOKEN_STUB_ADDR` | `:8099` |
| Access token | `--token` | `TOKEN_STUB_ACCESS_TOKEN` | `local-emulator-token` |
| Token lifetime | — | `TOKEN_STUB_EXPIRES_IN` | `3600` |
| Log level | — | `LOG_LEVEL` | `info` |

## Sections

Pub/Sub and Cloud Storage are independent services on different endpoints, so
they are separate sections that can be run separately. Credentials are a third
section that touches no endpoint at all, so it runs with no emulator up:

```bash
gcp-emulator-provisioner --section=pubsub       # topics and subscriptions only
gcp-emulator-provisioner --section=storage      # buckets only
gcp-emulator-provisioner --section=credentials  # key files only, no endpoint needed
gcp-emulator-provisioner                        # all three (default)
```

Within a full run, credentials are written first: the applications that need the
key file usually start alongside the provisioner rather than after it.

A section whose config is empty is skipped, and its client is never constructed —
running against a stack with only fake-gcs-server needs no Pub/Sub endpoint.

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `GCP_PROJECT_ID` | conditional | — | Project every resource is created under. Required unless `project_id` is set in the config |
| `PUBSUB_EMULATOR_HOST` | no | — | `host:port` of the Pub/Sub emulator. Unset means Google Cloud with Application Default Credentials |
| `STORAGE_EMULATOR_HOST` | no | — | Host of the Cloud Storage emulator, with or without a scheme. Unset means Google Cloud |
| `GCP_SECTION` | no | `all` | `pubsub`, `storage`, `credentials`, or `all` |
| `GCP_CONFIG_PATH` | no | `./config.yaml` | Path to the YAML config |
| `DRY_RUN` | no | `false` | Set to `true` to log every mutation as a preview without applying it |
| `LOG_LEVEL` | no | `info` | Log level (debug/info/warn/error) |

`GCP_PROJECT_ID` takes precedence over `project_id` in the config file.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--section` | `all` (or `GCP_SECTION`) | Provisioning section: `pubsub`, `storage`, `credentials` or `all`. The CLI flag takes precedence over the env var |
| `--dry-run` | `false` (or `DRY_RUN`) | Log every mutation as a preview without applying it. Read-only calls still run so the preview reflects live state. The CLI flag takes precedence over the env var |
| `--version` | — | Print version and exit |

## Configuration

See [`config.example.yaml`](config.example.yaml) for a fully commented example.

```yaml
strategy: update      # create | update — set globally or per resource
project_id: local-dev

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
          dead_letter:
            topic: orders-dlq         # must itself be declared under topics
            max_delivery_attempts: 5
          retry:
            minimum_backoff: 10s
            maximum_backoff: 600s

    - name: orders-dlq

storage:
  buckets:
    - name: documents
      versioning: true
```

Durations accept Go syntax (`30s`, `1h30m`) plus `Nd` day notation (`7d`).

### Limits

Duration fields are validated against the ranges Google Cloud accepts, and a
value outside them fails at config load rather than on the first API call. The
emulator is more permissive than Google Cloud here, so checking locally is what
stops a config from passing against the emulator and failing in production.

| Field | Range |
|---|---|
| `ack_deadline` | 10s – 600s, whole seconds only |
| `message_retention` (topic and subscription) | 10m – 31d |
| `expiration_ttl` | 1d – 365d, or `never` |
| `retry.minimum_backoff`, `retry.maximum_backoff` | 0s – 600s |
| `dead_letter.max_delivery_attempts` | 5 – 100 |

`ack_deadline` must be a whole number of seconds because Pub/Sub stores it as
`ack_deadline_seconds`: `10s500ms` would be truncated on the wire, read back as
`10s`, and then reported as drift on every subsequent run.

### Push subscriptions

A subscription with a `push_endpoint` is delivered over HTTP instead of pulled.
Three optional settings sit alongside it, all part of the same `push_config` and
all meaningless without an endpoint:

```yaml
push_endpoint: https://consumer.example.com/events
push_attributes:
  x-goog-version: v1
push_oidc_token:                 # Pub/Sub authenticates with an OIDC token
  service_account_email: pusher@my-project.iam.gserviceaccount.com
  audience: https://consumer.example.com   # defaults to the endpoint
push_wrapper: none               # "pubsub" (default envelope) | "none" (raw payload)
push_write_metadata: true        # X-Goog-Pubsub-* headers; only with push_wrapper: none
```

Omitting `push_wrapper` leaves whatever the subscription already has. Push
attributes already set out of band are preserved across an update, the same way
labels are — the update mask replaces the whole `push_config` message.

Neither emulator validates the service account, so `push_oidc_token` is inert
locally and only takes effect against Google Cloud.

### Tri-state fields

`versioning`, `retain_acked_messages`, `enable_message_ordering` and
`enable_exactly_once_delivery` are all optional booleans. Omitting one leaves the
live setting alone; setting it to `false` actively turns the feature off. This
matters for a bucket whose versioning was enabled out of band — a plain `false`
default would silently suspend it.

The same rule applies to the immutable fields below: a subscription with
`enable_message_ordering` or a `filter` set out of band is left alone by a config
that says nothing about them, rather than warning on every run.

`expiration_ttl` takes a duration, or the literal `never` to stop Pub/Sub
garbage-collecting an idle subscription. Omitting it leaves the default (31 days).

### Ordering

Every topic is created before any subscription, so a subscription may reference a
dead letter topic that is declared later in the file. A dead letter topic must be
declared somewhere in `topics` — the provisioner is additive and never creates a
resource it was not asked for, so an undeclared target is rejected at config load
rather than surfacing as a bare `NotFound` from the API.

## Immutable Fields

Pub/Sub fixes some subscription settings at creation. Changing them in the config
later produces a warning and is **not** applied — rewriting them would mean
deleting and recreating the subscription, discarding its backlog and
acknowledgement state.

| Field | Why |
|---|---|
| `filter` | Immutable on Google Cloud; the emulator rejects the update explicitly |
| `enable_message_ordering` | Immutable on Google Cloud and in the emulator |
| the subscription's topic | Immutable everywhere |

```
WARN Subscription field cannot be changed after creation, leaving it as is
     subscription=order-audit field=filter
     current="attributes.type = \"order.created\""
     configured="attributes.type = \"order.shipped\""
     remedy="delete the subscription by hand if the new value is required"
```

## Emulator Caveats

Both emulators are incomplete in ways that matter, all verified against the
images this repository tests with.

**The Pub/Sub emulator rejects update masks Google Cloud accepts.** `labels` on
topics and subscriptions, and `expiration_policy` on subscriptions, come back as
`InvalidArgument`. All three work at *creation* time. The provisioner downgrades
these specific failures to a warning so a config that is correct against Google
Cloud still runs locally; every other `InvalidArgument` remains fatal.

The emulator validates the whole update mask before touching the resource, so a
refused path would otherwise abandon every field batched with it. The provisioner
sends the batch first and, if it is refused, re-sends each path on its own — the
paths this endpoint supports are still applied, and the warning names the one
that was not:

```
WARN Subscription field not supported by this Pub/Sub endpoint, leaving it unchanged
     subscription=order-processor field=labels
```

**fake-gcs-server only implements versioning on its in-memory backend.** Started
without `-backend memory`, a bucket with `versioning: true` fails with a 500. The
provisioner recognises it and says so:

```
creating bucket "documents": ... does not support versioning ...
(fake-gcs-server only implements versioning on its in-memory backend:
 start it with -backend memory, or drop `versioning` from the bucket)
```

That 500 carries a retryable reason code, so the Cloud Storage client would
otherwise retry it forever. Every bucket call is therefore bounded by a 30s
deadline.

**fake-gcs-server ignores `location` and `storage_class`**, always reporting
`US-CENTRAL1` / `STANDARD`. Both are applied at creation only and never
reconciled — Cloud Storage cannot move a bucket — and both are honoured against
real Cloud Storage.

Against real Cloud Storage a live bucket that disagrees with the config is
reported as a warning, in the same shape as the immutable subscription fields
above. Against an emulator the check is switched off, because comparing with a
fabricated `US-CENTRAL1` would warn on every run about a difference that is not
real. `STORAGE_EMULATOR_HOST` is what decides.

**fake-gcs-server accepts bucket CORS and discards it**, exactly as it does
labels and lifecycle rules — the `POST` returns 200 and the bucket never reports
the policy back. That is why the config schema has no `cors`.

**The Pub/Sub emulator image is roughly a gigabyte**, so the first pull dominates
a cold start — which is why the Compose healthcheck allows a long `start_period`
and the integration tests allow a five-minute startup. Compose and the tests pin
`gcr.io/google.com/cloudsdktool/google-cloud-cli:581.0.0-emulators`, the renamed
`cloud-sdk` repository and the one publishing arm64, so an Apple Silicon host runs
it natively instead of under emulation.

## Dry Run

`--dry-run` (or `DRY_RUN=true`) logs what would change without applying anything.
Read-only calls still execute, so the preview reflects live state rather than
guessing — the one exception is a resource that does not exist yet, whose
settings cannot be read, so the run reports the intended create instead.

```console
$ gcp-emulator-provisioner --dry-run
INFO Dry run: no changes will be applied
INFO Would update topic topic=orders fields=["message_retention_duration"]
INFO Would update subscription subscription=order-processor fields=["ack_deadline_seconds"]
INFO Provisioning complete
```

## Kubernetes

Run it as a `Job`, or as an init container on the first workload that needs the
resources:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: gcp-emulator-provisioner
spec:
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: gcp-emulator-provisioner
          image: ghcr.io/datarocks-ag/gcp-emulator-provisioner:latest
          env:
            - name: GCP_PROJECT_ID
              value: local-dev
            - name: PUBSUB_EMULATOR_HOST
              value: pubsub:8085
            - name: STORAGE_EMULATOR_HOST
              value: gcs:4443
            - name: GCP_CONFIG_PATH
              value: /config/config.yaml
          volumeMounts:
            - name: config
              mountPath: /config
      volumes:
        - name: config
          configMap:
            name: gcp-emulator-provisioner-config
```

Against real Google Cloud, drop the two emulator variables and give the pod a
service account through Workload Identity.

## Exit Codes

- `0` — provisioning completed (Kubernetes `Job` compatible)
- `1` — invalid section, config load, connection, or provisioning failure

## Development

```sh
make build             # go build -o gcp-emulator-provisioner ./cmd/gcp-emulator-provisioner
make test              # go test -race ./...
make test-integration  # go test -race -tags=integration ./...   (requires Docker)
make lint              # go tool golangci-lint run
make docker            # docker build -t gcp-emulator-provisioner .
```

Integration tests boot both emulators with `testcontainers-go` and run the real
provisioner against them, including the idempotency, drift, dry-run and
immutable-field paths.

## License

MIT — see [LICENSE](LICENSE).
