//go:build integration

// Package integration runs sqsh against SNS, SQS, and DynamoDB: LocalStack by
// default, or a real AWS account when SQSH_IT_REAL_AWS=1 (task 9.7).
//
// Environment:
//
//	SQSH_IT_ENDPOINT   LocalStack endpoint (default http://localhost:4566)
//	SQSH_IT_REAL_AWS   1 = use the default AWS credential chain and no endpoint override
//	SQSH_IT_REGION     region (default eu-central-1)
//	SQSH_IT_SETTLE     wait after creating subscriptions (default 0s; 90s on real AWS,
//	                   since filter-policy changes take up to a minute to propagate)
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"sqsh/internal/agent"
	"sqsh/internal/awsx"
	"sqsh/internal/commands"
	"sqsh/internal/config"
	"sqsh/internal/dispatcher"
	"sqsh/internal/observability"
	"sqsh/internal/protocol"
	"sqsh/internal/results"
)

// FleetSize is the number of agents in the isolation test (design.md, open question).
const FleetSize = 3

func realAWS() bool { return os.Getenv("SQSH_IT_REAL_AWS") == "1" }

func region() string {
	if r := os.Getenv("SQSH_IT_REGION"); r != "" {
		return r
	}
	return "eu-central-1"
}

// env is one isolated set of resources: a topic, a queue and filtered
// subscription per host, and a results table. Names carry a random suffix so
// runs never collide, and everything is deleted on cleanup.
type env struct {
	t        *testing.T
	cfg      aws.Config
	sns      *sns.Client
	sqs      *sqs.Client
	ddb      *dynamodb.Client
	prefix   string
	topicARN string
	queues   map[string]string // host -> queue URL
	table    string
}

func newEnv(t *testing.T, hosts ...string) *env {
	t.Helper()
	ctx := context.Background()
	if !realAWS() {
		ep := os.Getenv("SQSH_IT_ENDPOINT")
		if ep == "" {
			ep = "http://localhost:4566"
		}
		t.Setenv("AWS_ENDPOINT_URL", ep)
		if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
			t.Setenv("AWS_ACCESS_KEY_ID", "test")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
		}
	}
	t.Setenv("AWS_REGION", region())
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region()))
	if err != nil {
		t.Fatal(err)
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	e := &env{t: t, cfg: cfg, sns: sns.NewFromConfig(cfg), sqs: sqs.NewFromConfig(cfg),
		ddb: dynamodb.NewFromConfig(cfg), prefix: "sqsh-it-" + hex.EncodeToString(suffix), queues: map[string]string{}}

	topic, err := e.sns.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String(e.prefix + "-commands")})
	if err != nil {
		t.Fatalf("create topic (is LocalStack running? `make localstack-up`): %v", err)
	}
	e.topicARN = aws.ToString(topic.TopicArn)
	t.Cleanup(func() {
		_, _ = e.sns.DeleteTopic(context.Background(), &sns.DeleteTopicInput{TopicArn: topic.TopicArn})
	})

	for _, h := range hosts {
		e.addHost(ctx, h)
	}

	e.table = e.prefix + "-results"
	_, err = e.ddb.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String(e.table),
		AttributeDefinitions: []ddbtypes.AttributeDefinition{{AttributeName: aws.String("request_id"), AttributeType: ddbtypes.ScalarAttributeTypeS}},
		KeySchema:            []ddbtypes.KeySchemaElement{{AttributeName: aws.String("request_id"), KeyType: ddbtypes.KeyTypeHash}},
		BillingMode:          ddbtypes.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.ddb.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(e.table)})
	})
	if err := dynamodb.NewTableExistsWaiter(e.ddb).Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(e.table)}, 2*time.Minute); err != nil {
		t.Fatalf("table not active: %v", err)
	}
	_, _ = e.ddb.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{TableName: aws.String(e.table),
		TimeToLiveSpecification: &ddbtypes.TimeToLiveSpecification{AttributeName: aws.String("ttl"), Enabled: aws.Bool(true)}})

	settle := time.Duration(0)
	if realAWS() {
		settle = 90 * time.Second
	}
	if s := os.Getenv("SQSH_IT_SETTLE"); s != "" {
		settle, _ = time.ParseDuration(s)
	}
	if settle > 0 {
		t.Logf("waiting %s for subscriptions and filter policies to propagate", settle)
		time.Sleep(settle)
	}
	return e
}

// addHost creates the queue, queue policy, and filtered raw-delivery
// subscription for one host, mirroring the manual setup in README.md.
func (e *env) addHost(ctx context.Context, host string) {
	t := e.t
	q, err := e.sqs.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(e.prefix + "-cmd-" + host)})
	if err != nil {
		t.Fatalf("create queue: %v", err)
	}
	url := aws.ToString(q.QueueUrl)
	e.queues[host] = url
	t.Cleanup(func() {
		_, _ = e.sqs.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(url)})
	})

	attrs, err := e.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	queueARN := attrs.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
	policy, _ := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect": "Allow", "Principal": map[string]any{"Service": "sns.amazonaws.com"},
			"Action": "sqs:SendMessage", "Resource": queueARN,
			"Condition": map[string]any{"ArnEquals": map[string]any{"aws:SourceArn": e.topicARN}},
		}},
	})
	if _, err := e.sqs.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{QueueUrl: aws.String(url),
		Attributes: map[string]string{"Policy": string(policy)}}); err != nil {
		t.Fatal(err)
	}
	filter, _ := json.Marshal(map[string][]string{"host": {host}})
	sub, err := e.sns.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: aws.String(e.topicARN), Protocol: aws.String("sqs"), Endpoint: aws.String(queueARN),
		Attributes:            map[string]string{"RawMessageDelivery": "true", "FilterPolicy": string(filter)},
		ReturnSubscriptionArn: true,
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.sns.Unsubscribe(context.Background(), &sns.UnsubscribeInput{SubscriptionArn: sub.SubscriptionArn})
	})
}

