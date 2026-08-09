package results_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"sqsh/internal/protocol"
	"sqsh/internal/results"
	"sqsh/internal/results/resultstest"
)

func TestMemoryConformance(t *testing.T) {
	resultstest.Run(t, func(*testing.T) results.ResultStore { return results.NewMemory(resultstest.Retention) })
}

// failingDynamo returns a fixed error for every call, to check the mapping of
// ConditionalCheckFailedException onto the sentinels.
type failingDynamo struct{ err error }

func (f failingDynamo) PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return nil, f.err
}
func (f failingDynamo) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{}, f.err
}

func TestDynamoErrorMapping(t *testing.T) {
	ctx := context.Background()
	ccf := &types.ConditionalCheckFailedException{Message: new(string)}
	r := protocol.Result{RequestID: "x", Status: protocol.StatusRunning}

	d := results.NewDynamo(failingDynamo{ccf}, "t", resultstest.Retention)
	if err := d.Claim(ctx, r); !errors.Is(err, results.ErrAlreadyClaimed) {
		t.Fatalf("claim: %v", err)
	}
	r.Status = protocol.StatusSucceeded
	if err := d.Complete(ctx, r); !errors.Is(err, results.ErrStaleWrite) {
		t.Fatalf("complete: %v", err)
	}

	other := errors.New("throttled")
	d = results.NewDynamo(failingDynamo{other}, "t", resultstest.Retention)
	if err := d.Claim(ctx, r); errors.Is(err, results.ErrAlreadyClaimed) || !errors.Is(err, other) {
		t.Fatalf("infrastructure errors must not look like duplicates: %v", err)
	}
	if _, err := d.Get(ctx, "x"); errors.Is(err, results.ErrNotFound) || !errors.Is(err, other) {
		t.Fatalf("a read failure must not look like a missing record: %v", err)
	}
	d = results.NewDynamo(failingDynamo{nil}, "t", resultstest.Retention)
	if _, err := d.Get(ctx, "x"); !errors.Is(err, results.ErrNotFound) {
		t.Fatalf("empty item must be ErrNotFound: %v", err)
	}
}
