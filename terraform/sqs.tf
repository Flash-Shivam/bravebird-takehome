# Two queues because SQS has no native priority; the dispatcher always drains
# high before low. Each queue gets its own DLQ after 3 failed receives.
locals {
  priorities = toset(["high", "low"])
}

resource "aws_sqs_queue" "dlq" {
  for_each = local.priorities
  name     = "${var.name}-jobs-${each.key}-dlq"
}

resource "aws_sqs_queue" "jobs" {
  for_each                   = local.priorities
  name                       = "${var.name}-jobs-${each.key}"
  visibility_timeout_seconds = 60
  message_retention_seconds  = 21600 # 6h: outlives any plausible backlog so queued jobs can't silently vanish

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq[each.key].arn
    maxReceiveCount     = 3 # must match maxReceiveCount in internal/dispatch
  })
}
