// The agent binary runs inside the ephemeral Fargate task: fetch the job,
// mark it RUNNING, execute the browser task, upload artifacts, write the
// terminal status. Local mode (-local) iterates on the browser task with no
// AWS at all: go run ./cmd/agent -local -prompt "golang generics" -out ./tmp
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/shivamjadhav/bravebird-takehome/internal/agentrun"
	"github.com/shivamjadhav/bravebird-takehome/internal/store"
)

const bootTimeout = 30 * time.Second

func main() {
	local := flag.Bool("local", false, "run without AWS; save screenshots to -out")
	prompt := flag.String("prompt", "", "search prompt (local mode)")
	out := flag.String("out", "./tmp", "output dir (local mode)")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// SIGTERM (ECS StopTask sends it, stopTimeout=30) cancels ctx so the
	// deferred status write and a final screenshot can still happen.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if *local {
		if *prompt == "" {
			fmt.Fprintln(os.Stderr, "-prompt required in local mode")
			os.Exit(2)
		}
		os.MkdirAll(filepath.Join(*out, "screens"), 0o755)
		save := func(_ context.Context, name string, data []byte) error {
			return os.WriteFile(filepath.Join(*out, name), data, 0o644)
		}
		if _, err := agentrun.Execute(ctx, *prompt, bootTimeout, save); err != nil {
			slog.Error("run failed", "err", err)
			os.Exit(1)
		}
		return
	}

	if err := run(ctx); err != nil {
		slog.Error("agent failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	jobID := os.Getenv("JOB_ID")
	table := os.Getenv("TABLE_NAME")
	bucket := os.Getenv("BUCKET")
	ttl, err := time.ParseDuration(envOr("JOB_TTL", "5m"))
	if err != nil {
		return fmt.Errorf("bad JOB_TTL: %w", err)
	}
	if jobID == "" || table == "" || bucket == "" {
		return errors.New("JOB_ID, TABLE_NAME and BUCKET are required")
	}
	log := slog.With("job", jobID)

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRetryer(func() aws.Retryer { return retry.NewAdaptiveMode() }))
	if err != nil {
		return err
	}
	st := store.New(dynamodb.NewFromConfig(awsCfg), table)
	s3c := s3.NewFromConfig(awsCfg)

	job, err := st.GetJob(ctx, jobID)
	if err != nil {
		return fmt.Errorf("fetch job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %s not found", jobID)
	}
	if store.Terminal(job.Status) {
		log.Warn("job already terminal, exiting", "status", job.Status)
		return nil
	}
	if err := st.Transition(ctx, jobID, store.StatusLaunching, store.StatusRunning, nil); err != nil {
		// Not fatal: dispatcher may not have recorded the arn yet; keep going.
		log.Warn("transition to RUNNING failed", "err", err)
	}

	// Reaper layer 1: the agent bounds its own lifetime.
	runCtx, cancel := context.WithTimeout(ctx, ttl)
	defer cancel()

	save := func(ctx context.Context, name string, data []byte) error {
		key := jobID + "/" + name
		_, err := s3c.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      &bucket,
			Key:         &key,
			Body:        bytes.NewReader(data),
			ContentType: strPtr("image/png"),
		})
		return err
	}

	res, runErr := agentrun.Execute(runCtx, job.Prompt, bootTimeout, save)

	// Terminal status write uses a fresh context: runCtx may be dead, and this
	// write must survive SIGTERM.
	finishCtx, cancelFinish := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFinish()
	switch {
	case runErr == nil:
		return st.Finish(finishCtx, jobID, store.StatusSucceeded,
			fmt.Sprintf("captured %d screenshots", res.Screenshots))
	case errors.Is(runErr, context.DeadlineExceeded):
		_ = st.Finish(finishCtx, jobID, store.StatusTimedOut, fmt.Sprintf("agent hit %s TTL", ttl))
		return runErr
	default:
		_ = st.Finish(finishCtx, jobID, store.StatusFailed, truncate(runErr.Error(), 512))
		return runErr
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func strPtr(s string) *string { return &s }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
