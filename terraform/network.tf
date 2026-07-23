# Default VPC + public subnets, deliberately: a NAT gateway is the most
# expensive idle resource this design could have (~$32/mo) and adds nothing to
# the demo. Prod would be private subnets + NAT or VPC endpoints — see README.
data "aws_vpc" "default" {
  default = true
}

data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
}

# Agent tasks: egress only, zero ingress. Nothing can connect to a job.
resource "aws_security_group" "agent" {
  name        = "${var.name}-agent"
  description = "Agent tasks: egress only"
  vpc_id      = data.aws_vpc.default.id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# Controlplane: API port from the operator's IP only.
resource "aws_security_group" "controlplane" {
  name        = "${var.name}-controlplane"
  description = "Controlplane API"
  vpc_id      = data.aws_vpc.default.id

  ingress {
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = [var.allowed_cidr]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}
