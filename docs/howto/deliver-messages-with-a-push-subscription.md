# Deliver messages with a push subscription

After this you can declare a subscription that makes Pub/Sub send each message to your service as
an HTTP `POST`, choose the shape of that request, and see it arrive locally.

## When you need this

Your service should receive messages over HTTP instead of pulling them with a client library: a
webhook-style consumer, a Cloud Run service, or an application that has no Pub/Sub client at all.
You want the push subscription declared in the same config as everything else, working against the
emulator on your machine and against Google Cloud.

## What you need to know first

- **Push subscription.** A subscription with a `push_endpoint` is delivered by Pub/Sub calling that
  URL. A `2xx` response acknowledges the message; anything else counts as a failed delivery and is
  retried. See [Push subscriptions](https://cloud.google.com/pubsub/docs/push) in the Google Cloud
  docs.
- **Wrapper.** By default Pub/Sub wraps each message in a JSON envelope (`push_wrapper: pubsub`).
  With `push_wrapper: none` the request body is the raw message payload, and
  `push_write_metadata: true` adds the message metadata as `X-Goog-Pubsub-*` headers. See
  [Payload unwrapping](https://cloud.google.com/pubsub/docs/payload-unwrapping).
- **OIDC token.** With `push_oidc_token`, Pub/Sub in Google Cloud signs each request with an OpenID
  Connect (OIDC) token for the service account you name, so the endpoint can check who is calling.
  The emulator stores the setting but sends no token, so it has no effect locally. See
  [Authenticate push subscriptions](https://cloud.google.com/pubsub/docs/authenticate-push-subscriptions).
- **One `push_config`.** All push settings belong to one message in the Pub/Sub API, and an update
  replaces it as a whole. That matters for settings made outside the config, see
  [Pitfalls](#pitfalls) and [Push subscriptions](../../README.md#push-subscriptions) in the README.

## Steps

### 1. Declare the push subscription

Add `push_endpoint` to the subscription, plus the optional settings you need. The endpoint must be
reachable **from the emulator container**, so in Compose use the service name of your consumer
(`consumer` here is an example), not `localhost`.

```yaml
project_id: local-dev

pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: order-webhook
          ack_deadline: 10s
          push_endpoint: http://consumer:8080/events
          push_attributes:               # attributes of the push endpoint itself
            x-goog-version: v1
          push_wrapper: none             # "pubsub" (default envelope) | "none" (raw payload)
          push_write_metadata: true      # X-Goog-Pubsub-* headers; only with push_wrapper: none
          push_oidc_token:               # used by Google Cloud only
            service_account_email: pusher@local-dev.iam.gserviceaccount.com
            audience: http://consumer:8080   # optional, defaults to the push endpoint
```

What each optional field does when you leave it out:

| Field | Omitted |
|---|---|
| `push_attributes` | No attributes are added; attributes already on the subscription stay |
| `push_wrapper` | The current wrapper stays; a new subscription gets the `pubsub` envelope |
| `push_write_metadata` | `false`; only valid together with `push_wrapper: none` |
| `push_oidc_token` | Unauthenticated push; a token already on the subscription stays (until an update, see [Pitfalls](#pitfalls)) |
| `push_oidc_token.audience` | Defaults to the push endpoint URL |

In a real setup, put the service account address in an environment variable and reference it as
`${PUSH_SERVICE_ACCOUNT}`, as [config.example.yaml](../../config.example.yaml) does. Only the plain
`${VAR}` form is expanded.

### 2. Check the config before you run it

Every push setting requires `push_endpoint`, and the combinations are checked when the config
loads. A mistake fails with the path of the field and exit code 1. These are the messages, captured
from the current binary:

| Mistake | Error |
|---|---|
| `push_attributes` without `push_endpoint` | `pubsub.topics[0].subscriptions[0].push_attributes: requires push_endpoint to be set` |
| `push_wrapper: pubsub` together with `push_write_metadata: true` | `pubsub.topics[0].subscriptions[0].push_write_metadata: requires push_wrapper: "none"` |
| `push_wrapper: raw` | `pubsub.topics[0].subscriptions[0].push_wrapper: invalid push wrapper "raw" (must be "pubsub" or "none")` |
| `push_oidc_token` without `service_account_email` | `pubsub.topics[0].subscriptions[0].push_oidc_token.service_account_email: is required` |

`push_wrapper` and `push_oidc_token` without an endpoint are rejected the same way, with
`requires push_endpoint to be set`. Each message appears in the log prefixed with
`loading configuration: validating config: `.

### 3. Run the provisioner

With the repository's Compose stack, run the Pub/Sub section:

```console
$ docker compose run --rm gcp-emulator-provisioner --section=pubsub
…
{"level":"INFO","msg":"Provisioning Pub/Sub","topics":1,"subscriptions":1}
{"level":"INFO","msg":"Creating topic","topic":"orders"}
{"level":"INFO","msg":"Creating subscription","subscription":"order-webhook","topic":"orders"}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

A second run finds nothing to change:

```console
$ docker compose run --rm gcp-emulator-provisioner --section=pubsub
…
{"level":"INFO","msg":"Provisioning Pub/Sub","topics":1,"subscriptions":1}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

The output was captured against `google-cloud-cli:593.0.0-emulators`; the startup lines and the
`time` field are trimmed.

### 4. Change the push settings later, if you need to

Every push setting can be changed on an existing subscription. Switching the example to
`push_wrapper: pubsub` (and removing `push_write_metadata`, which only goes with `none`) updates the
one `push_config` field on the next run, and the run after it is quiet:

```json
{"level":"INFO","msg":"Updating subscription","subscription":"order-webhook","fields":["push_config"]}
```

Removing `push_endpoint` from the config does **not** turn the subscription back into a pull
subscription: an omitted setting is never compared, and the provisioner never removes anything.

## Check that it works

First look at the subscription as the emulator stores it. The emulator also answers the Pub/Sub
REST API on its port (8085 in the repository's Compose file):

```console
$ curl -s http://localhost:8085/v1/projects/local-dev/subscriptions/order-webhook
{
  "name": "projects/local-dev/subscriptions/order-webhook",
  "topic": "projects/local-dev/topics/orders",
  "pushConfig": {
    "pushEndpoint": "http://consumer:8080/events",
    "attributes": {
      "x-goog-version": "v1"
    },
    "oidcToken": {
      "serviceAccountEmail": "pusher@local-dev.iam.gserviceaccount.com",
      "audience": "http://consumer:8080"
    },
    "noWrapper": {
      "writeMetadata": true
    }
  },
  "ackDeadlineSeconds": 10,
  "messageRetentionDuration": "604800s"
}
```

Then publish a message, safe against the local emulator only:

```console
$ curl -s -X POST http://localhost:8085/v1/projects/local-dev/topics/orders:publish \
    -H 'Content-Type: application/json' \
    -d '{"messages":[{"data":"eyJvcmRlcl9pZCI6NDJ9","attributes":{"type":"order.created"}}]}'
{
  "messageIds": ["1"]
}
```

`eyJvcmRlcl9pZCI6NDJ9` is the base64 form of `{"order_id":42}`. Your consumer receives one request.
With `push_wrapper: none` and `push_write_metadata: true`, the emulator sent this (printed by a
listener that logs every request it gets; generic headers trimmed):

```text
POST /events
Content-Type: application/x-www-form-urlencoded
Type: order.created
X-Goog-Pubsub-Message-Id: 1
X-Goog-Pubsub-Publish-Time: 2026-11-15T16:46:16.367Z
X-Goog-Pubsub-Subscription-Name: projects/local-dev/subscriptions/order-webhook

{"order_id":42}
```

The body is the raw payload, the message attribute arrives as a header, and there is no
`Authorization` header: the emulator mints no OIDC token. With `push_wrapper: pubsub` the same
message arrives as the JSON envelope instead:

```text
POST /events
Content-Type: application/json

{"subscription":"projects\/local-dev\/subscriptions\/order-webhook","message":{"publishTime":"2026-11-15T16:46:27.131Z","data":"eyJvcmRlcl9pZCI6NDN9","publish_time":"2026-11-15T16:46:27.131Z","messageId":"2","attributes":{"type":"order.created"},"message_id":"2"}}
```

Both requests were captured from the emulator; Google Cloud's requests may differ in detail (for
example, the `Content-Type` and the extra `Authorization` header with `push_oidc_token`).

## Pitfalls

- **Nothing arrives at the consumer.** The endpoint is resolved by the emulator, not by your host.
  `http://localhost:8080` points at the emulator container itself; use the Compose service name. The
  emulator keeps retrying a failing endpoint, so the message arrives once the consumer is up.
- **Removing a push setting from the config does not remove it.** Attributes, a wrapper or an
  OIDC token that the config leaves out are carried across every update, the same way labels are.
  Measured against the emulator: a subscription created with `push_wrapper: none`,
  `push_write_metadata: true` and an OIDC token kept all three after a config that declared only a
  new `push_endpoint` was applied; the run after that logged `Subscription up to date`. To change
  one of them, declare the new value in the config.
- **The consumer works locally and gets `401` or `403` in Google Cloud.** The emulator sends no
  token, so authentication code on the consumer is never exercised locally. In Google Cloud, Pub/Sub
  also needs permission to create tokens for the service account; the provisioner manages no
  Identity and Access Management (IAM) settings, so grant that in your infrastructure code, as
  described in [Authenticate push subscriptions](https://cloud.google.com/pubsub/docs/authenticate-push-subscriptions).
  The roles are not verified in this repository.
- **The consumer cannot parse the body.** It expects the envelope and the subscription uses
  `push_wrapper: none`, or the other way round. Declare `push_wrapper` explicitly instead of relying
  on whatever the subscription already has.

## Further reading

- [Push subscriptions](../../README.md#push-subscriptions) in the README.
- [config.example.yaml](../../config.example.yaml) for a push subscription next to the other
  subscription settings.
- [internal/provisioner/subscriptions.go](../../internal/provisioner/subscriptions.go) for how the
  push settings are compared and merged.
- [Push subscriptions](https://cloud.google.com/pubsub/docs/push) and
  [Payload unwrapping](https://cloud.google.com/pubsub/docs/payload-unwrapping) in the Google Cloud
  docs.
