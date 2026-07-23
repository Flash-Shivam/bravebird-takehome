#!/usr/bin/env bash
# Print the controlplane API URL by resolving the Fargate task's public IP.
# (No ALB by design — see README tradeoffs.)
set -euo pipefail
cd "$(dirname "$0")/.."

TF="terraform -chdir=terraform"
REGION=$($TF output -raw region)
CLUSTER=$($TF output -raw cluster)
SERVICE=$($TF output -raw controlplane_service)

TASK=$(aws ecs list-tasks --region "$REGION" --cluster "$CLUSTER" --service-name "$SERVICE" \
  --query 'taskArns[0]' --output text)
if [[ "$TASK" == "None" || -z "$TASK" ]]; then
  echo "no controlplane task running yet" >&2
  exit 1
fi
ENI=$(aws ecs describe-tasks --region "$REGION" --cluster "$CLUSTER" --tasks "$TASK" \
  --query "tasks[0].attachments[0].details[?name=='networkInterfaceId'].value" --output text)
IP=$(aws ec2 describe-network-interfaces --region "$REGION" --network-interface-ids "$ENI" \
  --query 'NetworkInterfaces[0].Association.PublicIp' --output text)
echo "http://$IP:8080"
