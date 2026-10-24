#!/usr/bin/env bash
set -euo pipefail
duration=${1:-180}
interval=${2:-5}
[[ "$duration" =~ ^[1-9][0-9]*$ && "$interval" =~ ^[1-9][0-9]*$ ]] || {
  echo 'duration and interval must be positive integer seconds' >&2; exit 2;
}
[[ $(kubectl config current-context) == kind-lab5 ]] || {
  echo 'Refusing chaos outside kind-lab5' >&2; exit 2;
}
end=$((SECONDS + duration))
printf 'timestamp,pod,ip,node\n'
while (( SECONDS < end )); do
  mapfile -t pods < <(kubectl --context kind-lab5 -n shop get pods -l 'app in (api,worker)' \
    --field-selector=status.phase=Running -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}} {{.status.podIP}} {{.spec.nodeName}}{{"\n"}}{{end}}{{end}}')
  if (( ${#pods[@]} == 0 )); then
    echo 'No running api/worker pods to delete' >&2; exit 1
  fi
  read -r pod ip node <<< "${pods[RANDOM % ${#pods[@]}]}"
  # Защита от расширения селектора: PostgreSQL и диагностические поды не цели хаоса.
  [[ "$pod" == api-* || "$pod" == worker-* ]] || exit 2
  timestamp=$(date +%s)
  kubectl --context kind-lab5 -n shop delete pod "$pod" --wait=false > /dev/null
  printf '%s,%s,%s,%s\n' "$timestamp" "$pod" "$ip" "$node"
  sleep "$interval"
done
