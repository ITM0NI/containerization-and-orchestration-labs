#!/bin/sh
# Запускать внутри client: kubectl -n shop exec -i client -- sh -s -- URL 180 < scripts/load.sh
set -u
url=${1:-http://api.shop.svc.cluster.local:8080/order}
duration=${2:-180}
interval=${3:-0.1}
case "$duration" in
  ''|*[!0-9]*|0) echo 'duration must be a positive integer (seconds)' >&2; exit 2 ;;
esac
end=$(( $(date +%s) + duration ))
printf 'timestamp,http_code,curl_exit,seconds\n'
while [ "$(date +%s)" -lt "$end" ]; do
  timestamp=$(date +%s)
  result=$(curl --silent --show-error --connect-timeout 1 --max-time 3 \
    --output /dev/null --write-out '%{http_code},%{time_total}' \
    --header 'Content-Type: application/json' --data '{"item":"chaos-tea"}' "$url")
  status=$?
  code=${result%%,*}
  seconds=${result#*,}
  printf '%s,%s,%s,%s\n' "$timestamp" "$code" "$status" "$seconds"
  sleep "$interval" || exit 2
done
