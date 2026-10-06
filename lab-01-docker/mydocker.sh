#!/usr/bin/env bash

set -Eeuo pipefail

readonly SCRIPT_DIR="$(
  cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
  pwd
)"

readonly CGROUP_NAME="lab1-mydocker"
readonly CGROUP_DIR="/sys/fs/cgroup/${CGROUP_NAME}"

readonly MEMORY_MAX=$((64 * 1024 * 1024))
readonly CPU_MAX="50000 100000"
readonly PIDS_MAX=20
readonly API_BIN="/tmp/lab1-api"

runtime_dir=""
start_gate=""
unshare_pid=""
container_pid=""

cleanup() {
  trap - EXIT INT TERM
  set +e

  if [[ -n "$unshare_pid" ]]; then
    kill "$unshare_pid" 2>/dev/null
    wait "$unshare_pid" 2>/dev/null
  fi

  if [[ -d "$CGROUP_DIR" ]]; then
    printf '1\n' \
      | sudo tee "$CGROUP_DIR/cgroup.kill" >/dev/null 2>&1

    sudo rmdir "$CGROUP_DIR"
    echo "cgroup removed: $CGROUP_DIR"
  fi

  if [[ -n "$start_gate" ]]; then
    rm -f -- "$start_gate"
  fi

  if [[ -n "$runtime_dir" && -d "$runtime_dir" ]]; then
    rmdir -- "$runtime_dir"
  fi
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for required_command in \
  sudo \
  go \
  unshare \
  nsenter \
  ip \
  setpriv \
  python3 \
  curl \
  pgrep \
  mkfifo
do
  if ! command -v "$required_command" >/dev/null; then
    echo "required command not found: $required_command" >&2
    exit 1
  fi
done

[[ -f "$SCRIPT_DIR/seccomp-profile.json" ]] || {
  echo "seccomp profile not found" >&2
  exit 1
}

[[ -f "$SCRIPT_DIR/seccomp-launcher.py" ]] || {
  echo "seccomp launcher not found" >&2
  exit 1
}

sudo -v
sudo mkdir "$CGROUP_DIR"

printf '%s\n' "$MEMORY_MAX" \
  | sudo tee "$CGROUP_DIR/memory.max" >/dev/null

printf '0\n' \
  | sudo tee "$CGROUP_DIR/memory.swap.max" >/dev/null

printf '1\n' \
  | sudo tee "$CGROUP_DIR/memory.oom.group" >/dev/null

printf '%s\n' "$CPU_MAX" \
  | sudo tee "$CGROUP_DIR/cpu.max" >/dev/null

printf '%s\n' "$PIDS_MAX" \
  | sudo tee "$CGROUP_DIR/pids.max" >/dev/null

printf 'memory.max: '
cat "$CGROUP_DIR/memory.max"

printf 'memory.swap.max: '
cat "$CGROUP_DIR/memory.swap.max"

printf 'memory.oom.group: '
cat "$CGROUP_DIR/memory.oom.group"

printf 'cpu.max: '
cat "$CGROUP_DIR/cpu.max"

printf 'pids.max: '
cat "$CGROUP_DIR/pids.max"

(
  cd "$SCRIPT_DIR/api"
  go build -o "$API_BIN" .
)

runtime_dir="$(mktemp -d)"
start_gate="$runtime_dir/start.fifo"
mkfifo "$start_gate"

unshare \
  --user --map-root-user \
  --pid --fork \
  --mount --mount-proc \
  --uts \
  --ipc \
  --net \
  --kill-child \
  bash -c '
    start_gate=$1
    api_bin=$2
    script_dir=$3

    IFS= read -r _ < "$start_gate"

    hostname lab1-api
    ip link set lo up

    exec setpriv \
      --bounding-set=-all \
      --inh-caps=-all \
      --ambient-caps=-all \
      --no-new-privs \
      python3 "$script_dir/seccomp-launcher.py" \
      "$script_dir/seccomp-profile.json" \
      "$api_bin"
  ' bash "$start_gate" "$API_BIN" "$SCRIPT_DIR" &

unshare_pid=$!

for _ in {1..100}; do
  container_pid="$(
    pgrep -P "$unshare_pid" \
      | head -n 1 \
      || true
  )"

  if [[ -n "$container_pid" ]]; then
    break
  fi

  sleep 0.05
done

if [[ -z "$container_pid" ]]; then
  echo "failed to find namespace child PID" >&2
  exit 1
fi

printf '%s\n' "$container_pid" \
  | sudo tee "$CGROUP_DIR/cgroup.procs" >/dev/null

echo "namespace process host PID: $container_pid"
cat "/proc/$container_pid/cgroup"

printf 'start\n' > "$start_gate"

echo "API started; press Ctrl+C to stop"
wait "$unshare_pid"