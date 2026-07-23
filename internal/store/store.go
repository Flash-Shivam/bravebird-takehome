// Package store holds job state in a single DynamoDB table.
// Every status transition is a conditional update: the transition is the
// idempotency mechanism for at-least-once SQS delivery.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	StatusQueued    = "QUEUED"
	StatusLaunching = "LAUNCHING"
	StatusRunning   = "RUNNING"
	StatusSucceeded = "SUCCEEDED"
	StatusFailed    = "FAILED"
	StatusTimedOut  = "TIMED_OUT"
)

func Terminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusTimedOut
}

type Job struct {
	ID         string `dynamodbav:"job_id" json:"job_id"`
	UserID     string `dynamodbav:"user_id" json:"user_id"`
	Prompt     string `dynamodbav:"prompt" json:"prompt"`
	Priority   string `dynamodbav:"priority" json:"priority"`
	Status     string `dynamodbav:"status" json:"status"`
	Reason     string `dynamodbav:"reason,omitempty" json:"reason,omitempty"`
	TaskARN    string `dynamodbav:"task_arn,omitempty" json:"task_arn,omitempty"`
	CreatedAt  int64  `dynamodbav:"created_at" json:"created_at"`
	ClaimedAt  int64  `dynamodbav:"claimed_at,omitempty" json:"claimed_at,omitempty"`
	StartedAt  int64  `dynamodbav:"started_at,omitempty" json:"started_at,omitempty"`
	FinishedAt int64  `dynamodbav:"finished_at,omitempty" json:"finished_at,omitempty"`
}

var ErrConflict = errors.New("conditional update failed")

type Store struct {
	db    *dynamodb.Client
	table string
}

func New(db *dynamodb.Client, table string) *Store {
	return &Store{db: db, table: table}
}

func (s *Store) CreateJob(ctx context.Context, j Job) error {
	item, err := attributevalue.MarshalMap(j)
	if err != nil {
		return err
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           &s.table,
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(job_id)"),
	})
	return err
}

func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table,
		Key:       map[string]types.AttributeValue{"job_id": &types.AttributeValueMemberS{Value: id}},
		// Strongly consistent: the dispatcher reads a job right after the API
		// writes it; an eventually-consistent miss would drop the job.
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, nil
	}
	var j Job
	if err := attributevalue.UnmarshalMap(out.Item, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// Transition moves a job from one status to another atomically, optionally
// setting extra attributes. Returns ErrConflict if the job is not in `from`.
func (s *Store) Transition(ctx context.Context, id, from, to string, extra map[string]types.AttributeValue) error {
	upd := "SET #s = :to"
	values := map[string]types.AttributeValue{
		":to":   &types.AttributeValueMemberS{Value: to},
		":from": &types.AttributeValueMemberS{Value: from},
	}
	names := map[string]string{"#s": "status"}
	i := 0
	for k, v := range extra {
		ph := fmt.Sprintf(":v%d", i)
		nm := fmt.Sprintf("#n%d", i)
		upd += fmt.Sprintf(", %s = %s", nm, ph)
		values[ph] = v
		names[nm] = k
		i++
	}
	_, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &s.table,
		Key:                       map[string]types.AttributeValue{"job_id": &types.AttributeValueMemberS{Value: id}},
		UpdateExpression:          &upd,
		ConditionExpression:       aws.String("#s = :from"),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: values,
	})
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return ErrConflict
	}
	return err
}

// Finish marks a job terminal from ANY non-terminal state (used by the agent's
// deferred write and the reconciler). No-op (ErrConflict) if already terminal.
func (s *Store) Finish(ctx context.Context, id, to, reason string) error {
	now := time.Now().Unix()
	_, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           &s.table,
		Key:                 map[string]types.AttributeValue{"job_id": &types.AttributeValueMemberS{Value: id}},
		UpdateExpression:    aws.String("SET #s = :to, reason = :r, finished_at = :f"),
		ConditionExpression: aws.String("NOT #s IN (:s1, :s2, :s3)"),
		ExpressionAttributeNames: map[string]string{"#s": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":to": &types.AttributeValueMemberS{Value: to},
			":r":  &types.AttributeValueMemberS{Value: reason},
			":f":  &types.AttributeValueMemberN{Value: fmt.Sprint(now)},
			":s1": &types.AttributeValueMemberS{Value: StatusSucceeded},
			":s2": &types.AttributeValueMemberS{Value: StatusFailed},
			":s3": &types.AttributeValueMemberS{Value: StatusTimedOut},
		},
	})
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return ErrConflict
	}
	return err
}

// ListNonTerminal returns all jobs still in flight, for the reconciler.
// ponytail: full-table Scan — fine to ~10k jobs; sparse GSI on status is the upgrade.
func (s *Store) ListNonTerminal(ctx context.Context) ([]Job, error) {
	var jobs []Job
	p := dynamodb.NewScanPaginator(s.db, &dynamodb.ScanInput{
		TableName:        &s.table,
		FilterExpression: aws.String("NOT #s IN (:s1, :s2, :s3)"),
		ExpressionAttributeNames: map[string]string{"#s": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":s1": &types.AttributeValueMemberS{Value: StatusSucceeded},
			":s2": &types.AttributeValueMemberS{Value: StatusFailed},
			":s3": &types.AttributeValueMemberS{Value: StatusTimedOut},
		},
	})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		var page []Job
		if err := attributevalue.UnmarshalListOfMaps(out.Items, &page); err != nil {
			return nil, err
		}
		jobs = append(jobs, page...)
	}
	return jobs, nil
}

// AllowRequest is a per-user fixed-window rate limiter stored in the same table
// (keys prefixed "ratelimit#" never collide with ULID job ids).
// ponytail: fixed window allows 2x burst at window edges; token bucket is the upgrade.
func (s *Store) AllowRequest(ctx context.Context, userID string, limitPerMinute int) (bool, error) {
	window := time.Now().Unix() / 60
	key := fmt.Sprintf("ratelimit#%s#%d", userID, window)
	_, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:           &s.table,
		Key:                 map[string]types.AttributeValue{"job_id": &types.AttributeValueMemberS{Value: key}},
		UpdateExpression:    aws.String("ADD #c :one SET expires_at = :exp"),
		ConditionExpression: aws.String("attribute_not_exists(#c) OR #c < :lim"),
		ExpressionAttributeNames: map[string]string{"#c": "req_count"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":one": &types.AttributeValueMemberN{Value: "1"},
			":lim": &types.AttributeValueMemberN{Value: fmt.Sprint(limitPerMinute)},
			":exp": &types.AttributeValueMemberN{Value: fmt.Sprint(time.Now().Add(2 * time.Minute).Unix())},
		},
	})
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return false, nil
	}
	return err == nil, err
}
