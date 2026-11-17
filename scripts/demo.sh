#!/usr/bin/env bash
# Starts a 3-node Mini Kubernetes cluster on this machine and walks through
# scheduling, self-healing and a rolling update. Needs Linux and root.
#
#   sudo ./scripts/demo.sh
#
# Everything runs from a temporary directory and is cleaned up on exit.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ "$(id -u)" -ne 0 ]; then
  echo "Run as root: sudo $0" >&2
  exit 1
fi

make -s build
./scripts/make-rootfs.sh rootfs >/dev/null

WORK="$(mktemp -d)"
mkdir -p "$WORK/images"
ln -s "$PWD/rootfs" "$WORK/images/alpine"
export MKUBE_SERVER=http://127.0.0.1:6443
PIDS=()

cleanup() {
  echo; echo "Cleaning up..."
  for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

step() { echo; echo "=== $* ==="; }
run() { echo "\$ $*"; "$@"; }

step "Starting the control plane and 3 node agents"
./bin/mkube server -listen 127.0.0.1:6443 -data "$WORK/state.json" >"$WORK/server.log" 2>&1 &
PIDS+=($!)
sleep 1
for i in 1 2 3; do
  ./bin/mkube agent -name "node$i" -listen "127.0.0.1:1025$i" -advertise "http://127.0.0.1:1025$i" \
    -cpu 1 -memory 512Mi -images "$WORK/images" -logs "$WORK/logs$i" >"$WORK/agent$i.log" 2>&1 &
  PIDS+=($!)
done
sleep 3
run ./bin/minictl get nodes

step "Deploying 3 replicas of hello"
run ./bin/minictl apply -f examples/hello.yaml
sleep 4
run ./bin/minictl get pods

POD="$(./bin/minictl get pods | awk 'NR==2 {print $1}')"
step "Reading logs straight from the node"
run ./bin/minictl logs "$POD"

step "Killing node3's agent (simulating a machine failure)"
{ kill -9 "${PIDS[3]}"; wait "${PIDS[3]}"; } 2>/dev/null || true
echo "Waiting for the control plane to notice (10 second heartbeat timeout)..."
sleep 14
run ./bin/minictl get nodes
run ./bin/minictl get pods
echo "(node3's containers died with its agent: the runtime sets PDEATHSIG, so the"
echo " kernel kills a container when the process that started it disappears)"

step "Scaling to 5"
run ./bin/minictl scale deployment hello 5
sleep 4
run ./bin/minictl get pods

step "Memory limits: a pod that uses too much memory"
run ./bin/minictl apply -f examples/oom.yaml
sleep 4
run ./bin/minictl get pods

step "Deleting everything"
run ./bin/minictl delete deployment hello
run ./bin/minictl delete pod memory-hog
sleep 3
run ./bin/minictl get pods
