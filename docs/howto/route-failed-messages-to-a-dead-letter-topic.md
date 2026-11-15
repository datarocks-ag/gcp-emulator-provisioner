# Route failed messages to a dead letter topic

After this you can declare a subscription that hands a message to a dead letter topic after a fixed
number of failed deliveries, with a retry backoff in between, and see it happen against the local
Pub/Sub emulator.

## When you need this

Your consumer cannot process some messages: a malformed payload, a missing reference, a bug. Without
a dead letter topic, Pub/Sub redelivers such a message until it expires, and the consumer keeps
failing on it. You want those messages set aside after a few attempts, somewhere you can inspect
them, and you want the same setup locally and in Google Cloud.

## What you need to know first

- **Dead letter topic.** A subscription with a dead letter policy forwards a message to another
  topic once it has been delivered `max_delivery_attempts` times without being acknowledged. The
  dead letter topic is an ordinary topic; it needs its own subscription, or the forwarded messages
  are kept by nobody. See [Dead-letter topics](https://cloud.google.com/pubsub/docs/handling-failures)
  in the Google Cloud docs.
- **Retry policy.** Pub/Sub waits between redeliveries, starting at `minimum_backoff` and growing
  exponentially up to `maximum_backoff`. A negative acknowledgement (nack) or an expired ack
  deadline both count as a failed delivery.
- **The provisioner only creates what you declare.** It never creates a topic it was not asked for,
  so the dead letter topic must be listed under `pubsub.topics` itself. It may come later in the
  file: every topic is created before any subscription. See [Ordering](../../README.md#ordering).
- **Omitted values are filled in by Pub/Sub.** If you leave `max_delivery_attempts` or a backoff
  out, Pub/Sub stores its own default. The provisioner compares only the values you set, so the
  default is not reported as drift, and a later update keeps it.

## Steps

### 1. Declare the subscription and its dead letter topic

Add `dead_letter` and `retry` to the subscription, and declare the dead letter topic under
`pubsub.topics` with a subscription that reads it. The names are examples; use your own.

```yaml
project_id: local-dev

pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: order-processor
          ack_deadline: 30s
          dead_letter:
            topic: orders-dlq            # declared further down
            max_delivery_attempts: 5
          retry:
            minimum_backoff: 1s
            maximum_backoff: 60s

    - name: orders-dlq
      message_retention: 7d
      subscriptions:
        - name: orders-dlq-inspector     # something has to read the dead letters
          message_retention: 7d
          expiration_ttl: never
```

Setting all three values, `max_delivery_attempts`, `minimum_backoff` and `maximum_backoff`, makes
the policy explicit in the config. You can leave any of them out to accept Pub/Sub's default.

The values are checked when the config loads, against the ranges Google Cloud accepts, so a config
that passes locally does not fail later in Google Cloud:

| Field | Range |
|---|---|
| `dead_letter.max_delivery_attempts` | 5 – 100 |
| `retry.minimum_backoff`, `retry.maximum_backoff` | 0s – 600s, and minimum not above maximum |

`expiration_ttl: never` on the inspector subscription stops Pub/Sub from deleting it after a period
without activity, which a dead letter subscription that is rarely read would otherwise reach.

### 2. Check the config before you run it

A dead letter topic that is not declared, or a value outside the ranges above, fails at load with
the path of the offending field and exit code 1. Leaving out the `orders-dlq` entry gives:

```json
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[0].subscriptions[0].dead_letter.topic: \"orders-dlq\" is not declared in pubsub.topics (the provisioner only creates topics listed in the config)"}
```

`max_delivery_attempts: 3` gives:

```json
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[0].subscriptions[0].dead_letter.max_delivery_attempts: must be between 5 and 100, got 3"}
```

And `maximum_backoff: 5s` together with `minimum_backoff: 10s` gives:

```json
{"level":"ERROR","msg":"Provisioning failed","error":"loading configuration: validating config: pubsub.topics[0].subscriptions[0].retry: minimum_backoff \"10s\" must not exceed maximum_backoff \"5s\""}
```

To check a config without any emulator running, see
[Validate a config without emulators](validate-a-config-without-emulators.md).

### 3. Run the provisioner

With the repository's Compose stack (`pubsub` published on port 8085), run the Pub/Sub section:

```console
$ docker compose run --rm gcp-emulator-provisioner --section=pubsub
…
{"level":"INFO","msg":"Provisioning Pub/Sub","topics":2,"subscriptions":2}
{"level":"INFO","msg":"Creating topic","topic":"orders"}
{"level":"INFO","msg":"Creating topic","topic":"orders-dlq"}
{"level":"INFO","msg":"Creating subscription","subscription":"order-processor","topic":"orders"}
{"level":"INFO","msg":"Creating subscription","subscription":"orders-dlq-inspector","topic":"orders-dlq"}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

Run it a second time. Nothing differs from the config, so nothing is logged between the two lines:

```console
$ docker compose run --rm gcp-emulator-provisioner --section=pubsub
…
{"level":"INFO","msg":"Provisioning Pub/Sub","topics":2,"subscriptions":2}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

The output above was captured against `google-cloud-cli:593.0.0-emulators`; the startup lines and
the `time` field are trimmed.

### 4. Change the policy later, if you need to

Both policies can be changed on an existing subscription. Raise `max_delivery_attempts` to `10`,
and the next run updates only that policy; the run after it is quiet again:

```json
{"level":"INFO","msg":"Updating subscription","subscription":"order-processor","fields":["dead_letter_policy"]}
```

Removing `dead_letter` or `retry` from the config does **not** remove the policy from the
subscription. The provisioner only compares what the config declares, and never deletes.

## Check that it works

First look at the subscription as the emulator stores it. The emulator also answers the Pub/Sub
REST API on its port:

```console
$ curl -s http://localhost:8085/v1/projects/local-dev/subscriptions/order-processor
{
  "name": "projects/local-dev/subscriptions/order-processor",
  "topic": "projects/local-dev/topics/orders",
  "pushConfig": {
  },
  "ackDeadlineSeconds": 30,
  "messageRetentionDuration": "604800s",
  "deadLetterPolicy": {
    "deadLetterTopic": "projects/local-dev/topics/orders-dlq",
    "maxDeliveryAttempts": 5
  },
  "retryPolicy": {
    "minimumBackoff": "1s",
    "maximumBackoff": "60s"
  }
}
```

Then watch a message fail its way to the dead letter topic. This script, safe to run against the
local emulator only, publishes one message and rejects it each time it arrives (it needs `curl` and
`jq`):

```bash
PUBSUB=http://localhost:8085/v1/projects/local-dev

curl -s -X POST "$PUBSUB/topics/orders:publish" -H 'Content-Type: application/json' \
  -d '{"messages":[{"data":"b3JkZXItNDI="}]}'          # base64 of "order-42"

for attempt in 1 2 3 4 5 6; do
  sleep 3
  pulled=$(curl -s -X POST "$PUBSUB/subscriptions/order-processor:pull" \
    -H 'Content-Type: application/json' -d '{"maxMessages":1,"returnImmediately":true}')
  ack_id=$(echo "$pulled" | jq -r '.receivedMessages[0].ackId // empty')
  echo "pull $attempt: deliveryAttempt=$(echo "$pulled" | jq -r '.receivedMessages[0].deliveryAttempt // "none"')"
  [ -n "$ack_id" ] || continue
  # An ack deadline of 0 is a negative acknowledgement: deliver it again.
  curl -s -X POST "$PUBSUB/subscriptions/order-processor:modifyAckDeadline" \
    -H 'Content-Type: application/json' -d "{\"ackIds\":[\"$ack_id\"],\"ackDeadlineSeconds\":0}" > /dev/null
done
```

After five deliveries the message is gone from `order-processor`:

```text
{
  "messageIds": ["1"]
}
pull 1: deliveryAttempt=1
pull 2: deliveryAttempt=2
pull 3: deliveryAttempt=3
pull 4: deliveryAttempt=4
pull 5: deliveryAttempt=5
pull 6: deliveryAttempt=none
```

and waiting on the dead letter subscription, with attributes that say where it came from:

```console
$ curl -s -X POST http://localhost:8085/v1/projects/local-dev/subscriptions/orders-dlq-inspector:pull \
    -H 'Content-Type: application/json' -d '{"maxMessages":1,"returnImmediately":true}'
{
  "receivedMessages": [{
    "ackId": "projects/local-dev/subscriptions/orders-dlq-inspector:6",
    "message": {
      "data": "b3JkZXItNDI=",
      "attributes": {
        "CloudPubSubDeadLetterSourceDeliveryCount": "5",
        "CloudPubSubDeadLetterSourceSubscription": "order-processor",
        "CloudPubSubDeadLetterSourceTopicPublishTime": "2026-11-15T16:49:14.379Z",
        "CloudPubSubDeadLetterSourceSubscriptionProject": "local-dev"
      },
      "messageId": "2",
      "publishTime": "2026-11-15T16:49:32.607Z"
    }
  }]
}
```

The three-second pause between pulls fits the 1s–60s backoff in the example. With a larger
`minimum_backoff`, a pull may come back empty while the message waits out its backoff; raise the
`sleep` accordingly.

## Pitfalls

- **Removing a value does not reset it.** Deleting `max_delivery_attempts` or a backoff from the
  config leaves the live value as it is, the same as every other omitted field. To go back to
  Pub/Sub's default, write the default value explicitly (the emulator reports 5 attempts and a
  600s maximum backoff).
- **Dead lettered messages vanish.** The dead letter topic has no subscription. Pub/Sub only keeps
  a message for subscriptions that exist when it is published, so declare one, as
  `orders-dlq-inspector` above.
- **Nothing is ever dead lettered in Google Cloud.** In Google Cloud, forwarding needs the Pub/Sub
  service account to be allowed to publish to the dead letter topic and to subscribe to the source
  subscription. The provisioner manages no Identity and Access Management (IAM) permissions; grant
  them in your infrastructure code. The exact roles are in Google's
  [dead-letter topic docs](https://cloud.google.com/pubsub/docs/handling-failures) and are not
  verified in this repository.
- **The emulator lost everything.** The Pub/Sub emulator keeps topics, subscriptions and messages
  in memory only. After a restart, run the provisioner again; messages are gone for good.

## Further reading

- [Ordering](../../README.md#ordering) and [Limits](../../README.md#limits) in the README.
- [config.example.yaml](../../config.example.yaml) for a dead letter subscription next to the
  other subscription settings.
- [Change settings on existing resources](change-settings-on-existing-resources.md) for how updates
  are compared and applied.
- [Handling message failures](https://cloud.google.com/pubsub/docs/handling-failures) in the Google
  Cloud docs.
