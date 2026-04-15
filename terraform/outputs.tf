# Outputs added incrementally as modules are created:
# sqs_queue_url — Stage 6
# rds_endpoint  — Stage 7
# api_url       — Stage 9

output "sqs_queue_url" {
  value = module.sqs.queue_url
}

output "rds_endpoint" {
  value     = module.rds.endpoint
  sensitive = true
}

output "api_url" {
  value = module.api_gateway.api_url
}
