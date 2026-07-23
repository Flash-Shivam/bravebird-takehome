// Package dispatch consumes the priority queues and launches one Fargate task
// per job, holding a global concurrency semaphore.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/shivamjadhav/bravebird-takehome/internal/store"
)

const maxReceiveCount = 3 // must match the SQS redrive policy in terraform

type Config struct {
	QueueHighURL   string
	QueueLowURL    string
	Cluster        string
	TaskDefinition string
	Subnets        []string
	SecurityGroup  string
	MaxConcurrent  int
}

type Dispatcher struct {
	sqs   *sqs.Client
	ecs   *ecs.Client
	store *store.Store
	cfg   Config

	sem      chan struct{}
	mu       sync.Mutex
	inflight map[string]struct{} // job ids holding a semaphore slot
}

func New(sq *sqs.Client, ec *ecs.Client, st *store.Store, cfg Config) *Dispatcher {
	return &Dispatcher{
		sqs: sq, ecs: ec, store: st, cfg: cfg,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
		inflight: make(map[string]struct{}),
	}
}

// Run blocks until ctx is cancelled. Priority = always check the high queue
// (short poll) before falling back to the low queue.
func (d *Dispatcher) Run(ctx context.Context) {
	go d.trackInflight(ctx)
	for ctx.Err() == nil {
		select {
		case d.sem <- struct{}{}: // wait for a free slot before pulling work
		case <-ctx.Done():
			return
		}
		msg, queueURL := d.receive(ctx)
		if msg == nil {
			<-d.sem
			continue
		}
		if !d.handle(ctx, msg, queueURL) {
			<-d.sem // job didn't launch; free the slot
		}
	}
}

func (d *Dispatcher) receive(ctx context.Context) (*sqstypes.Message, string) {
	for _, q := range []struct {
		url  string
		wait int32
	}{{d.cfg.QueueHighURL, 1}, {d.cfg.QueueLowURL, 4}} {
		out, err := d.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              &q.url,
			MaxNumberOfMessages:   1,
			WaitTimeSeconds:       q.wait,
			VisibilityTimeout:     60,
			AttributeNames:        []sqstypes.QueueAttributeName{"ApproximateReceiveCount"},
		})
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("sqs receive failed", "err", err, "queue", q.url)
				time.Sleep(2 * time.Second)
			}
			continue
		}
		if len(out.Messages) > 0 {
			return &out.Messages[0], q.url
		}
	}
	return nil, ""
}

// handle processes one message. Returns true iff the job launched and now
// holds the semaphore slot.
func (d *Dispatcher) handle(ctx context.Context, msg *sqstypes.Message, queueURL string) bool {
	jobID := aws.ToString(msg.Body)
	log := slog.With("job", jobID)
	deleteMsg := func() {
		if _, err := d.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: &queueURL, ReceiptHandle: msg.ReceiptHandle}); err != nil {
			log.Error("delete message failed", "err", err) // redelivery becomes a no-op via the QUEUED->LAUNCHING condition
		}
	}

	job, err := d.store.GetJob(ctx, jobID)
	if err != nil {
		log.Error("get job failed", "err", err)
		return false // leave message for redelivery
	}
	if job == nil || job.Status != store.StatusQueued {
		deleteMsg() // duplicate delivery or unknown id: already handled
		return false
	}

	// The idempotency gate: only one delivery wins this transition. claimed_at
	// anchors the reconciler's stuck-in-LAUNCHING deadline (created_at would
	// misfire for jobs that sat in a long queue backlog).
	claim := map[string]types.AttributeValue{
		"claimed_at": &types.AttributeValueMemberN{Value: fmt.Sprint(time.Now().Unix())},
	}
	if err := d.store.Transition(ctx, jobID, store.StatusQueued, store.StatusLaunching, claim); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			log.Error("transition to LAUNCHING failed", "err", err)
			return false
		}
		deleteMsg()
		return false
	}

	taskARN, err := d.runTask(ctx, jobID)
	if err != nil {
		receives, _ := strconv.Atoi(msg.Attributes[string(sqstypes.MessageSystemAttributeNameApproximateReceiveCount)])
		log.Error("RunTask failed", "err", err, "attempt", receives)
		if receives >= maxReceiveCount {
			// Last chance spent: mark FAILED ourselves so the job doesn't dangle
			// while the message lands in the DLQ.
			_ = d.store.Finish(ctx, jobID, store.StatusFailed, fmt.Sprintf("launch failed after %d attempts: %v", receives, err))
			deleteMsg()
			return false
		}
		// Revert so the redelivered message can retry; visibility timeout is the backoff.
		_ = d.store.Transition(ctx, jobID, store.StatusLaunching, store.StatusQueued, nil)
		return false
	}

	err = d.store.Transition(ctx, jobID, store.StatusLaunching, store.StatusLaunching, map[string]types.AttributeValue{
		"task_arn":   &types.AttributeValueMemberS{Value: taskARN},
		"started_at": &types.AttributeValueMemberN{Value: fmt.Sprint(time.Now().Unix())},
	})
	if err != nil {
		log.Error("record task arn failed", "err", err) // reaper lambda still bounds the task's lifetime
	}
	deleteMsg()
	d.mu.Lock()
	d.inflight[jobID] = struct{}{}
	d.mu.Unlock()
	log.Info("launched", "task", taskARN)
	return true
}

func (d *Dispatcher) runTask(ctx context.Context, jobID string) (string, error) {
	out, err := d.ecs.RunTask(ctx, &ecs.RunTaskInput{
		Cluster:        &d.cfg.Cluster,
		TaskDefinition: &d.cfg.TaskDefinition,
		LaunchType:     ecstypes.LaunchTypeFargate,
		Count:          aws.Int32(1),
		NetworkConfiguration: &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
				Subnets:        d.cfg.Subnets,
				SecurityGroups: []string{d.cfg.SecurityGroup},
				AssignPublicIp: ecstypes.AssignPublicIpEnabled,
			},
		},
		Overrides: &ecstypes.TaskOverride{
			ContainerOverrides: []ecstypes.ContainerOverride{{
				Name:        aws.String("agent"),
				Environment: []ecstypes.KeyValuePair{{Name: aws.String("JOB_ID"), Value: &jobID}},
			}},
		},
	})
	if err != nil {
		return "", err
	}
	if len(out.Failures) > 0 {
		f := out.Failures[0]
		return "", fmt.Errorf("ecs failure: %s: %s", aws.ToString(f.Reason), aws.ToString(f.Detail))
	}
	if len(out.Tasks) == 0 {
		return "", errors.New("RunTask returned no tasks and no failures")
	}
	return aws.ToString(out.Tasks[0].TaskArn), nil
}

// trackInflight frees semaphore slots once jobs reach a terminal state in DDB
// (written by the agent on success, or by the reconciler on crash/timeout).
func (d *Dispatcher) trackInflight(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		d.mu.Lock()
		ids := make([]string, 0, len(d.inflight))
		for id := range d.inflight {
			ids = append(ids, id)
		}
		d.mu.Unlock()
		for _, id := range ids {
			job, err := d.store.GetJob(ctx, id)
			if err != nil {
				slog.Error("inflight check failed", "err", err, "job", id)
				continue
			}
			if job == nil || store.Terminal(job.Status) {
				d.mu.Lock()
				delete(d.inflight, id)
				d.mu.Unlock()
				<-d.sem
				slog.Info("slot released", "job", id)
			}
		}
	}
}
