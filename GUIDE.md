# aggregate — Step-by-Step Build Guide

Work through stages in order. Each stage ends with something you can run.

**Module path used throughout:** `github.com/raymondkneipp/aggregate`

---

## Prerequisites

```bash
# Check these exist
go version        # need 1.21+
docker --version  # for local postgres
aws --version     # aws cli v2
terraform -version # 1.6+

# AWS credentials configured
aws configure
# Enter: Access Key ID, Secret Access Key, region (us-east-1), output (json)

# Verify AWS works
aws sts get-caller-identity
```

---

## Stage 1 — Go CLI (local, no AWS)

**Goal:** Refactor `main.go` into a proper package structure. CLI that fetches raw jobs from Jina and prints them.

### 1.1 Create directory structure

```bash
mkdir -p cmd/fetcher \
         internal/config \
         internal/domain \
         internal/jina
```

### 1.2 Update go.mod

```bash
go mod edit -go=1.23
```

### 1.3 `internal/domain/job.go`

Core types used everywhere.

```go
package domain

import "time"

// RawJob is what comes back from Jina before enrichment.
type RawJob struct {
	URL     string
	Title   string
	Site    string
	Role    string // the query that found it
}

// Job is the enriched, stored record.
type Job struct {
	ID         string
	URL        string
	Title      string
	Company    string
	Seniority  string // junior / mid / senior / staff / unknown
	Remote     string // remote / hybrid / onsite / unknown
	SalaryMin  *int
	SalaryMax  *int
	Skills     []string
	Location   string
	RawContent string
	EnrichedAt *time.Time
	CreatedAt  time.Time
}

// SQSMessage is what the fetcher sends to the enrichment queue.
type SQSMessage struct {
	URL       string `json:"url"`
	Site      string `json:"site"`
	RoleQuery string `json:"role_query"`
}
```

### 1.4 `internal/config/config.go`

Loads from env vars. Lambda will load from SSM later — this file stays the same.

```go
package config

import (
	"log"
	"os"
	"strings"
)

type Config struct {
	JinaAPIKey     string
	AnthropicAPIKey string
	DatabaseURL    string
	SQSQueueURL    string
	Environment    string

	Sites []string
	Roles []string
}

func Load() *Config {
	cfg := &Config{
		JinaAPIKey:      mustEnv("JINA_API_KEY"),
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"), // optional in stage 1
		DatabaseURL:     os.Getenv("DATABASE_URL"),       // optional in stage 1
		SQSQueueURL:     os.Getenv("SQS_QUEUE_URL"),      // optional in stage 1
		Environment:     getEnv("ENVIRONMENT", "local"),
		Sites: splitEnv("SITES",
			"jobs.lever.co,boards.greenhouse.io,jobs.ashby.io,apply.workable.com"),
		Roles: splitEnv("ROLES",
			"software engineer,software developer,frontend developer,full stack developer"),
	}
	return cfg
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s not set", key)
	}
	return v
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitEnv(key, fallback string) []string {
	v := getEnv(key, fallback)
	return strings.Split(v, ",")
}
```

### 1.5 `internal/jina/client.go`

Wraps the Jina search API. Uses context for timeout control.

```go
package jina

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/raymondkneipp/aggregate/internal/domain"
)

type Client struct {
	apiKey string
	http   *http.Client
}

type searchResponse struct {
	Code int `json:"code"`
	Data []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"data"`
}

func New(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) Search(ctx context.Context, site, role string) ([]domain.RawJob, error) {
	q := fmt.Sprintf(`site:%s "%s"`, site, role)
	u := "https://s.jina.ai/?" + url.Values{"q": {q}}.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Respond-With", "no-content")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jina request: %w", err)
	}
	defer resp.Body.Close()

	var result searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	jobs := make([]domain.RawJob, 0, len(result.Data))
	for _, d := range result.Data {
		if d.URL == "" {
			continue
		}
		jobs = append(jobs, domain.RawJob{
			URL:   d.URL,
			Title: d.Title,
			Site:  site,
			Role:  role,
		})
	}
	return jobs, nil
}
```

### 1.6 `cmd/fetcher/main.go`

Goroutines for parallel Jina calls (16 searches, all I/O — sequential would be ~32s, parallel ~2s).

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/raymondkneipp/aggregate/internal/config"
	"github.com/raymondkneipp/aggregate/internal/domain"
	"github.com/raymondkneipp/aggregate/internal/jina"
)

func main() {
	cfg := config.Load()
	if err := run(context.Background(), cfg); err != nil {
		slog.Error("fetcher failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config.Config) error {
	jinaClient := jina.New(cfg.JinaAPIKey)

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

	// Parallel: all 16 Jina calls fire at once
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

	// Close channel once all goroutines finish
	go func() {
		wg.Wait()
		close(results)
	}()

	total := 0
	for r := range results {
		if r.err != nil {
			slog.Warn("search failed", "site", r.site, "role", r.role, "error", r.err)
			continue
		}
		for _, job := range r.jobs {
			fmt.Printf("[%s] %s\n  %s\n\n", job.Site, job.Title, job.URL)
			total++
		}
	}

	slog.Info("fetch complete", "total_jobs", total)
	return nil
}
```

### 1.7 Run it

```bash
export JINA_API_KEY="REDACTED"

go run ./cmd/fetcher
```

**Verify:** Should print job titles + URLs from multiple sites. Takes ~2-3s (parallel).

### 1.8 Tidy

```bash
go mod tidy
```

---

## Stage 2 — AI Enrichment (local, no AWS)

**Goal:** Pass raw jobs through Claude. Get back structured fields: seniority, remote type, salary, skills.

### 2.1 Install Anthropic SDK

```bash
go get github.com/anthropics/anthropic-sdk-go
```

### 2.2 `internal/claude/enricher.go`

Uses tool use — Claude fills a JSON schema. More reliable than asking Claude to "return JSON".

> **Note (SDK v1.35+):** `NewClient` returns a value (`anthropic.Client`), not a pointer.
> Tools use `[]anthropic.ToolUnionParam` with `OfTool`. `ToolChoice` uses `ToolChoiceUnionParam` with `OfTool`.
> Check for tool_use blocks with `block.Type == "tool_use"` and access input via `block.AsToolUse().Input`.

