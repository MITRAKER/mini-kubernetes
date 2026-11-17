# Learning Mini Kubernetes

This file grows with the project. Each chapter explains one milestone: what
was built, why it was designed that way, the concepts behind it, and an
exercise so you can rebuild or extend it yourself.

- [Chapter 1: A container runtime from scratch](#chapter-1-a-container-runtime-from-scratch)
- [Chapter 2: The node agent](#chapter-2-the-node-agent)

---

## Chapter 1: A container runtime from scratch

**Code:** `internal/runtime/` and `cmd/mkube/`

### The big idea: a container is just a process

There is no "container" object in the Linux kernel. A container is an ordinary
process that the kernel has been told to lie to:

| The process asks... | Normally sees | In a container sees | Kernel feature |
|---|---|---|---|
| "What is my hostname?" | the machine's name | its own name | UTS namespace |
| "What processes exist?" | every process on the machine | only its own, and it is PID 1 | PID namespace |
| "What files exist?" | the host's disk | its own small filesystem | mount namespace + `pivot_root` |
| "What network cards exist?" | eth0, wlan0... | only `lo` | network namespace |
| "How much memory can I use?" | all of it | e.g. 64 MiB, then it is killed | cgroups |

Docker, containerd and the runtime Kubernetes uses (runc) all do exactly these
steps. `mkube` does them in about 400 lines of Go so you can read every one.

Try it:

```bash
make build
sudo ./scripts/make-rootfs.sh rootfs
sudo ./bin/mkube run -name demo -rootfs rootfs -memory 64Mi -cpu 500m -pids 50 -net -- sh
```

Inside, run `hostname`, `ps`, `ls /` and `cat /proc/net/dev`, and compare with
the same commands outside.

### Concept 1: Namespaces (what a process can *see*)

A namespace wraps one kind of global resource so a process gets its own copy.
You ask for new namespaces when creating a process with the `clone()` system
call, by passing flags:

```go
flags := syscall.CLONE_NEWUTS | // own hostname
         syscall.CLONE_NEWPID | // own process tree: the command is PID 1
         syscall.CLONE_NEWNS  | // own mount table
         syscall.CLONE_NEWIPC   // own shared memory and semaphores
```

You can see a process's namespaces on any Linux machine with
`ls -l /proc/self/ns`. Two processes are "in the same container" exactly when
those links point to the same namespace IDs.

Kubernetes connection: all containers in one **Pod** share a network namespace.
That is why they can talk to each other on `localhost`.

### Concept 2: The re-exec trick (why `mkube` starts itself)

The obvious design would be: call `unshare()` to enter new namespaces, set the
hostname, then run the command. That does not work in Go. A Go program runs on
many OS threads, and `unshare` only moves the calling thread, so half the
program would be inside the container and half outside.

So `Run` (in `container.go`) does what runc does. It starts **a fresh copy of
its own binary** (`/proc/self/exe __init`) with the clone flags set. That new
process starts life already inside the namespaces, runs `Init`, sets everything
up, and finally calls `exec` to *replace itself* with the user's command. The
command ends up as PID 1 with nothing of `mkube` left in memory.

```
mkube run ...                (host)
  └─ clone(NEWPID|NEWNS|...) ─▶ mkube __init   (PID 1 in the container)
                                   │ sethostname, pivot_root, mount /proc
                                   └─ exec("sh") ─▶ sh   (still PID 1)
```

### Concept 3: A pipe as a start signal (avoiding a race)

There is a moment between "child started" and "child placed in its cgroup".
If the user's command ran during that moment, it would run with no limits.

The fix: the parent sends the container's config to the child through a pipe,
and it only writes it *after* adding the child to the cgroup. The child blocks
on reading that pipe before doing anything. One pipe carries both the data and
the "go" signal. This kind of handshake is a common pattern in systems code.

### Concept 4: `pivot_root` (giving the container its own disk)

The container needs its own filesystem, a directory with `/bin`, `/lib`,
`/etc` and so on (the "rootfs"; a container image is just a packed rootfs).

`chroot` is the old way, but a root user can escape a chroot. `pivot_root`
actually swaps the root mount of the mount namespace, after which the old root
can be unmounted completely. `setupRootFS` in `rootfs.go` does it in order:

1. Make all mounts **private**, so nothing we mount leaks back to the host.
2. **Bind-mount** the rootfs onto itself. `pivot_root` requires the new root to
   be a mount point, and this is the standard trick to make one.
3. `chdir` into the rootfs and call `pivot_root(".", ".")`. This stacks the old
   root on top of the new one; `umount(".", MNT_DETACH)` then peels the old
   root off. The rootfs is now `/` and the host filesystem is unreachable.
4. Mount a fresh **`/proc`**. Because we are in a new PID namespace, it shows
   only the container's processes. This is why `ps` inside shows 3 processes.
5. Mount a small **`/dev`** with only safe devices (`null`, `zero`, `urandom`,
   ...), so the container cannot touch the host's disks.

> **A bug found while building milestone 2.** The first version used the
> textbook form, `pivot_root(rootfs, rootfs/.oldroot)`, then deleted
> `.oldroot`. It worked for one container. When the node agent started three
> containers from the same image at the same moment, they all shared one
> `.oldroot` directory, and one container deleted it while another still
> needed it. The `pivot_root(".", ".")` form needs no directory at all, which
> is exactly why runc uses it. Lesson: anything shared between containers
> (here, the image directory) must never be written to during set-up.

### Concept 5: Cgroups (what a process can *use*)

Namespaces hide things. **Control groups** limit things. A cgroup is a
directory in `/sys/fs/cgroup`; you write limits into files in it and write
process IDs into `cgroup.procs`. Every child process inherits the cgroup.

| Limit | cgroup v2 file | cgroup v1 file | What happens at the limit |
|---|---|---|---|
| Memory | `memory.max` | `memory/…/memory.limit_in_bytes` | Kernel OOM-kills the process (exit 137) |
| CPU | `cpu.max` = `quota period` | `cpu/…/cpu.cfs_quota_us` | Process is paused until the next period |
| Processes | `pids.max` | `pids/…/pids.max` | `fork()` fails |

CPU limits are worth understanding well. `-cpu 500m` (500 millicores) becomes
"50 000 µs of CPU in every 100 000 µs period". The process is not slowed down
evenly; it runs at full speed for 50 ms, then is frozen for the remaining 50 ms.
This is called **CFS throttling**, and it is why badly tuned CPU limits in
Kubernetes cause latency spikes.

Newer distributions use **cgroup v2** (one tree), older ones use **v1** (one
tree per controller). `cgroups.go` detects which one the host uses and
supports both.

Kubernetes connection: `resources.limits.memory: 64Mi` in a Pod spec ends up as
exactly this `memory.max` write. The status `OOMKilled` you see in
`kubectl get pods` comes from the kernel's `oom_kill` counter, which is what
`Cgroup.OOMKilled` reads.

### Concept 6: Exit codes

`mkube` exits with the container's exit code, so it behaves like any other
command in scripts. If the process was killed by a signal, the convention is
`128 + signal number`: an OOM kill is SIGKILL (9), so you get **137**. You will
see 137 constantly in real Kubernetes debugging; now you know where it comes
from.

### How it was verified

`internal/runtime/integration_test.go` starts real containers and checks:

- the hostname is the container name and the shell is PID 1,
- a network namespace has only `lo`,
- `exit 7` inside gives exit code 7 outside,
- allocating 200 MB under a 32 MiB limit gets OOM-killed with exit 137,
- forking 8 processes under `-pids 5` fails.

A manual check also confirmed `-cpu 500m` holds a busy loop at about 50% CPU.

### Exercises

1. **See namespaces yourself.** Run `sudo unshare --uts --pid --fork --mount-proc bash`,
   then `hostname test; ps`. You just did by hand what `Run` does.
2. **Add a user namespace.** Add `CLONE_NEWUSER` with `UidMappings` so that
   root inside the container is an unprivileged user outside ("rootless
   containers"). Hint: look at `syscall.SysProcAttr.UidMappings`.
3. **Make the rootfs read-only.** After `pivot_root`, remount `/` with
   `MS_RDONLY|MS_REMOUNT|MS_BIND`. Then try `touch /x` inside.
4. **Report memory usage.** After the container exits, read `memory.peak`
   (v2) or `memory.max_usage_in_bytes` (v1) and print it.

### Interview questions this chapter prepares you for

- What is the difference between a container and a virtual machine? (A VM has
  its own kernel; containers share the host kernel and are isolated by
  namespaces and cgroups.)
- What do namespaces do, and what do cgroups do? (See / use.)
- Why is a container's main process PID 1, and why does that matter? (PID 1
  gets no default signal handlers, so it must handle SIGTERM itself or it
  ignores `docker stop`.)
- What does exit code 137 mean?

---

## Chapter 2: The node agent

**Code:** `internal/agent/`, `internal/api/`, `internal/client/`, `cmd/mkube/agent.go`

### What it is

Chapter 1 gave us a way to run *one* container by hand. A cluster needs a
program on every machine that runs containers *on instruction*. In
Kubernetes that program is the **kubelet**; ours is `mkube agent`.

```
            API server  (chapter 3)
             ▲     │
 heartbeat,  │     │  "these pods are bound to you"
 pod status  │     ▼
          ┌──────────────┐
          │  mkube agent │  one per machine
          └──────┬───────┘
                 │ runtime.Run (chapter 1)
        ┌────────┼────────┐
        ▼        ▼        ▼
      pod A    pod B    pod C
```

The agent has three jobs:

1. **Heartbeat.** Every 2 seconds it tells the API server "I'm node1, I'm
   alive, and I have this much CPU and memory." If heartbeats stop, the
   control plane will treat the node as dead and move its pods (chapter 3).
2. **Reconcile.** Make the containers on this machine match the pods bound to
   it: start what is missing, stop what was deleted.
3. **Report.** Write back what actually happened: Running, Succeeded, Failed,
   the exit code, OOMKilled, and how many restarts.

### Concept 1: Desired state vs. observed state

Every Kubernetes object has two halves, and so does our `api.Pod`:

- `Spec` is the **desired state**: what someone *wants* (image, command,
  limits). Users and controllers write it.
- `Status` is the **observed state**: what is *actually* happening. Only the
  agent writes it.

Keeping them separate is what lets many independent programs cooperate
without stepping on each other. Each one owns a clear piece.

### Concept 2: Reconciliation loops (the heart of Kubernetes)

The agent does **not** react to events like "pod created" or "pod deleted".
Instead, every 2 seconds it asks: "What *should* be running here? What *is*
running here?" and fixes the difference. That is all `Sync` does:

```go
for each pod bound to me:   if not running here -> start it
for each pod running here:  if not bound to me any more -> stop it
```

Why poll instead of reacting to events? Because events get lost: the network
drops, the agent crashes and restarts, a message arrives twice. A loop that
compares *full state* heals itself from all of those automatically. If the
agent restarts, the next `Sync` simply notices which pods should be running
and starts them. This property is called being **level-triggered** (look at
the current level) rather than **edge-triggered** (react to each change).
Every Kubernetes controller works this way.

### Concept 3: One goroutine per pod

Each pod gets its own goroutine (`runPod`) and its own `context`. The
goroutine runs the container, waits for it to exit, decides whether to
restart, and reports status. To stop a pod, the agent calls the context's
`cancel()`. In chapter 1 we changed `runtime.Run` to use
`exec.CommandContext`, so cancelling kills the container's PID 1, and the
kernel then kills everything else in its PID namespace.

The `workers` map is shared between the sync loop and the goroutines, so it
is guarded by a `sync.Mutex`. The tests run with Go's race detector
(`go test -race`) to catch any access that forgot to lock.

### Concept 4: Restart policies and CrashLoopBackOff

When a container exits, the pod's `RestartPolicy` decides what happens:

| Policy | Restart when | Typical use |
|---|---|---|
| `Always` (default) | any exit | web servers, long-running services |
| `OnFailure` | exit code ≠ 0 | batch jobs that should retry |
| `Never` | never | one-shot tasks |

If a container keeps crashing, restarting it instantly would burn CPU in a
tight loop. So the agent waits before each restart, and **doubles** the wait
each time (1s, 2s, 4s, ... up to 30s). This is **exponential backoff**, and
it is exactly the `CrashLoopBackOff` status you see in `kubectl get pods`. If
a container ran for a good while before exiting, the delay resets, because
that is not a crash loop.

### Concept 5: Logs stay on the node

A container's stdout and stderr go to `/var/lib/mkube/logs/<pod>.log` on the
node. The API server never stores logs; that would make it a bottleneck. So
the agent runs its own small HTTP server with `GET /logs/{pod}`, and the CLI
(chapter 4) will fetch logs straight from the node. Real Kubernetes does the
same: `kubectl logs` is proxied to the kubelet on port 10250, which is why our
agent uses that port too.

### Concept 6: Testing with fakes

The agent depends on two things that are hard to use in tests: an API server
(doesn't exist yet) and real containers (need root). So the tests replace both:

- **A fake API server**: an `httptest.Server` holding pods in a map. The agent
  talks to it over real HTTP, so the client code is tested too.
- **A fake runner**: `Agent.Run` is a function field, so tests pass in a
  function that pretends to run a container and returns a scripted exit code.

This is **dependency injection**: the agent receives its dependencies instead
of creating them. It let us test restart policies, backoff, OOMKilled
reporting, missing images and log serving in about one second, with no root.

There is also a test with the real runtime. It hits a neat problem: the
runtime starts containers by re-running `/proc/self/exe`, and under `go test`
that is the *test binary*. `TestMain` in `integration_test.go` solves it by
checking for the `__init` argument and becoming the container's init process.

### The bug this milestone found

The real-container test runs three pods at once from the same image, and one
failed with `remove /.oldroot: no such file or directory`. Chapter 1's
`pivot_root` created and deleted a temporary `.oldroot` directory *inside the
shared image*, so parallel containers raced on it. The fix (see the note in
chapter 1) was the `pivot_root(".", ".")` form, which needs no directory. This
is a classic lesson: unit tests with fakes passed, and only a test with real
parallel containers found the bug.

### Try it

The agent needs the API server from chapter 3 to be useful, so for now run
the tests:

```bash
go test -race ./internal/agent/                          # fakes, no root
sudo MKUBE_ROOTFS=$PWD/rootfs go test ./internal/agent/  # real containers
```

### Exercises

1. **Liveness probes.** Add `Spec.LivenessCommand`. Every 10 seconds the agent
   runs it inside the container (hint: `nsenter`, or `setns()` into the
   container's namespaces); if it fails 3 times, kill and restart the pod.
2. **Graceful shutdown.** Today stopping a pod is an instant SIGKILL. Send
   SIGTERM first, wait up to 10 seconds, then SIGKILL, like Kubernetes'
   `terminationGracePeriodSeconds`.
3. **Copy-on-write images.** All pods using the same image share one
   directory, so a file written by one pod is seen by the others. Mount an
   **overlayfs** per container (`lowerdir` = image, `upperdir` = a per-pod
   directory) so writes stay private. This is how Docker image layers work.
4. **Log rotation.** Cap each log file at 10 MB.

### Interview questions this chapter prepares you for

- What does the kubelet do? (Registers the node, heartbeats, runs pods bound
  to its node, reports status, serves logs and exec.)
- What is the difference between `spec` and `status`?
- What does "level-triggered" mean, and why does Kubernetes prefer it?
- What is CrashLoopBackOff, and how would you debug it? (`kubectl logs
  --previous`, check the exit code: 137 means OOMKilled or killed, 1 means the
  app itself errored.)
