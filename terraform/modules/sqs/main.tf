variable "environment" { type = string }
variable "visibility_timeout" { type = number }
variable "alert_email" { type = string }

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

output "queue_url" { value = aws_sqs_queue.jobs.url }
output "queue_arn" { value = aws_sqs_queue.jobs.arn }
output "dlq_url" { value = aws_sqs_queue.dlq.url }
output "dlq_arn" { value = aws_sqs_queue.dlq.arn }
output "sns_arn" { value = aws_sns_topic.alerts.arn }
