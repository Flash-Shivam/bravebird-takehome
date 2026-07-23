resource "aws_ecs_cluster" "main" {
  name = var.name
}

resource "aws_ecs_task_definition" "agent" {
  family                   = "${var.name}-agent"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 1024
  memory                   = 2048 # headless Chromium wants headroom
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.agent.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  container_definitions = jsonencode([{
    name        = "agent"
    image       = "${aws_ecr_repository.agent.repository_url}:latest"
    essential   = true
    stopTimeout = 30 # SIGTERM grace: flush a final screenshot + status write
    environment = [
      { name = "TABLE_NAME", value = aws_dynamodb_table.jobs.name },
      { name = "BUCKET", value = aws_s3_bucket.artifacts.bucket },
      { name = "JOB_TTL", value = var.job_ttl },
      { name = "AWS_REGION", value = var.region },
    ]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.agent.name
        awslogs-region        = var.region
        awslogs-stream-prefix = "agent" # stream = agent/agent/{task-id}, relied on by the /logs endpoint
      }
    }
  }])
}

resource "aws_ecs_task_definition" "controlplane" {
  family                   = "${var.name}-controlplane"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.controlplane.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  container_definitions = jsonencode([{
    name      = "controlplane"
    image     = "${aws_ecr_repository.controlplane.repository_url}:latest"
    essential = true
    portMappings = [{ containerPort = 8080, protocol = "tcp" }]
    environment = [
      { name = "TABLE_NAME", value = aws_dynamodb_table.jobs.name },
      { name = "BUCKET", value = aws_s3_bucket.artifacts.bucket },
      { name = "QUEUE_HIGH_URL", value = aws_sqs_queue.jobs["high"].url },
      { name = "QUEUE_LOW_URL", value = aws_sqs_queue.jobs["low"].url },
      { name = "CLUSTER", value = aws_ecs_cluster.main.name },
      { name = "AGENT_TASK_DEF", value = aws_ecs_task_definition.agent.family }, # family alone = latest revision
      { name = "AGENT_LOG_GROUP", value = aws_cloudwatch_log_group.agent.name },
      { name = "SUBNETS", value = join(",", data.aws_subnets.default.ids) },
      { name = "SECURITY_GROUP", value = aws_security_group.agent.id },
      { name = "MAX_CONCURRENT", value = tostring(var.max_concurrent) },
      { name = "RATE_PER_MINUTE", value = tostring(var.rate_per_minute) },
      { name = "JOB_TTL", value = var.job_ttl },
      { name = "AWS_REGION", value = var.region },
    ]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.controlplane.name
        awslogs-region        = var.region
        awslogs-stream-prefix = "controlplane"
      }
    }
  }])
}

resource "aws_ecs_service" "controlplane" {
  name            = "${var.name}-controlplane"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.controlplane.arn
  desired_count   = 1
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = data.aws_subnets.default.ids
    security_groups  = [aws_security_group.controlplane.id]
    assign_public_ip = true
  }
}
