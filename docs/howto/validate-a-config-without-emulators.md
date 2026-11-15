# Validate a config without emulators

After this you can check a provisioner config for the mistakes the provisioner itself detects —
in a pre-commit hook, a CI (continuous integration) job or your terminal — without starting the
Pub/Sub emulator, fake-gcs-server or anything else.

## When you need this

You changed the config and want to know it loads before you commit it, before a teammate runs
`docker compose up`, or before the config reaches real Google Cloud. Starting the emulators for
that costs time — the Pub/Sub emulator image alone is roughly a gigabyte — and a CI job that only
reviews a pull request should not need Docker-in-Docker for it.

## What you need to know first

- **The whole config is validated on every run.** Before any section starts, the provisioner
  loads the file, expands `${VAR}` references and validates every topic, subscription, bucket and
  credential, whichever section you selected. The first problem stops the run with exit code `1`.
  See [Configuration](../../README.md#configuration) and [Exit Codes](../../README.md#exit-codes).
- **The `credentials` section connects to nothing.** It writes local service account key files
  and calls no API, so selecting it skips both the Pub/Sub and the Cloud Storage connection. See
  [Sections](../../README.md#sections).
- **Dry-run writes nothing.** With `--dry-run`, the credentials section only logs the key files
  it would write. Together, `--section=credentials --dry-run` loads and validates the config,
  connects to nothing and writes nothing. See
  [Preview changes with dry-run](preview-changes-with-dry-run.md).
- **The limits are Google Cloud's.** Durations and counts are checked against the ranges Google
  Cloud accepts, which are stricter than the emulator's. A config that passes here does not fail
  on those ranges in production. See [Limits](../../README.md#limits).

## Steps

### 1. Provide a project id

A project id is required even though nothing is provisioned. Either the config sets
`project_id`, or you pass `GCP_PROJECT_ID`. Without one the run fails after loading the config:

```json
{"level":"ERROR","msg":"Provisioning failed","error":"project id is required: set GCP_PROJECT_ID or project_id in the config file"}
```

`project_id` in the file is checked against Google Cloud's format (6–30 characters, lowercase
letters, digits and hyphens, starting with a letter). `GCP_PROJECT_ID` is not checked at all, so
pass the same value you use in the real run.

### 2. Run the credentials section in dry-run mode

With the binary on your machine:

```console
$ GCP_CONFIG_PATH=./config.yaml gcp-emulator-provisioner --section=credentials --dry-run
```

Or with the image, which reads `/config.yaml` by default, so the mount alone is enough:

```console
$ docker run --rm -v "$PWD/config.yaml:/config.yaml:ro" \
    ghcr.io/datarocks-ag/gcp-emulator-provisioner:latest \
    --section=credentials --dry-run
```

Leave `PUBSUB_EMULATOR_HOST` and `STORAGE_EMULATOR_HOST` unset; they are not read for this
section, and no Application Default Credentials (ADC) are needed either.

For [`config.example.yaml`](../../config.example.yaml) the output is the following (captured on
2026-11-15 with a local build of the binary and of the image, which print the same lines; `time`
removed from each line):

```json
{"level":"INFO","msg":"Starting gcp-emulator-provisioner","version":"dev"}
{"level":"INFO","msg":"Dry run: no changes will be applied"}
{"level":"INFO","msg":"Loading configuration","path":"./config.yaml"}
{"level":"INFO","msg":"Configuration loaded","section":"credentials","project":"local-dev","topics":3,"buckets":3,"credentials":1}
{"level":"INFO","msg":"Provisioning credentials","files":1}
{"level":"INFO","msg":"Would write credential file","path":"/secrets/fake-sa.json","account":"local-emulator@local-dev.iam.gserviceaccount.com","token_uri":"http://gcp-token-stub:8099/token"}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

`Configuration loaded` with your topic, bucket and credential counts, followed by `finished
successfully` and exit code `0`, means the config passed. The `Would write credential file` line
appears only if the config has a `credentials` entry whose file does not exist on the machine
running the check; it is a preview and nothing is written.

### 3. Read a failure

A failing config ends in one `ERROR` line and exit code `1`. The error names the first offending
field by its path in the file, so `pubsub.topics[0].subscriptions[0]` is the first subscription
of the first topic, counting from zero.

A dead letter topic that is not declared under `topics`:

```yaml
project_id: local-dev

pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: order-processor
          dead_letter:
            topic: orders-dlq           # not declared under topics
            max_delivery_attempts: 5
```

```json
{"level":"INFO","msg":"Loading configuration","path":"./config.yaml"}
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[0].subscriptions[0].dead_letter.topic: \"orders-dlq\" is not declared in pubsub.topics (the provisioner only creates topics listed in the config)"}
```

Fix it by adding `- name: orders-dlq` under `topics`. The provisioner never creates a topic you
did not list, so without this check the real run would fail later with a bare `NotFound` from
Pub/Sub. See [Route failed messages to a dead letter topic](route-failed-messages-to-a-dead-letter-topic.md).

A duration outside the range Google Cloud accepts:

```yaml
project_id: local-dev

pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: order-processor
          message_retention: 60d        # Google Cloud accepts 10m-31d
```

```json
{"level":"INFO","msg":"Loading configuration","path":"./config.yaml"}
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[0].subscriptions[0].message_retention: \"60d\" is outside the range Google Cloud accepts (10m-31d)"}
```

The Pub/Sub emulator would accept `60d`; Google Cloud would not. Both failures exit with code `1`
(captured on 2026-11-15 with the binary and the image; `time` removed).

Two other failures you may see before validation starts — a missing file, and a YAML syntax
error, whose line number points into your file:

```json
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: reading config file: open ./config.yaml: no such file or directory"}
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: parsing config YAML: yaml: line 3: did not find expected '-' indicator"}
```

### 4. Run it in CI

The exit code is all a CI step needs. A step that runs the command from step 2 fails the job on
any of the errors above. Run it with the same environment variables your real run uses for
`${VAR}` references, because expansion happens before validation: a variable that is not set
stays in the value literally and is then validated as that text. A topic named
`orders-${TOPIC_SUFFIX}` with `TOPIC_SUFFIX` unset fails like this:

```json
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[0].name: invalid topic name \"orders-${TOPIC_SUFFIX}\" (must be 3-255 chars, start with a letter, and contain only letters, digits and -_.~+%)"}
```

A variable used in a field without a format check, such as a label value, passes with the literal
text in it.

## Check that it works

Break the config on purpose and confirm the check catches it. Change an `ack_deadline` to `5s`
and run step 2 again; the command exits with `1` and names the field:

```console
$ GCP_CONFIG_PATH=./config.yaml gcp-emulator-provisioner --section=credentials --dry-run; echo "exit=$?"
…
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[0].subscriptions[0].ack_deadline: \"5s\" is outside the range Google Cloud accepts (10s-600s)"}
exit=1
```

The exact path in the message depends on where the subscription sits in your file. Restore the
value and the command exits with `0` again.

## Pitfalls

- **Unknown keys are ignored, not rejected.** A misspelled field such as `ack_dedline: 5s` is
  dropped silently and the config passes, so the setting you meant to make is not made. Compare
  the field names against [`config.example.yaml`](../../config.example.yaml) when a value seems to
  have no effect.
- **`${VAR:-default}` is not supported.** Only `${VAR}` is expanded; in `${VAR:-default}` the whole
  `VAR:-default` is taken as the variable name, and the reference stays as written.
- **Only the first error is reported.** Fix it and run again to find the next one.
- **Leaving out `--dry-run` writes key files.** Without it, `--section=credentials` writes every
  configured key file that does not exist yet. In CI that is usually harmless but never wanted.
- **The check covers the file, not the endpoint.** It does not check a subscription `filter`'s
  syntax, whether a `push_endpoint` answers, whether a service account in `push_oidc_token`
  exists, or whether the identity of a real run is allowed to create anything. Those surface only
  in a real run, or in a dry run against the endpoint.
- **A bad `GCP_PROJECT_ID` passes.** Only `project_id` in the file is format-checked; an invalid
  value in the environment variable is accepted here and rejected later by Pub/Sub or Cloud
  Storage.

## Further reading

- [Limits](../../README.md#limits), [Configuration](../../README.md#configuration) and
  [Sections](../../README.md#sections) in the README.
- [`internal/config/config.go`](../../internal/config/config.go) for every validation rule and
  its error text.
- [`config.example.yaml`](../../config.example.yaml), the commented reference for every field.
- [Preview changes with dry-run](preview-changes-with-dry-run.md) to check a config against live
  resources once the endpoints are up.
