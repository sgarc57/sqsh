package results

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"sqsh/internal/protocol"
)

// DynamoAPI is the subset of the DynamoDB client the store uses.
type DynamoAPI interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

// Dynamo is the DynamoDB ResultStore. The table's partition key is the
// string attribute `request_id`, and TTL is enabled on the numeric `ttl`.
type Dynamo struct {
	Client    DynamoAPI
	Table     string
	Retention time.Duration
	Now       func() time.Time
}

// NewDynamo returns a store writing to table with the given retention.
func NewDynamo(client DynamoAPI, table string, retention time.Duration) *Dynamo {
	return &Dynamo{Client: client, Table: table, Retention: retention, Now: time.Now}
}

func (d *Dynamo) Claim(ctx context.Context, r protocol.Result) error {
	err := d.put(ctx, r, "attribute_not_exists(request_id)", nil)
	if isConditionFailed(err) {
		return ErrAlreadyClaimed
	}
	return err
}

func (d *Dynamo) Complete(ctx context.Context, r protocol.Result) error {
	if !CompleteAllowed(nil, r.Status) {
		return ErrStaleWrite
	}
	err := d.put(ctx, r, "attribute_not_exists(request_id) OR #status = :running",
		map[string]types.AttributeValue{":running": &types.AttributeValueMemberS{Value: string(protocol.StatusRunning)}})
	if isConditionFailed(err) {
		return ErrStaleWrite
	}
	return err
}

func (d *Dynamo) Get(ctx context.Context, id string) (protocol.Result, error) {
	out, err := d.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(d.Table),
		Key:            map[string]types.AttributeValue{"request_id": &types.AttributeValueMemberS{Value: id}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return protocol.Result{}, fmt.Errorf("results: get %s: %w", id, err)
	}
	if len(out.Item) == 0 {
		return protocol.Result{}, ErrNotFound
	}
	var r protocol.Result
	if err := attributevalue.UnmarshalMapWithOptions(out.Item, &r, func(o *attributevalue.DecoderOptions) { o.TagKey = "json" }); err != nil {
		return protocol.Result{}, fmt.Errorf("results: decode %s: %w", id, err)
	}
	return r, nil
}

func (d *Dynamo) put(ctx context.Context, r protocol.Result, cond string, values map[string]types.AttributeValue) error {
	r.TTL = d.Now().Add(d.Retention).Unix()
	item, err := attributevalue.MarshalMapWithOptions(r, func(o *attributevalue.EncoderOptions) { o.TagKey = "json" })
	if err != nil {
		return fmt.Errorf("results: encode %s: %w", r.RequestID, err)
	}
	in := &dynamodb.PutItemInput{
		TableName:                 aws.String(d.Table),
		Item:                      item,
		ConditionExpression:       aws.String(cond),
		ExpressionAttributeValues: values,
	}
	if values != nil {
		in.ExpressionAttributeNames = map[string]string{"#status": "status"}
	}
	if _, err := d.Client.PutItem(ctx, in); err != nil {
		if isConditionFailed(err) {
			return err
		}
		return fmt.Errorf("results: put %s: %w", r.RequestID, err)
	}
	return nil
}

func isConditionFailed(err error) bool {
	var ccf *types.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}
