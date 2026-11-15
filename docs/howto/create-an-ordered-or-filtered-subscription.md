# Create an ordered or filtered subscription

After this you can declare a subscription that delivers messages in order per ordering key, or only
the messages whose attributes match a filter, and you know what happens when you change either
later.

## When you need this

A consumer must see the events for one customer or one order in the order they were published, or
a consumer only cares about some of the messages on a shared topic, for example only
`order.created` events. Both are subscription settings that Pub/Sub fixes when the subscription is
created, so it pays to get them right in the config before the first run against Google Cloud.

## What you need to know first

- **Message ordering.** With `enable_message_ordering: true`, Pub/Sub delivers messages that share
  an ordering key in the order they were published. The publisher sets the key on each message;
  messages without a key are not ordered. See
  [Order messages](https://cloud.google.com/pubsub/docs/ordering) in the Google Cloud docs.
- **Filter.** A `filter` is an expression over message attributes, such as
  `attributes.type = "order.created"`. Messages that do not match are acknowledged by Pub/Sub on the
  subscriber's behalf and never delivered. See
  [Filter messages](https://cloud.google.com/pubsub/docs/subscription-message-filter).
- **Fixed at creation.** `filter`, `enable_message_ordering` and the topic a subscription belongs
  to cannot be changed on an existing subscription, in Google Cloud or in the emulator. When the
  config disagrees with the live subscription, the provisioner logs a warning, leaves the field as
  it is, and still exits 0. See [Immutable Fields](../../README.md#immutable-fields).
- **Omitted means "leave it alone".** A subscription whose config does not mention `filter` or
  `enable_message_ordering` is not compared on them, so one created by hand with a filter does not
  warn on every run. See [Tri-state fields](../../README.md#tri-state-fields).

## Steps

### 1. Decide the values before the first run

Changing either setting later means deleting the subscription, and with it its backlog: messages
not yet acknowledged are lost. Settle the filter expression and the ordering choice first, ideally
against the emulator.

### 2. Declare the subscription

Set `enable_message_ordering`, `filter`, or both. The names are examples:

```yaml
project_id: local-dev

pubsub:
  topics:
    - name: orders
      subscriptions:
        - name: order-audit
          ack_deadline: 60s
          filter: 'attributes.type = "order.created"'
          enable_message_ordering: true
```

Quote the filter with single quotes in YAML, so the double quotes inside it reach Pub/Sub
unchanged. The provisioner does not check the expression; Pub/Sub does, when the subscription is
created (see [Pitfalls](#pitfalls) for how the emulator reports a broken one).

### 3. Run the provisioner

With the repository's Compose stack, run the Pub/Sub section:

```console
$ docker compose run --rm gcp-emulator-provisioner --section=pubsub
…
{"level":"INFO","msg":"Provisioning Pub/Sub","topics":1,"subscriptions":1}
{"level":"INFO","msg":"Creating topic","topic":"orders"}
{"level":"INFO","msg":"Creating subscription","subscription":"order-audit","topic":"orders"}
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

### 4. If you change them later

This is what a later change to a fixed field looks like. Suppose you edit the config from step 2
to:

```yaml
        - name: order-audit
          ack_deadline: 90s
          filter: 'attributes.type = "order.shipped"'
          enable_message_ordering: false
```

The next run applies the `ack_deadline` change, which can be updated, and warns about the two that
cannot:

```json
{"level":"WARN","msg":"Subscription field cannot be changed after creation, leaving it as is","subscription":"order-audit","field":"filter","current":"attributes.type = \"order.created\"","configured":"attributes.type = \"order.shipped\"","remedy":"delete the subscription by hand if the new value is required"}
{"level":"WARN","msg":"Subscription field cannot be changed after creation, leaving it as is","subscription":"order-audit","field":"enable_message_ordering","current":true,"configured":false,"remedy":"delete the subscription by hand if the new value is required"}
{"level":"INFO","msg":"Updating subscription","subscription":"order-audit","fields":["ack_deadline_seconds"]}
{"level":"INFO","msg":"gcp-emulator-provisioner finished successfully"}
```

Every later run repeats both warnings until the config and the subscription agree again.
`--dry-run` shows the same warnings, so a preview catches the change before it reaches Google Cloud.

Moving a subscription to another topic is fixed in the same way. Declaring `order-audit` under a
new topic `order-events` creates the topic and warns:

```json
{"level":"WARN","msg":"Subscription field cannot be changed after creation, leaving it as is","subscription":"order-audit","field":"topic","current":"orders","configured":"order-events","remedy":"delete the subscription by hand if the new value is required"}
```

To act on a warning you have two options:

- **Revert the config** to the live value. This is the safe choice when the change was a mistake.
- **Recreate the subscription.** Delete it by hand, then run the provisioner, which creates it with
  the new value. **Deleting a subscription discards its backlog and acknowledgement state**:
  messages it had not delivered or that were not yet acknowledged are lost, and messages published
  while it does not exist are never delivered to it. Do this in Google Cloud only when you accept
  that loss. Locally the Pub/Sub emulator keeps everything in memory, so restarting the emulator
  container (which drops every topic, subscription and message in it) and running the provisioner
  again gives you the new value.

All output in this step was captured against `google-cloud-cli:593.0.0-emulators`, with the
`time` field trimmed.

## Check that it works

With the config from step 2, look at the subscription as the emulator stores it. The emulator also
answers the Pub/Sub REST API on its port (8085 in the repository's Compose file):

```console
$ curl -s http://localhost:8085/v1/projects/local-dev/subscriptions/order-audit
{
  "name": "projects/local-dev/subscriptions/order-audit",
  "topic": "projects/local-dev/topics/orders",
  "pushConfig": {
  },
  "ackDeadlineSeconds": 60,
  "messageRetentionDuration": "604800s",
  "enableMessageOrdering": true,
  "filter": "attributes.type = \"order.created\""
}
```

Then publish one matching and one non-matching message with the same ordering key, and pull. Run
this against the local emulator only:

```console
$ curl -s -X POST http://localhost:8085/v1/projects/local-dev/topics/orders:publish \
    -H 'Content-Type: application/json' \
    -d '{"messages":[
          {"data":"YQ==","attributes":{"type":"order.created"},"orderingKey":"customer-7"},
          {"data":"Yg==","attributes":{"type":"order.shipped"},"orderingKey":"customer-7"}]}'
{
  "messageIds": ["1", "2"]
}
$ curl -s -X POST http://localhost:8085/v1/projects/local-dev/subscriptions/order-audit:pull \
    -H 'Content-Type: application/json' -d '{"maxMessages":10,"returnImmediately":true}'
{
  "receivedMessages": [{
    "ackId": "projects/local-dev/subscriptions/order-audit:1",
    "message": {
      "data": "YQ==",
      "attributes": {
        "type": "order.created"
      },
      "messageId": "1",
      "publishTime": "2026-11-15T16:46:57.990Z",
      "orderingKey": "customer-7"
    }
  }]
}
```

Only the `order.created` message is delivered; the filter dropped the other one.

## Pitfalls

- **The same warning on every run.** The config was changed after the subscription was created.
  The run still exits 0, so a pipeline does not notice; watch for `WARN` lines with
  `"remedy":"delete the subscription by hand if the new value is required"`.
- **The run hangs for about a minute, then fails with `context deadline exceeded`.** The filter
  has a syntax error. The emulator does not reject it cleanly: it answers with an internal error,
  the client retries until its deadline, and the subscription is not created. Measured with
  `filter: 'attributes.type = order.created'` (the value is not quoted):

  ```json
  {"level":"ERROR","msg":"Provisioning failed","error":"provisioning subscription \"order-badfilter\" on topic \"orders\": creating subscription \"order-badfilter\": context deadline exceeded"}
  ```

  The actual reason is only in the `pubsub` container's log (`docker compose logs pubsub`):

  ```text
  [pubsub] com.google.cloud.pubsub.testing.v1.FilterExpressionParser$ParserException: Unexpected token: 'order' at position 18: Expected a string literal.
  ```

- **Messages arrive out of order although ordering is on.** The publisher did not set an ordering
  key, or used different keys. Ordering only holds per key.
- **The filter matches nothing.** Filters compare message attributes, not the payload. A message
  whose type is only inside its JSON body never matches `attributes.type = …`.
- **Removing `filter` from the config does not remove the filter.** An omitted field is not
  compared, so the subscription keeps it, without a warning. Recreate the subscription if you need
  it gone.

## Further reading

- [Immutable Fields](../../README.md#immutable-fields) and
  [Tri-state fields](../../README.md#tri-state-fields) in the README.
- [config.example.yaml](../../config.example.yaml) for an ordered, filtered subscription next to
  the other subscription settings.
- [Preview changes with dry-run](preview-changes-with-dry-run.md) to see warnings before applying.
- [Order messages](https://cloud.google.com/pubsub/docs/ordering) and
  [Filter messages](https://cloud.google.com/pubsub/docs/subscription-message-filter) in the Google
  Cloud docs.
