# FAQ

Short answers to the questions that come up once you run the provisioner against the emulators or Google Cloud; the [how-tos](howto/) carry the long form.

Log lines on this page are real output with the `time` field removed and host ports shown as the Compose defaults; `…` marks what was left out.

- [Configuration errors](#configuration-errors)
  - [Why does the run fail with "project id is required"?](#why-does-the-run-fail-with-project-id-is-required)
  - [Why is my dead letter topic rejected as not declared?](#why-is-my-dead-letter-topic-rejected-as-not-declared)
  - [Why is `10s500ms` rejected for `ack_deadline`?](#why-is-10s500ms-rejected-for-ack_deadline)
  - [Why is my duration outside the range Google Cloud accepts when the emulator accepted it?](#why-is-my-duration-outside-the-range-google-cloud-accepts-when-the-emulator-accepted-it)
  - [Why can't two subscriptions share a name?](#why-cant-two-subscriptions-share-a-name)
  - [Why is my bucket name with a dot rejected?](#why-is-my-bucket-name-with-a-dot-rejected)
  - [Why do my push settings fail validation?](#why-do-my-push-settings-fail-validation)
  - [Why did a `${VAR}` stay literally in my resource?](#why-did-a-var-stay-literally-in-my-resource)
  - [Why did a misspelled field load without an error?](#why-did-a-misspelled-field-load-without-an-error)
  - [Where does the provisioner read its config from?](#where-does-the-provisioner-read-its-config-from)
- [Day-to-day behaviour](#day-to-day-behaviour)
  - [I removed a resource from the config — why does it still exist?](#i-removed-a-resource-from-the-config--why-does-it-still-exist)
  - [Why did labels or push attributes I set by hand survive an update?](#why-did-labels-or-push-attributes-i-set-by-hand-survive-an-update)
  - [What is the difference between omitting a boolean and setting it to `false`?](#what-is-the-difference-between-omitting-a-boolean-and-setting-it-to-false)
  - [When should I use `strategy: create`?](#when-should-i-use-strategy-create)
  - [Why wasn't my fake key file rewritten under a global `strategy: update`?](#why-wasnt-my-fake-key-file-rewritten-under-a-global-strategy-update)
  - [Is it safe to run the provisioner on every `up`, or twice at once?](#is-it-safe-to-run-the-provisioner-on-every-up-or-twice-at-once)
  - [Why do I see "field not supported by this Pub/Sub endpoint" warnings?](#why-do-i-see-field-not-supported-by-this-pubsub-endpoint-warnings)
  - [Why don't my `location` or `storage_class` changes apply?](#why-dont-my-location-or-storage_class-changes-apply)
- [Emulators](#emulators)
  - [Why does a bucket with `versioning: true` fail?](#why-does-a-bucket-with-versioning-true-fail)
  - [Why do bucket calls give up after 30 seconds?](#why-do-bucket-calls-give-up-after-30-seconds)
  - [Why do my storage notifications never arrive?](#why-do-my-storage-notifications-never-arrive)
  - [Why are my topics gone after the emulator restarts?](#why-are-my-topics-gone-after-the-emulator-restarts)
  - [Does `push_oidc_token` do anything locally?](#does-push_oidc_token-do-anything-locally)
  - [Why is the first `docker compose up` so slow?](#why-is-the-first-docker-compose-up-so-slow)
  - [Why can't I set labels, lifecycle rules or CORS on a bucket?](#why-cant-i-set-labels-lifecycle-rules-or-cors-on-a-bucket)
- [Token stub](#token-stub)
  - [Is the token stub safe to expose?](#is-the-token-stub-safe-to-expose)
  - [Why is the token stub unhealthy although it serves tokens?](#why-is-the-token-stub-unhealthy-although-it-serves-tokens)
  - [Why does the token stub exit immediately?](#why-does-the-token-stub-exit-immediately)
  - [How do I health-check the token stub when the image has no shell?](#how-do-i-health-check-the-token-stub-when-the-image-has-no-shell)
  - [Does the provisioner itself need the token stub?](#does-the-provisioner-itself-need-the-token-stub)
- [Operations](#operations)
  - [What happens if an emulator isn't up yet?](#what-happens-if-an-emulator-isnt-up-yet)
  - [Why does a run against real Google Cloud retry for five minutes and then fail?](#why-does-a-run-against-real-google-cloud-retry-for-five-minutes-and-then-fail)
  - [What do the exit codes mean?](#what-do-the-exit-codes-mean)
  - [Which values does `DRY_RUN` accept?](#which-values-does-dry_run-accept)
  - [Does dry-run need the emulators running?](#does-dry-run-need-the-emulators-running)
  - [What happens with an invalid `LOG_LEVEL`?](#what-happens-with-an-invalid-log_level)
  - [Can I provision Storage without a Pub/Sub emulator?](#can-i-provision-storage-without-a-pubsub-emulator)

## Configuration errors

Every error in this group comes from config validation, so you can reproduce it on your machine
without any emulator: `--section=credentials --dry-run` loads and validates the whole file and
connects to nothing. The error names the offending field by its path, such as
`pubsub.topics[0].subscriptions[1].ack_deadline`.

### Why does the run fail with "project id is required"?

Because neither `GCP_PROJECT_ID` nor `project_id` in the config is set, and the provisioner has no
default project.

Every topic, subscription and bucket is created under a project, and the generated key file's
`client_email` contains it, so even `--section=credentials` needs one. `GCP_PROJECT_ID` wins over
`project_id` when both are set. Only `project_id` in the file is checked against the Google Cloud
format (6–30 characters, lowercase letters, digits and hyphens, starting with a letter).

```console
$ GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=credentials --dry-run
…
{"level":"ERROR","msg":"Provisioning failed","error":"project id is required: set GCP_PROJECT_ID or project_id in the config file"}
```

Add `project_id: local-dev` to the file, or set `GCP_PROJECT_ID=local-dev`. Safe to run anywhere:
it reads only the config file.

Further reading: [Environment Variables](../README.md#environment-variables)

### Why is my dead letter topic rejected as not declared?

Because `dead_letter.topic` names a topic that is not in `pubsub.topics`, and the provisioner only
creates topics you list.

Without this check, the subscription would fail at creation with a bare `NotFound` from the API.
The dead letter topic may appear anywhere in the list: every topic is created before any
subscription.

```console
$ GCP_PROJECT_ID=local-dev GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=credentials --dry-run
…
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[0].subscriptions[0].dead_letter.topic: \"orders-dlq\" is not declared in pubsub.topics (the provisioner only creates topics listed in the config)"}
```

Declare it as a topic of its own:

```yaml
project_id: local-dev
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: order-processor
          dead_letter:
            topic: orders-dlq
            max_delivery_attempts: 5
    - name: orders-dlq
```

`max_delivery_attempts` is optional; left out, Pub/Sub uses its default of 5, and the provisioner
does not report that as a change.

Further reading: [Route failed messages to a dead letter topic](howto/route-failed-messages-to-a-dead-letter-topic.md),
[Ordering](../README.md#ordering)

### Why is `10s500ms` rejected for `ack_deadline`?

Because Pub/Sub stores the acknowledgement deadline in whole seconds, so a fractional value can
never match what is read back.

`10s500ms` would be sent as 10 seconds, read back as `10s`, and reported as drift on every
following run. The value must also lie between 10s and 600s.

```console
$ GCP_PROJECT_ID=local-dev GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=credentials --dry-run
…
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[0].subscriptions[0].ack_deadline: \"10s500ms\" must be a whole number of seconds (Pub/Sub stores it as ack_deadline_seconds, so a fractional value is truncated and then reported as drift on every run)"}
```

Round to a whole number of seconds, such as `ack_deadline: 11s`.

Further reading: [Limits](../README.md#limits)

### Why is my duration outside the range Google Cloud accepts when the emulator accepted it?

Because the provisioner checks durations against Google Cloud's limits at load time, not against
what the emulator tolerates.

The emulator accepts values Google Cloud rejects. Without the check, a config you only ever ran
locally would pass and then fail on its first run against Google Cloud with an `InvalidArgument`.
The limits are: `ack_deadline` 10s–600s, `message_retention` 10m–31d, `expiration_ttl` 1d–365d or
`never`, the retry backoffs 0s–600s, and `max_delivery_attempts` 5–100.

```console
$ GCP_PROJECT_ID=local-dev GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=credentials --dry-run
…
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[0].message_retention: \"60d\" is outside the range Google Cloud accepts (10m-31d)"}
```

Further reading: [Limits](../README.md#limits),
[`internal/config/config.go`](../internal/config/config.go)

### Why can't two subscriptions share a name?

Because Pub/Sub subscription names are unique per project, not per topic.

Two topics can therefore not each have a subscription called `audit`; the second one is
rejected at load:

```console
$ GCP_PROJECT_ID=local-dev GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=credentials --dry-run
…
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[1].subscriptions[0].name: duplicate subscription name \"audit\" (subscription names are unique per project)"}
```

Give each its own name, such as `orders-audit` and `payments-audit`.

Further reading: [`internal/config/config.go`](../internal/config/config.go)

### Why is my bucket name with a dot rejected?

Because the provisioner accepts only bucket names of 3–63 lowercase letters, digits and hyphens,
starting and ending with a letter or digit.

Cloud Storage also allows domain-named buckets containing dots; the provisioner deliberately
rejects them.

```console
$ GCP_PROJECT_ID=local-dev GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=credentials --dry-run
…
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: storage.buckets[0].name: invalid bucket name \"assets.example.com\" (must be 3-63 chars, lowercase alphanumeric and hyphens)"}
```

Use hyphens instead, such as `assets-example-com`.

Further reading: [`internal/config/config.go`](../internal/config/config.go)

### Why do my push settings fail validation?

Because every push setting belongs to the subscription's push configuration, and that only exists
when `push_endpoint` is set.

The rules: `push_attributes`, `push_wrapper` and `push_oidc_token` each require `push_endpoint`;
`push_wrapper` is `pubsub` or `none`; `push_write_metadata` requires `push_wrapper: none`; and
`push_oidc_token` requires `service_account_email`.

```console
$ GCP_PROJECT_ID=local-dev GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=credentials --dry-run
…
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[0].subscriptions[0].push_write_metadata: requires push_wrapper: \"none\""}
```

A combination that loads:

```yaml
project_id: local-dev
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: order-webhook
          push_endpoint: http://consumer:8080/events
          push_wrapper: none
          push_write_metadata: true
```

Further reading: [Deliver messages with a push subscription](howto/deliver-messages-with-a-push-subscription.md),
[Push subscriptions](../README.md#push-subscriptions)

### Why did a `${VAR}` stay literally in my resource?

Because the variable was not set in the provisioner's environment, and an unset variable is left
as written.

Only the `${VAR}` form is expanded. `$VAR` is not, and `${VAR:-default}` is not supported: the
provisioner looks for a variable literally named `VAR:-default`. What counts is the environment of
the provisioner process (in Compose, the `environment:` of the provisioner service), not your
shell. In a topic, subscription or bucket name the leftover `${…}` fails validation; in a label,
path or URL it loads silently:

```yaml
project_id: local-dev
credentials:
  - path: ${SECRETS_DIR:-/secrets}/fake-sa.json
```

```console
$ SECRETS_DIR=/secrets GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=credentials --dry-run
…
{"level":"INFO","msg":"Would write credential file","path":"${SECRETS_DIR:-/secrets}/fake-sa.json","account":"local-emulator@local-dev.iam.gserviceaccount.com","token_uri":""}
```

Write `${SECRETS_DIR}/fake-sa.json` and set `SECRETS_DIR`; the same command then logs
`"path":"/secrets/fake-sa.json"`. Safe to run anywhere: dry-run writes nothing.

Further reading: [`internal/config/config.go`](../internal/config/config.go)

### Why did a misspelled field load without an error?

Because unknown keys in the YAML are ignored, not rejected.

A typo such as `ack_dedline` is dropped, the subscription behaves as if the field were omitted,
and the run succeeds:

```yaml
project_id: local-dev
pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: order-processor
          ack_dedline: 30s   # typo: ignored
```

```console
$ GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=credentials --dry-run
…
{"level":"INFO","msg":"Configuration loaded","section":"credentials","project":"local-dev","topics":1,"buckets":0,"credentials":0}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

Compare your field names with [`config.example.yaml`](../config.example.yaml), which uses every
supported field.

Further reading: [Configuration](../README.md#configuration),
[Validate a config without emulators](howto/validate-a-config-without-emulators.md)

### Where does the provisioner read its config from?

From the path in `GCP_CONFIG_PATH`, or `./config.yaml` when it is not set.

`./config.yaml` is relative to the working directory. In the published image the working
directory is `/`, so a file mounted at `/config.yaml` is found without setting the variable. There
is no command-line flag for the path.

```console
$ GCP_PROJECT_ID=local-dev gcp-emulator-provisioner --section=credentials --dry-run
…
{"level":"INFO","msg":"Loading configuration","path":"./config.yaml"}
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: reading config file: open ./config.yaml: no such file or directory"}
```

Further reading: [Environment Variables](../README.md#environment-variables)

## Day-to-day behaviour

### I removed a resource from the config — why does it still exist?

Because the provisioner never deletes anything: not topics, subscriptions, buckets or key files.

Removing a line from the config must not be able to destroy a backlog or a bucket of objects.
Delete the resource with your usual tooling, after removing it from the config, or the next run
creates it again. Against the Pub/Sub emulator, restarting the `pubsub` container also removes
everything, see [Why are my topics gone after the emulator restarts?](#why-are-my-topics-gone-after-the-emulator-restarts).

Further reading: [Features](../README.md#features)

### Why did labels or push attributes I set by hand survive an update?

Because the provisioner merges them: the labels and push attributes in your config are written on
top of those already on the resource, and nothing is removed.

An update replaces the whole label map (or the whole push configuration), so writing only the
configured values would delete everything set elsewhere. Two limits apply: the Pub/Sub emulator
refuses label updates altogether (see [Why do I see "field not supported by this Pub/Sub endpoint" warnings?](#why-do-i-see-field-not-supported-by-this-pubsub-endpoint-warnings)), and when a push setting
changes, the push wrapper and OpenID Connect (OIDC) token set by hand outside the config are kept
as well, as long as the config does not declare its own.

Measured against the Pub/Sub emulator: `x-team` was added by hand, then the config changed the
endpoint to port 8081.

```console
$ GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=pubsub
…
{"level":"INFO","msg":"Updating subscription","subscription":"order-webhook","fields":["push_config"]}
…
$ curl -s http://localhost:8085/v1/projects/local-dev/subscriptions/order-webhook
…
  "pushConfig": {
    "pushEndpoint": "http://consumer:8081/events",
    "attributes": {
      "x-goog-version": "v1",
      "x-team": "orders"
    }
  },
```

Further reading: [Change settings on existing resources](howto/change-settings-on-existing-resources.md)

### What is the difference between omitting a boolean and setting it to `false`?

Omitting it leaves the live setting alone; `false` actively turns the feature off.

This applies to `versioning` on buckets and to `retain_acked_messages`,
`enable_message_ordering` and `enable_exactly_once_delivery` on subscriptions. It protects a
setting someone turned on outside the config. A dry-run against fake-gcs-server, on a bucket that
has versioning on and a config saying `versioning: false`:

```console
$ GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=storage --dry-run
…
{"level":"INFO","msg":"Would set bucket versioning","bucket":"documents","current":true,"enabled":false}
```

With `versioning` omitted, the same run logs nothing about versioning.

Further reading: [Tri-state fields](../README.md#tri-state-fields)

### When should I use `strategy: create`?

When the provisioner should only create a resource that is missing, and leave an existing one
entirely to someone else.

With `create`, an existing topic, subscription or bucket is skipped together with all its
settings; nothing is compared or updated. Set it per resource, or globally at the top of the file
(a resource's own `strategy` wins). The default is `update`. Key files are always create-only
unless you say otherwise, see the next question.

```yaml
project_id: local-dev
storage:
  buckets:
    - name: documents
      strategy: create
      versioning: false
```

```console
$ GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=storage
…
{"level":"INFO","msg":"Skipping existing bucket and its settings (strategy=create)","bucket":"documents"}
```

Further reading: [Configuration](../README.md#configuration)

### Why wasn't my fake key file rewritten under a global `strategy: update`?

Because key files ignore the global strategy: each one is written once and then left alone
unless its own entry says `strategy: update`.

The file holds a freshly generated keypair, so rewriting it would hand every application that
already loaded it a different private key on every run. An entry with `strategy: update` rewrites
the key on **every** run while it is set, so set it for one run and then remove it. Deleting the
file also gets you a new key on the next run.

```console
$ GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=credentials
…
{"level":"INFO","msg":"Skipping existing credential file (a key file is never rewritten unless strategy=update)","path":"./out/fake-sa.json"}
```

Further reading: [Fake service account keys](../README.md#fake-service-account-keys),
[Give a JVM application credentials for the emulators](howto/give-a-jvm-app-credentials-for-the-emulators.md)

### Is it safe to run the provisioner on every `up`, or twice at once?

Yes. A run on an unchanged config changes nothing, and two runs at the same time do not fail each
other.

Each resource is read first and only created or updated when it differs. When two runs both try
to create the same resource, the loser's "already exists" is treated as success. Measured against
the Pub/Sub emulator, two runs started together:

```console
$ gcp-emulator-provisioner --section=pubsub > c1.log & gcp-emulator-provisioner --section=pubsub > c2.log & wait
$ grep -h Creating c1.log c2.log
{"level":"INFO","msg":"Creating topic","topic":"payments"}
{"level":"INFO","msg":"Creating subscription","subscription":"payment-processor","topic":"payments"}
{"level":"INFO","msg":"Creating topic","topic":"payments"}
{"level":"INFO","msg":"Creating subscription","subscription":"payment-processor","topic":"payments"}
```

Both exit with code 0. One exception to "changes nothing": a `dead_letter` without
`max_delivery_attempts`, or a `retry` with only one of the two backoffs, is reported as changed on
every run, because the emulator fills in its own defaults. Set those values explicitly.

Further reading: [Route failed messages to a dead letter topic](howto/route-failed-messages-to-a-dead-letter-topic.md)

### Why do I see "field not supported by this Pub/Sub endpoint" warnings?

Because the Pub/Sub emulator refuses some updates that Google Cloud accepts, and the provisioner
reports them instead of failing the run.

The emulator rejects updates to `labels` on topics and subscriptions and to `expiration_policy`
(your `expiration_ttl`) on subscriptions; all three work when the resource is created. The other
fields in the same update are still applied. Because the live value never changes, the warning
repeats on every run until the config matches the resource. Against Google Cloud these updates
work. Measured against the emulator, changing `ack_deadline` and a label together:

```console
$ GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=pubsub
…
{"level":"INFO","msg":"Updating subscription","subscription":"order-processor","fields":["ack_deadline_seconds","labels"]}
{"level":"WARN","msg":"Subscription field not supported by this Pub/Sub endpoint, leaving it unchanged","subscription":"order-processor","field":"labels","error":"updating subscription \"order-processor\" (labels): rpc error: code = InvalidArgument desc = Invalid update_mask provided in the UpdateSubscriptionRequest: labels is not a known Subscription field. Note that field paths must be of the form 'push_config' rather than 'pushConfig'."}
```

The new ack deadline was applied. To get new labels locally, restart the emulator so the
subscription is created again.

Further reading: [Emulator Caveats](../README.md#emulator-caveats)

### Why don't my `location` or `storage_class` changes apply?

Because both are set when the bucket is created and never changed afterwards: Cloud Storage cannot
move a bucket.

Against real Cloud Storage, a difference is logged as a warning
(`Bucket attribute cannot be changed after creation, leaving it as is`) and the run continues.
Against fake-gcs-server the comparison is switched off, because the emulator reports
`US-CENTRAL1` / `STANDARD` for every bucket whatever it was created with; changing either field
there produces no log line at all. `STORAGE_EMULATOR_HOST` being set is what switches it off.

Further reading: [Change settings on existing resources](howto/change-settings-on-existing-resources.md),
[Emulator Caveats](../README.md#emulator-caveats)

## Emulators

### Why does a bucket with `versioning: true` fail?

Because fake-gcs-server implements versioning only on its in-memory backend, and yours runs on the
filesystem backend.

Only `versioning: true` fails; an omitted `versioning` leaves the bucket alone. The Compose files
in this repository start fake-gcs-server with `-backend memory`. If you switched to the filesystem
backend to keep objects across restarts, you have to choose between the two. Measured against
`fsouza/fake-gcs-server:1.56.1` with `-backend filesystem`:

```console
$ GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=storage
…
{"level":"INFO","msg":"Creating bucket","bucket":"documents"}
{"level":"ERROR","msg":"Provisioning failed","error":"provisioning bucket \"documents\": creating bucket \"documents\": retry failed with context deadline exceeded; last error: googleapi: Error 500: not implemented: fs storage type does not support versioning yet, internalError (fake-gcs-server only implements versioning on its in-memory backend: start it with -backend memory, or drop `versioning` from the bucket)"}
```

Further reading: [Turn bucket versioning on or off](howto/turn-bucket-versioning-on-or-off.md),
[Keep fake-gcs-server objects across restarts](howto/keep-objects-across-restarts.md)

### Why do bucket calls give up after 30 seconds?

Because every bucket call has a 30-second limit, which stops the Cloud Storage client from
retrying an emulator error forever.

fake-gcs-server answers requests it does not implement with a 500 error marked `internalError`,
which the Cloud Storage client library treats as temporary and keeps retrying. In the run in the
previous answer, `Creating bucket` was logged at 17:45:59 and the error at 17:46:29. If you see
`retry failed with context deadline exceeded`, read the `last error` part: it names the real
cause.

Further reading: [`internal/client/storage.go`](../internal/client/storage.go)

### Why do my storage notifications never arrive?

Usually because the notification topic does not exist, or because your fake-gcs-server is not
started with the notification arguments at all.

Check three things:

- The topic (`storage-notifications` by default) must be declared in your config, with a
  subscription on it, before the upload. If the topic is missing, the upload still answers `200`
  and the event is dropped.
- Only [`docker-compose.emulators.yaml`](../docker-compose.emulators.yaml) passes the `-event.*`
  arguments. The `gcs` service in the repository's own `docker-compose.yaml` does not publish
  notifications.
- Replacing the `gcs` `command` (for example to keep objects across restarts) replaces the whole
  argument list, so the `-event.*` arguments are gone unless you copy them in.

The only trace of a missing topic is in the `gcs` container log. Measured with
`fsouza/fake-gcs-server:1.56.1`:

```console
$ docker compose logs gcs
…
gcs-1  | time=2026-11-15T16:46:33.845Z level=INFO msg="error publishing event: rpc error: code = NotFound desc = Topic not found"
```

Further reading: [Receive Cloud Storage notifications locally](howto/receive-bucket-notifications-locally.md),
[Object notifications](../README.md#object-notifications)

### Why are my topics gone after the emulator restarts?

Because the Pub/Sub emulator keeps everything in memory, and a restart starts it empty.

The same holds for buckets and objects on fake-gcs-server's in-memory backend. Run the
provisioner again after the restart; in Compose it runs on every `up` anyway. Measured against
`google-cloud-cli:593.0.0-emulators`:

```console
$ curl -s http://localhost:8085/v1/projects/local-dev/topics | grep '"name"'
    "name": "projects/local-dev/topics/orders",
    "name": "projects/local-dev/topics/payments"
$ docker compose restart pubsub
$ curl -s http://localhost:8085/v1/projects/local-dev/topics
{
}
```

Further reading: [Keep fake-gcs-server objects across restarts](howto/keep-objects-across-restarts.md)

### Does `push_oidc_token` do anything locally?

It is stored, but it has no effect: the emulator does not validate the service account, so any
address is accepted.

OIDC tokens let Pub/Sub prove its identity to your push endpoint; that only
happens on Google Cloud. The provisioner calls no Identity and Access Management (IAM) API, so anything your service account
needs there is set up outside this tool. Read back from the Pub/Sub emulator after creating a
subscription with `push_oidc_token`:

```console
$ curl -s http://localhost:8085/v1/projects/local-dev/subscriptions/order-webhook
…
  "pushConfig": {
    "pushEndpoint": "http://consumer:8080/events",
    "oidcToken": {
      "serviceAccountEmail": "pusher@local-dev.iam.gserviceaccount.com"
    }
  },
```

Further reading: [Push subscriptions](../README.md#push-subscriptions),
[Deliver messages with a push subscription](howto/deliver-messages-with-a-push-subscription.md)

### Why is the first `docker compose up` so slow?

Because the Pub/Sub emulator image is roughly a gigabyte, and the first `up` has to pull it.

Later starts reuse the cached image. The `pubsub` healthcheck allows for the slow first start
with `start_period: 30s` and 40 retries 5 seconds apart, and the provisioner waits for
`service_healthy` before it runs. Check progress with `docker compose ps`, where `pubsub` shows
`(health: starting)` until it answers.

Further reading: [Emulator Caveats](../README.md#emulator-caveats),
[Add the GCP emulators to your Compose project](howto/add-emulators-to-your-compose-project.md)

### Why can't I set labels, lifecycle rules or CORS on a bucket?

Because fake-gcs-server accepts all three and then discards them, so the provisioner would see a
difference on every run and never get rid of it.

The config has no `labels`, `lifecycle` or `cors` for buckets. Since unknown keys are ignored, a
`labels:` block under a bucket still loads without an error; it does nothing. Manage these
settings for real Cloud Storage with your infrastructure tooling, such as Terraform.

Further reading: [Scope](../README.md#scope)

## Token stub

### Is the token stub safe to expose?

No. It hands a fixed access token to anyone who asks and does not check the signed request it
receives.

That is deliberate: nothing behind it checks tokens either. Keep it on the Compose network, or
publish its port only on `127.0.0.1`. Measured against a local stub on port 8099:

```console
$ curl -s -X POST http://localhost:8099/token
{"access_token":"local-emulator-token","token_type":"Bearer","expires_in":3600}
```

Further reading: [The token stub](../README.md#the-token-stub)

### Why is the token stub unhealthy although it serves tokens?

Because you moved it with `--addr`, while the healthcheck runs `/gcp-token-stub --ping` without
flags and therefore still probes port 8099.

Set the address with `TOKEN_STUB_ADDR` instead: the server and the probe both read it. Measured
with `command: ["--addr", ":9000"]`, from `docker inspect`:

```
{"level":"ERROR","msg":"Token stub is not answering","addr":":8099","error":"Get \"http://127.0.0.1:8099/token\": dial tcp 127.0.0.1:8099: connect: connection refused"}
```

The same container with `TOKEN_STUB_ADDR: ":9000"` and no `command` reports `(healthy)`.

Further reading: [The token stub](../README.md#the-token-stub)

### Why does the token stub exit immediately?

Usually because `TOKEN_STUB_EXPIRES_IN` is not a positive whole number of seconds, or because the
port is already taken.

Both end with exit code 1 and an `ERROR` line that names the cause. `--ping` and `--version` also
exit right away, by design.

```console
$ TOKEN_STUB_EXPIRES_IN=1h gcp-token-stub
{"level":"ERROR","msg":"Invalid TOKEN_STUB_EXPIRES_IN, want a positive number of seconds","value":"1h"}
$ TOKEN_STUB_ADDR=127.0.0.1:8099 gcp-token-stub   # a second stub on a port already in use
{"level":"INFO","msg":"Serving local OAuth2 token endpoint","version":"dev","addr":"127.0.0.1:8099","path":"/token","note":"static token, assertions are not verified — local development only"}
{"level":"ERROR","msg":"Token stub failed","error":"listen tcp 127.0.0.1:8099: bind: address already in use"}
```

Use `TOKEN_STUB_EXPIRES_IN: "3600"`, not `1h`.

Further reading: [`cmd/gcp-token-stub/main.go`](../cmd/gcp-token-stub/main.go)

### How do I health-check the token stub when the image has no shell?

Run the binary itself with `--ping`, in the exec form `CMD`, not `CMD-SHELL`.

`--ping` requests `/token` from the running stub and exits 0 if it answers with 200, 1 otherwise.
The image is built `FROM scratch`, so it contains no shell, `wget` or `curl`:

```yaml
healthcheck:
  test: ["CMD", "/gcp-token-stub", "--ping"]
```

A `CMD-SHELL` check never gets as far as your command. From `docker inspect`:

```
OCI runtime exec failed: exec failed: unable to start container process: exec: "/bin/sh": stat /bin/sh: no such file or directory
```

Further reading: [The token stub](../README.md#the-token-stub)

### Does the provisioner itself need the token stub?

No. The stub is for your applications, not for the provisioner.

The provisioner talks to the emulators without authentication, and to Google Cloud with
Application Default Credentials (ADC). The `token_uri` in a credential entry is only written into
the key file, for applications such as a Java Cloud Storage client that exchange a token before
every call.

Further reading: [Give a JVM application credentials for the emulators](howto/give-a-jvm-app-credentials-for-the-emulators.md)

## Operations

### What happens if an emulator isn't up yet?

The provisioner waits: it retries the connection with growing pauses for up to five minutes, then
fails with exit code 1.

For Cloud Storage, each attempt can take up to 30 seconds and logs a warning:

```console
$ STORAGE_EMULATOR_HOST=localhost:4443 GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner
…
{"level":"INFO","msg":"Connecting to Cloud Storage","project":"local-dev","emulator_host":"localhost:4443"}
{"level":"WARN","msg":"Cloud Storage not ready, retrying","attempt":1,"max_retries":15,"delay":1000000000,"error":"retry failed with context deadline exceeded; last error: Get \"http://localhost:4443/storage/v1/b?…\": dial tcp localhost:4443: connect: connection refused"}
```

For Pub/Sub, each attempt is bounded to 10 seconds, so the first warning appears about 10 seconds
after `Connecting to Pub/Sub`. Captured against a port where nothing listens, stopped with Ctrl-C
after 16 seconds (`time` trimmed):

```json
{"level":"WARN","msg":"Pub/Sub not ready, retrying","attempt":1,"max_retries":15,"delay":1000000000,"error":"context deadline exceeded"}
{"level":"ERROR","msg":"Provisioning failed","error":"connecting to Pub/Sub: connection cancelled after 16s: context canceled"}
```

A run that is cancelled says `connection cancelled after <elapsed>`; one that reaches the five
minute limit says `connection timeout after 5m0s`. Key files are written before
any connection is attempted, so they are not held up. In Compose, start the provisioner with
`depends_on: … condition: service_healthy` so it rarely has to wait; if an application needs its
key before the emulators are healthy, write it in a separate container with
`--section=credentials`.

Further reading: [Run one section per container](howto/run-one-section-per-container.md),
[Add the GCP emulators to your Compose project](howto/add-emulators-to-your-compose-project.md)

### Why does a run against real Google Cloud retry for five minutes and then fail?

Because a missing credential or permission looks the same as an outage to the connection check, so
it is retried like one.

Read the `error` field of the `not ready, retrying` warnings: `Unauthenticated` or
`PermissionDenied` means credentials or permissions, not a network problem. Below, ADC was not
available (no key file, no `gcloud` login, no reachable metadata server); this run made no request
to Google Cloud:

```console
$ GCP_PROJECT_ID=<project-id> GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=pubsub
…
{"level":"INFO","msg":"Connecting to Pub/Sub","project":"<project-id>","emulator_host":""}
{"level":"WARN","msg":"Pub/Sub not ready, retrying","attempt":1,"max_retries":15,"delay":1000000000,"error":"rpc error: code = Unauthenticated desc = transport: per-RPC creds failed due to error: credentials: cannot fetch token: Get \"http://…/computeMetadata/v1/instance/service-accounts/default/token?…\": dial tcp …: connect: connection refused"}
{"level":"WARN","msg":"Pub/Sub not ready, retrying","attempt":2,"max_retries":15,"delay":2000000000,"error":"rpc error: code = Unauthenticated desc = …"}
…
```

Also check that `PUBSUB_EMULATOR_HOST` and `STORAGE_EMULATOR_HOST` are really unset: an empty
value counts as unset, but a leftover emulator address sends the run to the emulator.

Further reading: [Provision resources in real Google Cloud](howto/provision-resources-in-real-google-cloud.md),
[Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials)

### What do the exit codes mean?

`0` means the run completed; `1` means it failed; `2` means a command-line flag was not
recognised.

`1` covers an invalid `--section`, an invalid `DRY_RUN`, a config that does not load or validate,
a connection that timed out, and any failed API call; the `ERROR` line says which. `--version` and
a successful dry-run also exit `0`. Code `2` comes from Go's flag parsing, before anything is
logged:

```console
$ gcp-emulator-provisioner --sections=storage
flag provided but not defined: -sections
Usage of gcp-emulator-provisioner:
…
$ echo $?
2
```

Further reading: [Exit Codes](../README.md#exit-codes)

### Which values does `DRY_RUN` accept?

`1`, `t`, `T`, `TRUE`, `true`, `True` for on, and `0`, `f`, `F`, `FALSE`, `false`, `False` for
off; anything else fails the run.

The value is parsed with Go's `strconv.ParseBool`. An empty `DRY_RUN` means off. The `--dry-run`
flag, when given, overrides the variable.

```console
$ DRY_RUN=yes gcp-emulator-provisioner --section=credentials
…
{"level":"ERROR","msg":"Invalid dry-run setting","error":"parsing DRY_RUN \"yes\" as a boolean: strconv.ParseBool: parsing \"yes\": invalid syntax"}
```

Further reading: [Environment Variables](../README.md#environment-variables)

### Does dry-run need the emulators running?

Yes, for any section that has resources: dry-run reads the live state so the preview shows real
differences, and only skips the changes.

Without a reachable endpoint, a dry-run waits and retries for up to five minutes like a normal
run. To check a config without any emulator, use `--section=credentials --dry-run`: it loads and
validates the whole file and connects to nothing.

```console
$ GCP_PROJECT_ID=local-dev GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner --section=credentials --dry-run
…
{"level":"INFO","msg":"Configuration loaded","section":"credentials","project":"local-dev","topics":1,"buckets":0,"credentials":0}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

Further reading: [Validate a config without emulators](howto/validate-a-config-without-emulators.md),
[Preview changes with dry-run](howto/preview-changes-with-dry-run.md)

### What happens with an invalid `LOG_LEVEL`?

The provisioner logs at `info` and says nothing about it.

Only the lowercase values `debug`, `info`, `warn` and `error` are recognised, so `WARN` or
`verbose` also mean `info`:

```console
$ LOG_LEVEL=WARN DRY_RUN=1 gcp-emulator-provisioner --section=credentials
{"level":"INFO","msg":"Starting gcp-emulator-provisioner","version":"dev"}
{"level":"INFO","msg":"Dry run: no changes will be applied"}
…
```

The token stub reads `LOG_LEVEL` the same way.

Further reading: [Environment Variables](../README.md#environment-variables)

### Can I provision Storage without a Pub/Sub emulator?

Yes. A section with no resources in the config never connects to its endpoint, so a config with
only buckets needs only fake-gcs-server.

`--section=storage` has the same effect for a config that also declares topics. The
`docker-compose.emulators.yaml` fragment still starts `pubsub`, because its `gcs` service depends
on it for notifications. Measured against fake-gcs-server alone, with `PUBSUB_EMULATOR_HOST`
unset:

```console
$ STORAGE_EMULATOR_HOST=localhost:4443 GCP_CONFIG_PATH=gcp-config.yaml gcp-emulator-provisioner
…
{"level":"INFO","msg":"Configuration loaded","section":"all","project":"local-dev","topics":0,"buckets":1,"credentials":0}
{"level":"INFO","msg":"Connecting to Cloud Storage","project":"local-dev","emulator_host":"localhost:4443"}
{"level":"INFO","msg":"Connected to Cloud Storage","project":"local-dev","emulator":true}
{"level":"INFO","msg":"Starting provisioning"}
{"level":"INFO","msg":"Provisioning Cloud Storage","buckets":1}
{"level":"INFO","msg":"Creating bucket","bucket":"uploads"}
{"level":"INFO","msg":"Provisioning complete"}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

Further reading: [Sections](../README.md#sections),
[Run one section per container](howto/run-one-section-per-container.md)
