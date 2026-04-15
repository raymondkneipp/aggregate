terraform {
  backend "s3" {
    bucket         = "aggregate-tfstate-raymond"
    key            = "aggregate/terraform.tfstate"
    region         = "us-east-1"
    dynamodb_table = "aggregate-terraform-locks"
    encrypt        = true
  }
}
