package awsx_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"sqsh/internal/awsx"
	"sqsh/internal/awsx/awsxfake"
)

type recordingSNS struct{ in *sns.PublishInput }

func (r *recordingSNS) Publish(_ context.Context, in *sns.PublishInput, _ ...func(*sns.Options)) (*sns.PublishOutput, error) {
	r.in = in
	return &sns.PublishOutput{MessageId: aws.String("m-1")}, nil
}

func TestPublishAttachesRoutingAttributes(t *testing.T) {
	rec := &recordingSNS{}
	p := &awsx.SNSPublisher{Client: rec, TopicARN: "arn:aws:sns:eu-central-1:1:sqsh-commands"}
	body := []byte(`{"version":1}`)
	id, err := p.Publish(context.Background(), body, awsx.Routing{Host: "web01", Command: "systemctl", ProtocolVersion: 1})
	if err != nil || id != "m-1" {
		t.Fatal(id, err)
	}
	if aws.ToString(rec.in.Message) != string(body) {
		t.Fatalf("body must be sent verbatim, got %q", aws.ToString(rec.in.Message))
	}
	attrs := rec.in.MessageAttributes
	check := func(name, typ, val string) {
		a, ok := attrs[name]
		if !ok || aws.ToString(a.DataType) != typ || aws.ToString(a.StringValue) != val {
			t.Errorf("attribute %s = %+v, want %s %s", name, a, typ, val)
		}
	}
	check(awsx.AttrHost, "String", "web01")
	check(awsx.AttrCommand, "String", "systemctl")
	check(awsx.AttrProtocolVersion, "Number", "1")
}

type recordingSQS struct {
	recv *sqs.ReceiveMessageInput
}

func (r *recordingSQS) ReceiveMessage(_ context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	r.recv = in
	return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{{
		MessageId: aws.String("id"), ReceiptHandle: aws.String("rh"), Body: aws.String("b"),
		Attributes: map[string]string{"ApproximateReceiveCount": "3"},
	}}}, nil
}
func (r *recordingSQS) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	return &sqs.DeleteMessageOutput{}, nil
}
func (r *recordingSQS) ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func TestReceiveRequestsReceiveCountAndClampsBatch(t *testing.T) {
	rec := &recordingSQS{}
	q := &awsx.SQSQueue{Client: rec, URL: "u"}
	msgs, err := q.Receive(context.Background(), 25, 20*time.Second, 90*time.Second)
	if err != nil || len(msgs) != 1 || msgs[0].ReceiveCount != 3 {
		t.Fatal(msgs, err)
	}
	if rec.recv.MaxNumberOfMessages != 10 || rec.recv.WaitTimeSeconds != 20 || rec.recv.VisibilityTimeout != 90 {
		t.Fatalf("unexpected input: %+v", rec.recv)
	}
	if len(rec.recv.MessageSystemAttributeNames) != 1 || rec.recv.MessageSystemAttributeNames[0] != sqstypes.MessageSystemAttributeNameApproximateReceiveCount {
		t.Fatal("ApproximateReceiveCount must be requested")
	}
}

func TestBackoffFullJitterGrowsAndResets(t *testing.T) {
	b := &awsx.Backoff{Base: time.Second, Max: time.Minute, Rand: func() float64 { return 0.999999 }}
	var prev time.Duration
	for i := range 10 {
		d := b.Next()
		if d < prev && d < 59*time.Second {
			t.Fatalf("attempt %d: delay %s shrank from %s", i, d, prev)
		}
		if d > time.Minute {
			t.Fatalf("delay %s above cap", d)
		}
		prev = d
	}
	if prev < 59*time.Second {
		t.Fatalf("expected to reach the 60s cap, got %s", prev)
	}
	b.Reset()
	if d := b.Next(); d > time.Second {
		t.Fatalf("after reset the first delay must be < 1s, got %s", d)
	}
	b.Rand = func() float64 { return 0 }
	if d := b.Next(); d != 0 {
		t.Fatalf("full jitter must be able to return 0, got %s", d)
	}
}

func TestKeepVisibleExtendsAndIsCapped(t *testing.T) {
	q := awsxfake.NewQueue()
	q.Send("x")
	msgs, _ := q.Receive(context.Background(), 1, 0, 3*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var calls int
	done := make(chan struct{})
	go func() {
		awsx.KeepVisible(ctx, q, msgs[0].ReceiptHandle, 3*time.Second, 4*time.Second, func(err error) {
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			calls++
			mu.Unlock()
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("KeepVisible did not stop at its cap")
	}
	_, _, vis := q.Snapshot()
	if len(vis) == 0 {
		t.Fatal("no extension happened")
	}
	// Ticks at 1s, 2s, 3s: 3s, then 2s, then 1s remaining before the 4s cap.
	for i, v := range vis {
		if v > 3*time.Second {
			t.Fatalf("extension %d = %s exceeds visibility", i, v)
		}
	}
	if last := vis[len(vis)-1]; last > 2*time.Second {
		t.Fatalf("final extension %s should be clipped by the cap: %v", last, vis)
	}
}

func TestBusRoutesByHostFilter(t *testing.T) {
	bus := &awsxfake.Bus{}
	qs := map[string]*awsxfake.Queue{}
	for _, h := range []string{"web01", "web02", "web03"} {
		qs[h] = awsxfake.NewQueue()
		bus.Subscribe(qs[h], h)
	}
	for i := range 6 {
		h := "web0" + strconv.Itoa(i%3+1)
		if _, err := bus.Publish(context.Background(), []byte(h), awsx.Routing{Host: h, Command: "systemctl", ProtocolVersion: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for h, q := range qs {
		msgs, _ := q.Receive(context.Background(), 10, 0, time.Minute)
		if len(msgs) != 2 {
			t.Fatalf("%s got %d messages", h, len(msgs))
		}
		for _, m := range msgs {
			if m.Body != h {
				t.Fatalf("%s received %s's message", h, m.Body)
			}
		}
	}
}

func TestFakeQueueRedeliversAfterVisibility(t *testing.T) {
	q := awsxfake.NewQueue()
	id := q.Send("x")
	first, _ := q.Receive(context.Background(), 1, 0, time.Second)
	if again, _ := q.Receive(context.Background(), 1, 0, time.Second); len(again) != 0 {
		t.Fatal("in-flight message was redelivered early")
	}
	time.Sleep(1100 * time.Millisecond)
	second, _ := q.Receive(context.Background(), 1, 0, time.Second)
	if len(second) != 1 || second[0].ReceiveCount != 2 || q.Receives(id) != 2 {
		t.Fatalf("expected redelivery with count 2: %+v", second)
	}
	if err := q.Delete(context.Background(), first[0].ReceiptHandle); err == nil {
		t.Fatal("stale receipt handle must not delete")
	}
}
