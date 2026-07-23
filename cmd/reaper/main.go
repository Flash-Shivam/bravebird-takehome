// reaper is layer 3 of cost control: an independent Lambda on a 1-minute
// EventBridge schedule that stops any agent task older than MAX_TTL, keyed
// purely on ECS state. It has no dependency on DynamoDB or the controlplane —
// even if every other component is down or wrong, no task outlives MAX_TTL.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"

	"github.com/shivamjadhav/bravebird-takehome/internal/reap"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	lambda.Start(func(ctx context.Context) error {
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return err
		}
		maxTTL, err := time.ParseDuration(os.Getenv("MAX_TTL"))
		if err != nil {
			maxTTL = 10 * time.Minute
		}
		stopped, err := reap.ReapCluster(ctx, ecs.NewFromConfig(cfg),
			os.Getenv("CLUSTER"), os.Getenv("AGENT_FAMILY"), maxTTL)
		slog.Info("reap pass done", "stopped", stopped)
		return err
	})
}
