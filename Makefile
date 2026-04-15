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

migrate-lambda:
	aws lambda invoke \
	  --function-name $(ENV)-aggregate-migrate \
	  --payload '{}' \
	  /tmp/migrate-out.json \
	  && cat /tmp/migrate-out.json

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

migrate-aws: migrate-lambda
