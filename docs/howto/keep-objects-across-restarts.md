# Keep fake-gcs-server objects across restarts

After this the objects you put into fake-gcs-server are still there after
`docker compose down` and `up`, stored in a directory of your project.

## When you need this

You seed the local buckets with test data, or your application builds up objects you want to
keep between sessions, and losing them on every restart costs you time. The emulator
fragment starts fake-gcs-server on its in-memory backend, which forgets everything when the
container stops.

## What you need to know first

- **Storage backends.** fake-gcs-server keeps objects either in memory (`-backend memory`) or
  in files under `/storage` (`-backend filesystem`). Only the in-memory backend implements
  object versioning, which is why the fragment uses it. See
  [Persisting objects](../../README.md#persisting-objects).
- **What the switch costs.** On the filesystem backend a bucket configured with
  `versioning: true` fails to provision. Leaving `versioning` out of a bucket leaves it alone,
  and that works on both backends.
- **How Compose overrides `command`.** When your `compose.yaml` declares `command` for the
  included `gcs` service, Compose replaces the whole argument list rather than merging it.
  Every argument the fragment passed and you still want has to be repeated: the notification
  arguments (`-event.*`) and `-public-host`. Without `-public-host`, fake-gcs-server falls
  back to `storage.googleapis.com`.
- **Only objects persist.** The Pub/Sub emulator stays in memory, and the provisioner still
  re-creates topics and subscriptions on every `up`.

## Steps

The steps assume a Compose project that includes the emulator fragment, as set up in
[Add the GCP emulators to your Compose project](add-emulators-to-your-compose-project.md).

### 1. Remove `versioning: true` from your buckets

Drop the `versioning` field from every bucket in your provisioner config, or the run fails
(see [Pitfalls](#pitfalls)). The [`config.example.yaml`](../../config.example.yaml) bucket
`documents` is one that has to change:

```yaml
project_id: local-dev

storage:
  buckets:
    - name: documents
      # versioning: true   # not supported on the filesystem backend
```

### 2. Create the storage directory yourself

```console
$ mkdir -p run/mock-storage
```

If the directory is missing, Docker creates it owned by `root`. fake-gcs-server then runs as
your user (step 3) and cannot write into it, and every bucket creation fails.

### 3. Override the `gcs` service in your Compose file

Add `gcs` to the `services:` of your own `compose.yaml`, next to the `include:`. Compose
merges it into the included service: the image, ports, healthcheck and environment stay as
the fragment defines them, and `command`, `volumes` and `user` come from you.

```yaml
include:
  - path: gcp/docker-compose.emulators.yaml

services:
  gcs:
    command:
      - -scheme
      - http
      - -host
      - 0.0.0.0
      - -port
      - "4443"
      - -backend
      - filesystem
      # Repeated from the fragment: a replaced command keeps none of its arguments.
      - -public-host
      - ${STORAGE_EMULATOR_PUBLIC_HOST:-localhost:4443}
      - -event.pubsub-project-id
      - ${GCP_PROJECT_ID:-local-dev}
      - -event.pubsub-topic
      - ${STORAGE_NOTIFICATIONS_TOPIC:-storage-notifications}
      - -event.list
      - ${STORAGE_NOTIFICATIONS_EVENTS:-finalize}
    volumes:
      - ./run/mock-storage:/storage
    # Defaulted, because an unset variable collapses to ":" and the container
    # runs as root, leaving files your user cannot read on the next run.
    user: "${STORAGE_EMULATOR_USERID:-1000}:${STORAGE_EMULATOR_GROUPID:-1000}"
```

Set `STORAGE_EMULATOR_USERID` and `STORAGE_EMULATOR_GROUPID` in `.env` to the output of
`id -u` and `id -g` if they are not 1000; on macOS the user id is typically 501. Leave out the
six `-event.*` lines only if you do not use
[bucket notifications](receive-bucket-notifications-locally.md).

The merge was verified with Docker Compose 5.7.1. Check the result with
`docker compose config`, which prints the merged `gcs` service.

### 4. Keep the directory out of version control

Add `run/` to your `.gitignore`. The directory holds fake-gcs-server's own file layout, one
directory per bucket and a `<bucket>.bucketMetadata` file next to it, and is not meant to be
edited by hand.

## Check that it works

Start the stack, upload an object and restart. `<gcs-port>` is `STORAGE_EMULATOR_PORT`
(`4443` by default). Captured on 2026-11-15 against `fake-gcs-server:1.56.1`, output trimmed:

```console
$ docker compose up -d
$ curl -s -o /dev/null -w '%{http_code}\n' -X POST --data-binary 'hello' \
    'http://localhost:<gcs-port>/upload/storage/v1/b/uploads/o?uploadType=media&name=hello.txt'
200
$ find run
run
run/mock-storage
run/mock-storage/uploads.bucketMetadata
run/mock-storage/uploads
run/mock-storage/uploads/hello.txt
$ docker compose down
$ docker compose up -d
$ curl -s 'http://localhost:<gcs-port>/download/storage/v1/b/uploads/o/hello.txt?alt=media'
hello
```

The files belong to your user, so you can delete them without `sudo`. The provisioner finds
the bucket already there and creates nothing for it on the second run.

## Pitfalls

- **`versioning: true` fails after 30 seconds.** fake-gcs-server answers with a 500 that the
  Cloud Storage client treats as retryable, so the provisioner retries until its 30-second
  per-call limit and then fails with exit code 1:

  ```json
  {"time":"…","level":"ERROR","msg":"Provisioning failed","error":"provisioning bucket \"documents\": creating bucket \"documents\": retry failed with context deadline exceeded; last error: googleapi: Error 500: not implemented: fs storage type does not support versioning yet, internalError (fake-gcs-server only implements versioning on its in-memory backend: start it with -backend memory, or drop `versioning` from the bucket)"}
  ```

  Remove the field (step 1). If you need versioning locally, stay on the in-memory backend.
  [Turn bucket versioning on or off](turn-bucket-versioning-on-or-off.md) covers the field.
- **`permission denied` means the directory belongs to root.** When Docker created
  `run/mock-storage` (step 2 skipped) or an earlier run wrote it as root, the provisioner fails
  the same way, after 30 seconds:

  ```json
  {"time":"…","level":"ERROR","msg":"Provisioning failed","error":"provisioning bucket \"uploads\": creating bucket \"uploads\": retry failed with context deadline exceeded; last error: googleapi: Error 500: mkdir /storage/uploads: permission denied, internalError"}
  ```

  Remove the directory (it is root-owned, so with `sudo` or from a container), recreate it as
  your user, and start again.
- **Notifications stop after the override.** If `-event.*` is missing from your `command`,
  uploads succeed and no message is ever published, and the `gcs` log shows no
  `sent event` lines. Compare your `command` with the fragment's.
- **Signed URLs answer 404.** Without `-public-host`, fake-gcs-server expects the
  `storage.googleapis.com` host, and a path-style signed URL followed from your browser does
  not match it.
- **`docker compose down -v` does not reset the data.** `-v` removes named volumes, not a bind
  mount. Delete `run/mock-storage` to start from empty.
- **Relative paths in an included file resolve elsewhere.** Put the override in your own
  `compose.yaml` as shown. A relative volume path in an extra file listed under the
  fragment's `include: path:` resolves relative to the fragment's directory, not yours.

## Further reading

- [Persisting objects](../../README.md#persisting-objects) and
  [Emulator Caveats](../../README.md#emulator-caveats) in the README.
- [`docker-compose.emulators.yaml`](../../docker-compose.emulators.yaml) for the arguments
  your `command` replaces.
- [`internal/provisioner/buckets.go`](../../internal/provisioner/buckets.go) for the versioning
  error message.
- [fake-gcs-server](https://github.com/fsouza/fake-gcs-server) upstream, for its backends and
  flags.
