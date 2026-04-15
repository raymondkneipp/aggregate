# aggregate

A serverless job aggregator that searches ATS platforms for software engineering roles, enriches listings with AI-extracted structured data, and exposes results via a REST API.

## How it works

1. **Fetcher Lambda** — triggered on a schedule via EventBridge, searches Lever, Greenhouse, Ashby, and Workable for configured job titles using the Jina search API, and sends URLs to an SQS queue
2. **Enricher Lambda** — consumes the queue, fetches full job content via Jina reader, passes it to Claude for structured extraction (seniority, remote type, salary, skills, company), and upserts into RDS Postgres
3. **API Lambda** — serves a REST API backed by RDS, exposed publicly via API Gateway

```
EventBridge → Fetcher → SQS → Enricher → RDS
                                            ↑
                                        API Gateway → API Lambda
```

## Stack

- **Language:** Go 1.23
- **Infrastructure:** AWS Lambda, RDS Postgres, SQS, API Gateway, EventBridge — managed with Terraform
- **AI:** Claude Haiku via Anthropic SDK (tool use for structured extraction)
- **Search:** Jina AI search + reader APIs
- **CI/CD:** GitHub Actions with AWS OIDC (no stored credentials)

## API

```
GET /jobs                          List jobs (paginated)
GET /jobs?seniority=senior         Filter by seniority
GET /jobs?remote=remote            Filter by remote type
GET /jobs?company=stripe           Filter by company name
GET /jobs?limit=20&offset=40       Pagination
GET /jobs/{id}                     Get job by ID
GET /health                        Health check
```

### Example response

```json
{
  "id": "...",
  "url": "https://jobs.lever.co/...",
  "title": "Senior Software Engineer",
  "company": "Acme Corp",
  "seniority": "senior",
  "remote": "remote",
  "salary_min": 160000,
  "salary_max": 200000,
  "skills": ["Go", "Kubernetes", "PostgreSQL"],
  "location": "Remote"
}
```

## Local development

**Prerequisites:** Go 1.23+, Docker, AWS CLI v2, Terraform 1.6+

```bash
# Start local Postgres
make db-up
make migrate-up

# Run API server
make run-api

# Run fetcher (requires env vars)
export JINA_API_KEY="..."
export ANTHROPIC_API_KEY="..."
make run-fetcher
```

## Deployment

Infrastructure is managed with Terraform. Secrets are stored in AWS SSM Parameter Store.

```bash
# Store secrets (one-time)
aws ssm put-parameter --name "/dev/aggregate/jina_key"      --value "..." --type SecureString
aws ssm put-parameter --name "/dev/aggregate/anthropic_key" --value "..." --type SecureString

# Deploy
make deploy-dev

# Run database migrations
make migrate-lambda ENV=dev

# Invoke fetcher manually
make invoke-fetcher ENV=dev
```

## Project structure

```
cmd/
  api/        HTTP API Lambda + local server
  enricher/   SQS consumer Lambda
  fetcher/    EventBridge-triggered job search Lambda
  migrate/    Database migration Lambda
internal/
  claude/     Anthropic SDK enrichment client
  config/     Environment/SSM config loader
  domain/     Shared types
  handler/    HTTP handlers
  jina/       Jina search + reader client
  repository/ Postgres data access
migrations/   SQL migration files
terraform/
  modules/    api_gateway, eventbridge, lambda, rds, sqs
  environments/ dev.tfvars, prod.tfvars
scripts/
  build.sh    Compiles Lambda binaries for arm64
```
