#!/usr/bin/env bash
set -euo pipefail
[[ $(kubectl config current-context) == kind-lab5 ]] || {
  echo 'Refusing network break outside kind-lab5' >&2; exit 2;
}
# BREAK_CASE=1..4 только для предварительной проверки каждого варианта; по умолчанию выбор скрыт.
choice=${BREAK_CASE:-$((RANDOM % 4 + 1))}
case "$choice" in
  1) override='networkPolicy: {database: {sources: [unknown-source]}}' ;;
  2) override='networkPolicy: {database: {destination: unknown-destination}}' ;;
  3) override='networkPolicy: {dns: {enabled: false}}' ;;
  4) override='networkPolicy: {database: {port: 5433}}' ;;
  *) echo 'BREAK_CASE must be 1..4' >&2; exit 2 ;;
esac
cd "$(dirname "$0")/.."
mkdir -p "$HOME/lab5"
umask 077
values=$(mktemp "$HOME/lab5/hidden-values-XXXXXX.yaml")
printf '%s\n' "$override" > "$values"
printf '%s\n' "$values" > "$HOME/lab5/hidden-values-path"
helm --kube-context kind-lab5 upgrade shop ./shop -n shop -f shop/values.yaml -f "$values" \
  --wait --timeout 2m > "$HOME/lab5/break-upgrade.log" 2>&1
printf 'Поломка применена. Диагностируйте по симптомам и Hubble, не читая override.\n'
printf 'Восстановление: helm --kube-context kind-lab5 upgrade shop ./shop -n shop -f shop/values.yaml --wait\n'
