# Job Search Aggregator — Plan

## What It Does

CLI tool and REST API that aggregates job listings from ATS platforms (Lever, Greenhouse, Ashby, Workable), enriches them with Claude AI, and lets you search them.

```
aggregate search --role "software engineer" --location "remote" --site lever
```

---

## Architecture

```
EventBridge (cron, every 6h)
    │
    ▼
Lambda: Fetcher          ──► Jina Search API (s.jina.ai)
    │                         │
    │  push raw job URLs      │
    ▼                         │
SQS: jobs-to-enrich ◄─────────
    │
    ▼
Lambda: Enricher         ──► Claude API (tool use, structured extraction)
    │  (batch, handles rate limits)
    ▼
RDS PostgreSQL
    │
    ▼
Lambda: API              ──► API Gateway ──► Users
```

**Why SQS:** Claude has rate limits. SQS buffers enrichment so fetcher doesn't block. Failed messages go to DLQ — no data loss. Enricher scales independently.

---

## Tech Stack

| Layer | Choice | Why |
|---|---|---|
| Language | Go 1.23+ | Fast, compiled, great stdlib |
| HTTP Router | chi | Lightweight, composable |
| DB Driver | pgx/v5 | Best Postgres driver, no ORM |
| Migrations | golang-migrate | SQL-native, supports rollback |
| AI | Claude API (Anthropic SDK) | Structured extraction via tool use |
| Job Discovery | Jina Search API | Web-native ATS search |
| IaC | Terraform | Industry standard |
| Compute | Lambda (arm64) | No servers, per-invocation billing, 20% cheaper than x86 |
| Queue | SQS + DLQ | Decouple fetcher→enricher, handle rate limits |
| Database | RDS PostgreSQL t3.micro | Free tier eligible |
| API | API Gateway HTTP API | Cheap, zero-config HTTPS |
| Secrets | SSM Parameter Store | Free (vs Secrets Manager $0.40/secret/mo) |
| Scheduler | EventBridge | Cron trigger for fetcher |
| Observability | CloudWatch Logs + Alarms | Logs, error rates, queue depth |
| CI/CD | GitHub Actions + OIDC | No stored AWS keys |

---

## AWS Cost Estimate

| Resource | Cost |
|---|---|
| Lambda | ~$0 (free tier: 1M req + 400K GB-sec/mo) |
| RDS t3.micro | ~$0 first 12 months, ~$13/mo after |
| API Gateway HTTP API | ~$0 (free tier: 1M req/mo for 12 months) |
| SQS | ~$0 (free tier: 1M req/mo) |
| SSM Parameter Store | $0 (standard tier free) |
| EventBridge | $0 (14M invocations/mo free) |
| S3 (Terraform state) | ~$0.02/mo |
| CloudWatch Logs | ~$0.50/mo |
| **Total** | **~$0–2/mo in free tier, ~$15/mo after** |

> Upgrade path: RDS → Aurora Serverless v2 for auto-scaling. Lambda → ECS Fargate for persistent connections or tasks >15 min.

---

## Project Structure

```
aggregate/
├── cmd/
│   ├── fetcher/main.go        # Lambda: crawl Jina, push URLs to SQS
│   ├── enricher/main.go       # Lambda: consume SQS, call Claude, write to DB
│   └── api/main.go            # Lambda: REST API
├── internal/
│   ├── config/                # Load from env vars / SSM
│   │   └── config.go
│   ├── domain/                # Core types: Job, EnrichedJob, SQSMessage
│   │   └── job.go
│   ├── jina/                  # Jina Search API client
│   │   └── client.go
│   ├── claude/                # Claude enrichment (tool use)
│   │   └── enricher.go
│   ├── repository/            # Postgres queries (pgx)
│   │   └── jobs.go
│   └── handler/               # HTTP handlers (chi)
│       └── jobs.go
├── migrations/
│   ├── 000001_create_jobs.up.sql
│   └── 000001_create_jobs.down.sql
├── terraform/
│   ├── environments/
│   │   ├── dev.tfvars         # dev config (12h schedule, t3.micro, 256MB lambda)
│   │   └── prod.tfvars        # prod config (6h schedule, deletion protection on)
│   ├── modules/
│   │   ├── rds/               # RDS PostgreSQL + security group
│   │   ├── lambda/            # 3 Lambdas + IAM role + SQS event source mapping
│   │   ├── sqs/               # jobs queue + DLQ + depth alarm
│   │   ├── eventbridge/       # cron rule + Lambda permission
│   │   └── api_gateway/       # HTTP API + routes
│   ├── main.tf
│   ├── variables.tf
│   ├── outputs.tf
│   └── backend.tf             # S3 state + DynamoDB lock table
├── .github/workflows/
│   ├── pr.yml                 # go build + test + terraform plan on PR
│   └── deploy.yml             # deploy dev → manual approval → deploy prod
├── scripts/
│   └── build.sh               # build all 3 Lambda zips (GOARCH=arm64 GOOS=linux)
├── docker-compose.yml         # local Postgres
├── Makefile
├── go.mod
└── go.sum
```

