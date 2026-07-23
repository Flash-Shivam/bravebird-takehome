#!/usr/bin/env bash
# Concurrency demo: 50 simultaneous submissions across 10 users (mixed
# priorities), a live status histogram until the queue drains, then a 15-job
# burst from one user to show the per-user rate limit.
set -euo pipefail

API=${1:?usage: loadtest.sh <api-url>}
JOBS_FILE=$(mktemp)
trap 'rm -f "$JOBS_FILE"' EXIT

echo "==> minting tokens for 10 users"
tokens=()
for u in $(seq 0 9); do
  tokens[$u]=$(scripts/with-env.sh go run ./cmd/token -sub "user-$u")
done

echo "==> submitting 50 jobs (10 users x 5, every 5th is high priority)"
for i in $(seq 1 50); do
  u=$(( i % 10 ))
  prio=$([ $(( i % 5 )) -eq 0 ] && echo high || echo low)
  curl -s -X POST "$API/jobs" \
    -H "Authorization: Bearer ${tokens[$u]}" \
    -H 'Content-Type: application/json' \
    -d "{\"prompt\":\"load test job $i\",\"priority\":\"$prio\"}" \
    | jq -r --arg u "$u" '.job_id // empty | "\($u) \(.)"' >> "$JOBS_FILE" &
done
wait
total=$(wc -l < "$JOBS_FILE" | tr -d ' ')
echo "==> $total accepted"

echo "==> draining (max-concurrent Fargate tasks at a time); histogram every 10s"
while true; do
  # Ownership is enforced now, so each poll uses the submitting user's token.
  histo=$(while read -r u id; do
    curl -s "$API/jobs/$id" -H "Authorization: Bearer ${tokens[$u]}" | jq -r '.status'
  done < "$JOBS_FILE" | sort | uniq -c | sort -rn | awk '{printf "%s=%s  ", $2, $1}')
  echo "$(date +%H:%M:%S)  $histo"
  remaining=$(echo "$histo" | grep -cE 'QUEUED|LAUNCHING|RUNNING' || true)
  [ "$remaining" -eq 0 ] && break
  sleep 10
done
echo "==> drained"

echo "==> rate limit demo: 15 rapid submissions from one user (limit is 10/min)"
RL_TOKEN=$(scripts/with-env.sh go run ./cmd/token -sub rate-limit-demo)
codes=$(for i in $(seq 1 15); do
  curl -s -o /dev/null -w '%{http_code}\n' -X POST "$API/jobs" \
    -H "Authorization: Bearer $RL_TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"prompt":"rate limit probe","priority":"low"}'
done | sort | uniq -c)
echo "$codes"
echo "==> done (429s above are the rate limiter working)"