```go
package claude

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/raymondkneipp/aggregate/internal/domain"
)

type Enricher struct {
	client anthropic.Client
}

func NewEnricher(apiKey string) *Enricher {
	return &Enricher{
		client: anthropic.NewClient(option.WithAPIKey(apiKey)),
	}
}

// toolSchema defines what fields Claude must extract.
var toolSchema = anthropic.ToolInputSchemaParam{
	Properties: map[string]any{
		"title":   map[string]any{"type": "string", "description": "Job title"},
		"company": map[string]any{"type": "string", "description": "Company name"},
		"seniority": map[string]any{
			"type": "string",
			"enum": []string{"junior", "mid", "senior", "staff", "principal", "unknown"},
		},
		"remote": map[string]any{
			"type": "string",
			"enum": []string{"remote", "hybrid", "onsite", "unknown"},
		},
		"salary_min": map[string]any{"type": "integer", "description": "Min annual salary USD, null if not mentioned"},
		"salary_max": map[string]any{"type": "integer", "description": "Max annual salary USD, null if not mentioned"},
		"skills":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Required technical skills"},
		"location":   map[string]any{"type": "string", "description": "City/region, or 'Remote'"},
	},
	Required: []string{"title", "company", "seniority", "remote", "skills"},
}

type enrichedFields struct {
	Title     string   `json:"title"`
	Company   string   `json:"company"`
	Seniority string   `json:"seniority"`
	Remote    string   `json:"remote"`
	SalaryMin *int     `json:"salary_min"`
	SalaryMax *int     `json:"salary_max"`
	Skills    []string `json:"skills"`
	Location  string   `json:"location"`
}

func (e *Enricher) Enrich(ctx context.Context, job domain.RawJob, content string) (*domain.Job, error) {
	prompt := fmt.Sprintf(
		"Extract structured details from this job posting.\n\nURL: %s\n\nContent:\n%s",
		job.URL, truncate(content, 4000),
	)

	resp, err := e.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5, // fast + cheap for extraction
		MaxTokens: 1024,
		Tools: []anthropic.ToolUnionParam{
			{
				OfTool: &anthropic.ToolParam{
					Name:        "extract_job_details",
					Description: anthropic.String("Extract structured job details from a job posting"),
					InputSchema: toolSchema,
				},
			},
		},
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{
				Name: "extract_job_details",
			},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("claude API: %w", err)
	}

	// Find the tool use block in the response
	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			var fields enrichedFields
			if err := json.Unmarshal([]byte(block.AsToolUse().Input), &fields); err != nil {
				return nil, fmt.Errorf("parse tool output: %w", err)
			}
			return &domain.Job{
				URL:       job.URL,
				Title:     fields.Title,
				Company:   fields.Company,
				Seniority: fields.Seniority,
				Remote:    fields.Remote,
				SalaryMin: fields.SalaryMin,
				SalaryMax: fields.SalaryMax,
				Skills:    fields.Skills,
				Location:  fields.Location,
			}, nil
		}
	}

	return nil, fmt.Errorf("no tool use block in response")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "... [truncated]"
}
```

### 2.3 Update `cmd/fetcher/main.go`

Add enrichment after Jina results. Still no DB — just print enriched records.

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/raymondkneipp/aggregate/internal/claude"
	"github.com/raymondkneipp/aggregate/internal/config"
	"github.com/raymondkneipp/aggregate/internal/domain"
	"github.com/raymondkneipp/aggregate/internal/jina"
)

