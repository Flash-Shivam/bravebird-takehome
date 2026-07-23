resource "aws_ecr_repository" "controlplane" {
  name         = "${var.name}-controlplane"
  force_delete = true
}

resource "aws_ecr_repository" "agent" {
  name         = "${var.name}-agent"
  force_delete = true
}

# Single table: job records keyed by ULID, plus "ratelimit#..." counter rows.
# expires_at TTL auto-cleans the counters.
resource "aws_dynamodb_table" "jobs" {
  name         = "${var.name}-jobs"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "job_id"

  attribute {
    name = "job_id"
    type = "S"
  }

  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }
}

resource "aws_s3_bucket" "artifacts" {
  bucket_prefix = "${var.name}-artifacts-"
  force_destroy = true # take-home: terraform destroy must leave nothing behind
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket                  = aws_s3_bucket.artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  rule {
    id     = "expire-artifacts"
    status = "Enabled"
    filter {}
    expiration {
      days = 7
    }
  }
}

resource "aws_cloudwatch_log_group" "controlplane" {
  name              = "/${var.name}/controlplane"
  retention_in_days = 3
}

resource "aws_cloudwatch_log_group" "agent" {
  name              = "/${var.name}/agent"
  retention_in_days = 3
}
