#!/usr/bin/env bash
set -euo pipefail

# A bounded mixed load: errors and slow responses, sequential /load batches.
duration=${1:-240}
if [[ ! "$duration" =~ ^[0-9]{3}$ ]] || (( 10#$duration < 180 || 10#$duration > 600 )); then
  echo "Usage: bash scripts/trigger-alerts.sh [seconds: 180..600]" >&2
  exit 2
fi
duration=$((10#$duration))
api_url=${LAB2_API_URL:-http://127.0.0.1:18080}
api_url=${api_url%/}

echo "Generating mixed /fail and /slow traffic for about $duration seconds."
echo "Keep the API port-forward running. Ctrl+C stops the load."
deadline=$((SECONDS + duration))
while (( SECONDS < deadline )); do
  curl --fail-with-body --silent --show-error --max-time 35 \
    "$api_url/load?n=50&concurrency=5&path=/fail"
  (( SECONDS < deadline )) || break
  curl --fail-with-body --silent --show-error --max-time 35 \
    "$api_url/load?n=20&concurrency=5&path=/slow"
  sleep 2
done
echo "Load finished. The 2-minute rate window still contains recent requests."
