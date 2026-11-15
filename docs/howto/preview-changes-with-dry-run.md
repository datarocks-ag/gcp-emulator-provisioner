# Preview changes with dry-run

After this you can see exactly which topics, subscriptions, buckets and key files a config change
would create or update, before anything is changed.

## When you need this

You have edited the provisioner config — a longer ack deadline, a new topic, versioning on a
bucket — and the stack it runs against already holds resources. Before you let the change loose,
especially against a shared environment or real Google Cloud, you want to see what the run would
do to the resources that exist today, not to an empty project.

## What you need to know first

- **Dry-run reads, but never writes.** In a dry run the provisioner still connects to each
  endpoint and reads every resource, so the preview compares the config with live state. Only
  the calls that create or change something are skipped and logged as a `Would …` line instead.
  See [Dry Run](../../README.md#dry-run) in the README.
- **A resource that does not exist yet shows only the create intent.** There are no live
  settings to compare, so the line names the resource and a few of its settings, not a diff.
- **Strategy.** Each resource is either `update` (create it, or bring an existing one in line
  with the config — the default) or `create` (create it if missing, otherwise leave it alone).
  A dry run follows the same strategy as a real run. See
  [Change settings on existing resources](change-settings-on-existing-resources.md).
- **Sections.** A run covers three sections: `credentials` (local key files), `pubsub` and
  `storage`. A section with nothing configured is skipped without connecting. See
  [Sections](../../README.md#sections).
- **Logs are JSON lines**, one object per line, written to standard output. `jq` or `grep`
  filters them well.

## Steps

The examples use a stack where this config was provisioned earlier:

```yaml
project_id: local-dev

pubsub:
  topics:
    - name: orders
      message_retention: 24h
      subscriptions:
        - name: order-processor
          ack_deadline: 30s
          dead_letter:
            topic: orders-dlq
            max_delivery_attempts: 5
    - name: orders-dlq

storage:
  buckets:
    - name: documents
```

### 1. Make your change in the config

Edit the config as you intend to ship it. This version changes three existing settings, turns on
versioning for an existing bucket, and adds a topic, a subscription and a bucket:

```yaml
project_id: local-dev

pubsub:
  topics:
    - name: orders
      message_retention: 48h            # was 24h
      subscriptions:
        - name: order-processor
          ack_deadline: 60s             # was 30s
          dead_letter:
            topic: orders-dlq
            max_delivery_attempts: 10   # was 5
    - name: orders-dlq
    - name: invoices                    # new topic
      message_retention: 7d
      subscriptions:
        - name: invoice-mailer          # new subscription
          ack_deadline: 20s

storage:
  buckets:
    - name: documents
      versioning: true                  # was omitted
    - name: archive                     # new bucket
```

### 2. Run the provisioner with dry-run turned on

Pass `--dry-run`, or set `DRY_RUN=true` in the environment — in Compose or a Kubernetes `Job`
the environment variable is usually easier than changing the command. Everything else stays as
in a normal run, including the emulator hosts, because the dry run needs the endpoints:

```console
$ GCP_CONFIG_PATH=./config.yaml \
  PUBSUB_EMULATOR_HOST=localhost:8085 \
  STORAGE_EMULATOR_HOST=http://localhost:4443 \
  gcp-emulator-provisioner --dry-run
```

`DRY_RUN` is parsed as a Go boolean (`strconv.ParseBool`): `1`, `t`, `T`, `TRUE`, `true`,
`True` turn it on; `0`, `f`, `F`, `FALSE`, `false`, `False` or an empty value turn it off. Any
other value, such as `yes`, stops the run before it loads the config:

```json
{"level":"ERROR","msg":"Invalid dry-run setting","error":"parsing DRY_RUN \"yes\" as a boolean: strconv.ParseBool: parsing \"yes\": invalid syntax"}
```

The process exits with code `1`. When you pass the flag, it wins over the environment variable
in either direction: `DRY_RUN=true` with `--dry-run=false` applies the changes.

### 3. Read the preview

This is the output for the change above, captured against the emulators
(`google-cloud-cli:593.0.0-emulators` and `fake-gcs-server:1.56.1`) on 2026-11-15. The `time`
field is removed from each line, and the config path and host ports are shown as in the command
above:

```json
{"level":"INFO","msg":"Starting gcp-emulator-provisioner","version":"dev"}
{"level":"INFO","msg":"Dry run: no changes will be applied"}
{"level":"INFO","msg":"Loading configuration","path":"./config.yaml"}
{"level":"INFO","msg":"Configuration loaded","section":"all","project":"local-dev","topics":3,"buckets":2,"credentials":0}
{"level":"INFO","msg":"Connecting to Pub/Sub","project":"local-dev","emulator_host":"localhost:8085"}
{"level":"INFO","msg":"Connected to Pub/Sub","project":"local-dev","emulator":true}
{"level":"INFO","msg":"Connecting to Cloud Storage","project":"local-dev","emulator_host":"http://localhost:4443"}
{"level":"INFO","msg":"Connected to Cloud Storage","project":"local-dev","emulator":true}
{"level":"INFO","msg":"Starting provisioning"}
{"level":"INFO","msg":"Provisioning Pub/Sub","topics":3,"subscriptions":2}
{"level":"INFO","msg":"Would update topic","topic":"orders","fields":["message_retention_duration"]}
{"level":"INFO","msg":"Would create topic","topic":"invoices","labels":0,"message_retention":604800000000000}
{"level":"INFO","msg":"Would update subscription","subscription":"order-processor","fields":["ack_deadline_seconds","dead_letter_policy"]}
{"level":"INFO","msg":"Would create subscription","subscription":"invoice-mailer","topic":"invoices","ack_deadline":20000000000,"push":false}
{"level":"INFO","msg":"Provisioning Cloud Storage","buckets":2}
{"level":"INFO","msg":"Would set bucket versioning","bucket":"documents","current":false,"enabled":true}
{"level":"INFO","msg":"Would create bucket","bucket":"archive","versioning":false,"location":"","storage_class":""}
{"level":"INFO","msg":"Provisioning complete"}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

Read it line by line:

- **`Would update topic` / `Would update subscription`** list the drifted settings in `fields`,
  using the Pub/Sub API names: `message_retention_duration`, `ack_deadline_seconds`,
  `dead_letter_policy`, and so on. These are exactly the fields a real run would send. A
  resource with nothing to change logs no `Would` line at all; at `LOG_LEVEL=debug` it logs
  `Topic up to date` or `Subscription up to date`.
- **`Would create topic` / `Would create subscription` / `Would create bucket`** are new
  resources. Durations in these lines are in nanoseconds — `604800000000000` is 7 days,
  `20000000000` is 20 seconds — and an empty `location` means the config did not set one.
- **`Would set bucket versioning`** shows the live value in `current` and the configured one in
  `enabled`.
- **Warnings still appear.** A field that cannot be changed after creation, such as a
  subscription's `filter`, is reported with the same `WARN` line as in a real run, because that
  check runs before the dry-run guard. See
  [Change settings on existing resources](change-settings-on-existing-resources.md).
- **`Provisioning complete` and exit code `0`** mean the preview ran, not that anything changed.

To pull out only the planned changes, filter on the message prefix:

```console
$ gcp-emulator-provisioner --dry-run | grep '"msg":"Would'
```

### 4. Preview the key files, if you have any

Key files are previewed the same way. With the `credentials` entry from
[`config.example.yaml`](../../config.example.yaml) and no file at `/secrets/fake-sa.json` yet, the
dry run reports the file it would write and the account it would contain:

```json
{"level":"INFO","msg":"Would write credential file","path":"/secrets/fake-sa.json","account":"local-emulator@local-dev.iam.gserviceaccount.com","token_uri":"http://gcp-token-stub:8099/token"}
```

If the file already exists, a key file is never rewritten, so you see the normal skip line
instead — `Skipping existing credential file (a key file is never rewritten unless
strategy=update)`. With `strategy: update` on the entry, the line becomes
`Would replace credential file`, which is your warning that a real run would generate a new
private key. The credentials section reads the local file system only, so it needs no endpoint;
[Validate a config without emulators](validate-a-config-without-emulators.md) builds on that.

### 5. Apply the change

When the preview shows what you expect, run again without `--dry-run` (or with `DRY_RUN` unset
or `false`). The real run reads live state again, so anything that changed in between is
taken into account.

## Check that it works

A dry run changes nothing, so the live resources still hold their old values. Against the Pub/Sub
emulator you can read a subscription over its REST interface on the same port:

```console
$ curl -s http://localhost:8085/v1/projects/local-dev/subscriptions/order-processor
{
  "name": "projects/local-dev/subscriptions/order-processor",
  "topic": "projects/local-dev/topics/orders",
  …
  "ackDeadlineSeconds": 30,
  …
}
```

A topic the dry run said it would create still returns `404`:

```console
$ curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8085/v1/projects/local-dev/topics/invoices
404
```

## Pitfalls

- **The endpoints must be up.** Because a dry run reads live state, it connects to Pub/Sub and
  Cloud Storage like a real run, and waits for them with the same retries — up to five minutes —
  before failing with `connecting to Pub/Sub: connection timeout after 5m0s: …`. The log sits at
  `Connecting to Pub/Sub` while it waits. To check only the config, without endpoints, use
  `--section=credentials --dry-run` as described in
  [Validate a config without emulators](validate-a-config-without-emulators.md).
- **A new resource shows no diff.** `Would create …` cannot say what would differ, because there
  is nothing to compare with. A subscription on a topic that is also new is listed as `Would
  create subscription` even though the topic does not exist yet; the real run creates every topic
  before any subscription.
- **Emulator update gaps are not previewed.** The Pub/Sub emulator refuses some updates that
  Google Cloud accepts (labels, and a subscription's `expiration_policy`). A dry run lists them
  under `fields` as if they would apply; only the real run discovers the refusal and logs a
  warning. See [Emulator Caveats](../../README.md#emulator-caveats).
- **`strategy: create` hides changes.** A resource under `create` that already exists is logged
  as `Skipping existing … (strategy=create)` with no diff, in a dry run as in a real one.

## Further reading

- [Dry Run](../../README.md#dry-run) and [CLI Flags](../../README.md#cli-flags) in the README.
- [Reconciling one resource](../ARCHITECTURE.md#reconciling-one-resource) in
  `docs/ARCHITECTURE.md`, for the read–compare–update flow a dry run stops short of applying.
- [`cmd/gcp-emulator-provisioner/main.go`](../../cmd/gcp-emulator-provisioner/main.go)
  (`resolveDryRun`) and [`internal/provisioner/`](../../internal/provisioner/) for each
  `Would …` message.
- [Change settings on existing resources](change-settings-on-existing-resources.md) for what a
  real update does once the preview looks right.
