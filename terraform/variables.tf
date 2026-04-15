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
