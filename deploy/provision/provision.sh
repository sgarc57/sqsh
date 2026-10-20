#!/usr/bin/env bash
# Create the AWS resources for one sqsh host: the shared topic and results table,
# and the host's queue, DLQ, topic-scoped queue policy, and raw-delivery subscription
# filtered on the host. Re-running is a no-op when everything already matches.
#
# Anything that exists with different settings stops the script with an error. Nothing
# is overwritten or deleted here (design D2). The scripted equivalent of README "Manual
# AWS setup", without its hand-escaped JSON.
#
# Usage: deploy/provision/provision.sh <host-id>    (or HOST_ID in the environment)

source "$(dirname "$0")/../common.sh"

HOST_ID="${1:-${HOST_ID:-}}"
require_host_id "$HOST_ID"
need aws
need jq

QUEUE_NAME="sqsh-cmd-$HOST_ID"
DLQ_NAME="$QUEUE_NAME-dlq"

awsr() { aws --region "$REGION" "$@"; }

# Prints the queue URL, or nothing if the queue does not exist.
existing_queue_url() {
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

# Topic. CreateTopic with the same name and no attributes returns the existing topic.
TOPIC_ARN=$(awsr sns create-topic --name "$TOPIC_NAME" --query TopicArn --output text)
echo "topic: ok"

# Results table, shared by all hosts.
if out=$(awsr dynamodb describe-table --table-name "$RESULTS_TABLE" 2>&1); then
	echo "results table: exists"
elif [[ "$out" == *ResourceNotFoundException* ]]; then
	awsr dynamodb create-table --table-name "$RESULTS_TABLE" \
		--attribute-definitions AttributeName=request_id,AttributeType=S \
		--key-schema AttributeName=request_id,KeyType=HASH \
		--billing-mode PAY_PER_REQUEST >/dev/null
	awsr dynamodb wait table-exists --table-name "$RESULTS_TABLE"
	echo "results table: created"
else
	die "$out"
fi

ttl=$(awsr dynamodb describe-time-to-live --table-name "$RESULTS_TABLE" \
	--query TimeToLiveDescription.TimeToLiveStatus --output text)
case "$ttl" in
ENABLED | ENABLING)
	echo "results ttl: $ttl"
	;;
*)
	awsr dynamodb update-time-to-live --table-name "$RESULTS_TABLE" \
		--time-to-live-specification Enabled=true,AttributeName=ttl >/dev/null
	echo "results ttl: enabled"
	;;
esac

# DLQ. Rejected and repeatedly failing messages land here (SPEC §8.6).
DLQ_URL=$(existing_queue_url "$DLQ_NAME")
if [ -z "$DLQ_URL" ]; then
	DLQ_URL=$(awsr sqs create-queue --queue-name "$DLQ_NAME" --query QueueUrl --output text)
	echo "dlq: created"
else
	echo "dlq: exists"
fi
DLQ_ARN=$(awsr sqs get-queue-attributes --queue-url "$DLQ_URL" \
	--attribute-names QueueArn --query Attributes.QueueArn --output text)

# Command queue. VisibilityTimeout 90 satisfies visibility_timeout >= max_command_timeout + 30s
# with the default 60 s (design D4).
REDRIVE=$(jq -cn --arg a "$DLQ_ARN" '{deadLetterTargetArn: $a, maxReceiveCount: "3"}')
QUEUE_URL=$(existing_queue_url "$QUEUE_NAME")
if [ -z "$QUEUE_URL" ]; then
	QUEUE_URL=$(awsr sqs create-queue --queue-name "$QUEUE_NAME" \
		--attributes "$(jq -cn --arg r "$REDRIVE" '{VisibilityTimeout: "90", RedrivePolicy: $r}')" \
		--query QueueUrl --output text)
	echo "queue: created"
else
	cur=$(awsr sqs get-queue-attributes --queue-url "$QUEUE_URL" \
		--attribute-names VisibilityTimeout RedrivePolicy --output json)
	vt=$(jq -r '.Attributes.VisibilityTimeout // ""' <<<"$cur")
	redrive_ok=$(jq -r --arg a "$DLQ_ARN" \
		'((.Attributes.RedrivePolicy // "{}") | fromjson) as $r
		| ($r.deadLetterTargetArn == $a and ($r.maxReceiveCount | tostring) == "3")' <<<"$cur")
	[ "$vt" = 90 ] && [ "$redrive_ok" = true ] ||
		die "queue $QUEUE_NAME exists with different settings (VisibilityTimeout=$vt); fix or tear it down first"
	echo "queue: exists with expected settings"
fi
QUEUE_ARN=$(awsr sqs get-queue-attributes --queue-url "$QUEUE_URL" \
	--attribute-names QueueArn --query Attributes.QueueArn --output text)

# Queue policy: only this topic may deliver to the queue. Setting it again is harmless.
POLICY=$(jq -cn --arg q "$QUEUE_ARN" --arg t "$TOPIC_ARN" '{
	Version: "2012-10-17",
	Statement: [{
		Effect: "Allow",
		Principal: {Service: "sns.amazonaws.com"},
		Action: "sqs:SendMessage",
		Resource: $q,
		Condition: {ArnEquals: {"aws:SourceArn": $t}}
	}]
}')
awsr sqs set-queue-attributes --queue-url "$QUEUE_URL" \
	--attributes "$(jq -cn --arg p "$POLICY" '{Policy: $p}')"
echo "queue policy: set (topic-scoped)"

# Raw-delivery subscription filtered on this host. The filter is set at creation (design D3).
WANT_FILTER=$(jq -cn --arg h "$HOST_ID" '{host: [$h]}')
SUB_ARN=$(awsr sns list-subscriptions-by-topic --topic-arn "$TOPIC_ARN" --output json |
	jq -r --arg e "$QUEUE_ARN" '[.Subscriptions[] | select(.Protocol == "sqs" and .Endpoint == $e)][0].SubscriptionArn // empty')

if [ -n "$SUB_ARN" ]; then
	sub=$(awsr sns get-subscription-attributes --subscription-arn "$SUB_ARN" --output json)
	raw=$(jq -r '.Attributes.RawMessageDelivery // "false"' <<<"$sub")
	filter=$(jq -cS '(.Attributes.FilterPolicy // "{}") | fromjson' <<<"$sub")
	[ "$raw" = true ] && [ "$filter" = "$(jq -cS . <<<"$WANT_FILTER")" ] ||
		die "subscription exists with different attributes (RawMessageDelivery=$raw, FilterPolicy=$filter); fix or tear it down first"
	echo "subscription: exists with expected settings"
else
	SUB_ATTRS=$(jq -cn --arg f "$WANT_FILTER" '{RawMessageDelivery: "true", FilterPolicy: $f}')
	awsr sns subscribe --topic-arn "$TOPIC_ARN" --protocol sqs \
		--notification-endpoint "$QUEUE_ARN" --attributes "$SUB_ATTRS" >/dev/null
	echo "subscription: created"
fi

echo "done. Filter-policy changes can take up to a minute to take effect in SNS."
