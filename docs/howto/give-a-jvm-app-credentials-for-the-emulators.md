# Give a JVM application credentials for the emulators

After this, your Java or Spring application (any application on the Java Virtual Machine, JVM) starts against the local Pub/Sub emulator and
fake-gcs-server with a generated service account key, and its token exchange is answered by a
local stub instead of Google.

## When you need this

Your JVM application refuses to start in the local stack. Typical causes are a missing or
unparseable `GOOGLE_APPLICATION_CREDENTIALS` at startup, or Cloud Storage calls that try to
reach `oauth2.googleapis.com` before they ever reach fake-gcs-server. Both happen even though
neither emulator checks credentials at all.

## What you need to know first

- **Why a placeholder key is not enough.** Spring Cloud GCP calls
  `GoogleCredentials.fromStream` at startup and parses the private key there and then, so the
  file must be a structurally valid service account key. The provisioner's `credentials:`
  section generates one: a real RSA keypair (Rivest–Shamir–Adleman) that Google holds no
  public key for, so it authenticates to nothing. See
  [Fake service account keys](../../README.md#fake-service-account-keys).
- **Why a token stub.** `google-cloud-storage` for Java has no `STORAGE_EMULATOR_HOST`
  equivalent. The application reaches fake-gcs-server through `spring.cloud.gcp.storage.host`
  while still holding real `ServiceAccountCredentials`, which sign a JSON Web Token (JWT) and
  exchange it for an OAuth 2.0 access token at the key file's `token_uri`. `gcp-token-stub`
  answers that exchange with a static token and does not verify the signature. See
  [The token stub](../../README.md#the-token-stub).
- **`token_uri` rewrites all four OAuth URLs.** Setting it on a credential entry points
  `auth_uri`, `token_uri`, `auth_provider_x509_cert_url` and `client_x509_cert_url` at the same
  address (`internal/credentials/credentials.go`).
- **A full run writes keys late.** Without `--section`, the provisioner connects to Pub/Sub and
  Cloud Storage first — each with up to five minutes of retries — and writes key files only
  after both answer (`cmd/gcp-emulator-provisioner/main.go`). `--section=credentials` writes
  them without contacting any endpoint. This guide therefore writes the key in its own
  container.

This is the order the services start in, and what each one waits for:

```mermaid
flowchart LR
    creds["gcp-credentials<br/>--section=credentials"] -->|completed| prov[gcp-emulator-provisioner]
    pubsub -->|healthy| prov
    gcs -->|healthy| prov
    creds -->|completed| app[my-service]
    stub[gcp-token-stub] -->|healthy| app
    prov -->|completed| app
```

## Steps

The steps extend a Compose project that already runs `pubsub` and `gcs`; if yours does not,
start with [add-emulators-to-your-compose-project.md](add-emulators-to-your-compose-project.md).
`my-service` and `gcp-config.yaml` stand for your own application and config file.

### 1. Declare the key in the provisioner config

Add a `credentials:` entry to `gcp-config.yaml`. The path is inside the container, on the
volume you share in step 3; `token_uri` names the stub by its Compose service name.

```yaml
project_id: local-dev

credentials:
  - path: /secrets/fake-sa.json
    token_uri: http://gcp-token-stub:8099/token
    # account: local-emulator      # optional, the default; becomes
    #                              # local-emulator@local-dev.iam.gserviceaccount.com

pubsub:
  topics:
    - name: orders

storage:
  buckets:
    - name: documents
```

Leave `strategy` off the entry. Credentials default to `create` and ignore the global
`strategy`, so the key is written once and never rotated under a running application.

### 2. Run the token stub

Add the stub as a long-lived service. Its image is built on `scratch`, which has no shell,
`curl` or `wget`, so the healthcheck runs the binary itself with `--ping`:

```yaml
services:
  gcp-token-stub:
    image: ghcr.io/datarocks-ag/gcp-token-stub:latest
    healthcheck:
      test: ["CMD", "/gcp-token-stub", "--ping"]
      interval: 5s
      timeout: 5s
      retries: 10
```

It listens on `:8099` by default. To move it, set `TOKEN_STUB_ADDR` rather than passing
`--addr`: `--ping` reads the same variable, so the probe follows the server.

### 3. Write the key in a credentials-only container

Run the provisioner a second time with `--section=credentials`, sharing a named volume with
the main provisioner. Make the main provisioner wait for it, so the two never write the same
file at once:

```yaml
  gcp-credentials:
    image: ghcr.io/datarocks-ag/gcp-emulator-provisioner:latest
    command: ["--section=credentials"]
    environment:
      GCP_PROJECT_ID: local-dev
      GCP_CONFIG_PATH: /config.yaml
    volumes:
      - ./gcp-config.yaml:/config.yaml:ro
      - gcp-secrets:/secrets

  gcp-emulator-provisioner:
    image: ghcr.io/datarocks-ag/gcp-emulator-provisioner:latest
    depends_on:
      gcp-credentials: { condition: service_completed_successfully }
      pubsub: { condition: service_healthy }
      gcs: { condition: service_healthy }
    environment:
      GCP_PROJECT_ID: local-dev
      PUBSUB_EMULATOR_HOST: pubsub:8085
      STORAGE_EMULATOR_HOST: gcs:4443
      GCP_CONFIG_PATH: /config.yaml
    volumes:
      - ./gcp-config.yaml:/config.yaml:ro
      - gcp-secrets:/secrets

volumes:
  gcp-secrets:
```

The main provisioner reads the same config, finds the file already there and skips it. The
image has no `USER`, so the key is owned by root; it is written `0644` in a `0755` directory
on purpose, so an application running under any other user ID can read it.

### 4. Point the application at the key, the stub and the emulators

Mount the volume read-only, set `GOOGLE_APPLICATION_CREDENTIALS`, and wait for the key, the
stub and the provisioned resources:

```yaml
  my-service:
    depends_on:
      gcp-credentials: { condition: service_completed_successfully }
      gcp-token-stub: { condition: service_healthy }
      gcp-emulator-provisioner: { condition: service_completed_successfully }
    environment:
      GOOGLE_APPLICATION_CREDENTIALS: /secrets/fake-sa.json
      PUBSUB_EMULATOR_HOST: pubsub:8085
      STORAGE_EMULATOR_HOST: gcs:4443
    volumes:
      - gcp-secrets:/secrets:ro
```

Drop the `gcp-emulator-provisioner` dependency only if the application must start before its
topics and buckets exist; the key no longer depends on it.

Then route Cloud Storage to fake-gcs-server with `spring.cloud.gcp.storage.host`, for example
as the environment variable `SPRING_CLOUD_GCP_STORAGE_HOST`. The value is the in-network
address of `gcs` (`http://gcs:4443`); this repository does not pin the exact format
Spring Cloud GCP expects, so check the Spring Cloud GCP documentation for your version. How
your Spring version picks up the Pub/Sub emulator is likewise documented there, not here.

## Check that it works

Start the stack and read the credentials container's log:

```console
$ docker compose up -d
$ docker compose logs gcp-credentials --no-log-prefix
```

Captured against `gcp-emulator-provisioner` and `gcp-token-stub` built from this repository
(`time` trimmed):

```json
{"time":"…","level":"INFO","msg":"Starting gcp-emulator-provisioner","version":"dev"}
{"time":"…","level":"INFO","msg":"Loading configuration","path":"/config.yaml"}
{"time":"…","level":"INFO","msg":"Configuration loaded","section":"credentials","project":"local-dev","topics":1,"buckets":1,"credentials":1}
{"time":"…","level":"INFO","msg":"Provisioning credentials","files":1}
{"time":"…","level":"INFO","msg":"Wrote credential file","path":"/secrets/fake-sa.json","account":"local-emulator@local-dev.iam.gserviceaccount.com","token_uri":"http://gcp-token-stub:8099/token","replaced":false,"note":"generated locally, authenticates to nothing"}
{"time":"…","level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

The main provisioner then logs
`"Skipping existing credential file (a key file is never rewritten unless strategy=update)"`.

Look at the key from inside the application container (`my-service` here; any container with
the volume works):

```console
$ docker compose exec my-service cat /secrets/fake-sa.json
```

```json
{
  "type": "service_account",
  "project_id": "local-dev",
  "private_key_id": "local-emulator-key",
  "private_key": "-----BEGIN PRIVATE KEY-----\n…\n-----END PRIVATE KEY-----\n",
  "client_email": "local-emulator@local-dev.iam.gserviceaccount.com",
  "client_id": "000000000000000000000",
  "auth_uri": "http://gcp-token-stub:8099/token",
  "token_uri": "http://gcp-token-stub:8099/token",
  "auth_provider_x509_cert_url": "http://gcp-token-stub:8099/token",
  "client_x509_cert_url": "http://gcp-token-stub:8099/token"
}
```

The private key is shortened to `…` here. `ls -l` on the file shows
`-rw-r--r-- 1 root root`.

Exchange a token the way the client library does. Publish the stub's port first
(`ports: ["127.0.0.1:8099:8099"]`), then from your workstation:

```console
$ curl -s -X POST -d 'grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer&assertion=x' http://127.0.0.1:8099/token
{"access_token":"local-emulator-token","token_type":"Bearer","expires_in":3600}
```

The assertion `x` is accepted because the stub verifies nothing. The JVM side — Spring
loading the key and calling the stub — is not captured in this repository; your
application's startup log is where it shows.

## Pitfalls

- **Key missing at application start.** If the application depends only on the main
  provisioner and an emulator never becomes healthy, no key is ever written: a full run
  writes credentials only after both emulators answer. Keep the credentials-only container.
- **The key changes on every run.** `strategy: update` on the credential entry regenerates
  the key on *every* run while it is set. Set it for one run, then remove it. Deleting the
  file (or the `gcp-secrets` volume) also produces a new key on the next run; restart
  applications that already loaded the old one.
- **Stub unhealthy but serving.** You set `--addr` in `command`; `--ping` still probes
  `:8099`. Use `TOKEN_STUB_ADDR` instead.
- **A shell healthcheck on the stub** (`CMD-SHELL`, `curl`, `wget`) always fails: the image
  has no shell.
- **`405 {"error":"invalid_request"}` from the stub** means a method other than `GET` or
  `POST` on `/token`; any other path answers `404 {"error":"not_found"}`.
- **Token requests go to Google.** The key was written before you added `token_uri` and was
  never rewritten. Remove the file or the volume and run again.
- **Never expose the stub.** It hands a token to anyone and checks no assertion — local
  development only.

## Further reading

- [Fake service account keys](../../README.md#fake-service-account-keys) and
  [The token stub](../../README.md#the-token-stub) in the README.
- [Why a fake key and a token stub exist at all](../ARCHITECTURE.md#why-a-fake-key-and-a-token-stub-exist-at-all)
  in the architecture notes.
- [`docker-compose.yaml`](../../docker-compose.yaml) for the commented application service
  and the stub's settings.
- [run-one-section-per-container.md](run-one-section-per-container.md) for `--section` in
  general.
- [Spring Cloud GCP reference documentation](https://googlecloudplatform.github.io/spring-cloud-gcp/)
  for the storage host and Pub/Sub emulator properties.
