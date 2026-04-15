variable "environment" { type = string }
variable "vpc_id" { type = string }
variable "private_subnet_ids" { type = list(string) }
variable "lambda_memory_mb" { type = number }
variable "sqs_queue_url" { type = string }
variable "sqs_queue_arn" { type = string }
variable "dlq_arn" { type = string }
variable "aws_region" { type = string }

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

output "fetcher_arn" { value = aws_lambda_function.fetcher.arn }
output "fetcher_name" { value = aws_lambda_function.fetcher.function_name }
output "enricher_arn" { value = aws_lambda_function.enricher.arn }
output "enricher_name" { value = aws_lambda_function.enricher.function_name }
output "api_arn" { value = aws_lambda_function.api.arn }
output "api_name" { value = aws_lambda_function.api.function_name }
output "security_group_id" { value = aws_security_group.lambda.id }

