package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/raymondkneipp/aggregate/internal/config"
	"github.com/raymondkneipp/aggregate/internal/domain"
	"github.com/raymondkneipp/aggregate/internal/jina"
)

func main() {
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		lambda.Start(lambdaHandler)
	} else {
		cfg := config.Load()
		if err := run(context.Background(), cfg); err != nil {
			slog.Error("fetcher failed", "error", err)
			os.Exit(1)
		}
	}
}

func lambdaHandler(ctx context.Context, _ events.EventBridgeEvent) error {
	cfg := config.Load()
	return run(ctx, cfg)
}

func run(ctx context.Context, cfg *config.Config) error {
	jinaClient := jina.New(cfg.JinaAPIKey)

	awscfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	sqsClient := sqs.NewFromConfig(awscfg)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	type result struct {
		jobs []domain.RawJob
		err  error
		site string
		role string
	}

	results := make(chan result, len(cfg.Sites)*len(cfg.Roles))
	var wg sync.WaitGroup

	for _, site := range cfg.Sites {
		for _, role := range cfg.Roles {
			wg.Add(1)
			go func(site, role string) {
				defer wg.Done()
				jobs, err := jinaClient.Search(ctx, site, role)
				results <- result{jobs: jobs, err: err, site: site, role: role}
			}(site, role)
		}
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	sent := 0
	for r := range results {
		if r.err != nil {
			slog.Warn("search failed", "site", r.site, "role", r.role, "error", r.err)
			continue
		}
		for _, job := range r.jobs {
			msg := domain.SQSMessage{URL: job.URL, Site: job.Site, RoleQuery: job.Role}
			body, _ := json.Marshal(msg)
			_, err := sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
				QueueUrl:    &cfg.SQSQueueURL,
				MessageBody: stringPtr(string(body)),
			})
			if err != nil {
				slog.Warn("sqs send failed", "url", job.URL, "error", err)
				continue
			}
			sent++
		}
	}

	slog.Info("fetcher done", "messages_sent", sent)
	return nil
}

func stringPtr(s string) *string { return &s }
