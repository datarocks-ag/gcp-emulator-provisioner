# Provision resources in real Google Cloud

After this you can run the same config you use against the emulators against a real Google
Cloud project, authenticated with Application Default Credentials.

## When you need this

Your topics, subscriptions and buckets work locally, and now the same resources must exist in
a development, staging or production project. You want the config you tested against the
emulators to be the one you ship, instead of re-describing it in another tool.

## What you need to know first

- **The emulator variables decide the target.** With `PUBSUB_EMULATOR_HOST` set, the
  provisioner talks to the Pub/Sub emulator without credentials; unset, it talks to Google
  Cloud. `STORAGE_EMULATOR_HOST` does the same for Cloud Storage. Nothing else switches.
  See [Environment Variables](../../README.md#environment-variables).
- **Application Default Credentials (ADC).** Against Google Cloud, both clients find their
  credentials the standard way: `GOOGLE_APPLICATION_CREDENTIALS`, the credentials
  `gcloud auth application-default login` stores on a workstation, or the metadata server of
  the platform you run on — on GKE (Google Kubernetes Engine) through Workload Identity. See
  [How Application Default Credentials works](https://cloud.google.com/docs/authentication/application-default-credentials).
- **Additive only.** The provisioner creates missing resources and updates drifted settings.
  It never deletes a topic, subscription or bucket, and never removes labels or push
  attributes it does not manage. See [Features](../../README.md#features).
- **No IAM.** The provisioner grants nothing: no IAM (Identity and Access Management)
  bindings, no service accounts. The identity that runs it needs its permissions granted
  beforehand, for example in Terraform. See [Scope](../../README.md#scope).

## Steps

### 1. Unset both emulator variables

Remove `PUBSUB_EMULATOR_HOST` and `STORAGE_EMULATOR_HOST` from the environment entirely. A
container definition copied from your local stack still carries them; an empty value counts
as unset.

If one service should stay on an emulator, do not mix targets in one run: run each section in
its own container, as described in
[run-one-section-per-container.md](run-one-section-per-container.md).

### 2. Set the real project id

```console
$ export GCP_PROJECT_ID=<your-project-id>
```

`GCP_PROJECT_ID` overrides `project_id` in the config, so one config file can serve every
project. The variable is not validated; `project_id` in the file must match Google's rules
(6–30 characters, lowercase letters, digits and hyphens, starting with a letter).

### 3. Give the provisioner an identity

Pick one, depending on where it runs:

- **Workstation:** `gcloud auth application-default login`, then run the binary.
- **GKE:** bind the pod's Kubernetes service account to a Google service account with
  Workload Identity; the Job from
  [run-the-provisioner-as-a-kubernetes-job.md](run-the-provisioner-as-a-kubernetes-job.md)
  needs no other change. See
  [Workload Identity Federation for GKE](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity).
- **Elsewhere:** a real service account key file, mounted and named in
  `GOOGLE_APPLICATION_CREDENTIALS`.

Never point `GOOGLE_APPLICATION_CREDENTIALS` at a key from the `credentials:` section: that
key is generated locally and authenticates to nothing.

### 4. Grant the permissions behind each API call

These are the calls the provisioner makes (`internal/client/pubsub.go`,
`internal/client/storage.go`). The permission names are the ones Google documents for each
method; the mapping is **not verified** in this repository, so check it against Google's
references before granting.

| Call | When | Permission (not verified) |
|---|---|---|
| Pub/Sub `ListTopics` (page size 1) | connection check, every run with topics | `pubsub.topics.list` |
| Pub/Sub `GetTopic` | every configured topic | `pubsub.topics.get` |
| Pub/Sub `CreateTopic` | topic missing | `pubsub.topics.create` |
| Pub/Sub `UpdateTopic` | topic drifted | `pubsub.topics.update` |
| Pub/Sub `GetSubscription` | every configured subscription | `pubsub.subscriptions.get` |
| Pub/Sub `CreateSubscription` | subscription missing | `pubsub.subscriptions.create`, and `pubsub.topics.attachSubscription` on the topic |
| Pub/Sub `UpdateSubscription` | subscription drifted | `pubsub.subscriptions.update` |
| Cloud Storage list buckets (`buckets.list`) | connection check, every run with buckets | `storage.buckets.list` on the project |
| Cloud Storage get bucket (`buckets.get`) | every configured bucket | `storage.buckets.get` |
| Cloud Storage create bucket (`buckets.insert`) | bucket missing | `storage.buckets.create` |
| Cloud Storage update bucket (`buckets.patch`) | versioning drifted | `storage.buckets.update` |

Predefined roles such as Pub/Sub Editor and Storage Admin are commonly used for this, but
which role is the narrowest fit is not verified here. Note that the Cloud Storage connection
check lists buckets at project level, so a role granted on individual buckets is not enough.

Settings that make Pub/Sub act on your behalf — a dead letter topic, a push subscription with
an OIDC (OpenID Connect) token — also need grants to Pub/Sub's own service agent. The
provisioner sets the subscription fields only; it does not make those grants. See
[Dead-letter topics](https://cloud.google.com/pubsub/docs/handling-failures) and
[Authentication for push subscriptions](https://cloud.google.com/pubsub/docs/authenticate-push-subscriptions).

### 5. Preview, then run

Run a dry run first. It reads live state and logs every change it would make, without
applying any:

```console
$ GCP_CONFIG_PATH=./gcp-config.yaml gcp-emulator-provisioner --dry-run
```

Then run it without `--dry-run`. See
[preview-changes-with-dry-run.md](preview-changes-with-dry-run.md) for reading the preview.

## Check that it works

No output in this section is captured: this repository has no Google Cloud project to run
against. The lines below are what the code logs (`cmd/gcp-emulator-provisioner/main.go`,
`internal/client/`); `time` is trimmed and `<your-project-id>` stands for your project.

The connection lines show an empty `emulator_host` and `"emulator":false`, which is how you
tell that the run went to Google Cloud:

```json
{"time":"…","level":"INFO","msg":"Connecting to Pub/Sub","project":"<your-project-id>","emulator_host":""}
{"time":"…","level":"INFO","msg":"Connected to Pub/Sub","project":"<your-project-id>","emulator":false}
{"time":"…","level":"INFO","msg":"Connecting to Cloud Storage","project":"<your-project-id>","emulator_host":""}
{"time":"…","level":"INFO","msg":"Connected to Cloud Storage","project":"<your-project-id>","emulator":false}
```

Then the same `Creating topic`, `Creating subscription`, `Creating bucket` and
`Provisioning complete` lines as against the emulators. Confirm in the project:

```console
$ gcloud pubsub topics list --project=<your-project-id>
$ gcloud storage buckets list --project=<your-project-id>
```

## Pitfalls

- **No credentials fails at once.** Without ADC the client cannot be created and the run exits
  `1` immediately. This one is captured, on a workstation without ADC:

  ```json
  {"time":"…","level":"ERROR","msg":"Provisioning failed","error":"connecting to Cloud Storage: creating storage client: dialing: credentials: could not find default credentials. See https://cloud.google.com/docs/authentication/external/set-up-adc for more information"}
  ```

  Pub/Sub fails the same way, with `connecting to Pub/Sub: creating topic admin client: credentials: could not find default credentials. …`.
- **Missing permissions look like an outage.** Credentials that exist but lack a permission
  fail the connection check, and the provisioner retries it like an unreachable endpoint — for
  up to five minutes — logging `Pub/Sub not ready, retrying` or
  `Cloud Storage not ready, retrying` with the API's error in the `error` field. Read the
  first warning rather than waiting for the timeout. (Expected from the code, not captured.
  Each Pub/Sub attempt is bounded to 10 seconds, so even an error the client library keeps
  retrying internally surfaces as a warning with `"error":"context deadline exceeded"`.)
- **A leftover emulator variable** sends that service to the emulator address instead of
  Google Cloud: the connection line shows the address in `emulator_host`, and where no
  emulator answers the run fails after the five-minute retry budget. Check both connection
  lines.
- **Bucket names are global.** A bucket name that already belongs to another project fails at
  the first read with an error from `getting bucket "<name>"`, not with a create error. Choose
  project-specific names. (Expected, not captured.)
- **Location and storage class warnings.** Against real Cloud Storage only, a bucket whose
  `location` or `storage_class` differs from the config logs a warning and is left as it is,
  because neither can be changed after creation:
  `"msg":"Bucket attribute cannot be changed after creation, leaving it as is","bucket":"<name>","field":"location",…,"remedy":"create a new bucket and migrate the objects if the new value is required"`.
  Against fake-gcs-server this check is off, because it reports `US-CENTRAL1`/`STANDARD` for
  every bucket. (Expected from `internal/provisioner/buckets.go`, not captured.)
- **Limits are already Google's.** Duration ranges (for example `ack_deadline` 10s–600s) are
  checked at config load against Google Cloud's limits, so a config that loads locally will
  not be rejected for range here. See [Limits](../../README.md#limits).
- **Immutable fields.** A `filter` or `enable_message_ordering` that differs from an existing
  subscription is reported as a warning and not applied, exactly as locally. See
  [Immutable Fields](../../README.md#immutable-fields).
- **The `credentials:` section is for emulators only.** It still writes its local key file in
  a run against Google Cloud, which does no harm but serves no purpose; leave it out of
  production configs or run with `--section=pubsub` / `--section=storage`.

## Further reading

- [Environment Variables](../../README.md#environment-variables), [Scope](../../README.md#scope)
  and [Emulator Caveats](../../README.md#emulator-caveats) in the README.
- [One binary targets two very different endpoints](../ARCHITECTURE.md#one-binary-targets-two-very-different-endpoints)
  in the architecture notes.
- [`internal/client/pubsub.go`](../../internal/client/pubsub.go) and
  [`internal/client/storage.go`](../../internal/client/storage.go) for every API call.
- Google Cloud: [Pub/Sub access control](https://cloud.google.com/pubsub/docs/access-control)
  and [Cloud Storage IAM permissions](https://cloud.google.com/storage/docs/access-control/iam-permissions).
