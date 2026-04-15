variable "environment" { type = string }
variable "vpc_id" { type = string }
variable "private_subnet_ids" { type = list(string) }
variable "lambda_sg_id" { type = string }
variable "db_instance_class" { type = string }
variable "aws_region" { type = string }

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

output "endpoint" { value = aws_db_instance.this.endpoint }
output "security_group_id" { value = aws_security_group.rds.id }
