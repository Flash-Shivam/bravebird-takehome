# Production Runbook

What stands between the current take-home deployment and a real production service, then how to operate it day-2. Ordered by "can't go live without it" → "should do soon" → "do when scale demands".

## P0 — Go-live blockers

### 1. Real authentication — DONE (shared-secret JWT)
HS256 Bearer JWTs are validated in `internal/api`; identity, rate-limit key,
and job ownership all derive from the verified `sub` claim. Mint tokens with
`make token SUB=alice`. Remaining hardening:
- Move `JWT_SECRET` from task-def env (visible via `ecs:DescribeTaskDefinition`) to an SSM SecureString `secrets` block.
- Swap the shared secret for a real issuer (Cognito/Auth0 OIDC) when there are actual users to onboard.

### 2. TLS + stable endpoint
The API is `http://<ephemeral-task-ip>:8080`, SG-scoped to the deployer's IP; the IP changes on every redeploy.
- ALB + ACM certificate + Route53 record. Point the ECS service at a target group.
- Delete the `allowed_cidr` public-IP hack once the ALB exists.

### 3. Private networking
Tasks run in public subnets with public IPs.
- Private subnets for controlplane + agent tasks, `assign_public_ip = false`.
- Either NAT gateway (~$32/mo) or VPC endpoints for ECR (api+dkr), S3, DynamoDB, CloudWatch Logs, SQS, ECS.
- Agent SG already has zero ingress — keep it.

### 4. Terraform remote state
State is a local file on one laptop; a second operator or a lost disk bricks the environment.
- S3 backend + `use_lockfile` (or DynamoDB lock table). Migrate with `terraform init -migrate-state`.

### 5. Reproducible deploys (kill `:latest`)
Both images push and run as `:latest` — no rollback target, and a service restart can silently pick up a newer image.
- Tag images with the git SHA, pass the tag as a terraform var into the task definitions.
- Move `make deploy` into CI (GitHub Actions + OIDC role, no long-lived AWS keys). Terraform plan on PR, apply on merge.

### 6. Durability guards
Demo conveniences that delete data must go:
- Remove `force_destroy` on the S3 bucket and `force_delete` on ECR.
- Enable DynamoDB PITR + deletion protection.
- Revisit retention: 3-day logs / 7-day artifacts — set to whatever compliance actually requires.

## P1 — First weeks in prod

### 7. Alarms (page-worthy)
None exist today. Minimum set, via CloudWatch → SNS/PagerDuty:
- DLQ depth > 0 (either queue) — the README already calls this the "page a human" signal.
- Reaper lambda errors > 0 — layer 3 is the hard TTL guarantee; if it breaks, runaway cost.
- Controlplane `RunningTaskCount` < 1, or crash-looping.
- API 5xx rate, high-queue oldest-message age (jobs not draining).
- Running agent task count at `MAX_CONCURRENT` for a sustained period (saturation).

### 8. Controlplane HA
Single task, in-memory dispatcher semaphore — a crash pauses dispatch until ECS restarts it (jobs queue safely, nothing is lost, but latency spikes).
- `desired_count = 2` across AZs requires moving the concurrency cap out of memory: DynamoDB lease/counter, or shard queues per dispatcher.

### 9. Reconciler scaling
Full-table scan for non-terminal jobs — fine to ~10k jobs.
- Sparse GSI on `status`, query instead of scan.

### 10. Security hardening
- Per-job STS credentials scoped to the job's S3 prefix (today all agents share one task role).
- Egress allowlist for the agent (filtering proxy) — a prompt-injected browser agent can currently reach anything on the internet.
- WAF on the ALB; ECR image scanning on push; dependency update automation.

## P2 — When scale demands

- **Warm pool** of paused agent tasks: Time-to-Task ~45s → ~2s.
- **Fargate Spot** capacity provider on the low queue (~70% cheaper; reclaim already handled by the reconciler path).
- **EventBridge ECS task-state events** replacing the 30s polling reconciler.
- **Service quotas**: Fargate on-demand vCPU quota and `RunTask` API throttles cap effective `MAX_CONCURRENT` — request increases before raising it past ~50.
- Token-bucket rate limiter (fixed window allows 2× edge bursts).

---

# Day-2 operations

## Deploy
```bash
make deploy          # test → build lambda → terraform → build/push images → apply
make api-url         # endpoint (service takes ~1 min)
```
Rollback (after P0.5 lands): re-apply with the previous image tag var. Today: `git checkout <prev> && make deploy`.

## Health check
```bash
API=$(make -s api-url)
TOKEN=$(make -s token SUB=canary)
curl -s -X POST $API/jobs -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"prompt":"golang generics","priority":"high"}'
# then poll GET /jobs/<id> until SUCCEEDED and confirm /artifacts returns frames
```

## Incident: jobs stuck QUEUED
1. Controlplane alive? `aws ecs describe-services --cluster bravebird --services bravebird-controlplane`
2. Dispatcher saturated? Check running agent task count vs `MAX_CONCURRENT` (10).
3. Fargate capacity/throttle errors in controlplane logs → messages redeliver automatically; sustained failures land in the DLQ after 3 attempts.

## Incident: DLQ has messages
1. `aws sqs receive-message` on the DLQ to inspect job IDs; `GET /jobs/<id>` shows the failure reason (reconciler copies ECS `stoppedReason`).
2. Fix the cause (bad image, quota, capacity), then redrive: SQS console "Start DLQ redrive" back to the source queue. Redelivery is safe — conditional writes make dispatch idempotent.

## Incident: runaway tasks / unexpected spend
- Layer-3 reaper lambda guarantees nothing outlives `MAX_TTL` (10m). If spend spikes anyway, check the lambda's error metric first — it's the only hard backstop.
- Manual kill: `aws ecs list-tasks --cluster bravebird` → `aws ecs stop-task`.

## Incident: agent jobs all failing `env_unhealthy`
Chromium isn't booting inside 30s — almost always a bad agent image push or ECR pull failure. Check `GET /jobs/<id>` for `stoppedReason`, redeploy the last known-good image.

## Tuning knobs (terraform/variables.tf)
| Var | Default | Meaning |
|---|---|---|
| `max_concurrent` | 10 | simultaneous agent tasks |
| `rate_per_minute` | 10 | per-user submissions/min |
| `job_ttl` | 5m | per-job budget (layers 1–2) |
| `max_ttl` | 10m | hard ceiling (layer-3 lambda) |

## Teardown
```bash
make destroy   # currently destroys DATA too — see P0.6 before prod
```
