# Run one section per container

After this you can run the Pub/Sub, Cloud Storage and credentials parts of one config as
separate provisioner containers, each needing only the endpoint it provisions.

## When you need this

Your Pub/Sub emulator and fake-gcs-server live in different Compose projects or namespaces,
so no single container can reach both. Or an application needs its service account key file
before any emulator is up. By default the provisioner runs every section and needs every
endpoint that has resources configured.

## What you need to know first

- **Sections.** The config has three independent parts: `pubsub` (topics and subscriptions),
  `storage` (buckets) and `credentials` (local key files). `--section` or the `GCP_SECTION`
  environment variable picks one of `pubsub`, `storage`, `credentials` or `all` (the
  default); the flag wins over the variable. The `--help` text lists only `pubsub`, `storage`
  and `all`, but `credentials` is accepted. See [Sections](../../README.md#sections).
- **Only the selected section connects.** A container started with `--section=pubsub`
  connects to Pub/Sub only and ignores `STORAGE_EMULATOR_HOST` — it never constructs a
  Cloud Storage client. The credentials section connects to nothing.
- **An empty section never connects.** If the selected section has no resources in the
  config, its client is never created and the run exits `0`
  (`cmd/gcp-emulator-provisioner/main.go`).
- **The whole config is always loaded and validated**, whichever section you pick, and a
  project id (`GCP_PROJECT_ID` or `project_id`) is required even for `credentials`.

## Steps

### 1. Keep one config for all sections

You do not split the config file: every container mounts the same file and provisions its own
part of it. This example is the one the captured output below uses:

```yaml
project_id: local-dev

credentials:
  - path: /secrets/fake-sa.json

pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: order-processor

storage:
  buckets:
    - name: documents
```

Dead letter topics must still be declared under `pubsub.topics` in the same file; validation
checks this for every section.

### 2. Run the Pub/Sub section next to the Pub/Sub emulator

In the stack that runs `pubsub`, set only `PUBSUB_EMULATOR_HOST`:

```yaml
services:
  gcp-provisioner-pubsub:
    image: ghcr.io/datarocks-ag/gcp-emulator-provisioner:latest
    command: ["--section=pubsub"]
    depends_on:
      pubsub: { condition: service_healthy }
    environment:
      GCP_PROJECT_ID: local-dev
      PUBSUB_EMULATOR_HOST: pubsub:8085
      GCP_CONFIG_PATH: /config.yaml
    volumes:
      - ./gcp-config.yaml:/config.yaml:ro
```

### 3. Run the storage section next to fake-gcs-server

In the stack that runs `gcs`, use `GCP_SECTION` (or the flag) and set only
`STORAGE_EMULATOR_HOST`:

```yaml
services:
  gcp-provisioner-storage:
    image: ghcr.io/datarocks-ag/gcp-emulator-provisioner:latest
    depends_on:
      gcs: { condition: service_healthy }
    environment:
      GCP_SECTION: storage
      GCP_PROJECT_ID: local-dev
      STORAGE_EMULATOR_HOST: gcs:4443
      GCP_CONFIG_PATH: /config.yaml
    volumes:
      - ./gcp-config.yaml:/config.yaml:ro
```

### 4. Write key files without any emulator

The credentials section touches no endpoint, so it needs no emulator host and no
`depends_on` on an emulator. Give it a volume to write into:

```yaml
services:
  gcp-credentials:
    image: ghcr.io/datarocks-ag/gcp-emulator-provisioner:latest
    command: ["--section=credentials"]
    environment:
      GCP_PROJECT_ID: local-dev
      GCP_CONFIG_PATH: /config.yaml
    volumes:
      - ./gcp-config.yaml:/config.yaml:ro
      - gcp-secrets:/secrets

volumes:
  gcp-secrets:
```

This is also the way to get a key file early in a single stack. A full run writes credentials
before it connects to anything, but a provisioner service that `depends_on` the emulators being
healthy does not start until they are. A credentials-only container has no such dependency.
[give-a-jvm-app-credentials-for-the-emulators.md](give-a-jvm-app-credentials-for-the-emulators.md)
builds a whole stack around it.

## Check that it works

Each container's log names its section in `Configuration loaded` and connects to one endpoint
at most. Captured with the binary from this repository against
`google-cloud-cli:593.0.0-emulators` and `fake-gcs-server:1.56.1` (`time` trimmed; host
ports differ from the Compose examples).

The Pub/Sub section connects to Pub/Sub only:

```json
{"time":"…","level":"INFO","msg":"Configuration loaded","section":"pubsub","project":"local-dev","topics":1,"buckets":1,"credentials":1}
{"time":"…","level":"INFO","msg":"Connecting to Pub/Sub","project":"local-dev","emulator_host":"127.0.0.1:21085"}
{"time":"…","level":"INFO","msg":"Connected to Pub/Sub","project":"local-dev","emulator":true}
{"time":"…","level":"INFO","msg":"Provisioning Pub/Sub","topics":1,"subscriptions":1}
{"time":"…","level":"INFO","msg":"Creating topic","topic":"orders"}
{"time":"…","level":"INFO","msg":"Creating subscription","subscription":"order-processor","topic":"orders"}
{"time":"…","level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

The storage section connects to Cloud Storage only:

```json
{"time":"…","level":"INFO","msg":"Configuration loaded","section":"storage","project":"local-dev","topics":1,"buckets":1,"credentials":1}
{"time":"…","level":"INFO","msg":"Connecting to Cloud Storage","project":"local-dev","emulator_host":"127.0.0.1:21443"}
{"time":"…","level":"INFO","msg":"Connected to Cloud Storage","project":"local-dev","emulator":true}
{"time":"…","level":"INFO","msg":"Provisioning Cloud Storage","buckets":1}
{"time":"…","level":"INFO","msg":"Creating bucket","bucket":"documents"}
{"time":"…","level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

The credentials section, with no emulator host set at all:

```json
{"time":"…","level":"INFO","msg":"Configuration loaded","section":"credentials","project":"local-dev","topics":1,"buckets":1,"credentials":1}
{"time":"…","level":"INFO","msg":"Provisioning credentials","files":1}
{"time":"…","level":"INFO","msg":"Wrote credential file","path":"/secrets/fake-sa.json","account":"local-emulator@local-dev.iam.gserviceaccount.com","token_uri":"https://oauth2.googleapis.com/token","replaced":false,"note":"generated locally, authenticates to nothing"}
{"time":"…","level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

`token_uri` is Google's endpoint here because the entry sets none. To see that an empty
section skips its endpoint, run `--section=pubsub` against a config without topics at
`LOG_LEVEL=debug`; the skip is logged at debug level only:

```json
{"time":"…","level":"INFO","msg":"Configuration loaded","section":"pubsub","project":"local-dev","topics":0,"buckets":1,"credentials":0}
{"time":"…","level":"DEBUG","msg":"No topics configured, skipping Pub/Sub section"}
{"time":"…","level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

## Pitfalls

- **A missing emulator host means Google Cloud.** If you leave `--section` at `all` in a
  stack that sets only `PUBSUB_EMULATOR_HOST`, the storage section connects to real Cloud
  Storage with Application Default Credentials (ADC). Without credentials it fails at once
  (captured on a workstation without ADC):

  ```json
  {"time":"…","level":"INFO","msg":"Connecting to Cloud Storage","project":"local-dev","emulator_host":""}
  {"time":"…","level":"ERROR","msg":"Provisioning failed","error":"connecting to Cloud Storage: creating storage client: dialing: credentials: could not find default credentials. See https://cloud.google.com/docs/authentication/external/set-up-adc for more information"}
  ```

  With ADC on your machine it would provision the real project instead. Always set a section
  when a container sees only one emulator.
- **A typo in the section** fails before the config is read, with exit code `1`:
  `{"level":"ERROR","msg":"Invalid section","section":"buckets","valid":["all","pubsub","storage","credentials"]}`.
- **Two containers writing the same key file.** If the credentials container and a full-run
  provisioner share a volume and both list the credential, make the full run wait for the
  credentials container (`service_completed_successfully`) so one writes and the other skips.

## Further reading

- [Sections](../../README.md#sections) and [CLI Flags](../../README.md#cli-flags) in the README.
- [Sections](../ARCHITECTURE.md#sections) in the architecture notes.
- [`cmd/gcp-emulator-provisioner/main.go`](../../cmd/gcp-emulator-provisioner/main.go) for
  how a section selects which clients are constructed.
- [validate-a-config-without-emulators.md](validate-a-config-without-emulators.md) for using
  the credentials section with `--dry-run` as a config check.
