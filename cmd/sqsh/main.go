// Command sqsh is the operator CLI. See SPEC.md for the full design; this
// build implements the walking-skeleton subset described in
// openspec/changes/add-walking-skeleton.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"sqsh/internal/awsx"
	"sqsh/internal/results"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := &cli{stdout: os.Stdout, stderr: os.Stderr, connect: connectAWS, now: time.Now, sleep: sleepCtx}
	os.Exit(c.run(ctx, os.Args[1:]))
}

// connectAWS builds the AWS-backed CLI backend from the default credential
// chain. AWS_ENDPOINT_URL is honoured, which is how tests reach LocalStack.
func connectAWS(ctx context.Context, o options, _ bool) (*backend, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if o.region != "" {
		opts = append(opts, awsconfig.WithRegion(o.region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("cannot load AWS configuration: %w", err)
	}
	stsClient := sts.NewFromConfig(cfg)
	return &backend{
		publisher: &awsx.SNSPublisher{Client: sns.NewFromConfig(cfg), TopicARN: o.topicARN},
		// Retention only affects writes; the CLI never writes.
		results: results.NewDynamo(dynamodb.NewFromConfig(cfg), o.resultsTable, 0),
		issuer: func(ctx context.Context) (string, error) {
			out, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
			if err != nil {
				return "", err
			}
			return aws.ToString(out.Arn), nil
		},
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
