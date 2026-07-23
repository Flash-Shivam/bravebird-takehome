variable "region" {
  type    = string
  default = "us-east-1"
}

variable "name" {
  description = "Prefix for all resource names"
  type        = string
  default     = "bravebird"
}

variable "allowed_cidr" {
  description = "CIDR allowed to reach the API (make deploy sets this to your IP)"
  type        = string
  default     = "0.0.0.0/0"
}

variable "max_concurrent" {
  description = "Global cap on simultaneously running agent tasks"
  type        = number
  default     = 10
}

variable "rate_per_minute" {
  description = "Per-user job submissions per minute"
  type        = number
  default     = 10
}

variable "job_ttl" {
  description = "Per-job time budget (reaper layers 1 and 2)"
  type        = string
  default     = "5m"
}

variable "max_ttl" {
  description = "Hard ceiling enforced by the reaper lambda (layer 3); > job_ttl"
  type        = string
  default     = "10m"
}
