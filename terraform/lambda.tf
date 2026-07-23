# Reaper layer 3: independent of DDB and the controlplane. Even if every other
# component is down or wrong, no agent task outlives max_ttl.
data "archive_file" "reaper" {
  type        = "zip"
  source_file = "${path.module}/../build/reaper/bootstrap" # built by `make build-reaper`
  output_path = "${path.module}/../build/reaper.zip"
}

resource "aws_iam_role" "reaper" {
  name = "${var.name}-reaper"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "lambda.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role_policy_attachment" "reaper_logs" {
  role       = aws_iam_role.reaper.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

resource "aws_iam_role_policy" "reaper" {
  name = "reaper"
  role = aws_iam_role.reaper.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["ecs:ListTasks", "ecs:DescribeTasks", "ecs:StopTask"]
      Resource = "*"
      Condition = {
        ArnEquals = { "ecs:cluster" = aws_ecs_cluster.main.arn }
      }
    }]
  })
}

resource "aws_lambda_function" "reaper" {
  function_name    = "${var.name}-reaper"
  role             = aws_iam_role.reaper.arn
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  timeout          = 30
  filename         = data.archive_file.reaper.output_path
  source_code_hash = data.archive_file.reaper.output_base64sha256

  environment {
    variables = {
      CLUSTER      = aws_ecs_cluster.main.name
      AGENT_FAMILY = aws_ecs_task_definition.agent.family
      MAX_TTL      = var.max_ttl
    }
  }
}

resource "aws_cloudwatch_event_rule" "reaper" {
  name                = "${var.name}-reaper"
  schedule_expression = "rate(1 minute)"
}

resource "aws_cloudwatch_event_target" "reaper" {
  rule = aws_cloudwatch_event_rule.reaper.name
  arn  = aws_lambda_function.reaper.arn
}

resource "aws_lambda_permission" "reaper" {
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.reaper.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.reaper.arn
}