---

## Stage Progression

Each stage produces something you can actually run.

### Stage 1 — Go CLI (local, no AWS)

Fetch raw job listings from Jina, print to stdout.

```
go run ./cmd/fetcher --role "frontend developer" --site lever
```

**Learn:** Go modules, CLI flags, HTTP clients, JSON parsing, project structure.

**Deliverable:** Binary that searches and prints job listings.

---

### Stage 2 — AI Enrichment (local, no AWS)

Pipe raw Jina results through Claude. Extract: title, company, salary range, skills, remote/hybrid/onsite, seniority.

Use Claude **tool use** — define `extract_job_details` tool, pass raw content, get structured JSON. More reliable than asking Claude to return JSON directly.

**Learn:** Anthropic Go SDK, Claude tool use, prompt design, structured output.

**Deliverable:** CLI prints enriched, structured job records.

---

### Stage 3 — Local API + Database

Add Postgres (Docker Compose), store enriched jobs, expose REST API.

```
GET  /health
GET  /jobs?seniority=senior&remote=remote&limit=20&offset=0
GET  /jobs/:id
POST /jobs/search   { "role": "...", "location": "remote" }
```

Schema: `jobs` table with `url TEXT UNIQUE` as dedup key.

**Learn:** pgx/v5, SQL migrations, chi router, HTTP handlers, Docker Compose.

**Deliverable:** Local API you can curl. Jobs stored in DB.

---

### Stage 4 — Terraform Bootstrap (one-time, manual)

Create S3 state bucket and DynamoDB lock table before any `terraform apply`.

```bash
aws s3api create-bucket --bucket aggregate-terraform-state-yourname --region us-east-1
aws s3api put-bucket-versioning --bucket aggregate-terraform-state-yourname \
  --versioning-configuration Status=Enabled
aws dynamodb create-table --table-name aggregate-terraform-locks \
  --attribute-definitions AttributeName=LockID,AttributeType=S \
  --key-schema AttributeName=LockID,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST
```

**Learn:** Terraform state, remote backends, state locking.

**Deliverable:** Backend ready. Can run `terraform init`.

---

### Stage 5 — Terraform: VPC + RDS + SSM

Deploy VPC, private subnets, RDS PostgreSQL, security groups. Store DB URL in SSM.

```bash
terraform apply -var-file=environments/dev.tfvars
```

`dev.tfvars`: `db_instance_class = "db.t3.micro"`, `skip_final_snapshot = true`
`prod.tfvars`: `deletion_protection = true`, `backup_retention_period = 7`

Push secrets to SSM before Lambdas deploy:
```bash
aws ssm put-parameter --name "/dev/aggregate/anthropic_key" --value "sk-ant-..." --type SecureString
aws ssm put-parameter --name "/dev/aggregate/jina_key"      --value "jina_..."  --type SecureString
```

**Learn:** Terraform modules, VPC, security groups, RDS in private subnet, SSM SecureString.

**Deliverable:** RDS running in AWS. DB URL in SSM. Can connect via tunnel.

---

### Stage 6 — Terraform: SQS + DLQ

Add jobs queue and dead letter queue. Fetcher sends to queue; failed enrichments go to DLQ after 3 attempts.

SQS message payload (small — enricher re-fetches content by URL):
```json
{ "url": "https://jobs.lever.co/acme/abc123", "site": "lever", "role_query": "software engineer" }
```

DLQ alarm: fires if `ApproximateNumberOfMessagesVisible > 0`. Means enrichment failing.

**Learn:** SQS, DLQ, redrive policy, CloudWatch alarms.

**Deliverable:** Queue exists. Fetcher can send. Enrichment failures captured.

---

### Stage 7 — Terraform + Go: Lambda Deployment

Three Lambdas: fetcher, enricher, api. All use `provided.al2023` runtime (Go custom runtime), `arm64`.

Key wiring:
- SQS → enricher Lambda via `aws_lambda_event_source_mapping` (batch size 5, 30s window)
- Enricher uses `ReportBatchItemFailures` — one bad job doesn't fail the whole batch
- All Lambdas in VPC private subnets for RDS access
- IAM role: SSM read, SQS send/receive, CloudWatch logs, VPC networking

Build:
```bash
GOARCH=arm64 GOOS=linux go build -o bootstrap -tags lambda.norpc ./cmd/fetcher
zip fetcher.zip bootstrap
aws lambda update-function-code --function-name dev-aggregate-fetcher --zip-file fileb://fetcher.zip
```

Go handler pattern — dual-mode (Lambda + local):
```go
func main() {
    if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
        lambda.Start(handler)
    } else {
        run(context.Background())
    }
}
```

