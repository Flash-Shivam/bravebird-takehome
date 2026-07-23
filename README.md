# Bravebird Take-Home — Ephemeral Computer-Use Job Runner

A backend that accepts "computer use" jobs over HTTP, provisions a **fresh Fargate task per job**, runs a placeholder browser agent (open browser → search → capture screenshots), streams its logs, uploads a screenshot-per-5s "flight recorder" trail to S3, and **destroys the environment** — with three independent layers making sure nothing outlives its TTL.

In plain terms: a user sends a text prompt ("golang generics"). The system spins up a fresh, isolated cloud container running a real Chromium browser, which searches for that prompt, screenshots along the way, uploads the frames to S3, and dies. The user polls an API for status and gets back links to the screenshots. Everything else in this repo exists to make that safe, fair, observable, and impossible to leak money.

This README has three parts: **[how it works](#part-i--how-it-works)** (a guided tour for understanding and demoing), **[design deep-dives](#part-ii--design-deep-dives)** (the take-home's focus areas), and **[production architecture](#part-iii--production-architecture)** (what changes to make this a real system).

```mermaid
flowchart LR
    C[curl / client] -->|POST /jobs| API
    subgraph controlplane [controlplane — one Fargate service]
        API --> RL[rate limiter]
        DISP[dispatcher]
        REC[reconciler]
    end
    API -->|job record| DDB[(DynamoDB)]
    API -->|job id| QH[SQS high]
    API -->|job id| QL[SQS low]
    QH --> DISP
    QL --> DISP
    QH -.-> DLQH[DLQ]
    QL -.-> DLQL[DLQ]
    DISP -->|RunTask| AGENT[agent task<br/>fresh Fargate task per job]
    AGENT -->|screenshots| S3[(S3 artifacts)]
    AGENT -->|status| DDB
    AGENT -->|stdout| CW[CloudWatch Logs]
    REC -->|"StopTask / sync"| AGENT
    LAMBDA[reaper lambda<br/>every 1 min] -->|"StopTask &gt; MAX_TTL"| AGENT
    API -->|"GET /jobs/:id/logs"| CW
    API -->|"presigned URLs"| S3
```

**Job lifecycle:** `QUEUED → LAUNCHING → RUNNING → SUCCEEDED | FAILED | TIMED_OUT`. Every transition is a DynamoDB conditional update — that's the idempotency mechanism that makes SQS's at-least-once delivery safe.

## Quick start

Prereqs: AWS credentials in your environment, Terraform ≥ 1.5, Docker, Go 1.26, `jq`.

```bash
make deploy        # terraform init → ECR repos → build+push images → everything else
make api-url       # prints http://<ip>:8080 (service takes ~1 min to start)
```

Trigger a job and watch it:

```bash
API=$(make -s api-url)
TOKEN=$(make -s token SUB=demo)

curl -s -X POST $API/jobs -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"prompt":"golang generics","priority":"high"}'
# => {"job_id":"01K...","status":"QUEUED"}

AUTH="Authorization: Bearer $TOKEN"
curl -s $API/jobs/01K... -H "$AUTH"            # status, timings, failure reason if any
curl -s $API/jobs/01K.../logs -H "$AUTH"       # tail agent logs (pass ?since=<next_since> to follow)
curl -s $API/jobs/01K.../artifacts -H "$AUTH"  # pre-signed URLs for every screenshot (15-min expiry)
```

The concurrency demo — 50 simultaneous jobs, live drain histogram, then a rate-limit burst:

```bash
make loadtest
```

Tear everything down (bucket has `force_destroy`, ECR has `force_delete` — nothing survives):

```bash
make destroy
```

Local dev: `make run-local` runs the controlplane on your laptop against the real AWS resources (it's all client-side SDK calls); `make agent-local` iterates on the browser task with no AWS and no Docker at all.

## Why this stack

- **Fargate, one task per job** — real kernel-level isolation per job with zero fleet management and zero idle cost: capacity exists only while a job runs. The price is a ~30–60s cold start per job (see tradeoffs).
- **Go** — two small static binaries; the dispatcher's concurrency cap is a buffered channel, the agent and its Chromium fit in one ~100MB image layer on top of `chromedp/headless-shell`.
- **SQS + DynamoDB** — both serverless, pay-per-request, and effectively free at take-home scale. SQS gives at-least-once delivery + DLQs for free; DynamoDB conditional writes give the state machine its atomicity.
- **Terraform, flat files, no modules** — it's one environment; modules would be structure for structure's sake.

Everything idle costs ≈ **$0.30/day** (one 0.25-vCPU controlplane task). No NAT, no ALB, no RDS.

---

# Part I — How it works

A guided tour for understanding (and demoing) the system. If the sections above are too crisp, read this once top to bottom and you can explain every box in the diagram.

## The five moving parts

### 1. Controlplane (one small Fargate container, `cmd/controlplane`)

One Go binary running three loops as goroutines:

- **API** (`internal/api`) — the HTTP surface. `POST /jobs`, `GET /jobs/{id}`, `/logs`, `/artifacts`. Validates your JWT, enforces the per-user rate limit, writes the job record, drops a message on a queue. It never runs jobs itself.
- **Dispatcher** (`internal/dispatch`) — an infinite loop: wait for a free concurrency slot → poll the queues (high-priority queue first, always) → claim the job → launch an agent container for it. The claim is the clever bit (see "the state machine" below).
- **Reconciler** (`internal/reap`) — a janitor that runs every 30 seconds comparing what DynamoDB *believes* is happening against what ECS says is *actually* happening, and fixing disagreements (a crashed agent that never reported back, a task running too long, etc.).

### 2. Agent (one Fargate container **per job**, `cmd/agent` + `internal/agentrun`)

Launched fresh for every single job, killed after. It:
1. Reads its job ID from an environment variable, fetches the job record from DynamoDB.
2. Marks the job `RUNNING`.
3. Boots headless Chromium (the image is `chromedp/headless-shell` — a stripped Chrome). If Chromium doesn't come up within 30s, the job fails as "env unhealthy" rather than hanging.
4. Drives the browser: open Wikipedia, type the prompt, search. (Wikipedia because Google bot-walls headless browsers; the assignment grades the infrastructure, not the search.)
5. Runs a **flight recorder**: a screenshot every 5 seconds, uploaded to S3 under `jobID/screens/...` as it goes — so even if the job dies mid-flight, you have frames up to the moment of death.
6. Writes the terminal status (`SUCCEEDED` / `FAILED` / `TIMED_OUT`) to DynamoDB and exits.

Why a fresh container per job? **Isolation.** Job A and Job B never share a filesystem, memory, or browser profile. A malicious page in job A cannot touch job B. This is the core design decision of the whole system.

### 3. DynamoDB (one table) — the source of truth

Every job is one row keyed by a ULID (`job_id`). The row carries status, timestamps, the ECS task ARN, the failure reason. **Every status change is a conditional write** — "set status to LAUNCHING *only if* it is currently QUEUED." If two workers try the same transition, exactly one wins and the other gets a clean conflict error it can ignore. This one idiom is what makes the whole system safe under retries and duplicates.

The same table also stores rate-limit counters (rows keyed `ratelimit#<user>#<minute>`), auto-deleted by a TTL.

### 4. SQS (two queues + two DLQs) — the buffer

`POST /jobs` doesn't launch anything; it enqueues. That means a burst of 50 submissions is absorbed instantly (all become `QUEUED`) and drained at the concurrency cap. Two queues implement priority: the dispatcher always polls **high** before **low**. The message body is *only the job ID* — the DynamoDB row is the real data, so a duplicated or replayed message can't cause a duplicated job.

Each queue has a **dead-letter queue**: a message that fails processing 3 times gets shunted there instead of looping forever. DLQ depth > 0 is the "page a human" signal.

### 5. S3 + CloudWatch — the outputs

Screenshots land in S3 (auto-deleted after 7 days). The API hands out 15-minute **presigned URLs** — the bucket itself is fully private. Agent logs go to CloudWatch with a predictable stream name derived from the task ARN, which is how `GET /jobs/{id}/logs` can tail them.

## Life of a job, step by step

```mermaid
sequenceDiagram
    participant C as client (curl)
    participant API as API
    participant DDB as DynamoDB
    participant Q as SQS
    participant D as Dispatcher
    participant ECS as ECS/Fargate
    participant AG as Agent + Chromium
    participant S3 as S3

    C->>API: POST /jobs (Bearer JWT, prompt, priority)
    API->>API: verify JWT, validate prompt, rate-limit check
    API->>DDB: create row {status: QUEUED}
    API->>Q: enqueue job_id (high or low queue)
    API-->>C: 202 {job_id, QUEUED}
    D->>Q: poll (high first)
    D->>DDB: conditional: QUEUED → LAUNCHING (the "claim")
    D->>ECS: RunTask (fresh agent container)
    D->>Q: delete message
    ECS->>AG: container starts
    AG->>DDB: LAUNCHING → RUNNING
    AG->>AG: boot Chromium, search Wikipedia
    loop every 5s
        AG->>S3: screenshot frame
    end
    AG->>DDB: RUNNING → SUCCEEDED ("captured 2 screenshots")
    C->>API: GET /jobs/{id} → SUCCEEDED
    C->>API: GET /jobs/{id}/artifacts
    API->>S3: presign each object
    API-->>C: 15-min URLs
```

In prose:

1. **Submit.** `curl -X POST $API/jobs` with a Bearer token. The API verifies the JWT signature (HS256, shared secret), takes your identity from the token's `sub` claim, checks you haven't exceeded 10 submissions/minute (a counter row in DynamoDB), writes the job row as `QUEUED`, and enqueues the ID. You get a `202` immediately — nothing has launched yet.
2. **Claim.** A dispatcher loop wakes up holding a free slot (max 10 concurrent agents). It receives the message and attempts the conditional write `QUEUED → LAUNCHING`. If a duplicate delivery of the same message races it, only one claim succeeds — the loser just deletes its copy.
3. **Launch.** The dispatcher calls `ecs:RunTask`: "start one container of the agent task definition, in these subnets, with `JOB_ID=...` in its environment." Then it deletes the queue message — from here on, the DynamoDB row and the reconciler own the job's fate. Fargate pulls the image and boots the container (~30–60s — this is the cold start a warm pool would fix, see Part III).
4. **Execute.** The agent flips the row to `RUNNING`, boots Chromium, does the browsing, streams screenshots to S3 every 5 seconds, and finally writes `SUCCEEDED` with a human-readable reason. The container exits; Fargate bills stop.
5. **Read.** You poll `GET /jobs/{id}` (only your own jobs — ownership is checked against your token's `sub`; other users get 404). `/logs` tails the agent's CloudWatch stream with a cursor for follow-mode. `/artifacts` lists the S3 prefix and presigns each object.

## What happens when things go wrong (the 3-layer reaper, in plain language)

The nightmare scenario for this design is a **zombie**: a Fargate container that never stops, billing forever. Three independent layers prevent it, each covering the failure of the one before:

- **Layer 1 — the agent polices itself.** The whole job runs inside a Go context with a 5-minute timeout (`JOB_TTL`). Timeout fires → agent writes `TIMED_OUT` and exits. Covers: slow pages, hung browsing. Fails if: the agent process itself crashes or hangs hard.
- **Layer 2 — the reconciler (every 30s).** Compares DynamoDB against ECS: job says `LAUNCHING` but no task appeared within 3 minutes → `FAILED`. Task stopped but the job never got a terminal status (agent crashed, OOM) → `FAILED`, with the ECS stop reason copied in. Task alive past `JOB_TTL` → stop it, `TIMED_OUT`. Covers: agent crashes. Fails if: the controlplane itself is down.
- **Layer 3 — the reaper Lambda (every 1 min, independent).** Doesn't read DynamoDB, doesn't know about the controlplane. Asks ECS one question: "any agent task older than 10 minutes (`MAX_TTL`)?" and kills it. Covers: *everything else being broken simultaneously.* This is the hard money-loss ceiling: no container outlives 10 minutes, period.

If the controlplane crashes, nothing is lost — submitted jobs sit safely in SQS (6h retention) until ECS restarts the service and dispatch resumes.

## The demo deployment (what `make deploy` built)

Everything is Terraform (`terraform/`, ~30 resources) plus two Docker images:

| Piece | AWS resource | Notes |
|---|---|---|
| Controlplane | ECS Fargate service ×1 on cluster `bravebird` | 0.25 vCPU — ~$9/mo, the only always-on compute |
| Agent | ECS task definition (no service — launched per job) | 1 vCPU / 2GB, Chromium needs the headroom |
| Images | 2 ECR repos, ARM64 | built by `make push-images` |
| State | DynamoDB `bravebird-jobs` | on-demand billing |
| Queues | SQS `bravebird-jobs-high/low` + 2 DLQs | |
| Artifacts | S3 `bravebird-artifacts-*` | 7-day auto-expiry |
| Reaper | Lambda + EventBridge 1-min schedule | ARM64 static Go binary |
| Auth secret | `random_password` in Terraform state → task env | demo-grade; Part III moves it to SSM |
| Network | default VPC, public subnets | API reachable **only from your IP** (SG rule set at deploy time); agent containers accept zero inbound |

Demo-isms to be aware of (all addressed in Part III): the API is plain HTTP on an ephemeral task IP that changes each deploy (`make api-url` re-resolves it), images deploy as `:latest`, and Terraform state is a local file.

## Worth showing off in a demo

- `make loadtest` — 50 jobs from 10 users, mixed priorities: watch the queue drain 10-at-a-time (the concurrency cap), high-priority jobs jump the line, and a 15-in-a-row burst get `429`s (the rate limiter).
- `GET /jobs/<id>/logs` while a job runs — live structured logs from inside the agent container.
- Fetch someone else's job ID — `404`, because ownership derives from the JWT.
- `make agent-local` — the same agent code running on your laptop with no AWS at all (screenshots land in `./tmp`). Proves the agent is testable in isolation.

## The three ideas to remember

1. **Conditional writes are the concurrency story.** No locks, no leader, no transactions — every state change says "only if currently X," and DynamoDB rejects the losers. Duplicates and retries become no-ops instead of bugs.
2. **A container per job is the security story.** Jobs can't see each other because they never share anything. The blast radius of one bad page is one disposable container with narrowly-scoped credentials and a 10-minute lifespan.
3. **Three independent reapers are the cost story.** Each layer assumes everything above it is broken. The last one depends only on ECS itself, so "runaway Fargate bill" requires AWS itself to fail.

---

# Part II — Design deep-dives

## Deep-dive 1: Concurrency & Scheduling

- **50 simultaneous requests:** the API only does two fast writes (DynamoDB + SQS), so all 50 are accepted in milliseconds. The queue absorbs the backlog; the dispatcher drains it at `MAX_CONCURRENT` (default 10) simultaneous Fargate tasks, enforced by a buffered-channel semaphore — a slot is taken before `RunTask` and released when the job reaches a terminal state. `make loadtest` shows the histogram draining.
- **Priority:** two SQS queues; the dispatcher always polls `high` before falling back to `low`. Two queues beat message attributes because SQS has no native priority — with attributes you'd have to receive a message before knowing you didn't want it yet.
- **Per-user rate limiting:** fixed-window counter in DynamoDB (`ADD count 1` with a `count < N` condition; TTL attribute auto-expires the rows). Exceeding it returns 429. *Ceiling: fixed windows allow a 2× burst at window edges; token bucket is the upgrade.*
- **Poison messages:** 3 failed receives → DLQ (per queue). DLQ depth is the "page a human" alarm signal.

## Deep-dive 2: Observability — the flight recorder

- **Session replay:** the agent screenshots the browser every 5 seconds and uploads each frame to S3 *as it goes* — a crash mid-run still leaves a replayable trail up to the moment of death. `GET /jobs/{id}/artifacts` returns 15-minute pre-signed URLs for every frame.
- **Log streaming:** agent stdout is structured JSON (`log/slog`) shipped by the `awslogs` driver; the stream name is deterministic from the task ARN, so `GET /jobs/{id}/logs?since=<cursor>` tails CloudWatch directly. Polling with the returned cursor gives a `tail -f` experience with zero extra infrastructure.
- **Hung-VM health check (bonus):** the agent bounds Chromium startup at 30s. If the browser never comes up, the job fails fast as `env_unhealthy` — the environment was dead before the task even started, and the status says exactly that.
- **Failure forensics:** when a task dies without reporting (OOM, image pull failure, crash), the reconciler copies ECS's `stoppedReason` onto the job record, so `GET /jobs/{id}` tells you *why*.

## Deep-dive 3: Cost control — three concentric reapers

| Layer | Mechanism | Survives |
|---|---|---|
| 1 | `context.WithTimeout(JOB_TTL)` inside the agent itself | agent task hang |
| 2 | Reconciler (controlplane, every 30s): DynamoDB ⇄ ECS sync; stops over-TTL tasks, fails jobs whose tasks died silently or never launched | agent crash / harness bug |
| 3 | **Independent Lambda** (EventBridge, every 1 min): `ListTasks` → `StopTask` anything older than `MAX_TTL`. No DynamoDB, no controlplane — keyed purely on ECS truth | the entire controlplane being down |

ECS has no native max-task-duration, so layer 3 is the actual hard guarantee: **even if every other line of code is wrong, no task outlives `MAX_TTL`** (default 10m). Also cost-relevant: S3 artifacts auto-expire after 7 days, log retention is 3 days, and `stopTimeout=30` gives a reaped agent 30s to flush a final screenshot and status write.

## Failure modes

| Failure | Behavior |
|---|---|
| `RunTask` fails (capacity, throttle) | Message isn't deleted → SQS redelivers after the visibility timeout (natural backoff); job reverts to `QUEUED`. On the 3rd attempt the job is marked `FAILED` and the message goes to the DLQ |
| Duplicate SQS delivery | The `QUEUED→LAUNCHING` conditional write fails → message deleted, no second task launched |
| Image pull failure / container dies at boot | Task stops; reconciler surfaces ECS's `stoppedReason` on the job |
| Dispatcher crashes between claiming a job and `RunTask` | Job is stuck `LAUNCHING` with no task; reconciler fails it after `LAUNCH_DEADLINE` (redelivered messages deliberately skip non-QUEUED jobs) |
| Agent crashes mid-run | Terminal status write is lost → reconciler sees STOPPED task vs non-terminal job → `FAILED`; screenshots up to the crash are already in S3 |
| Chromium hangs before the task starts | 30s boot deadline → `env_unhealthy` |
| Flaky network | AWS SDK adaptive retry mode on every client; ECS restarts the controlplane service if it dies |
| All of the above at once | Layer-3 Lambda still kills every over-age task |

## Security posture (demo-tier by design)

Done: separate IAM task roles — the agent can only `s3:PutObject` artifacts and read/update job records; it cannot launch tasks, read queues, or see other jobs' credentials. Agent security group has **zero ingress**. Each job gets a fresh task: no shared filesystem, no shared memory between Job A and Job B. Artifacts bucket blocks all public access; URLs are short-lived presigns. No secrets in images; the JWT signing secret rides in the controlplane task-def env (SSM SecureString would fix that — see Part III).

Done: API authentication — HS256 Bearer JWTs verified in `internal/api`; identity, rate limiting, and job ownership derive from the `sub` claim (`make token SUB=alice` mints one). Next in prod: per-job STS credentials scoped to the job's S3 prefix, egress controls, private subnets + VPC endpoints (all in Part III).

## Tradeoffs, honestly

- **Public subnets, no NAT:** a NAT gateway is ~$32/mo idle — the single most expensive resource this design could have — for zero demo value. Prod: private subnets + NAT (or VPC endpoints for ECR/S3/DynamoDB/logs) and no public IPs on tasks. On EC2 you'd also block IMDS; Fargate tasks get task-role credentials without an instance metadata service to firewall.
- **No ALB:** the API is one task reached by its public IP (`make api-url`), SG-restricted to your IP. Prod: ALB + ACM + a stable DNS name.
- **Single dispatcher:** the in-memory semaphore assumes one controlplane instance. Honest ceiling; the upgrade is a distributed cap (Part III, D3).
- **Poll-based log tail** vs WebSocket/CloudWatch Live Tail: chose zero infra; the cursor API makes a follow-mode client trivial.
- **Reconciler scans** the table for non-terminal jobs: fine to ~10k jobs; a sparse GSI is the upgrade (Part III, D5).
- **Placeholder agent searches Wikipedia**, not Google/DuckDuckGo — both bot-wall headless Chromium (DuckDuckGo literally asked the agent to identify pictures of ducks). The assignment evaluates the infrastructure, so the task target just needs to be deterministic.

---

# Part III — Production architecture

The delta between the demo above and a system you'd let strangers pay for. The main body designs for **small real prod** — 1–10k jobs/day, tens of concurrent agents, one region. The [final section](#10-when-scale-demands-100kday-hundreds-concurrent) sketches the 100k+/day evolution. Ordering principle (shared with the [RUNBOOK](RUNBOOK.md)): correctness → reachability → redundancy → cost optimization, never the reverse.

## 1. What deliberately does not change

The rewrite instinct was resisted. These parts are already prod-grade patterns and survive intact:

- **The DynamoDB conditional-write state machine** (`internal/store`). It *is* the idempotency mechanism under SQS at-least-once — and it is already multi-writer-safe, which Phase 2 exploits directly.
- **The 3-layer reaper**, especially the ECS-state-only Lambda. It depends on nothing and is the hard guarantee that no task outlives `MAX_TTL`. That's the cost ceiling; it stays exactly as is.
- **IAM role separation** (`terraform/iam.tf`).
- **Two-queue priority, job-id-only messages.**
- **Ephemeral task-per-job isolation.** The warm pool (§10) must preserve use-once semantics — a pool of *reusable* agents would be a different, worse product.
- **Polling log tail, single DDB table, flat Terraform.** Modules arrive with a second environment, not before.

## 2. Correctness fixes first

A prod plan that misses real bugs is decoration. Code review for this document found four; three are invisible in the demo and only bite under production traffic.

| # | Bug | Fix | Phase |
|---|-----|-----|-------|
| 1 | The reconciler's `ListNonTerminal` scan filter (`NOT status IN terminal`) matches `ratelimit#` rows — they have no `status` attribute, so the filter is true. They unmarshal as empty jobs, fall into the launch-deadline branch (`internal/reap/reap.go:86–97`), and get a garbage `FAILED` status written onto rate-limit keys within 30s of creation. | Interim: skip `ratelimit#`-prefixed ids (2 lines). Structural: the sparse GSI (D5) — rate-limit rows never enter the index. | 1 / 2 |
| 2 | Poison path: if `GetJob` keeps erroring or a non-conflict transition error occurs, the message dead-letters after 3 receives while the job stays `QUEUED`. The reconciler explicitly skips `QUEUED` (`reap.go:83–84`), nothing consumes the DLQs, and the DLQs use SQS's default 4-day retention — so the evidence evaporates and the job is stranded forever. | Reconciler fails `QUEUED` jobs older than the 6h message retention — past that the message provably cannot redeliver. Plus D16: 14-day DLQ retention + depth alarm. | 1 |
| 3 | Fixed-window rate limiter allows a 2× burst at window edges (self-flagged in `internal/store/store.go:179`). | Token bucket. At 10/min the burst is 20 requests — a documented ceiling, not an incident. | 4 |
| 4 | Slot release lags job completion by up to 15s (`trackInflight` polls DDB every 15s, `internal/dispatch/dispatch.go:205`) — roughly 6–12% throughput loss at saturation on 1–2 minute jobs. | EventBridge task-state fast path (D6) shrinks it to ~1s. Not worth touching at small tier. | 4 |

## 3. Target architecture (small real prod)

```mermaid
flowchart LR
    U[client] -->|HTTPS jobs.example.com| R53[Route53] --> ALB[ALB + ACM<br/>public subnets]
    subgraph vpc [dedicated VPC]
        ALB
        CP[controlplane ×2<br/>private subnets, multi-AZ]
        AG2[agent tasks<br/>private subnets, use-once]
        NAT[NAT gateway ×1]
    end
    ALB --> CP
    CP -->|conditional writes| DDB2[(DynamoDB<br/>sparse active GSI<br/>concurrency counter<br/>PITR on)]
    CP --> QH2[SQS high] & QL2[SQS low]
    QH2 -.-> DLQH2[DLQ 14d + alarm]
    QL2 -.-> DLQL2[DLQ 14d + alarm]
    CP -->|RunTask + per-job STS| AG2
    AG2 -->|screenshots| S32[(S3 artifacts<br/>gateway endpoint)]
    AG2 -->|status| DDB2
    AG2 -->|browse| NAT --> NET((internet))
    EB[EventBridge 1min] --> L3[reaper lambda<br/>unchanged]
    L3 -->|StopTask > MAX_TTL| AG2
    CW2[alarms: DLQ, reaper errors,<br/>oldest msg age, 5xx, saturation] --> SNS[SNS → pager]
    GH[GitHub Actions OIDC<br/>SHA tags, tf plan/apply] --> ECR[(ECR)]
```

A job's path: client hits `https://jobs.example.com` through the ALB (TLS terminates at ACM), the API validates the JWT and writes the record + enqueues. A dispatcher on either controlplane replica claims it via the conditional write, takes a slot from the DynamoDB concurrency counter, and launches an agent task in a private subnet with STS credentials scoped to that job. The agent browses through the NAT, writes frames to S3 through the free gateway endpoint, and finishes. The reconciler and reaper Lambda are unchanged.

What changed vs. today, and the specific weakness each change removes:

- **ALB + ACM + Route53** → replaces `http://<ephemeral-task-ip>:8080` whose IP rotates every deploy and whose SG allowlist defaults to `0.0.0.0/0` if you forget a `-var`.
- **Dedicated VPC, private subnets, no public IPs** → today every task has a public IP in the default VPC.
- **DDB concurrency counter** → replaces the in-memory semaphore that silently doubles the global cap at 2 replicas and forgets everything on restart.
- **Controlplane ×2 across AZs** → today a crash pauses dispatch *and* layer-2 reconciliation until ECS restarts the single task.
- **Sparse `active` GSI** → replaces the full-table scan (and structurally kills bug 1).
- **Per-job STS credentials** → today all agents share one task role; any agent can write any job's artifacts and rows.
- **SHA-tagged images via CI** → replaces `:latest` from a laptop with local Terraform state.
- **Alarms** → today the "page a human" signal (DLQ depth) is documented but not wired to anything.

## 4. Decisions

### D1 — Front door: ALB, not API Gateway

ALB + ACM certificate + Route53 record, target group on the existing ECS service, health check on the `/healthz` that's already unauthenticated. Auth already lives in the `requireAuth` middleware, so API Gateway's authorizers, per-request pricing, 29-second integration timeout, and VPC-link plumbing buy nothing. Kill the `allowed_cidr` variable — its default is `0.0.0.0/0`, which makes the current "SG-scoped to deployer IP" one forgotten `-var` away from an open, unauthenticated-transport port. ~$21/mo.

### D2 — Egress: one NAT gateway + free gateway endpoints

Agents browse the arbitrary internet, so VPC-endpoints-only is not an option — NAT is mandatory. The cost math that matters: ECR layer blobs are served from S3, so a **free S3 gateway endpoint** takes the ~100MB-per-job image pull off the NAT — at 5k jobs/day that's ~$22/day of NAT data processing avoided. DynamoDB gateway endpoint: also free, also on. What's left crossing the NAT is actual browse traffic (~10MB/job ≈ $70/mo at 5k/day) plus small API chatter. Interface endpoints ($7.30/mo/AZ each, ~5 services) are dismissed at this tier: they cost more than the traffic they'd save.

### D3 — Distributed concurrency cap: DynamoDB atomic counter

One item, `concurrency#global` — the `ratelimit#` prefix convention already proves non-ULID keys coexist safely in this table. Claim = `ADD active :1` with condition `active < :max`, taken exactly where `d.sem <- struct{}{}` sits today (`dispatch.go:61`); release = `ADD active :-1` where `trackInflight` frees slots. This is the same conditional-write idiom the codebase already trusts for job claiming.

The failure mode, honestly: a dispatcher crash between increment and `RunTask`, or a lost decrement, leaks counter capacity. The heal: the reconciler already computes ground truth every 30s (non-terminal jobs × ECS task state), so it writes `SET active = :observed` each pass — leaks self-clear in ≤30s, and any transient overshoot in that window is bounded by the layer-3 Lambda anyway.

Dismissed: per-slot lease items (N items of machinery for the same guarantee); queue sharding with static per-dispatcher caps (strands capacity when one dispatcher idles, and breaks high-before-low draining across shards).

### D4 — Dispatcher HA: active-active, no leader election

The key observation: the `QUEUED → LAUNCHING` conditional claim (`dispatch.go:129`) is *already* multi-dispatcher-safe — SQS distributes messages, the conditional write dedupes, and only the in-memory semaphore is process-local. D3 removes it. So: `desired_count = 2`, zero coordination code. The reconciler runs on both replicas too; every action it takes is idempotent (`Finish` is conditional, `StopTask` is idempotent), so a duplicate pass costs a redundant query, not a wrong write. Leader election is dismissed: it adds a split-brain failure mode to solve a problem the state machine already solved.

### D5 — GSI: sparse `active` index, not a status-keyed one

GSI partition key = a constant attribute `active = "1"`, sort key = `created_at`. Written in `CreateJob`, `REMOVE active` in the same update expression as `Finish`. Terminal jobs and `ratelimit#` rows never appear in the index, so the reconciler's full-table scan (`store.go:151` — the code's own comment names this exact upgrade) becomes one Query, and bug 1 becomes structurally impossible. Honest ceiling: a single-value partition key caps at one partition's throughput — fine to hundreds of thousands of concurrently active jobs; shard to `active#0..9` when scale demands (§10). A status-keyed GSI is dismissed: three queries instead of one, and it indexes terminal jobs forever.

### D6 — Reconciliation: keep the 30s poll; EventBridge is a scale-tier fast path, never the only path

EventBridge ECS task-state-change events are at-least-once with no delivery-latency guarantee, so the poller must exist regardless — which means at small tier EventBridge adds a component without removing one. At scale it earns its place: crashed-agent detection drops from ≤30s to ~1s and slot-release lag (bug 4) from ≤15s to ~1s, with the poller demoted to a 2-minute sweeper for dropped events. Event-driven-only reconciliation is the classic way to strand jobs silently.

### D7 — Per-job credentials: controlplane-vended STS with inline session policies

At `RunTask` time the dispatcher calls `sts:AssumeRole` on a new agent-base role with an inline session policy: `s3:PutObject` on `${bucket}/${job_id}/*` and DynamoDB `Get/UpdateItem` with `dynamodb:LeadingKeys = ["${job_id}"]`. The credentials ride the same `ContainerOverride.Environment` that already carries `JOB_ID` (`dispatch.go:182–187`). STS's 15-minute session minimum conveniently exceeds `MAX_TTL = 10m`, so mid-job expiry is impossible by construction. Exposure via `ecs:DescribeTasks` is IAM-gated and the creds are dead in ≤15 minutes; accepted. Dismissed: per-job IAM roles (API limits + garbage collection); agent-pulls-creds-from-controlplane (opens an agent→controlplane network path that currently doesn't exist and shouldn't).

### D8 — Agent egress control: SG-level now, Network Firewall priced but not bought

Ranked prompt-injection blast radius (see §5): internal pivot is killed for free by an agent SG egress rule denying the VPC CIDR (today it's egress-all) plus private subnets — and Fargate has no IMDS to steal from, worth stating. Attacking the internet from our NAT IPs is accepted at small tier, abuse contact on the EIPs. AWS Network Firewall with domain allowlisting is real money (~$285/mo + $0.065/GB) and only coherent if the product can constrain browse targets — priced in §10, not bought now. A TLS-inspecting proxy is dismissed in one line: it breaks half the web and you become a CA; that's two products.

### D9 — Auth: keep the JWT, move the secret; OIDC when there are external users

`internal/auth` and the `sub`-derived identity/ownership/rate-key already give the right seams; swapping HS256-shared-secret for RS256-against-JWKS is a ~50-line change inside one package whenever a real issuer exists. The actual current vulnerability is the secret living in Terraform state and plaintext task-def env (readable via `ecs:DescribeTaskDefinition`) — fixed by generating it out-of-band into an SSM SecureString and using the task-def `secrets` block. Cognito now is gold-plating: migrating auth providers before having users. Secrets Manager: $0.40/secret/mo for rotation machinery nobody asked for; SSM standard tier is free.

### D10 — Multi-AZ: two controlplane tasks, one NAT, one region

Controlplane ×2 across AZs behind the ALB — which needs D3 first; that dependency is why Phase 2 is ordered after Phase 1. Agents spread across 2–3 private subnets. DynamoDB/SQS/S3 are already regional. **One NAT, deliberately**: lose that AZ and agents everywhere lose egress until it recovers — jobs fail, the reconciler cleans up, the queue absorbs retries. A second NAT is $33/mo of insurance bought at scale tier, not before. Single region, full stop; multi-region appears only as §10's cellular sketch.

### D11 — Artifacts: presigns stay, bucket policy hardens, no versioning, no KMS

The 15-minute presigned GET is right — clients re-fetch the listing for fresh URLs. Add an `aws:SecureTransport` deny to the bucket policy, drop `force_destroy`, and make the 7-day lifecycle a variable — compliance owns that number, not the code. Versioning: dismissed — screenshots are immutable write-once and jobs are re-runnable; versioning doubles storage for zero recovery value. KMS-CMK: dismissed with a number — a screenshot every 5 seconds makes per-request KMS charges a tax on the flight recorder; SSE-S3 is already on.

### D12 — Observability: EMF from the existing slog output; no agents, no tracing

Both binaries already emit structured JSON via `slog` to the awslogs driver; CloudWatch EMF means metrics are just log lines — zero new runtime components. Metric set: dispatch latency, slot-wait time, active count vs cap, reconciler repairs by type, TTL reaps, launch failures. `ApproximateAgeOfOldestMessage` and Lambda errors come free. Alarms are the RUNBOOK P1 list, now actually built: DLQ depth > 0, reaper Lambda errors > 0, `RunningTaskCount` < 1, API 5xx rate, oldest-message age, sustained saturation at `MAX_CONCURRENT` — all → SNS → pager. X-Ray: dismissed — the trace of a job already exists as the job record, a deterministic log stream, and timestamped flight-recorder frames; distributed tracing across a linear one-hop pipeline is plumbing without questions it can answer.

### D13 — CI/CD: GitHub Actions OIDC, SHA tags, pinned revisions, remote state

Pipeline: test → build ARM64 images tagged with the git SHA → push → `terraform plan` on PR, `apply` on merge, `image_tag` as a variable. Rollback = re-apply the previous SHA. Two codebase-specific traps this fixes: `:latest` means a controlplane restart can silently pick up a newer image, and `AGENT_TASK_DEF` is passed as a bare family — "family alone = latest revision" (`terraform/ecs.tf`) — so a deploy mid-backlog silently flips agent versions for already-queued jobs. Pin the full task-definition revision ARN. State: S3 backend with `use_lockfile` (Terraform ≥ 1.10 — no DynamoDB lock table needed). ECR: scan-on-push, drop `force_delete`, lifecycle keep-last-20.

### D14 — DynamoDB capacity: on-demand at both tiers

10k jobs/day × ~8 writes/job ≈ $3/mo; even 100k/day is ~$40/mo. Provisioned-with-autoscaling would halve a number that small in exchange for throttle-tuning ops. Revisit only if the bill says so. Closed.

### D15 — Fargate Spot on the low queue only

Short jobs (≤5m TTL) are the ideal Spot workload: low interruption probability, cheap retry. Change `RunTask` (`dispatch.go:169`) from `LaunchType` to a capacity-provider strategy keyed on the job's priority. The reconciler already *detects* reclaim (STOPPED task + non-terminal job) but currently marks it `FAILED` — teach it to recognize the Spot interruption stop code and transition back to `QUEUED` + re-enqueue, bounded by a `retry_count` attribute to prevent loops. The high queue stays on-demand; that's what priority means. ~70% off the low-queue compute line.

### D16 — DLQs: 14-day retention, depth alarm, no auto-redrive

Today nothing consumes the DLQs *and* they silently drop messages after SQS's default 4 days (`terraform/sqs.tf` sets no retention) — the evidence of a poison job evaporates. Fix: 14-day retention, depth > 0 alarm, and the reconciler `QUEUED` age-out from bug 2 as the correctness backstop. Redrive stays a human action (the RUNBOOK already documents it): auto-redriving a poison message is a loop with extra steps.

## 5. Security posture and trust boundaries

| Boundary | Enforced by |
|---|---|
| internet → API | ALB + ACM TLS; WAF rate rule; JWT validation in `requireAuth` |
| JWT subject → resources | Ownership + rate key from verified `sub`; cross-user reads 404 |
| controlplane → AWS | Task role: queues, table, `RunTask` on the agent family, `PassRole` on exactly two roles |
| agent → everything | Per-job STS session policy; private subnet; SG deny-to-VPC; no IMDS on Fargate |

The centerpiece is the agent: **it is untrusted the moment Chromium loads a page.** A prompt-injected or malicious page is arbitrary influence over a process holding AWS credentials inside your VPC. The design question is not "how do we prevent injection" (you can't) but "what can a hijacked agent reach":

1. **Its own credentials** — reduced by D7 to one S3 prefix and one DynamoDB row, dead in ≤15 minutes.
2. **The internal network** — killed by private subnets + SG egress deny-to-VPC-CIDR. There is no agent→controlplane path and none should be added.
3. **The internet, from our IPs** — accepted at small tier; Network Firewall is the priced upgrade (D8, §10).

Plus: artifacts bucket blocks public access with a TLS-only policy (D11), secrets ride SSM SecureStrings (D9), images are scanned on push (D13).

## 6. Failure modes the new topology introduces

| Failure | Automatic behavior | Residual |
|---|---|---|
| One controlplane replica dies | Other replica keeps dispatching; ALB drains the target | none |
| AZ loss (controlplane AZ) | Survivor replica in other AZ carries load | none |
| AZ loss (NAT AZ) | Agents lose egress; jobs fail; reconciler cleans; queue absorbs resubmits | manual: rerun failed jobs; upgrade: 2nd NAT (§10) |
| Spot reclaim (low queue) | Reconciler sees Spot stop code → re-queue, `retry_count`-bounded | none |
| Concurrency counter leak (dispatcher crash) | Reconciler resets counter to observed truth ≤30s | none |
| EventBridge event lost (scale tier) | 2-min poller sweep catches it | none |
| Poison message | 3 receives → DLQ (14d) + depth alarm; reconciler fails the stranded `QUEUED` job after 6h | human: inspect DLQ, redrive or drop |
| Both replicas down | Dispatch pauses; jobs queue safely (6h retention); L3 Lambda still enforces `MAX_TTL` | page: `RunningTaskCount` alarm |

## 7. SLOs and alarms

| SLO | Target | Measured from |
|---|---|---|
| API availability (30d) | 99.9% — 99.95 would be dishonest for single-region ×2 tasks | ALB metrics |
| Time-to-task (accepted → RUNNING, high queue, below cap) | p50 ≤ 60s, p95 ≤ 120s (warm pool tier: p50 ≤ 5s, p95 ≤ 30s) | `started_at − created_at`, already on every job record |
| No-zombie guarantee | 99.99% of launched jobs terminal within `MAX_TTL` + 2min | GSI query for over-age active jobs — this metric *is* the L2/L3 health check |
| Artifact retrieval | 99.9% availability; durability inherits S3's 11 nines | `/artifacts` non-5xx |
| Flight-recorder completeness | ≥95% of expected frames (duration ÷ 5s) on succeeded jobs | frame count vs duration, both already recorded |

Error-budget policy in one sentence: DLQ depth > 0, reaper-Lambda errors > 0, and zombie count > 0 page immediately regardless of remaining budget — they are correctness signals, not latency signals.

## 8. Rollout

Phases 0–3 total roughly **12 engineer-days**. Everything is additive; the only downtime is the VPC cutover in Phase 1, and the queue's 6h retention absorbs it — jobs queued during the cutover dispatch after.

- **Phase 0 — Stop the bleeding** (1–2 days). S3 remote state + lockfile; SHA-tagged images + pinned agent task-def revision; DynamoDB PITR + deletion protection; drop `force_destroy`/`force_delete`; `JWT_SECRET` → SSM; DLQ retention 14d. *Unblocks everything — no later phase is safe to deploy on `:latest` from a laptop with local state.*
- **Phase 1 — Front door + correctness** (3–4 days). Dedicated VPC: private subnets, one NAT, S3+DDB gateway endpoints, `assign_public_ip = false`; ALB + ACM + Route53, delete `allowed_cidr`; agent SG deny-to-VPC; bug 1 guard + bug 2 age-out; GitHub Actions pipeline. *Unblocks Phase 2: two tasks need a load balancer, and safe deploys need CI.*
- **Phase 2 — Two of everything** (3–4 days). DDB concurrency counter + reconciler heal (D3); `desired_count = 2` multi-AZ (D4); sparse `active` GSI (D5); EMF metrics + full alarm set + one dashboard (D12). *After this, `MAX_CONCURRENT` is finally a real global invariant instead of a per-process accident.*
- **Phase 3 — Locked doors + cheaper compute** (3–5 days). Per-job STS (D7); artifacts bucket policy (D11); ECR scan-on-push; ALB rate-based WAF rule only — managed rule groups are SQLi noise on a JSON-only JWT API; Fargate Spot on the low queue + reclaim-requeue (D15).
- **Phase 4 — Scale tier.** Not scheduled; triggered by numbers: sustained >30 concurrent or >20k jobs/day. Contents in §10.

## 9. Cost, honestly

us-east-1, ARM64 Fargate ($0.0395/hr per 1 vCPU + 2GB agent-task-hour):

| Line | Today (demo) | Small prod (5k jobs/day, ~2min jobs) | Scale (100k jobs/day) |
|---|---|---|---|
| Controlplane | $9 (×1) | $18 (×2) | $40 |
| Agent compute | ~$0 | $200 → **~$160 with Spot** | $4,000 → ~$2,900 with Spot |
| Warm pool | — | — | $600–1,500 |
| ALB (+ WAF rule) | — | $26 | $60 |
| NAT + data | — | $53 | $1,400 ← the scale-tier problem line |
| DynamoDB | ~$0 | $5 | $50 |
| S3 (PUTs dominate: ~24 frames/job) | ~$0 | $20 | $400 |
| CloudWatch | ~$0 | $20 | $200 |
| SQS, EventBridge, Route53, misc | ~$0 | $5 | $30 |
| **Total /mo** | **~$9** | **~$310–360** | **~$5,600–6,600** |

Unit economics: ~$0.002–0.003/job at both tiers. Compute is ~60%, NAT data ~20%, S3 PUTs ~7%. Two traps deliberately avoided: ECR pulls through the NAT (the free S3 gateway endpoint saves ~$22/day at 5k jobs/day — without it, NAT data processing would exceed the compute bill), and KMS-CMK on the flight recorder (a per-request charge every 5 seconds).

## 10. When scale demands (100k+/day, hundreds concurrent)

- **Warm pool.** Pre-launched paused agent tasks cut time-to-task ~45s → ~2s. Must stay use-once: a task serves one job, then dies. Conflict to solve: per-job STS creds are injected at `RunTask` (D7), but pool tasks launch *before* job assignment — creds move to a vend-at-assignment call, which means a minimal, authenticated agent→controlplane endpoint that D7 explicitly avoided. That tradeoff is the price of the warm pool; make it consciously.
- **EventBridge fast path** (D6): task-state events drive reconciliation and slot release (~1s); the poller demotes to a 2-minute sweeper. Fixes bug 4.
- **Sharded GSI**: `active` → `active#0..9` (job-id hash), reconciler queries all shards. Removes the single-partition ceiling from D5.
- **Token-bucket rate limiter** (bug 3), still as DDB conditional writes.
- **Quotas before code**: Fargate on-demand vCPU quota and `RunTask` API throttles are the real ceiling past `MAX_CONCURRENT` ≈ 50 — request increases first, then tune.
- **Second NAT** ($33/mo): removes the single-AZ egress ceiling from D10.
- **Network Firewall revisit** (D8): only if the product can constrain browse targets to an allowlist; otherwise it's $285/mo of nothing.
- **Cellular isolation**: at serious multi-tenant scale, a cell = the full stack (table, queues, cluster, VPC) per N tenants. Cheap here because the flat ~30-resource Terraform *is* the cell template already.

## 11. Appendix: considered and rejected

- **AWS Batch** — owns the queue + dispatch loop, but hides the job state machine that *is* this product's API.
- **Step Functions** — per-transition pricing for a 5-state machine DynamoDB conditional writes do for free; the reconciler would still be needed for ECS truth.
- **EKS** — fleet management is the thing task-per-job exists to avoid.
- **API Gateway** — see D1.
- **X-Ray** — see D12; the job record is the trace.
- **Secrets Manager** — see D9; SSM standard tier is free.
- **S3 versioning** — see D11; immutable write-once artifacts.
- **TLS-inspecting egress proxy** — breaks half the web, and you become a CA.

---

## Repo map

```
cmd/controlplane   API + dispatcher + reconciler (one binary, three goroutines)
cmd/agent          runs inside the ephemeral task; -local mode for laptop iteration
cmd/reaper         the layer-3 Lambda
cmd/token          JWT minting helper for demos (make token SUB=alice)
internal/api       HTTP handlers, JWT middleware, presigning
internal/auth      HS256 mint/verify (stdlib only)
internal/store     DynamoDB state machine + rate limiter
internal/dispatch  weighted queue polling, semaphore, RunTask, idempotency
internal/reap      reconciler + hard reaper (shared with the Lambda)
internal/agentrun  chromedp task + flight recorder + boot health check
terraform/         flat, one environment, ~30 resources
scripts/           api-url, run-local env, loadtest
```

See also: [RUNBOOK.md](RUNBOOK.md) — day-2 operations, incident playbooks, and the prioritized P0–P2 gap list this document's Part III absorbs.