func main() {
	cfg := config.Load()
	if err := run(context.Background(), cfg); err != nil {
		slog.Error("fetcher failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config.Config) error {
	jinaClient := jina.New(cfg.JinaAPIKey)
	enricher := claude.NewEnricher(cfg.AnthropicAPIKey)

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

	for r := range results {
		if r.err != nil {
			slog.Warn("search failed", "site", r.site, "role", r.role, "error", r.err)
			continue
		}
		for _, raw := range r.jobs {
			// Enrich each job (sequential per batch — Claude rate limits apply)
			job, err := enricher.Enrich(ctx, raw, "")
			if err != nil {
				slog.Warn("enrich failed", "url", raw.URL, "error", err)
				continue
			}
			fmt.Printf("%-50s %-10s %-8s %s\n", job.Title, job.Seniority, job.Remote, job.Company)
		}
	}

	return nil
}
```

### 2.4 Run it

```bash
export JINA_API_KEY="REDACTED"
export ANTHROPIC_API_KEY="your-key-here"

go run ./cmd/fetcher
```

**Verify:** Structured output — title, seniority (senior/mid/etc), remote type, company name.

```
Staff Software Engineer                            staff      remote   Acme Corp
Frontend Developer (React)                         mid        hybrid   Some Startup
```

---

## Stage 3 — Local API + Database

**Goal:** Store jobs in Postgres. Expose a REST API. Run Postgres locally via Docker.

### 3.1 Install dependencies

```bash
go get github.com/jackc/pgx/v5
go get github.com/go-chi/chi/v5
go get github.com/lib/pq
go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
```

### 3.2 `docker-compose.yml`

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: aggregate
      POSTGRES_USER: aggregate
      POSTGRES_PASSWORD: secret
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U aggregate"]
      interval: 5s
      timeout: 5s
      retries: 5

volumes:
  pgdata:
```

### 3.3 Migrations

```bash
mkdir -p migrations
```

`migrations/000001_create_jobs.up.sql`:

```sql
CREATE TABLE jobs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    url         TEXT UNIQUE NOT NULL,
    title       TEXT,
    company     TEXT,
    seniority   TEXT,
    remote      TEXT,
    salary_min  INT,
    salary_max  INT,
    skills      TEXT[],
    location    TEXT,
    raw_content TEXT,
    enriched_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_jobs_seniority  ON jobs(seniority);
CREATE INDEX idx_jobs_remote     ON jobs(remote);
CREATE INDEX idx_jobs_created_at ON jobs(created_at DESC);
CREATE INDEX idx_jobs_company    ON jobs(company);
```

`migrations/000001_create_jobs.down.sql`:

```sql
DROP TABLE IF EXISTS jobs;
```

### 3.4 `internal/repository/jobs.go`

```go
package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/raymondkneipp/aggregate/internal/domain"
)

type JobRepository struct {
	db *pgxpool.Pool
}

func NewJobRepository(db *pgxpool.Pool) *JobRepository {
	return &JobRepository{db: db}
}

// Upsert inserts a job or updates it if URL already exists.
// Returns true if this was a new record (not a duplicate).
func (r *JobRepository) Upsert(ctx context.Context, job *domain.Job) (bool, error) {
	query := `
		INSERT INTO jobs (url, title, company, seniority, remote, salary_min, salary_max, skills, location, enriched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
		ON CONFLICT (url) DO UPDATE SET
			title      = EXCLUDED.title,
			company    = EXCLUDED.company,
			seniority  = EXCLUDED.seniority,
			remote     = EXCLUDED.remote,
			salary_min = EXCLUDED.salary_min,
			salary_max = EXCLUDED.salary_max,
			skills     = EXCLUDED.skills,
			location   = EXCLUDED.location,
			enriched_at = NOW()
		WHERE jobs.enriched_at IS NULL  -- only re-enrich if not yet enriched
		RETURNING (xmax = 0) AS inserted
	`
	var inserted bool
	err := r.db.QueryRow(ctx, query,
		job.URL, job.Title, job.Company, job.Seniority, job.Remote,
		job.SalaryMin, job.SalaryMax, job.Skills, job.Location,
	).Scan(&inserted)
	if err != nil {
		return false, fmt.Errorf("upsert job: %w", err)
	}
	return inserted, nil
}

type ListParams struct {
	Seniority string
	Remote    string
	Company   string
	Limit     int
	Offset    int
}

func (r *JobRepository) List(ctx context.Context, p ListParams) ([]domain.Job, error) {
	if p.Limit == 0 || p.Limit > 100 {
		p.Limit = 20
	}

	query := `
		SELECT id, url, title, company, seniority, remote,
		       salary_min, salary_max, skills, location, created_at
		FROM jobs
		WHERE ($1 = '' OR seniority = $1)
		  AND ($2 = '' OR remote    = $2)
		  AND ($3 = '' OR company ILIKE '%' || $3 || '%')
		ORDER BY created_at DESC
		LIMIT $4 OFFSET $5
	`
	rows, err := r.db.Query(ctx, query, p.Seniority, p.Remote, p.Company, p.Limit, p.Offset)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []domain.Job
	for rows.Next() {
		var j domain.Job
		if err := rows.Scan(
			&j.ID, &j.URL, &j.Title, &j.Company, &j.Seniority, &j.Remote,
			&j.SalaryMin, &j.SalaryMax, &j.Skills, &j.Location, &j.CreatedAt,
		); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func (r *JobRepository) GetByID(ctx context.Context, id string) (*domain.Job, error) {
	query := `
		SELECT id, url, title, company, seniority, remote,
		       salary_min, salary_max, skills, location, created_at
		FROM jobs WHERE id = $1
	`
	var j domain.Job
	err := r.db.QueryRow(ctx, query, id).Scan(
		&j.ID, &j.URL, &j.Title, &j.Company, &j.Seniority, &j.Remote,
		&j.SalaryMin, &j.SalaryMax, &j.Skills, &j.Location, &j.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}
	return &j, nil
}
```

### 3.5 `internal/handler/jobs.go`

```go
package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/raymondkneipp/aggregate/internal/repository"
)

type JobHandler struct {
	repo *repository.JobRepository
}

func NewJobHandler(repo *repository.JobRepository) *JobHandler {
	return &JobHandler{repo: repo}
}

func (h *JobHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Get("/{id}", h.GetByID)
	return r
}

func (h *JobHandler) List(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	jobs, err := h.repo.List(r.Context(), repository.ListParams{
		Seniority: r.URL.Query().Get("seniority"),
		Remote:    r.URL.Query().Get("remote"),
		Company:   r.URL.Query().Get("company"),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respond(w, http.StatusOK, jobs)
}

func (h *JobHandler) GetByID(w http.ResponseWriter, r *http.Request) {
	job, err := h.repo.GetByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	respond(w, http.StatusOK, job)
}

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
```

### 3.6 `cmd/api/main.go`

```go
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/raymondkneipp/aggregate/internal/config"
	"github.com/raymondkneipp/aggregate/internal/handler"
	"github.com/raymondkneipp/aggregate/internal/repository"
)

func main() {
	cfg := config.Load()

	db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		slog.Error("connect to DB", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(context.Background()); err != nil {
		slog.Error("ping DB", "error", err)
		os.Exit(1)
	}

	repo := repository.NewJobRepository(db)
	jobHandler := handler.NewJobHandler(repo)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})
	r.Mount("/jobs", jobHandler.Routes())

	addr := ":8080"
	slog.Info("API listening", "addr", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
```

### 3.7 `Makefile`

```makefile
DB_URL=postgres://aggregate:secret@localhost:5432/aggregate?sslmode=disable

.PHONY: db-up db-down migrate-up migrate-down run-api run-fetcher

db-up:
	docker compose up -d

db-down:
	docker compose down

migrate-up:
	migrate -path migrations -database "$(DB_URL)" up

migrate-down:
	migrate -path migrations -database "$(DB_URL)" down 1

run-api:
	DATABASE_URL="$(DB_URL)" go run ./cmd/api

run-fetcher:
	DATABASE_URL="$(DB_URL)" \
	JINA_API_KEY="$(JINA_API_KEY)" \
	ANTHROPIC_API_KEY="$(ANTHROPIC_API_KEY)" \
	go run ./cmd/fetcher
```

### 3.8 Update `cmd/fetcher/main.go` to write to DB

Add DB upsert after enrichment:

```go
// After enricher and db pool setup, replace the print loop:

db, err := pgxpool.New(ctx, cfg.DatabaseURL)
if err != nil {
    return fmt.Errorf("connect db: %w", err)
}
defer db.Close()
repo := repository.NewJobRepository(db)

// Inside the results loop, after enriching:
inserted, err := repo.Upsert(ctx, job)
if err != nil {
    slog.Warn("db upsert failed", "url", raw.URL, "error", err)
    continue
}
if inserted {
    slog.Info("new job", "title", job.Title, "company", job.Company)
} else {
    slog.Debug("duplicate, skipped", "url", job.URL)
}
```

### 3.9 Run it

```bash
# Start Postgres
make db-up

# Wait a second, then run migrations
make migrate-up

# Start API in one terminal
make run-api

# Run fetcher in another terminal
make run-fetcher

# Query results
curl "http://localhost:8080/jobs?seniority=senior&remote=remote" | jq .
curl "http://localhost:8080/jobs?limit=5" | jq .
```

**Verify:** Jobs appear in DB. API returns JSON. Duplicate runs don't create duplicate records.

---

## Stage 4 — Terraform Bootstrap (one-time)

**Goal:** Create S3 bucket for Terraform state + DynamoDB table for locking. Run once, never again.

### 4.1 Install Terraform

```bash
# macOS
brew install terraform

# Verify
terraform -version
```

### 4.2 Create S3 state bucket

Replace `yourname` with something globally unique (e.g., your username).

```bash
export TF_STATE_BUCKET="aggregate-tfstate-raymond"
export AWS_REGION="us-east-1"

# Create bucket
aws s3api create-bucket \
  --bucket "$TF_STATE_BUCKET" \
  --region "$AWS_REGION"

# Enable versioning (protects state file)
aws s3api put-bucket-versioning \
  --bucket "$TF_STATE_BUCKET" \
  --versioning-configuration Status=Enabled

# Block public access
aws s3api put-public-access-block \
  --bucket "$TF_STATE_BUCKET" \
  --public-access-block-configuration "BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"
```

### 4.3 Create DynamoDB lock table

Prevents two `terraform apply` from running simultaneously and corrupting state.

```bash
aws dynamodb create-table \
  --table-name aggregate-terraform-locks \
  --attribute-definitions AttributeName=LockID,AttributeType=S \
  --key-schema AttributeName=LockID,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST \
  --region "$AWS_REGION"
```

### 4.4 Create Terraform structure

```bash
mkdir -p terraform/environments \
         terraform/modules/vpc \
         terraform/modules/rds \
         terraform/modules/sqs \
         terraform/modules/lambda \
         terraform/modules/eventbridge \
         terraform/modules/api_gateway
```

### 4.5 `terraform/backend.tf`

```hcl
terraform {
  backend "s3" {
    bucket         = "aggregate-tfstate-raymond"   # replace
    key            = "aggregate/terraform.tfstate"
    region         = "us-east-1"
    dynamodb_table = "aggregate-terraform-locks"
    encrypt        = true
  }
}
```

### 4.6 `terraform/variables.tf`

```hcl
variable "environment" {
  description = "dev or prod"
  type        = string
}

variable "aws_region" {
  type    = string
  default = "us-east-1"
}

variable "db_instance_class" {
  type = string
}

variable "lambda_memory_mb" {
  type    = number
  default = 256
}

variable "fetch_schedule" {
  description = "EventBridge cron expression"
  type        = string
}

variable "sqs_visibility_timeout" {
  type    = number
  default = 300
}

variable "alert_email" {
  description = "Email for CloudWatch alarms (prod only)"
  type        = string
  default     = ""
}
```

### 4.7 `terraform/environments/dev.tfvars`

```hcl
environment            = "dev"
db_instance_class      = "db.t3.micro"
lambda_memory_mb       = 256
fetch_schedule         = "rate(12 hours)"
sqs_visibility_timeout = 300
alert_email            = ""
```

### 4.8 `terraform/environments/prod.tfvars`

```hcl
environment            = "prod"
db_instance_class      = "db.t3.micro"
lambda_memory_mb       = 512
fetch_schedule         = "rate(6 hours)"
sqs_visibility_timeout = 300
alert_email            = "you@example.com"
```

**Verify:** S3 bucket exists, DynamoDB table exists. `terraform init` will succeed in next stage.

---

## Stage 5 — Terraform: VPC + RDS

**Goal:** VPC with private subnets, RDS Postgres, security groups. DB URL stored in SSM.

### 5.1 `terraform/main.tf`

```hcl
terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.0"
    }
  }
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      Project     = "aggregate"
      Environment = var.environment
      ManagedBy   = "terraform"
    }
  }
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 5.0"

  name = "${var.environment}-aggregate"
  cidr = "10.0.0.0/16"

  azs             = ["${var.aws_region}a", "${var.aws_region}b"]
  private_subnets = ["10.0.1.0/24", "10.0.2.0/24"]
  public_subnets  = ["10.0.101.0/24", "10.0.102.0/24"]

  enable_nat_gateway = true
  single_nat_gateway = true   # not HA, but saves ~$32/mo

  enable_dns_hostnames = true
  enable_dns_support   = true
}

```

> `module "rds"` is added in Stage 7 alongside `module "lambda"` — RDS ingress rules reference the Lambda security group, so they must be deployed together.

### 5.2 `terraform/outputs.tf`

```hcl
# Outputs are added incrementally as modules are created.
# rds_endpoint added in Stage 7, sqs_queue_url in Stage 6, api_url in Stage 9.
```

### 5.3 `terraform/modules/rds/main.tf`

```hcl
variable "environment"        { type = string }
variable "vpc_id"             { type = string }
variable "private_subnet_ids" { type = list(string) }
variable "lambda_sg_id"       { type = string }
variable "db_instance_class"  { type = string }
variable "aws_region"         { type = string }

resource "random_password" "db" {
  length  = 32
  special = false
}

resource "aws_db_subnet_group" "this" {
  name       = "${var.environment}-aggregate"
  subnet_ids = var.private_subnet_ids
}

resource "aws_security_group" "rds" {
  name   = "${var.environment}-aggregate-rds"
  vpc_id = var.vpc_id

  ingress {
    description     = "PostgreSQL from Lambda"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [var.lambda_sg_id]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_db_instance" "this" {
  identifier        = "${var.environment}-aggregate"
  engine            = "postgres"
  engine_version    = "16"
  instance_class    = var.db_instance_class
  allocated_storage = 20
  db_name           = "aggregatedb"
  username          = "aggregateuser"
  password          = random_password.db.result

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [aws_security_group.rds.id]

  skip_final_snapshot     = var.environment == "dev"
  deletion_protection     = var.environment == "prod"
  backup_retention_period = var.environment == "prod" ? 7 : 1

  # Required for pgvector (Stage 11 optional upgrade)
  parameter_group_name = aws_db_parameter_group.this.name
}

resource "aws_db_parameter_group" "this" {
  name   = "${var.environment}-aggregate-pg16"
  family = "postgres16"
}

# Store connection string in SSM — Lambdas fetch this at startup
resource "aws_ssm_parameter" "db_url" {
  name  = "/${var.environment}/aggregate/db_url"
  type  = "SecureString"
  value = "postgres://${aws_db_instance.this.username}:${random_password.db.result}@${aws_db_instance.this.endpoint}/aggregatedb?sslmode=require"
}

output "endpoint"      { value = aws_db_instance.this.endpoint }
output "security_group_id" { value = aws_security_group.rds.id }
```

### 5.4 Push secrets to SSM (before terraform apply)

```bash
# Anthropic API key
aws ssm put-parameter \
  --name "/dev/aggregate/anthropic_key" \
  --value "sk-ant-your-key" \
  --type SecureString \
  --region us-east-1

aws ssm put-parameter \
  --name "/prod/aggregate/anthropic_key" \
  --value "sk-ant-your-key" \
  --type SecureString \
  --region us-east-1

# Jina API key
aws ssm put-parameter \
  --name "/dev/aggregate/jina_key" \
  --value "jina_your-key" \
  --type SecureString \
  --region us-east-1

aws ssm put-parameter \
  --name "/prod/aggregate/jina_key" \
  --value "jina_your-key" \
  --type SecureString \
  --region us-east-1
```

### 5.5 Deploy dev infrastructure

```bash
cd terraform

terraform init

# Preview what will be created
terraform plan -var-file=environments/dev.tfvars

# Deploy (takes ~10 min — RDS takes a while)
terraform apply -var-file=environments/dev.tfvars
```

**Verify:** `terraform apply` completes. Go to AWS Console → RDS → see `dev-aggregate` instance.

---

## Stage 6 — Terraform: SQS + DLQ

**Goal:** Add jobs queue and dead letter queue. Wire alarms.

### 6.1 `terraform/modules/sqs/main.tf`

```hcl
variable "environment"        { type = string }
variable "visibility_timeout" { type = number }
variable "alert_email"        { type = string }

# Dead Letter Queue
# Messages land here after failing enrichment 3 times
resource "aws_sqs_queue" "dlq" {
  name                      = "${var.environment}-aggregate-dlq"
  message_retention_seconds = 1209600 # 14 days
}

# Main enrichment queue
resource "aws_sqs_queue" "jobs" {
  name                       = "${var.environment}-aggregate-jobs"
  visibility_timeout_seconds = var.visibility_timeout
  message_retention_seconds  = 86400 # 1 day

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    maxReceiveCount     = 3 # fail 3 times → DLQ
  })
}

# Store queue URL in SSM so Lambdas can read it
resource "aws_ssm_parameter" "sqs_url" {
  name  = "/${var.environment}/aggregate/sqs_url"
  type  = "String"
  value = aws_sqs_queue.jobs.url
}

# SNS topic for alarms
resource "aws_sns_topic" "alerts" {
  name = "${var.environment}-aggregate-alerts"
}

resource "aws_sns_topic_subscription" "email" {
  count     = var.alert_email != "" ? 1 : 0
  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "email"
  endpoint  = var.alert_email
}

# Alarm: DLQ has messages = enrichment is failing
resource "aws_cloudwatch_metric_alarm" "dlq_not_empty" {
  alarm_name          = "${var.environment}-aggregate-dlq-not-empty"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = aws_sqs_queue.dlq.name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  alarm_actions       = var.alert_email != "" ? [aws_sns_topic.alerts.arn] : []
}

# Alarm: queue depth > 100 = enricher falling behind
resource "aws_cloudwatch_metric_alarm" "queue_backlog" {
  alarm_name          = "${var.environment}-aggregate-queue-backlog"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = aws_sqs_queue.jobs.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 3
  threshold           = 100
  comparison_operator = "GreaterThanThreshold"
}

output "queue_url"    { value = aws_sqs_queue.jobs.url }
output "queue_arn"    { value = aws_sqs_queue.jobs.arn }
output "dlq_url"      { value = aws_sqs_queue.dlq.url }
output "dlq_arn"      { value = aws_sqs_queue.dlq.arn }
output "sns_arn"      { value = aws_sns_topic.alerts.arn }
```

### 6.2 Add sqs module to `terraform/main.tf`

```hcl
module "sqs" {
  source = "./modules/sqs"

  environment        = var.environment
  visibility_timeout = var.sqs_visibility_timeout
  alert_email        = var.alert_email
}
```

### 6.3 Add to `terraform/outputs.tf`

```hcl
output "sqs_queue_url" {
  value = module.sqs.queue_url
}
```

```bash
cd terraform
terraform apply -var-file=environments/dev.tfvars
```

**Verify:** SQS queues visible in AWS Console → SQS.

---

## Stage 7 — Terraform + Go: Lambda Deployment

**Goal:** Build 3 Lambda binaries, create Lambda functions in AWS, wire SQS to enricher.

### 7.1 Install Lambda dependencies

```bash
go get github.com/aws/aws-lambda-go/lambda
go get github.com/aws/aws-lambda-go/events
go get github.com/aws/aws-sdk-go-v2/config
go get github.com/aws/aws-sdk-go-v2/service/sqs
go get github.com/aws/aws-sdk-go-v2/service/ssm
go mod tidy
```

### 7.2 Update `internal/config/config.go` for SSM loading

Config now loads from SSM when running in Lambda, env vars when local.

```go
package config

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

type Config struct {
	JinaAPIKey      string
	AnthropicAPIKey string
	DatabaseURL     string
	SQSQueueURL     string
	Environment     string
	Sites           []string
	Roles           []string
}

func Load() *Config {
	env := getEnv("ENVIRONMENT", "local")

	cfg := &Config{
		Environment: env,
		Sites: splitEnv("SITES",
			"jobs.lever.co,boards.greenhouse.io,jobs.ashby.io,apply.workable.com"),
		Roles: splitEnv("ROLES",
			"software engineer,software developer,frontend developer,full stack developer"),
	}

	if env == "local" {
		// Local dev: read directly from env vars
		cfg.JinaAPIKey      = mustEnv("JINA_API_KEY")
		cfg.AnthropicAPIKey = mustEnv("ANTHROPIC_API_KEY")
		cfg.DatabaseURL     = mustEnv("DATABASE_URL")
		cfg.SQSQueueURL     = os.Getenv("SQS_QUEUE_URL")
	} else {
		// Lambda: fetch from SSM at startup (not per-request)
		cfg.JinaAPIKey      = getSSM(env, "jina_key")
		cfg.AnthropicAPIKey = getSSM(env, "anthropic_key")
		cfg.DatabaseURL     = getSSM(env, "db_url")
		cfg.SQSQueueURL     = getSSM(env, "sqs_url")
	}

	return cfg
}

func getSSM(env, name string) string {
	path := "/" + env + "/aggregate/" + name
	awscfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("load aws config: %v", err)
	}
	client := ssm.NewFromConfig(awscfg)
	resp, err := client.GetParameter(context.Background(), &ssm.GetParameterInput{
		Name:           aws.String(path),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		log.Fatalf("get SSM param %s: %v", path, err)
	}
	return *resp.Parameter.Value
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s not set", key)
	}
	return v
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitEnv(key, fallback string) []string {
	return strings.Split(getEnv(key, fallback), ",")
}
```

### 7.3 Update `cmd/fetcher/main.go` for Lambda

Lambda receives an EventBridge event. Same `run()` function works for both local and Lambda.

```go
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
```

### 7.4 `cmd/enricher/main.go`

SQS triggers this. Processes batch of 5. Uses partial batch failure — one failed job doesn't kill the batch.

```go
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
	cfg      *config.Config
	db       *pgxpool.Pool
	enricher *claude.Enricher
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
```

### 7.5 Add `SearchByURL` to Jina client

```go
// Add to internal/jina/client.go

func (c *Client) SearchByURL(ctx context.Context, jobURL string) ([]Result, error) {
	u := "https://r.jina.ai/" + jobURL   // Jina reader API — fetches full page content

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Data, nil
}
```

### 7.6 `cmd/api/main.go` — Lambda version

API Gateway sends proxy events. `aws-lambda-go` adapter handles the conversion.

```go
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/awslabs/aws-lambda-go-api-proxy/chi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/raymondkneipp/aggregate/internal/config"
	"github.com/raymondkneipp/aggregate/internal/handler"
	"github.com/raymondkneipp/aggregate/internal/repository"
)

func main() {
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		startLambda()
	} else {
		startLocal()
	}
}

func buildRouter(cfg *config.Config) (http.Handler, error) {
	db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	repo := repository.NewJobRepository(db)
	jobHandler := handler.NewJobHandler(repo)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	r.Mount("/jobs", jobHandler.Routes())
	return r, nil
}

func startLambda() {
	cfg := config.Load()
	router, err := buildRouter(cfg)
	if err != nil {
		slog.Error("setup router", "error", err)
		os.Exit(1)
	}
	adapter := chiadapter.New(router.(*chi.Mux))
	lambda.Start(adapter.ProxyWithContext)
}

func startLocal() {
	cfg := config.Load()
	router, err := buildRouter(cfg)
	if err != nil {
		slog.Error("setup router", "error", err)
		os.Exit(1)
	}
	slog.Info("listening", "addr", ":8080")
	http.ListenAndServe(":8080", router)
}
```

```bash
go get github.com/awslabs/aws-lambda-go-api-proxy/chi
go mod tidy
```

### 7.7 `terraform/modules/lambda/main.tf`

```hcl
variable "environment"        { type = string }
variable "vpc_id"             { type = string }
variable "private_subnet_ids" { type = list(string) }
variable "lambda_memory_mb"   { type = number }
variable "sqs_queue_url"      { type = string }
variable "sqs_queue_arn"      { type = string }
variable "dlq_arn"            { type = string }
variable "aws_region"         { type = string }

data "aws_caller_identity" "current" {}

# Security group for all Lambdas
resource "aws_security_group" "lambda" {
  name   = "${var.environment}-aggregate-lambda"
  vpc_id = var.vpc_id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# Shared IAM role
resource "aws_iam_role" "lambda" {
  name = "${var.environment}-aggregate-lambda"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "lambda" {
  role = aws_iam_role.lambda.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "ec2:CreateNetworkInterface",
          "ec2:DescribeNetworkInterfaces",
          "ec2:DeleteNetworkInterface"
        ]
        Resource = "*"
      },
      {
        Effect   = "Allow"
        Action   = ["ssm:GetParameter", "ssm:GetParameters"]
        Resource = "arn:aws:ssm:${var.aws_region}:${data.aws_caller_identity.current.account_id}:parameter/${var.environment}/aggregate/*"
      },
      {
        Effect = "Allow"
        Action = [
          "sqs:SendMessage",
          "sqs:ReceiveMessage",
          "sqs:DeleteMessage",
          "sqs:GetQueueAttributes"
        ]
        Resource = [var.sqs_queue_arn, var.dlq_arn]
      },
      {
        Effect   = "Allow"
        Action   = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = "arn:aws:logs:*:*:*"
      }
    ]
  })
}

locals {
  common_env = {
    ENVIRONMENT   = var.environment
    SQS_QUEUE_URL = var.sqs_queue_url
  }
}

resource "aws_lambda_function" "fetcher" {
  function_name = "${var.environment}-aggregate-fetcher"
  role          = aws_iam_role.lambda.arn
  handler       = "bootstrap"
  runtime       = "provided.al2023"
  architectures = ["arm64"]
  filename      = "${path.root}/../fetcher.zip"
  memory_size   = var.lambda_memory_mb
  timeout       = 300

  vpc_config {
    subnet_ids         = var.private_subnet_ids
    security_group_ids = [aws_security_group.lambda.id]
  }

  environment { variables = local.common_env }
}

resource "aws_lambda_function" "enricher" {
  function_name = "${var.environment}-aggregate-enricher"
  role          = aws_iam_role.lambda.arn
  handler       = "bootstrap"
  runtime       = "provided.al2023"
  architectures = ["arm64"]
  filename      = "${path.root}/../enricher.zip"
  memory_size   = var.lambda_memory_mb
  timeout       = 300 # must be <= SQS visibility_timeout

  vpc_config {
    subnet_ids         = var.private_subnet_ids
    security_group_ids = [aws_security_group.lambda.id]
  }

  environment { variables = local.common_env }
}

# Wire SQS → enricher Lambda
resource "aws_lambda_event_source_mapping" "sqs" {
  event_source_arn                   = var.sqs_queue_arn
  function_name                      = aws_lambda_function.enricher.arn
  batch_size                         = 5
  maximum_batching_window_in_seconds = 30
  function_response_types            = ["ReportBatchItemFailures"]
}

resource "aws_lambda_function" "api" {
  function_name = "${var.environment}-aggregate-api"
  role          = aws_iam_role.lambda.arn
  handler       = "bootstrap"
  runtime       = "provided.al2023"
  architectures = ["arm64"]
  filename      = "${path.root}/../api.zip"
  memory_size   = var.lambda_memory_mb
  timeout       = 30

  vpc_config {
    subnet_ids         = var.private_subnet_ids
    security_group_ids = [aws_security_group.lambda.id]
  }

  environment { variables = local.common_env }
}

output "fetcher_arn"       { value = aws_lambda_function.fetcher.arn }
output "fetcher_name"      { value = aws_lambda_function.fetcher.function_name }
output "enricher_arn"      { value = aws_lambda_function.enricher.arn }
output "enricher_name"     { value = aws_lambda_function.enricher.function_name }
output "api_arn"           { value = aws_lambda_function.api.arn }
output "api_name"          { value = aws_lambda_function.api.function_name }
output "security_group_id" { value = aws_security_group.lambda.id }
```

### 7.8 `migrations/embed.go`

Embeds SQL files into the migrate Lambda binary so no filesystem access is needed.

```go
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
```

### 7.8a `cmd/migrate/main.go`

Lambda that runs migrations from inside the VPC — no public RDS access needed.

```go
package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/raymondkneipp/aggregate/internal/config"
	"github.com/raymondkneipp/aggregate/migrations"
)

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context) (string, error) {
	cfg := config.Load()

	d, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return "", fmt.Errorf("load migrations: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", d, cfg.DatabaseURL)
	if err != nil {
		return "", fmt.Errorf("create migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return "", fmt.Errorf("run migrations: %w", err)
	}

	version, _, _ := m.Version()
	slog.Info("migrations complete", "version", version)
	return fmt.Sprintf("migrated to version %d", version), nil
}
```

```bash
go get github.com/golang-migrate/migrate/v4/source/iofs
go mod tidy
```

Also add the migrate Lambda to `terraform/modules/lambda/main.tf` (before the outputs):

```hcl
resource "aws_lambda_function" "migrate" {
  function_name = "${var.environment}-aggregate-migrate"
  role          = aws_iam_role.lambda.arn
  handler       = "bootstrap"
  runtime       = "provided.al2023"
  architectures = ["arm64"]
  filename      = "${path.root}/../migrate.zip"
  memory_size   = 256
  timeout       = 60

  vpc_config {
    subnet_ids         = var.private_subnet_ids
    security_group_ids = [aws_security_group.lambda.id]
  }

  environment { variables = { ENVIRONMENT = var.environment } }
}
```

### 7.9 `scripts/build.sh`

```bash
#!/bin/bash
set -e

echo "Building Lambda binaries (arm64)..."

for cmd in fetcher enricher api migrate; do
  echo "  Building $cmd..."
  GOARCH=arm64 GOOS=linux go build \
    -tags lambda.norpc \
    -o bootstrap \
    "./cmd/$cmd"
  zip -q "$cmd.zip" bootstrap
  rm bootstrap
  echo "  → $cmd.zip"
done

echo "Done."
```

```bash
chmod +x scripts/build.sh
```

### 7.9 Add Lambda zip upload to Makefile

```makefile
ENV ?= dev

build:
	./scripts/build.sh

deploy-dev: build
	cd terraform && terraform apply -var-file=environments/dev.tfvars -auto-approve
	$(MAKE) update-lambdas ENV=dev

deploy-prod: build
	cd terraform && terraform apply -var-file=environments/prod.tfvars
	$(MAKE) update-lambdas ENV=prod

update-lambdas:
	aws lambda update-function-code --function-name $(ENV)-aggregate-fetcher  --zip-file fileb://fetcher.zip
	aws lambda update-function-code --function-name $(ENV)-aggregate-enricher --zip-file fileb://enricher.zip
	aws lambda update-function-code --function-name $(ENV)-aggregate-api      --zip-file fileb://api.zip
	aws lambda update-function-code --function-name $(ENV)-aggregate-migrate  --zip-file fileb://migrate.zip

invoke-fetcher:
	aws lambda invoke \
	  --function-name $(ENV)-aggregate-fetcher \
	  --payload '{}' \
	  /tmp/fetcher-out.json \
	  && cat /tmp/fetcher-out.json

logs-fetcher:
	aws logs tail /aws/lambda/$(ENV)-aggregate-fetcher --follow

logs-enricher:
	aws logs tail /aws/lambda/$(ENV)-aggregate-enricher --follow

logs-api:
	aws logs tail /aws/lambda/$(ENV)-aggregate-api --follow

queue-depth:
	aws sqs get-queue-attributes \
	  --queue-url $$(aws ssm get-parameter --name /$(ENV)/aggregate/sqs_url --query Parameter.Value --output text) \
	  --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible

migrate-lambda:
	aws lambda invoke \
	  --function-name $(ENV)-aggregate-migrate \
	  --payload '{}' \
	  /tmp/migrate-out.json \
	  && cat /tmp/migrate-out.json

migrate-aws: migrate-lambda
```

### 7.10 Add lambda + rds modules to `terraform/main.tf`

```hcl
module "lambda" {
  source = "./modules/lambda"

  environment        = var.environment
  vpc_id             = module.vpc.vpc_id
  private_subnet_ids = module.vpc.private_subnets
  lambda_memory_mb   = var.lambda_memory_mb
  sqs_queue_url      = module.sqs.queue_url
  sqs_queue_arn      = module.sqs.queue_arn
  dlq_arn            = module.sqs.dlq_arn
  aws_region         = var.aws_region
}

module "rds" {
  source = "./modules/rds"

  environment        = var.environment
  vpc_id             = module.vpc.vpc_id
  private_subnet_ids = module.vpc.private_subnets
  lambda_sg_id       = module.lambda.security_group_id
  db_instance_class  = var.db_instance_class
  aws_region         = var.aws_region
}
```

Add to `terraform/outputs.tf`:

```hcl
output "rds_endpoint" {
  value     = module.rds.endpoint
  sensitive = true
}
```

### 7.11 Deploy

```bash
# Build zips
make build

# Deploy infrastructure + upload Lambda code
make deploy-dev

# Run migrations against RDS
make migrate-aws ENV=dev

# Manually trigger fetcher and watch it work
make invoke-fetcher ENV=dev

# Tail enricher logs in another terminal
make logs-enricher ENV=dev
```

**Verify:** `invoke-fetcher` returns no error. Enricher logs show jobs being processed. `queue-depth` shows 0 after a few minutes.

---

## Stage 8 — EventBridge Scheduler

**Goal:** Cron rule triggers fetcher automatically. Disabled in dev by default.

### 8.1 `terraform/modules/eventbridge/main.tf`

```hcl
variable "environment"         { type = string }
variable "fetch_schedule"      { type = string }
variable "fetcher_lambda_arn"  { type = string }
variable "fetcher_lambda_name" { type = string }

resource "aws_cloudwatch_event_rule" "schedule" {
  name                = "${var.environment}-aggregate-fetch"
  description         = "Trigger job fetcher Lambda"
  schedule_expression = var.fetch_schedule

  # Only enable in prod — dev triggers manually
  state = var.environment == "prod" ? "ENABLED" : "DISABLED"
}

resource "aws_cloudwatch_event_target" "fetcher" {
  rule      = aws_cloudwatch_event_rule.schedule.name
  target_id = "FetcherLambda"
  arn       = var.fetcher_lambda_arn
}

resource "aws_lambda_permission" "eventbridge" {
  statement_id  = "AllowEventBridgeInvoke"
  action        = "lambda:InvokeFunction"
  function_name = var.fetcher_lambda_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.schedule.arn
}
```

### 8.2 Add eventbridge module to `terraform/main.tf`

```hcl
module "eventbridge" {
  source = "./modules/eventbridge"

  environment         = var.environment
  fetch_schedule      = var.fetch_schedule
  fetcher_lambda_arn  = module.lambda.fetcher_arn
  fetcher_lambda_name = module.lambda.fetcher_name
}
```

```bash
cd terraform && terraform apply -var-file=environments/dev.tfvars
```

**Verify:** EventBridge rule exists in AWS Console → EventBridge → Rules. State = DISABLED in dev, ENABLED in prod.

---

## Stage 9 — API Gateway

**Goal:** Public HTTPS endpoint for the API Lambda.

### 9.1 `terraform/modules/api_gateway/main.tf`

```hcl
variable "environment"      { type = string }
variable "api_lambda_arn"   { type = string }
variable "api_lambda_name"  { type = string }
variable "aws_region"       { type = string }

data "aws_caller_identity" "current" {}

resource "aws_apigatewayv2_api" "this" {
  name          = "${var.environment}-aggregate"
  protocol_type = "HTTP"
}

resource "aws_apigatewayv2_stage" "default" {
  api_id      = aws_apigatewayv2_api.this.id
  name        = "$default"
  auto_deploy = true

  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.api.arn
    format          = "$context.requestId $context.httpMethod $context.routeKey $context.status $context.integrationErrorMessage"
  }
}

resource "aws_cloudwatch_log_group" "api" {
  name              = "/aws/apigateway/${var.environment}-aggregate"
  retention_in_days = 14
}

resource "aws_apigatewayv2_integration" "lambda" {
  api_id                 = aws_apigatewayv2_api.this.id
  integration_type       = "AWS_PROXY"
  integration_uri        = var.api_lambda_arn
  payload_format_version = "2.0"
}

resource "aws_apigatewayv2_route" "proxy" {
  api_id    = aws_apigatewayv2_api.this.id
  route_key = "ANY /{proxy+}"
  target    = "integrations/${aws_apigatewayv2_integration.lambda.id}"
}

resource "aws_lambda_permission" "api_gateway" {
  statement_id  = "AllowAPIGatewayInvoke"
  action        = "lambda:InvokeFunction"
  function_name = var.api_lambda_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.this.execution_arn}/*"
}

output "api_url" { value = aws_apigatewayv2_stage.default.invoke_url }
```

### 9.2 Add api_gateway module to `terraform/main.tf`

```hcl
module "api_gateway" {
  source = "./modules/api_gateway"

  environment     = var.environment
  api_lambda_arn  = module.lambda.api_arn
  api_lambda_name = module.lambda.api_name
  aws_region      = var.aws_region
}
```

Add to `terraform/outputs.tf`:

```hcl
output "api_url" {
  value = module.api_gateway.api_url
}
```

```bash
cd terraform && terraform apply -var-file=environments/dev.tfvars

# Get the API URL
terraform output api_url
```

**Verify:**

```bash
API_URL=$(cd terraform && terraform output -raw api_url)
curl "$API_URL/health"
# → {"status":"ok"}

curl "$API_URL/jobs?limit=5" | jq .
```

---

## Stage 10 — Observability

**Goal:** Structured logs, Lambda error alarms, CloudWatch log groups with retention.

### 10.1 CloudWatch log groups with retention (add to lambda module)

```hcl
# Add to terraform/modules/lambda/main.tf

resource "aws_cloudwatch_log_group" "fetcher" {
  name              = "/aws/lambda/${aws_lambda_function.fetcher.function_name}"
  retention_in_days = var.environment == "prod" ? 30 : 7
}

resource "aws_cloudwatch_log_group" "enricher" {
  name              = "/aws/lambda/${aws_lambda_function.enricher.function_name}"
  retention_in_days = var.environment == "prod" ? 30 : 7
}

resource "aws_cloudwatch_log_group" "api" {
  name              = "/aws/lambda/${aws_lambda_function.api.function_name}"
  retention_in_days = var.environment == "prod" ? 30 : 7
}

# Alarm: enricher errors
resource "aws_cloudwatch_metric_alarm" "enricher_errors" {
  alarm_name          = "${var.environment}-enricher-errors"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.enricher.function_name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 5
  comparison_operator = "GreaterThanThreshold"
}
```

### 10.2 Logs Insights queries to bookmark

Save these in AWS Console → CloudWatch → Logs Insights:

**Slow enrichments:**
```
fields @timestamp, company, duration_ms
| filter ispresent(duration_ms)
| sort duration_ms desc
| limit 20
```

**Errors in last hour:**
```
fields @timestamp, @message
| filter @message like /ERROR/
| sort @timestamp desc
| limit 50
```

**Jobs per run:**
```
fields @timestamp, messages_sent
| filter ispresent(messages_sent)
| stats sum(messages_sent) by bin(1h)
```

---

## Stage 11 — CI/CD

**Goal:** Push to main → auto-deploy dev → approve → deploy prod. No stored AWS keys.

### 11.1 Create IAM role for GitHub Actions (one-time)

```bash
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
REPO="yourgithubusername/aggregate"  # replace

# Create OIDC provider for GitHub
aws iam create-open-id-connect-provider \
  --url "https://token.actions.githubusercontent.com" \
  --client-id-list "sts.amazonaws.com" \
  --thumbprint-list "6938fd4d98bab03faadb97b34396831e3780aea1"

# Create trust policy
cat > /tmp/trust-policy.json << EOF
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "Federated": "arn:aws:iam::${ACCOUNT_ID}:oidc-provider/token.actions.githubusercontent.com"
    },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "token.actions.githubusercontent.com:aud": "sts.amazonaws.com"
      },
      "StringLike": {
        "token.actions.githubusercontent.com:sub": "repo:${REPO}:*"
      }
    }
  }]
}
EOF

aws iam create-role \
  --role-name github-actions-aggregate \
  --assume-role-policy-document file:///tmp/trust-policy.json

# Attach permissions needed for deploy
aws iam attach-role-policy \
  --role-name github-actions-aggregate \
  --policy-arn arn:aws:iam::aws:policy/AdministratorAccess  # tighten this for prod
```

### 11.2 Set GitHub repo secret

```
AWS_ACCOUNT_ID = your 12-digit account ID
```

Set in: GitHub repo → Settings → Secrets → Actions.

### 11.3 `.github/workflows/pr.yml`

```yaml
name: PR Check
on:
  pull_request:

permissions:
  id-token: write
  contents: read

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: '1.23'

      - uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: arn:aws:iam::${{ secrets.AWS_ACCOUNT_ID }}:role/github-actions-aggregate
          aws-region: us-east-1

      - uses: hashicorp/setup-terraform@v3

      - name: Go build (all Lambdas)
        run: |
          GOARCH=arm64 GOOS=linux go build -tags lambda.norpc ./cmd/fetcher
          GOARCH=arm64 GOOS=linux go build -tags lambda.norpc ./cmd/enricher
          GOARCH=arm64 GOOS=linux go build -tags lambda.norpc ./cmd/api

      - name: Go test
        run: go test ./...

      - name: Terraform plan
        working-directory: terraform
        run: |
          terraform init
          terraform plan -var-file=environments/dev.tfvars -no-color
```

### 11.4 `.github/workflows/deploy.yml`

```yaml
name: Deploy
on:
  push:
    branches: [main]

permissions:
  id-token: write
  contents: read

jobs:
  deploy-dev:
    runs-on: ubuntu-latest
    environment: dev
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.23'
      - uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: arn:aws:iam::${{ secrets.AWS_ACCOUNT_ID }}:role/github-actions-aggregate
          aws-region: us-east-1
      - uses: hashicorp/setup-terraform@v3
      - name: Build
        run: ./scripts/build.sh
      - name: Terraform apply dev
        working-directory: terraform
        run: terraform init && terraform apply -var-file=environments/dev.tfvars -auto-approve
      - name: Update Lambda code
        run: |
          aws lambda update-function-code --function-name dev-aggregate-fetcher  --zip-file fileb://fetcher.zip
          aws lambda update-function-code --function-name dev-aggregate-enricher --zip-file fileb://enricher.zip
          aws lambda update-function-code --function-name dev-aggregate-api      --zip-file fileb://api.zip

  deploy-prod:
    runs-on: ubuntu-latest
    environment: prod          # requires manual approval in GitHub
    needs: deploy-dev
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.23'
      - uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: arn:aws:iam::${{ secrets.AWS_ACCOUNT_ID }}:role/github-actions-aggregate
          aws-region: us-east-1
      - uses: hashicorp/setup-terraform@v3
      - name: Build
        run: ./scripts/build.sh
      - name: Terraform apply prod
        working-directory: terraform
        run: terraform init && terraform apply -var-file=environments/prod.tfvars -auto-approve
      - name: Update Lambda code
        run: |
          aws lambda update-function-code --function-name prod-aggregate-fetcher  --zip-file fileb://fetcher.zip
          aws lambda update-function-code --function-name prod-aggregate-enricher --zip-file fileb://enricher.zip
          aws lambda update-function-code --function-name prod-aggregate-api      --zip-file fileb://api.zip
```

### 11.5 Set up GitHub Environments

In GitHub repo → Settings → Environments:
1. Create `dev` — no protection rules
2. Create `prod` — add **Required reviewers** (yourself) — this creates the approval gate

Push to main → dev deploys automatically → you get a notification to approve prod.

---

## Final Verification Checklist

```bash
# 1. Local works
make db-up && make migrate-up && make run-api
curl localhost:8080/health

# 2. Lambda fetcher runs
make invoke-fetcher ENV=dev
make logs-fetcher ENV=dev

# 3. Enricher processes queue
make queue-depth ENV=dev     # should drain to 0
make logs-enricher ENV=dev   # should show enriched jobs

# 4. API returns results
API_URL=$(cd terraform && terraform output -raw api_url)
curl "$API_URL/jobs?remote=remote&seniority=senior" | jq .

# 5. Prod schedule running
aws events list-rules --name-prefix prod-aggregate
# state should be ENABLED

# 6. CI passes
# Push a commit, watch GitHub Actions
```

---

## What to build next

- **`/jobs/search` POST endpoint** — full-text search with `tsvector`
- **Stage 11: pgvector** — semantic search with embeddings
- **Rate limit handling** — exponential backoff in enricher when Claude returns 429
- **Dedup improvement** — hash job content, skip re-enrichment if content unchanged
