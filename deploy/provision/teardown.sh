#!/usr/bin/env bash
# Remove the AWS resources that provision.sh created for one host.
#
# Removes this host's subscription, command queue, and DLQ. The topic is removed only when
# no subscriptions remain on it, since other hosts may share it. The results table is shared
# and holds history, so it is removed only with DROP_RESULTS=yes.
#
# Usage: CONFIRM=yes deploy/provision/teardown.sh <host-id>    (or HOST_ID in the environment)

source "$(dirname "$0")/../common.sh"

HOST_ID="${1:-${HOST_ID:-}}"
require_host_id "$HOST_ID"
need aws
need jq

[ "${CONFIRM:-}" = yes ] || die "this deletes queue, DLQ, and subscription for $HOST_ID; rerun with CONFIRM=yes"

QUEUE_NAME="sqsh-cmd-$HOST_ID"
DLQ_NAME="$QUEUE_NAME-dlq"

awsr() { aws --region "$REGION" "$@"; }

# Prints the URL of the named queue, or nothing if it does not exist.
queue_url() {
	local out
	if out=$(awsr sqs get-queue-url --queue-name "$1" --query QueueUrl --output text 2>&1); then
		echo "$out"
	elif [[ "$out" == *NonExistentQueue* ]]; then
		echo ""
	else
		die "$out"
	fi
}

echo "region: $REGION  host: $HOST_ID"

TOPIC_ARN=$(awsr sns create-topic --name "$TOPIC_NAME" --query TopicArn --output text)
QUEUE_URL=$(queue_url "$QUEUE_NAME")
DLQ_URL=$(queue_url "$DLQ_NAME")

# Subscriptions first, so the queues are no longer targets when they are deleted.
if [ -n "$QUEUE_URL" ]; then
	QUEUE_ARN=$(awsr sqs get-queue-attributes --queue-url "$QUEUE_URL" \
		--attribute-names QueueArn --query Attributes.QueueArn --output text)
	awsr sns list-subscriptions-by-topic --topic-arn "$TOPIC_ARN" --output json |
		jq -r --arg e "$QUEUE_ARN" '.Subscriptions[] | select(.Protocol == "sqs" and .Endpoint == $e) | .SubscriptionArn' |
		while read -r sub; do
			awsr sns unsubscribe --subscription-arn "$sub"
			echo "subscription: removed"
		done
fi

if [ -n "$QUEUE_URL" ]; then
	awsr sqs delete-queue --queue-url "$QUEUE_URL"
	echo "queue: removed"
else
	echo "queue: not found"
fi

if [ -n "$DLQ_URL" ]; then
	awsr sqs delete-queue --queue-url "$DLQ_URL"
	echo "dlq: removed"
else
	echo "dlq: not found"
fi

remaining=$(awsr sns list-subscriptions-by-topic --topic-arn "$TOPIC_ARN" --query 'length(Subscriptions)' --output text)
if [ "$remaining" = 0 ]; then
	awsr sns delete-topic --topic-arn "$TOPIC_ARN"
	echo "topic: removed"
else
	echo "topic: kept ($remaining other subscription(s) remain)"
fi

if [ "${DROP_RESULTS:-}" = yes ]; then
	if out=$(awsr dynamodb delete-table --table-name "$RESULTS_TABLE" 2>&1); then
		echo "results table: removed"
	elif [[ "$out" == *ResourceNotFoundException* ]]; then
		echo "results table: not found"
	else
		die "$out"
	fi
else
	echo "results table: kept (set DROP_RESULTS=yes to remove it and its history)"
fi
