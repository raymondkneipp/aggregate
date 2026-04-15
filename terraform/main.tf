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
  single_nat_gateway = true # not HA, but saves ~$32/mo

  enable_dns_hostnames = true
  enable_dns_support   = true
}

module "sqs" {
  source = "./modules/sqs"

  environment        = var.environment
  visibility_timeout = var.sqs_visibility_timeout
  alert_email        = var.alert_email
}

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

module "eventbridge" {
  source = "./modules/eventbridge"

  environment         = var.environment
  fetch_schedule      = var.fetch_schedule
  fetcher_lambda_arn  = module.lambda.fetcher_arn
  fetcher_lambda_name = module.lambda.fetcher_name
}

module "api_gateway" {
  source = "./modules/api_gateway"

  environment     = var.environment
  api_lambda_arn  = module.lambda.api_arn
  api_lambda_name = module.lambda.api_name
  aws_region      = var.aws_region
}