func (e *env) store() *results.Dynamo {
	return results.NewDynamo(e.ddb, e.table, time.Hour)
}

func (e *env) publisher() *awsx.SNSPublisher {
	return &awsx.SNSPublisher{Client: e.sns, TopicARN: e.topicARN}
}

// queueDepth returns visible plus in-flight messages on host's queue.
func (e *env) queueDepth(host string) int {
	out, err := e.sqs.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(e.queues[host]),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	var n int
	for _, v := range out.Attributes {
		var x int
		fmt.Sscan(v, &x)
		n += x
	}
	return n
}

// recordingQueue records every message an agent receives.
type recordingQueue struct {
	awsx.Queue
	mu       sync.Mutex
	received []awsx.Message
}

func (r *recordingQueue) Receive(ctx context.Context, n int, wait, vis time.Duration) ([]awsx.Message, error) {
	msgs, err := r.Queue.Receive(ctx, n, wait, vis)
	r.mu.Lock()
	r.received = append(r.received, msgs...)
	r.mu.Unlock()
	return msgs, err
}

func (r *recordingQueue) requestIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for _, m := range r.received {
		req, _ := protocol.Parse([]byte(m.Body))
		ids = append(ids, req.RequestID)
	}
	return ids
}

const stubScript = `#!/bin/sh
echo "$@" >> "%LOG%"
case "$4" in
  slow.service)   sleep 5 ;;
  broken.service) echo "unit is broken" >&2; exit 4 ;;
esac
echo "%HOST% active"
exit 0
`

// runningAgent is an in-process sqshd wired to real AWS clients.
type runningAgent struct {
	host    string
	queue   *recordingQueue
	metrics *observability.Metrics
	execLog string
	stop    func()
}

func (r *runningAgent) executions() int {
	b, _ := os.ReadFile(r.execLog)
	return strings.Count(string(b), "\n")
}

// startAgent runs an agent for host. mutate may adjust the config, including
// into states config validation forbids (the visibility test needs that).
func (e *env) startAgent(host string, mutate func(*config.Config)) *runningAgent {
	t := e.t
	dir := t.TempDir()
	execLog := filepath.Join(dir, "exec.log")
	stub := filepath.Join(dir, "systemctl")
	script := strings.NewReplacer("%LOG%", execLog, "%HOST%", host).Replace(stubScript)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		HostID: host, Region: region(), TopicARN: e.topicARN, QueueURL: e.queues[host], ResultsTable: e.table,
		WaitTimeSeconds: 2, AllowedCommands: []string{"systemctl"},
		Commands: config.Commands{Systemctl: config.SystemctlConfig{
			Verbs: []string{"status", "is-active"},
			Units: []string{"nginx.service", "slow.service", "broken.service"},
		}},
	}
	cfg.ApplyDefaults()
	if mutate != nil {
		mutate(cfg)
	}
	exec := &dispatcher.Executor{MaxOutputBytes: int(cfg.MaxOutputBytes), KillGrace: time.Second}
	reg := dispatcher.NewRegistry(cfg.AllowedCommands)
	reg.Register(&commands.Systemctl{Path: stub, Exec: exec}, true)
	reg.Register(&commands.Puppet{Exec: exec}, false)

	q := &recordingQueue{Queue: &awsx.SQSQueue{Client: e.sqs, URL: e.queues[host]}}
	m := observability.NewMetrics()
	var logw io.Writer = io.Discard
	if testing.Verbose() {
		logw = os.Stderr
	}
	a := agent.New(agent.Deps{Config: cfg, Queue: q, Store: e.store(), Registry: reg,
		Logger: observability.NewLogger(logw, "info"), Metrics: m, Version: "it"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	ra := &runningAgent{host: host, queue: q, metrics: m, execLog: execLog}
	var once sync.Once
	ra.stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(ra.stop)
	return ra
}

// request builds a valid v1 request body for host.
func request(t *testing.T, host string, args ...string) (protocol.Request, []byte) {
	t.Helper()
	id, err := protocol.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r := protocol.Request{Version: protocol.Version, RequestID: id, Target: protocol.Target{Host: host},
		Command: "systemctl", Args: args, IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute),
		TimeoutSeconds: 10, Issuer: "integration-test"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return r, b
}

func (e *env) publish(r protocol.Request, body []byte) {
	e.t.Helper()
	rt := awsx.Routing{Host: r.Target.Host, Command: r.Command, ProtocolVersion: protocol.Version}
	if _, err := e.publisher().Publish(context.Background(), body, rt); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) sendDirect(host string, body []byte) {
	e.t.Helper()
	if _, err := e.sqs.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(e.queues[host]), MessageBody: aws.String(string(body))}); err != nil {
		e.t.Fatal(err)
	}
}

// waitResult polls the store until id has a terminal result.
func (e *env) waitResult(id string, within time.Duration) protocol.Result {
	e.t.Helper()
	s := e.store()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		r, err := s.Get(context.Background(), id)
		if err == nil && r.Status.Terminal() {
			return r
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.t.Fatalf("no terminal result for %s within %s", id, within)
	return protocol.Result{}
}

func waitUntil(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
