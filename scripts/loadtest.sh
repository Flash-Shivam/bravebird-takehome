#!/usr/bin/env bash
# Concurrency demo: 50 simultaneous submissions across 10 users (mixed
# priorities), a live status histogram until the queue drains, then a 15-job
# burst from one user to show the per-user rate limit.
set -euo pipefail

API=${1:?usage: loadtest.sh <api-url>}
JOBS_FILE=$(mktemp)
trap 'rm -f "$JOBS_FILE"' EXIT

echo "==> submitting 50 jobs (10 users x 5, every 5th is high priority)"
for i in $(seq 1 50); do
  user="user-$(( i % 10 ))"
  prio=$([ $(( i % 5 )) -eq 0 ] && echo high || echo low)
  curl -s -X POST "$API/jobs" \
    -H "X-User-Id: $user" \
    -H 'Content-Type: application/json' \
    -d "{\"prompt\":\"load test job $i\",\"priority\":\"$prio\"}" \
    | jq -r '.job_id // empty' >> "$JOBS_FILE" &
done
wait
total=$(wc -l < "$JOBS_FILE" | tr -d ' ')
echo "==> $total accepted"

echo "==> draining (max-concurrent Fargate tasks at a time); histogram every 10s"
while true; do
  histo=$(while read -r id; do
    curl -s "$API/jobs/$id" | jq -r '.status'
  done < "$JOBS_FILE" | sort | uniq -c | sort -rn | awk '{printf "%s=%s  ", $2, $1}')
  echo "$(date +%H:%M:%S)  $histo"
  remaining=$(echo "$histo" | grep -cE 'QUEUED|LAUNCHING|RUNNING' || true)
  [ "$remaining" -eq 0 ] && break
  sleep 10
done
echo "==> drained"

echo "==> rate limit demo: 15 rapid submissions from one user (limit is 10/min)"
codes=$(for i in $(seq 1 15); do
  curl -s -o /dev/null -w '%{http_code}\n' -X POST "$API/jobs" \
    -H 'X-User-Id: rate-limit-demo' \
    -H 'Content-Type: application/json' \
    -d '{"prompt":"rate limit probe","priority":"low"}'
done | sort | uniq -c)
echo "$codes"
echo "==> done (429s above are the rate limiter working)"
