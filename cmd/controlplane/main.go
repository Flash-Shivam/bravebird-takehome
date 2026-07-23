// controlplane runs the API server, dispatcher, and reconciler as goroutines
// in one binary. It deploys as a small Fargate service and runs identically on
// a laptop (make run-local) — it's all client-side AWS SDK calls.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/shivamjadhav/bravebird-takehome/internal/api"
	"github.com/shivamjadhav/bravebird-takehome/internal/dispatch"
	"github.com/shivamjadhav/bravebird-takehome/internal/reap"
	"github.com/shivamjadhav/bravebird-takehome/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRetryer(func() aws.Retryer { return retry.NewAdaptiveMode() }))
	if err != nil {
		slog.Error("aws config", "err", err)
		os.Exit(1)
	}

	st := store.New(dynamodb.NewFromConfig(awsCfg), mustEnv("TABLE_NAME"))
	sqsClient := sqs.NewFromConfig(awsCfg)
	ecsClient := ecs.NewFromConfig(awsCfg)

	server := api.NewServer(st, sqsClient, s3.NewFromConfig(awsCfg), cloudwatchlogs.NewFromConfig(awsCfg), api.Config{
		QueueHighURL:  mustEnv("QUEUE_HIGH_URL"),
		QueueLowURL:   mustEnv("QUEUE_LOW_URL"),
		Bucket:        mustEnv("BUCKET"),
		AgentLogGroup: mustEnv("AGENT_LOG_GROUP"),
		RatePerMinute: envInt("RATE_PER_MINUTE", 10),
		JWTSecret:     []byte(mustEnv("JWT_SECRET")),
	})

	dispatcher := dispatch.New(sqsClient, ecsClient, st, dispatch.Config{
		QueueHighURL:   mustEnv("QUEUE_HIGH_URL"),
		QueueLowURL:    mustEnv("QUEUE_LOW_URL"),
		Cluster:        mustEnv("CLUSTER"),
		TaskDefinition: mustEnv("AGENT_TASK_DEF"),
		Subnets:        strings.Split(mustEnv("SUBNETS"), ","),
		SecurityGroup:  mustEnv("SECURITY_GROUP"),
		MaxConcurrent:  envInt("MAX_CONCURRENT", 10),
	})

	reconciler := reap.NewReconciler(ecsClient, st, reap.Config{
		Cluster:        mustEnv("CLUSTER"),
		TTL:            envDuration("JOB_TTL", 5*time.Minute),
		LaunchDeadline: envDuration("LAUNCH_DEADLINE", 3*time.Minute),
	})

	go dispatcher.Run(ctx)
	go reconciler.Run(ctx, 30*time.Second)

	addr := ":" + envOr("PORT", "8080")
	httpServer := &http.Server{Addr: addr, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(shutCtx)
	}()
	slog.Info("controlplane up", "addr", addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server", "err", err)
		os.Exit(1)
	}
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		slog.Error("missing required env var", "var", k)
		os.Exit(1)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