**Learn:** Lambda packaging, custom runtime, IAM, VPC Lambda, SQS event source mapping, partial batch failure.

**Deliverable:** All 3 Lambdas deployed. Can invoke fetcher manually, watch enricher process queue.

---

### Stage 8 — Terraform: EventBridge Scheduler

Cron rule triggers fetcher every 6h in prod, disabled in dev by default.

```hcl
state = var.environment == "prod" ? "ENABLED" : "DISABLED"
```

Trigger manually in dev:
```bash
make invoke-fetcher ENV=dev
```

**Learn:** EventBridge rules, Lambda permissions, cron expressions.

**Deliverable:** Jobs auto-populate in prod. Dev still manual trigger.

---

### Stage 9 — Observability

Structured logging with `log/slog`. CloudWatch alarms for:
- Lambda error rate > 5 in 5 min (enricher + fetcher)
- SQS queue depth > 100 (enricher falling behind)
- DLQ depth > 0 (enrichment failing, data stuck)

Alarms notify SNS → email in prod. No-op in dev.

CloudWatch Logs Insights for slow enrichments:
```
fields @timestamp, job_url, duration_ms
| filter duration_ms > 5000
| sort duration_ms desc
```

**Learn:** `slog`, CloudWatch Alarms, SNS, Logs Insights queries.

**Deliverable:** Tail logs with `make logs-enricher ENV=prod`. Email alert on failures.

---

### Stage 10 — CI/CD (GitHub Actions + OIDC)

No stored AWS keys. OIDC lets GitHub Actions assume IAM role directly.

`pr.yml` — on pull request:
1. `go build` all 3 binaries
2. `go test ./...`
3. `terraform plan -var-file=environments/dev.tfvars`

`deploy.yml` — on merge to main:
1. Build + deploy to **dev** (auto)
2. GitHub Environment approval gate
3. Build + deploy to **prod** (after approval)

**Learn:** GitHub Actions, OIDC for AWS auth, environment protection rules.

**Deliverable:** Push → dev auto-deploys. Prod deploys after manual approval click.

---

### Stage 11 (Optional) — Vector Search

Add pgvector to Postgres, generate embeddings for job descriptions, enable semantic search.

```
POST /search  { "query": "startup that cares about accessibility" }
```

**Learn:** Vector embeddings, cosine similarity, pgvector, ivfflat/hnsw indexes.

**Deliverable:** Natural language job search.

---

## Execution Order

```
1.  Stage 1   → CLI fetches raw jobs locally
2.  Stage 2   → Claude enrichment works locally
3.  Stage 3   → Local API + DB, curl it
4.  Stage 4   → Bootstrap S3 + DynamoDB (one-time manual)
5.  Stage 5   → terraform apply dev → VPC + RDS in AWS. Push secrets to SSM.
6.  Stage 6   → Add SQS module, terraform apply dev
7.  Stage 7   → Build Lambda zips, terraform apply (adds Lambdas + SQS mapping)
               → make invoke-fetcher ENV=dev → confirm queue fills
               → make logs-enricher ENV=dev → confirm jobs enriched in DB
8.  Stage 8   → Add EventBridge, terraform apply prod → auto-schedule live
9.  Stage 9   → Add alarms, SNS → email for prod failures
10. Stage 10  → Wire GitHub Actions, OIDC IAM role, enable auto-deploy
11. Stage 11  → (optional) pgvector + semantic search
```

---

## Key Design Decisions

**SQS between fetcher and enricher** — Claude has TPM rate limits. Queue absorbs bursts. DLQ catches enrichment failures. Enricher scales independently. Batch size 5 + `ReportBatchItemFailures` = no full-batch loss on single error.

**arm64 Lambda runtime** — 20% cheaper than x86. Faster cold starts. Build with `GOARCH=arm64`.

**EventBridge disabled in dev** — Don't burn API credits on dev schedule. Manual `make invoke-fetcher ENV=dev`.

**SSM over Secrets Manager** — Secrets Manager costs $0.40/secret/month. SSM Standard free. Fetch at Lambda startup, not per-request.

**`-var-file` over Terraform workspaces** — Explicit. No state workspace confusion. `dev.tfvars` and `prod.tfvars` in source control.

**`skip_final_snapshot = true` in dev** — Clean teardown without orphaned snapshots.

**`deletion_protection = true` in prod** — Can't accidentally destroy prod DB.

**OIDC for CI/CD** — No long-lived AWS keys in GitHub secrets. IAM role trusted only for your repo + branch.

**Lambda over ECS to start** — No containers, no idle cost, scales to zero. Upgrade to ECS Fargate when needing persistent DB connections at scale or tasks >15 min.

**RDS t3.micro over Aurora** — Aurora Serverless v2 minimum ~$86/mo. RDS t3.micro free 12 months, $13/mo after. Migrate to Aurora when needing auto-scaling.
