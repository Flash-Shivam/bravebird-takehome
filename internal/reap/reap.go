// Package reap contains reaper layers 2 and 3:
//   - Reconciler (layer 2): DDB-driven; syncs job records with ECS truth,
//     stops over-TTL tasks, fails jobs whose tasks died or never launched.
//   - ReapCluster (layer 3, used by the Lambda): ECS-only; stops any agent
//     task older than a hard max TTL. No DDB or controlplane dependency.
package reap

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/shivamjadhav/bravebird-takehome/internal/store"
)

type Config struct {
	Cluster        string
	TTL            time.Duration // per-job budget from launch to finish
	LaunchDeadline time.Duration // max time a job may sit in LAUNCHING without a task
}

type Reconciler struct {
	ecs   *ecs.Client
	store *store.Store
	cfg   Config
}

func NewReconciler(ec *ecs.Client, st *store.Store, cfg Config) *Reconciler {
	return &Reconciler{ecs: ec, store: st, cfg: cfg}
}

func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.reconcile(ctx); err != nil {
				slog.Error("reconcile pass failed", "err", err)
			}
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context) error {
	jobs, err := r.store.ListNonTerminal(ctx)
	if err != nil {
		return err
	}
	now := time.Now()

	// Batch-describe every task we know about (DescribeTasks caps at 100 ARNs).
	byARN := map[string]ecstypes.Task{}
	var arns []string
	for _, j := range jobs {
		if j.TaskARN != "" {
			arns = append(arns, j.TaskARN)
		}
	}
	for i := 0; i < len(arns); i += 100 {
		out, err := r.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
			Cluster: &r.cfg.Cluster,
			Tasks:   arns[i:min(i+100, len(arns))],
		})
		if err != nil {
			return err
		}
		for _, t := range out.Tasks {
			byARN[aws.ToString(t.TaskArn)] = t
		}
	}

	for _, j := range jobs {
		log := slog.With("job", j.ID, "status", j.Status)
		switch {
		case j.Status == store.StatusQueued:
			// Backlog is legitimate (that's what the queue is for) — never reaped.

		case j.TaskARN == "":
			// LAUNCHING with no task: dispatcher likely died between the
			// transition and RunTask. The redelivered message skips non-QUEUED
			// jobs, so nothing else will ever pick this up.
			claimed := j.ClaimedAt
			if claimed == 0 {
				claimed = j.CreatedAt
			}
			if now.Sub(time.Unix(claimed, 0)) > r.cfg.LaunchDeadline {
				log.Warn("stuck in LAUNCHING with no task, failing")
				_ = r.store.Finish(ctx, j.ID, store.StatusFailed, "stuck in LAUNCHING (dispatcher interrupted)")
			}

		default:
			task, known := byARN[j.TaskARN]
			if !known {
				// ECS retains stopped tasks ~1h; an unknown ARN means it
				// stopped long ago and the terminal write was lost.
				log.Warn("task unknown to ECS, failing")
				_ = r.store.Finish(ctx, j.ID, store.StatusFailed, "task no longer exists")
				continue
			}
			if aws.ToString(task.LastStatus) == "STOPPED" {
				// Task ended but job is still non-terminal: agent crashed, was
				// OOM-killed, or the image never pulled. Surface ECS's reason.
				reason := aws.ToString(task.StoppedReason)
				if reason == "" {
					reason = "task stopped without reporting status"
				}
				log.Warn("task stopped but job not terminal", "reason", reason)
				_ = r.store.Finish(ctx, j.ID, store.StatusFailed, reason)
				continue
			}
			started := j.StartedAt
			if started == 0 {
				started = j.CreatedAt
			}
			if now.Sub(time.Unix(started, 0)) > r.cfg.TTL {
				log.Warn("job over TTL, stopping task", "task", j.TaskARN)
				_, err := r.ecs.StopTask(ctx, &ecs.StopTaskInput{
					Cluster: &r.cfg.Cluster,
					Task:    &j.TaskARN,
					Reason:  aws.String("reaped: job TTL exceeded"),
				})
				if err != nil {
					log.Error("StopTask failed", "err", err)
					continue
				}
				_ = r.store.Finish(ctx, j.ID, store.StatusTimedOut, fmt.Sprintf("reaped after %s TTL", r.cfg.TTL))
			}
		}
	}
	return nil
}

// ReapCluster is layer 3: kill any agent-family task older than maxTTL, keyed
// purely on ECS state. Returns the number of tasks stopped.
func ReapCluster(ctx context.Context, ec *ecs.Client, cluster, family string, maxTTL time.Duration) (int, error) {
	stopped := 0
	p := ecs.NewListTasksPaginator(ec, &ecs.ListTasksInput{
		Cluster: &cluster,
		Family:  &family, // excludes the controlplane service's tasks
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return stopped, err
		}
		if len(page.TaskArns) == 0 {
			continue
		}
		out, err := ec.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: &cluster, Tasks: page.TaskArns})
		if err != nil {
			return stopped, err
		}
		for _, t := range out.Tasks {
			born := t.StartedAt
			if born == nil {
				born = t.CreatedAt
			}
			if born == nil || time.Since(*born) < maxTTL {
				continue
			}
			arn := aws.ToString(t.TaskArn)
			_, err := ec.StopTask(ctx, &ecs.StopTaskInput{
				Cluster: &cluster,
				Task:    &arn,
				Reason:  aws.String(fmt.Sprintf("hard reap: task exceeded max TTL %s", maxTTL)),
			})
			if err != nil {
				slog.Error("hard reap StopTask failed", "err", err, "task", arn)
				continue
			}
			slog.Warn("hard reaped task", "task", arn, "age", time.Since(*born).Round(time.Second))
			stopped++
		}
	}
	return stopped, nil
}
