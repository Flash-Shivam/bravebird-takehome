#!/usr/bin/env bash
# Export the controlplane's env from terraform outputs, then exec the given
# command. Used by `make run-local`.
set -euo pipefail
cd "$(dirname "$0")/.."

TF="terraform -chdir=terraform"
export AWS_REGION=$($TF output -raw region)
export TABLE_NAME=$($TF output -raw table_name)
export BUCKET=$($TF output -raw artifacts_bucket)
export QUEUE_HIGH_URL=$($TF output -raw queue_high_url)
export QUEUE_LOW_URL=$($TF output -raw queue_low_url)
export CLUSTER=$($TF output -raw cluster)
export AGENT_TASK_DEF=$($TF output -raw agent_task_family)
export AGENT_LOG_GROUP=$($TF output -raw agent_log_group)
export SUBNETS=$($TF output -raw subnets)
export SECURITY_GROUP=$($TF output -raw agent_security_group)

exec "$@"
