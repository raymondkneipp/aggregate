variable "environment" { type = string }
variable "fetch_schedule" { type = string }
variable "fetcher_lambda_arn" { type = string }
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
