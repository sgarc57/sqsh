// Package awsx wraps SNS and SQS behind the narrow interfaces the agent and
// CLI consume (SPEC.md §5), so no call site depends on the SDK directly.
package awsx

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// Routing attribute names (§6.1). The filter policy matches on Host.
const (
	AttrHost            = "host"
	AttrCommand         = "command"
	AttrProtocolVersion = "protocol_version"
)

// Routing holds the message attributes that drive subscription filter policies.
type Routing struct {
	Host            string
	Command         string
	ProtocolVersion int
}

// Message is one received SQS message.
type Message struct {
	ID            string
	ReceiptHandle string
	Body          string
	ReceiveCount  int
}

// Publisher publishes a request to the command topic.
type Publisher interface {
	Publish(ctx context.Context, body []byte, rt Routing) (messageID string, err error)
}

// Receiver long-polls a queue.
type Receiver interface {
	Receive(ctx context.Context, n int, wait time.Duration, visibility time.Duration) ([]Message, error)
}

// Deleter removes a handled message.
type Deleter interface {
	Delete(ctx context.Context, receiptHandle string) error
}

// VisibilityChanger extends or resets a message's visibility timeout.
type VisibilityChanger interface {
	ChangeVisibility(ctx context.Context, receiptHandle string, timeout time.Duration) error
}

// Queue is everything the agent needs from its one queue.
type Queue interface {
	Receiver
	Deleter
	VisibilityChanger
}

// SNSAPI is the subset of the SNS client used.
type SNSAPI interface {
	Publish(ctx context.Context, in *sns.PublishInput, opts ...func(*sns.Options)) (*sns.PublishOutput, error)
}

// SNSPublisher publishes to one topic.
type SNSPublisher struct {
	Client   SNSAPI
	TopicARN string
}

// Publish sends body verbatim with the routing attributes attached. Raw
// message delivery on each subscription keeps the SQS body identical to it.
func (p *SNSPublisher) Publish(ctx context.Context, body []byte, rt Routing) (string, error) {
	out, err := p.Client.Publish(ctx, &sns.PublishInput{
		TopicArn: aws.String(p.TopicARN),
		Message:  aws.String(string(body)),
		MessageAttributes: map[string]snstypes.MessageAttributeValue{
			AttrHost:            {DataType: aws.String("String"), StringValue: aws.String(rt.Host)},
			AttrCommand:         {DataType: aws.String("String"), StringValue: aws.String(rt.Command)},
			AttrProtocolVersion: {DataType: aws.String("Number"), StringValue: aws.String(strconv.Itoa(rt.ProtocolVersion))},
		},
	})
	if err != nil {
		return "", fmt.Errorf("sns publish: %w", err)
	}
	return aws.ToString(out.MessageId), nil
}

// SQSAPI is the subset of the SQS client used.
type SQSAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, opts ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
}

// SQSQueue is one SQS queue.
type SQSQueue struct {
	Client SQSAPI
	URL    string
}

// Receive long-polls for up to n (1..10) messages.
func (q *SQSQueue) Receive(ctx context.Context, n int, wait, visibility time.Duration) ([]Message, error) {
	out, err := q.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(q.URL),
		MaxNumberOfMessages:         int32(min(max(n, 1), 10)),
		WaitTimeSeconds:             int32(wait / time.Second),
		VisibilityTimeout:           int32(visibility / time.Second),
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{sqstypes.MessageSystemAttributeNameApproximateReceiveCount},
	})
	if err != nil {
		return nil, fmt.Errorf("sqs receive: %w", err)
	}
	msgs := make([]Message, 0, len(out.Messages))
	for _, m := range out.Messages {
		n, _ := strconv.Atoi(m.Attributes[string(sqstypes.MessageSystemAttributeNameApproximateReceiveCount)])
		msgs = append(msgs, Message{
			ID: aws.ToString(m.MessageId), ReceiptHandle: aws.ToString(m.ReceiptHandle),
			Body: aws.ToString(m.Body), ReceiveCount: n,
		})
	}
	return msgs, nil
}

func (q *SQSQueue) Delete(ctx context.Context, receiptHandle string) error {
	_, err := q.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(q.URL), ReceiptHandle: aws.String(receiptHandle)})
	if err != nil {
		return fmt.Errorf("sqs delete: %w", err)
	}
	return nil
}

func (q *SQSQueue) ChangeVisibility(ctx context.Context, receiptHandle string, timeout time.Duration) error {
	_, err := q.Client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(q.URL), ReceiptHandle: aws.String(receiptHandle),
		VisibilityTimeout: int32(timeout / time.Second),
	})
	if err != nil {
		return fmt.Errorf("sqs change visibility: %w", err)
	}
	return nil
}
