# Verification record

## Task 9.7 — isolation test against real AWS

- **Date:** 2026-10-08
- **Account / region:** <account-id> / eu-central-1 (IAM user `<iam-user>`)
- **Command:** `SQSH_IT_REGION=eu-central-1 make integration-real-aws`
  (`TestIsolation`, `SQSH_IT_REAL_AWS=1`, fleet size 3)
- **Setup:** one SNS topic; three SQS queues, each with a topic-scoped queue policy and a
  raw-delivery subscription filtered on `{"host": ["webNN"]}`; one DynamoDB results table.
  The test waited 90 s after subscribing so the filter policies could propagate.
- **Result: PASS** (118.8 s). Two rounds of one command per host: every agent received
  exactly its own two requests, no agent recorded an `invalid_target` rejection,
  all three queues drained to zero, and a command published for an unsubscribed host
  (`web99`) reached no queue and produced no result.
- **Cleanup:** after the run, no `sqsh-it-*` topics, subscriptions, queues, or tables remained.

Conclusion: real SNS subscription filter policies route as SPEC.md §3 assumes. This
change's premise holds on the real service, not only on LocalStack.

## LocalStack (tasks 5.6, 9.1–9.5)

`make integration` against `localstack/localstack:4.9`, all passing:
`TestIsolation`, `TestEndToEnd`, `TestDuplicateDelivery` (sequential and concurrent),
`TestVisibilityExtension`, `TestDynamoConformance`.
