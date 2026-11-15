# Turn bucket versioning on or off

After this you can turn object versioning on or off for a Cloud Storage bucket from the config, or
deliberately leave it alone, and run the same config against fake-gcs-server and Google Cloud.

## When you need this

Your application relies on Cloud Storage keeping the previous version of an object when it is
overwritten or deleted, for example to restore a document, and you want your local stack to behave
the same way. Or versioning was switched on for a bucket and you need it off again.

## What you need to know first

- **Object versioning.** On a bucket with versioning, overwriting or deleting an object keeps the
  old content as a noncurrent version with its own generation number. Turning versioning off
  (suspending it) stops new versions from being kept. See
  [Object Versioning](https://cloud.google.com/storage/docs/object-versioning) in the Google Cloud
  docs.
- **Three states, not two.** `versioning` is optional. `true` turns it on, `false` turns it off,
  and leaving the field out tells the provisioner not to touch it, so versioning switched on by
  someone else is not silently suspended. See [Tri-state fields](../../README.md#tri-state-fields).
- **fake-gcs-server backends.** fake-gcs-server, the Cloud Storage emulator in the repository's
  Compose files, stores objects either in memory (`-backend memory`) or on disk (its default
  filesystem backend). Only the in-memory backend implements versioning. The repository's Compose
  files already start it with `-backend memory`. See
  [Emulator Caveats](../../README.md#emulator-caveats).
- **Strategy.** With `strategy: create` the provisioner creates a missing bucket with its settings,
  but skips an existing bucket entirely, including its versioning. The default, `update`, also
  reconciles existing buckets. See [Configuration](../../README.md#configuration).

## Steps

### 1. Make sure fake-gcs-server runs on the in-memory backend

Check the `gcs` service's `command` in your Compose file. It must contain `-backend` followed by
`memory`, as in the repository's [docker-compose.yaml](../../docker-compose.yaml):

```yaml
  gcs:
    image: fsouza/fake-gcs-server:1.56.1
    command: ["-scheme", "http", "-host", "0.0.0.0", "-port", "4443", "-backend", "memory", "-public-host", "gcs:4443"]
```

Without it, a bucket with `versioning: true` fails (see [Pitfalls](#pitfalls)). The price of the
in-memory backend is that objects are gone when the container restarts.

### 2. Declare versioning on each bucket

Set `versioning` where you want a definite state, and leave it out where you do not. The bucket
names are examples:

```yaml
project_id: local-dev

storage:
  buckets:
    - name: documents
      versioning: true       # keep every version of every object
    - name: uploads
      versioning: false      # actively suspend versioning
    - name: media-assets     # versioning omitted: whatever the bucket has stays
```

### 3. Run the provisioner

With the repository's Compose stack (`gcs` published on port 4443), run the storage section:

```console
$ docker compose run --rm gcp-emulator-provisioner --section=storage
…
{"level":"INFO","msg":"Provisioning Cloud Storage","buckets":3}
{"level":"INFO","msg":"Creating bucket","bucket":"documents"}
{"level":"INFO","msg":"Creating bucket","bucket":"uploads"}
{"level":"INFO","msg":"Creating bucket","bucket":"media-assets"}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

A second run finds nothing to change:

```console
$ docker compose run --rm gcp-emulator-provisioner --section=storage
…
{"level":"INFO","msg":"Provisioning Cloud Storage","buckets":3}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

### 4. Switch it later, if you need to

Versioning can be changed on an existing bucket. Swap the two values, `documents` to `false` and
`uploads` to `true`. A dry run shows the change against the live state first:

```console
$ docker compose run --rm gcp-emulator-provisioner --section=storage --dry-run
…
{"level":"INFO","msg":"Would set bucket versioning","bucket":"documents","current":true,"enabled":false}
{"level":"INFO","msg":"Would set bucket versioning","bucket":"uploads","current":false,"enabled":true}
…
```

The real run applies it, and the run after it is quiet again:

```console
$ docker compose run --rm gcp-emulator-provisioner --section=storage
…
{"level":"INFO","msg":"Provisioning Cloud Storage","buckets":3}
{"level":"INFO","msg":"Setting bucket versioning","bucket":"documents","enabled":false}
{"level":"INFO","msg":"Setting bucket versioning","bucket":"uploads","enabled":true}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

`media-assets` never appears: its config has no `versioning`, so it is not compared.

All output in these steps was captured against `fsouza/fake-gcs-server:1.56.1` on the in-memory
backend; the startup lines and the `time` field are trimmed.

## Check that it works

Ask fake-gcs-server for the bucket's versioning setting through the Cloud Storage JSON API
(`jq` only picks the field; safe against the local emulator):

```console
$ curl -s http://localhost:4443/storage/v1/b/documents | jq .versioning
{
  "enabled": true
}
```

Then upload the same object twice and list it with and without its old versions:

```console
$ for v in 1 2; do
    curl -s -X POST "http://localhost:4443/upload/storage/v1/b/documents/o?uploadType=media&name=report.txt" \
      -H 'Content-Type: text/plain' --data-binary "version $v" > /dev/null
  done
$ curl -s "http://localhost:4443/storage/v1/b/documents/o?versions=true" | jq -r '.items[] | "\(.name) \(.generation)"'
report.txt 1794761719992723
report.txt 1794761719984539
$ curl -s "http://localhost:4443/storage/v1/b/documents/o" | jq -r '.items[] | "\(.name) \(.generation)"'
report.txt 1794761719992723
```

Two generations exist, and only the newest is the current object. Your generation numbers differ.

## Pitfalls

- **The run fails after 30 seconds with `does not support versioning`.** fake-gcs-server runs on
  its filesystem backend. The run stops at the first bucket with `versioning: true` and exits 1:

  ```json
  {"level":"ERROR","msg":"Provisioning failed","error":"provisioning bucket \"documents\": creating bucket \"documents\": retry failed with context deadline exceeded; last error: googleapi: Error 500: not implemented: fs storage type does not support versioning yet, internalError (fake-gcs-server only implements versioning on its in-memory backend: start it with -backend memory, or drop `versioning` from the bucket)"}
  ```

  The 30 seconds are the provisioner's limit for one bucket call: fake-gcs-server marks the error
  as retryable, so the client retries until then. Start fake-gcs-server with `-backend memory`, or
  remove `versioning: true`. Only `true` fails: `versioning: false` and an omitted field work on the
  filesystem backend too.
- **You need versioning and objects that survive a restart.** fake-gcs-server cannot do both: only
  the filesystem backend keeps objects, and only the in-memory backend versions them. Choose one
  per stack. If you switch to the filesystem backend by replacing the `gcs` service's `command`,
  that also drops the `-event.*` arguments (no more object notifications) and the `-public-host`
  default of the emulator fragment; see
  [Keep fake-gcs-server objects across restarts](keep-objects-across-restarts.md).
- **The config says `true`, but versioning stays off.** The bucket, or the whole config, has
  `strategy: create`, and the bucket already existed. The run logs
  `{"level":"INFO","msg":"Skipping existing bucket and its settings (strategy=create)","bucket":"documents"}`
  and changes nothing. Remove the `strategy` or set it to `update`.
- **Removing `versioning` from the config does not turn it off.** An omitted field leaves the
  bucket alone. Write `versioning: false` to suspend it.
- **`location` and `storage_class` are not changed either.** Both are applied only when the bucket
  is created; Cloud Storage cannot move a bucket. fake-gcs-server ignores them and always reports
  `US-CENTRAL1` and `STANDARD`, so the provisioner does not compare them when
  `STORAGE_EMULATOR_HOST` is set. Against real Cloud Storage, a difference produces this warning on
  every run (text from [internal/provisioner/buckets.go](../../internal/provisioner/buckets.go),
  not captured here):

  ```json
  {"level":"WARN","msg":"Bucket attribute cannot be changed after creation, leaving it as is","bucket":"<bucket>","field":"location","current":"<live value>","configured":"<config value>","remedy":"create a new bucket and migrate the objects if the new value is required"}
  ```

## Further reading

- [Tri-state fields](../../README.md#tri-state-fields) and
  [Emulator Caveats](../../README.md#emulator-caveats) in the README.
- [config.example.yaml](../../config.example.yaml) for bucket settings with comments.
- [Preview changes with dry-run](preview-changes-with-dry-run.md) and
  [Change settings on existing resources](change-settings-on-existing-resources.md).
- [Object Versioning](https://cloud.google.com/storage/docs/object-versioning) in the Google Cloud
  docs.
