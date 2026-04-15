package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/raymondkneipp/aggregate/internal/claude"
	"github.com/raymondkneipp/aggregate/internal/config"
	"github.com/raymondkneipp/aggregate/internal/domain"
	"github.com/raymondkneipp/aggregate/internal/jina"
	"github.com/raymondkneipp/aggregate/internal/repository"
)

var (
	cfg        *config.Config
	db         *pgxpool.Pool
	enricher   *claude.Enricher
	jinaClient *jina.Client
)

func main() {
	// Init once at cold start, not per-invocation
	cfg = config.Load()

	var err error
	db, err = pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		slog.Error("connect db", "error", err)
		os.Exit(1)
	}

	enricher = claude.NewEnricher(cfg.AnthropicAPIKey)
	jinaClient = jina.New(cfg.JinaAPIKey)

	lambda.Start(handler)
}

func handler(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	repo := repository.NewJobRepository(db)
	var failures []events.SQSBatchItemFailure

	for _, record := range event.Records {
		if err := processRecord(ctx, record, repo); err != nil {
			slog.Error("process record failed",
				"message_id", record.MessageId,
				"error", err,
			)
			// Only this message returns to queue; others succeed
			failures = append(failures, events.SQSBatchItemFailure{
				ItemIdentifier: record.MessageId,
			})
		}
	}

	return events.SQSEventResponse{BatchItemFailures: failures}, nil
}

func processRecord(ctx context.Context, record events.SQSMessage, repo *repository.JobRepository) error {
	var msg domain.SQSMessage
	if err := json.Unmarshal([]byte(record.Body), &msg); err != nil {
		return err
	}

	start := time.Now()

	// Fetch full content by URL
	results, err := jinaClient.SearchByURL(ctx, msg.URL)
	content := ""
	if err == nil && len(results) > 0 {
		content = results[0].Content
	}

	raw := domain.RawJob{URL: msg.URL, Site: msg.Site, Role: msg.RoleQuery}
	job, err := enricher.Enrich(ctx, raw, content)
	if err != nil {
		return err
	}

	inserted, err := repo.Upsert(ctx, job)
	if err != nil {
		return err
	}

	slog.Info("enriched",
		"url", msg.URL,
		"company", job.Company,
		"new", inserted,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}
