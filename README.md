# Mini Kubernetes

A small container orchestrator written from scratch in Go, to learn how Linux
containers and Kubernetes actually work. Every layer is built on raw Linux
primitives, with no Docker or containerd underneath.

> Learning notes for every milestone are in [LEARNING.md](LEARNING.md).

## Roadmap

| # | Milestone | Status |
|---|---|---|
| 1 | **Container runtime**: namespaces, cgroups (v1 and v2), `pivot_root` | ✅ Done |
| 2 | **Node agent**: heartbeats, reconciliation, restart policies, logs | ✅ Done |
| 3 | **Control plane**: API server, scheduler, Deployment and node controllers | ✅ Done |
| 4 | **`minictl` CLI**: apply YAML, get, logs, scale, delete | ✅ Done |
| 5 | **Demo**: 3-node local cluster, CI, architecture diagram | Planned |

## Quick start (milestone 1)

Needs Linux, Go 1.22+ and root.

```bash
make build
sudo ./scripts/make-rootfs.sh rootfs      # Alpine minirootfs, ~3 MB
sudo ./bin/mkube run -name demo -rootfs rootfs \
     -memory 64Mi -cpu 500m -pids 50 -net -- sh
```

Inside the container:

```text
/ # hostname
demo
/ # ps
PID   USER     TIME  COMMAND
    1 root      0:00 sh
    7 root      0:00 ps
/ # cat /proc/net/dev     # only the loopback interface
```

### `mkube run` flags

| Flag | Meaning |
|---|---|
| `-name` | Container name (also the default hostname) |
| `-rootfs` | Directory with the container's filesystem |
| `-memory` | Memory limit, e.g. `64Mi`, `1Gi`. Exceeding it gives exit 137 (OOMKilled) |
| `-cpu` | CPU limit, e.g. `500m` (half a core) or `2` |
| `-pids` | Maximum number of processes |
| `-net` | Own empty network namespace (loopback only) |
| `-env` | `KEY=VALUE`, repeatable |

## Tests

```bash
make test                 # unit tests, no root needed
sudo make test-integration  # starts real containers
```

## Layout

```text
cmd/mkube/            runtime, node agent and control plane binary
cmd/minictl/          the kubectl-like client
internal/manifest/    YAML manifests
examples/             ready-to-apply manifests
internal/runtime/     namespaces, cgroups, rootfs setup
internal/agent/       node agent (like the kubelet)
internal/api/         Node and Pod types shared by every component
internal/client/      API server HTTP client
internal/apiserver/   REST API and the state store (like etcd)
internal/scheduler/   picks a node for each pod
internal/controller/  Deployment and node-failure control loops
test/e2e/             a whole cluster inside one test
scripts/              rootfs builder
LEARNING.md           how and why, chapter by chapter
```
