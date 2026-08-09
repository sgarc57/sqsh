// Command sqshd is the sqsh agent daemon. See SPEC.md for the full design;
// this build implements the walking-skeleton subset described in
// openspec/changes/add-walking-skeleton.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"sqsh/internal/agent"
	"sqsh/internal/awsx"
	"sqsh/internal/commands"
	"sqsh/internal/config"
	"sqsh/internal/dispatcher"
	"sqsh/internal/observability"
	"sqsh/internal/results"
	"sqsh/internal/version"
)

func main() { os.Exit(run()) }

func run() int {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "/etc/sqshd/config.yaml", "path to config file")
	flag.Parse()

	if *showVersion {
		fmt.Println("sqshd", version.String())
		return 0
	}

	// 1–2. Load and validate config: no partial start on any error.
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sqshd:", err)
		return config.ExitConfig
	}
	log := observability.NewLogger(os.Stdout, cfg.LogLevel).With("host", cfg.HostID)
	log.Info("sqshd starting", "version", version.Version, "commit", version.Commit, "config", *configPath)

	exec := &dispatcher.Executor{
		MaxOutputBytes: int(cfg.MaxOutputBytes),
		KillGrace:      dispatcher.DefaultKillGrace,
		DryRun:         !cfg.ShouldExecute(),
	}
	registry, err := commands.NewRegistry(cfg, exec)
	if err != nil {
		log.Error("invalid command configuration", "err", err)
		return config.ExitConfig
	}
	for _, name := range registry.Disabled() {
		log.Warn("allowed command is disabled in this release and will be rejected", "command", name)
	}
	if exec.DryRun {
		log.Warn("execute: false — dry-run mode, no command will be spawned")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		log.Error("cannot load AWS configuration", "err", err)
		return config.ExitConfig
	}

	// 6. Verify AWS identity and log the ARN.
	ident, err := sts.NewFromConfig(awsCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		log.Error("cannot verify AWS identity", "err", err)
		return config.ExitConfig
	}
	log.Info("AWS identity verified", "arn", aws.ToString(ident.Arn), "account", aws.ToString(ident.Account))

	// 9. Resolve the queue: it must exist and be reachable before polling.
	sqsClient := sqs.NewFromConfig(awsCfg)
	attrs, err := sqsClient.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(cfg.QueueURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	if err != nil {
		log.Error("cannot resolve queue", "queue_url", cfg.QueueURL, "err", err)
		var missing *sqstypes.QueueDoesNotExist
		if errors.As(err, &missing) {
			return config.ExitConfig
		}
		return 1 // possibly transient; let the supervisor retry
	}
	log.Info("queue resolved", "queue_arn", attrs.Attributes[string(sqstypes.QueueAttributeNameQueueArn)])

	// 14. Metrics listener, then the receive loop.
	metrics := observability.NewMetrics()
	if cfg.MetricsAddr != "" {
		if err := metrics.Serve(ctx, cfg.MetricsAddr, log); err != nil {
			log.Error("cannot start metrics listener", "addr", cfg.MetricsAddr, "err", err)
			return config.ExitConfig
		}
		log.Info("metrics listening", "addr", cfg.MetricsAddr)
	}

	a := agent.New(agent.Deps{
		Config:   cfg,
		Queue:    &awsx.SQSQueue{Client: sqsClient, URL: cfg.QueueURL},
		Store:    results.NewDynamo(dynamodb.NewFromConfig(awsCfg), cfg.ResultsTable, cfg.ResultRetention.D()),
		Registry: registry,
		Logger:   log.With(slog.String("component", "agent")),
		Metrics:  metrics,
		Version:  version.Version,
	})
	if err := a.Run(ctx); err != nil {
		log.Error("agent stopped with error", "err", err)
		return 1
	}
	log.Info("sqshd stopped")
	return 0
}
