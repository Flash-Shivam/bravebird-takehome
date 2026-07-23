output "region" {
  value = var.region
}

output "ecr_controlplane" {
  value = aws_ecr_repository.controlplane.repository_url
}

output "ecr_agent" {
  value = aws_ecr_repository.agent.repository_url
}

output "cluster" {
  value = aws_ecs_cluster.main.name
}

output "controlplane_service" {
  value = aws_ecs_service.controlplane.name
}

output "artifacts_bucket" {
  value = aws_s3_bucket.artifacts.bucket
}

output "table_name" {
  value = aws_dynamodb_table.jobs.name
}

output "queue_high_url" {
  value = aws_sqs_queue.jobs["high"].url
}

output "queue_low_url" {
  value = aws_sqs_queue.jobs["low"].url
}

output "agent_task_family" {
  value = aws_ecs_task_definition.agent.family
}

output "agent_log_group" {
  value = aws_cloudwatch_log_group.agent.name
}

output "agent_security_group" {
  value = aws_security_group.agent.id
}

output "subnets" {
  value = join(",", data.aws_subnets.default.ids)
}
