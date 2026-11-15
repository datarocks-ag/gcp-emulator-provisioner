# Run the provisioner as a Kubernetes Job

After this, a Kubernetes `Job` (or an init container) creates your topics, subscriptions and
buckets from a ConfigMap and finishes with a status Kubernetes can act on.

## When you need this

You run the Pub/Sub emulator and fake-gcs-server in a cluster — a preview environment, a
shared test namespace — or you provision real Google Cloud from inside the cluster, and the
resources must exist before your workloads start. Docker Compose's
`service_completed_successfully` is not available there, so a `Job` or an init container takes
its place.

## What you need to know first

- **One-shot.** The provisioner runs, reconciles every configured resource and exits. Exit
  code `0` means provisioning completed; `1` means an invalid section, a config that failed to
  load, a connection that never succeeded, or a failed API call. See
  [Exit Codes](../../README.md#exit-codes).
- **Connection retries.** Before provisioning, the provisioner waits for each endpoint it
  needs — Pub/Sub, then Cloud Storage — retrying with exponential backoff for up to five
  minutes per endpoint (`internal/client/client.go`). A pod that starts before the emulators
  are ready does not fail at once; one that can never connect fails after about five minutes.
- **Idempotent.** A second run creates nothing new and changes only drifted settings, so a
  `Job` that Kubernetes restarts, or an init container that runs on every pod start, is safe.
  Nothing is ever deleted.
- **Configuration.** The YAML config is read from `GCP_CONFIG_PATH` (default
  `./config.yaml`); emulator addresses come from `PUBSUB_EMULATOR_HOST` and
  `STORAGE_EMULATOR_HOST`. See [Environment Variables](../../README.md#environment-variables).

## Steps

The examples assume Services named `pubsub` (port 8085) and `gcs` (port 4443) in the same
namespace, and use `gcp-config.yaml` as your config file.

### 1. Pick an image tag

The release workflow (`.github/workflows/release.yaml`) publishes
`ghcr.io/datarocks-ag/gcp-emulator-provisioner` with these tags:

| Pushed | Tags |
|---|---|
| a `v<major>.<minor>.<patch>` git tag | `<major>.<minor>.<patch>` and `<major>.<minor>` (without the `v`), plus `sha-<short-sha>` |
| the `develop` branch | `develop`, plus `sha-<short-sha>` |

The workflow does not list `latest` itself. The README and Compose files use `:latest`, which
`docker/metadata-action`'s default `latest=auto` flavor adds on a release-tag push (the tag
exists in the registry as of 2026-11-15). Pin `<major>.<minor>` or a full version in a cluster,
so a new release does not change a running environment without a commit.

### 2. Put the config into a ConfigMap

```console
$ kubectl create configmap gcp-emulator-provisioner-config --from-file=config.yaml=./gcp-config.yaml
```

The key `config.yaml` becomes the file name when the ConfigMap is mounted, which is what
`GCP_CONFIG_PATH` points at in the next step.

### 3. Create the Job

This manifest extends the one in the README with an explicit retry budget:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: gcp-emulator-provisioner
spec:
  backoffLimit: 3
  activeDeadlineSeconds: 1800
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: gcp-emulator-provisioner
          image: ghcr.io/datarocks-ag/gcp-emulator-provisioner:<version>
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
              readOnly: true
      volumes:
        - name: config
          configMap:
            name: gcp-emulator-provisioner-config
```

Why these numbers:

- **`restartPolicy: Never`** makes every failed attempt a separate pod, so `kubectl logs`
  still shows the failed run. `OnFailure`, as in the README, restarts the container in the
  same pod instead; both work.
- **`backoffLimit`** counts failed attempts. The provisioner already retries a slow endpoint
  for up to five minutes, so more Kubernetes retries mostly repeat a failure that will not go
  away — a config error fails the same way every time.
- **`activeDeadlineSeconds`** caps the whole Job, across all attempts. One attempt can take up
  to five minutes per unreachable endpoint (about ten when both are), plus a 30-second limit
  on each bucket call, so a deadline shorter than that can kill an attempt that would have
  succeeded. Size it to `backoffLimit + 1` attempts.

Apply it:

```console
$ kubectl apply -f gcp-emulator-provisioner-job.yaml
```

### 4. Or run it as an init container

If one workload is the only consumer, run the provisioner as an init container on that
workload instead. The pod's main containers start only after it exits `0`:

```yaml
spec:
  template:
    spec:
      initContainers:
        - name: gcp-emulator-provisioner
          image: ghcr.io/datarocks-ag/gcp-emulator-provisioner:<version>
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
              readOnly: true
      containers:
        - name: my-service
          # …
      volumes:
        - name: config
          configMap:
            name: gcp-emulator-provisioner-config
```

It runs on every pod start, which is harmless because a run against provisioned resources
only reads. The emulators keep everything in memory, so an emulator restart loses every topic
and bucket; an init container re-creates them the next time your pod starts, a finished Job
does not.

### 5. Against real Google Cloud

Drop `PUBSUB_EMULATOR_HOST` and `STORAGE_EMULATOR_HOST`, set your real project id, and give
the pod a Kubernetes service account bound to a Google service account through Workload
Identity. [provision-resources-in-real-google-cloud.md](provision-resources-in-real-google-cloud.md)
covers the details.

## Check that it works

Wait for the Job and read its log:

```console
$ kubectl wait --for=condition=complete job/gcp-emulator-provisioner --timeout=15m
job.batch/gcp-emulator-provisioner condition met
$ kubectl logs job/gcp-emulator-provisioner
```

The Kubernetes side is not captured here. The container log is: this is the same image run
with `docker run`, the config mounted at `/config/config.yaml` and the same environment,
against `google-cloud-cli:593.0.0-emulators` and `fake-gcs-server:1.56.1` (`time` trimmed),
for a config with one topic `orders`, one subscription `order-processor` and one bucket
`documents`:

```json
{"time":"…","level":"INFO","msg":"Starting gcp-emulator-provisioner","version":"dev"}
{"time":"…","level":"INFO","msg":"Loading configuration","path":"/config/config.yaml"}
{"time":"…","level":"INFO","msg":"Configuration loaded","section":"all","project":"local-dev","topics":1,"buckets":1,"credentials":0}
{"time":"…","level":"INFO","msg":"Connecting to Pub/Sub","project":"local-dev","emulator_host":"pubsub:8085"}
{"time":"…","level":"INFO","msg":"Connected to Pub/Sub","project":"local-dev","emulator":true}
{"time":"…","level":"INFO","msg":"Connecting to Cloud Storage","project":"local-dev","emulator_host":"gcs:4443"}
{"time":"…","level":"INFO","msg":"Connected to Cloud Storage","project":"local-dev","emulator":true}
{"time":"…","level":"INFO","msg":"Starting provisioning"}
{"time":"…","level":"INFO","msg":"Provisioning Pub/Sub","topics":1,"subscriptions":1}
{"time":"…","level":"INFO","msg":"Creating topic","topic":"orders"}
{"time":"…","level":"INFO","msg":"Creating subscription","subscription":"order-processor","topic":"orders"}
{"time":"…","level":"INFO","msg":"Provisioning Cloud Storage","buckets":1}
{"time":"…","level":"INFO","msg":"Creating bucket","bucket":"documents"}
{"time":"…","level":"INFO","msg":"Provisioning complete"}
{"time":"…","level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

A released image logs its version instead of `dev`. On a second run the `Creating …` lines
disappear: an unchanged resource is logged at debug level only.

## Pitfalls

- **ConfigMap not mounted, or the key has another name.** The run fails at once with exit
  code `1` (captured):
  `{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: reading config file: open /config/config.yaml: no such file or directory"}`.
  Check the `--from-file=config.yaml=…` key and the `mountPath`.
- **Unreachable emulator looks like a hang.** The pod stays `Running` for five minutes per
  unreachable endpoint before it fails. Captured with nothing listening at the address:
  - Cloud Storage logs `"msg":"Cloud Storage not ready, retrying"` warnings, the first after
    30 seconds and then every 30 to 60 seconds, with the cause in `error` (here
    `dial tcp 127.0.0.1:21097: connect: connection refused`). Read the first one instead of
    waiting.
  - Pub/Sub logs **nothing** after `Connecting to Pub/Sub` for the full five minutes, then
    `"msg":"Provisioning failed","error":"connecting to Pub/Sub: connection timeout after 5m0s: context deadline exceeded"`.
    A silent pod after that line means `PUBSUB_EMULATOR_HOST` points at nothing reachable.
- **A ConfigMap change does not re-run a finished Job.** Delete the Job and apply it again, or
  give each version a new Job name.
- **Pod security policies that require a non-root user.** The image declares no `USER`, so it
  runs as root by default. Pub/Sub and storage provisioning need no privileges: the same image
  ran successfully with `docker run --user 65532:65532 --read-only`, so a `securityContext`
  with `runAsNonRoot: true`, `runAsUser: 65532` and `readOnlyRootFilesystem: true` should work.
  The Kubernetes side of that is not verified here. A `credentials:` entry needs a writable
  volume (for example an `emptyDir`) at its path.
- **`latest` moves.** See step 1.

## Further reading

- [Kubernetes](../../README.md#kubernetes), [Environment Variables](../../README.md#environment-variables)
  and [Exit Codes](../../README.md#exit-codes) in the README.
- [`internal/client/client.go`](../../internal/client/client.go) for the retry budget.
- Kubernetes documentation:
  [Jobs](https://kubernetes.io/docs/concepts/workloads/controllers/job/) (`backoffLimit`,
  `activeDeadlineSeconds`) and
  [Init containers](https://kubernetes.io/docs/concepts/workloads/pods/init-containers/).
- [docker/metadata-action](https://github.com/docker/metadata-action) for how the image tags
  are derived.
- [run-one-section-per-container.md](run-one-section-per-container.md) when the emulators run
  in different namespaces.
