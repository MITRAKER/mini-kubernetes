# Learning Mini Kubernetes

This file grows with the project. Each chapter explains one milestone: what
was built, why it was designed that way, the concepts behind it, and an
exercise so you can rebuild or extend it yourself.

- [Chapter 1: A container runtime from scratch](#chapter-1-a-container-runtime-from-scratch)
- [Chapter 2: The node agent](#chapter-2-the-node-agent)
- [Chapter 3: The control plane](#chapter-3-the-control-plane)
- [Chapter 4: minictl, a small kubectl](#chapter-4-minictl-a-small-kubectl)
- [Chapter 5: Demo, CI, and presenting the project](#chapter-5-demo-ci-and-presenting-the-project)
- [Chapter 6: Professional quality and the NASA/JPL rules](#chapter-6-professional-quality-and-the-nasajpl-rules)

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
- forking 32 processes under `-pids 16` fails.

> **A lesson from CI:** the first version of this test used `-pids 5`, and it
> failed now and then with `pthread_create failed` instead of a fork error.
> The pids controller counts *threads*, not just processes, and the container
> starts as our Go init process, which runs a few runtime threads before it
> execs the command. With a tiny limit, the Go runtime itself ran out. The fix:
> start init with `GOMAXPROCS=1` (fewer threads) and refuse limits below
> `MinPids` (16). Real runtimes have the same issue: runc's own Go init also
> runs inside the container's cgroup.

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

---

## Chapter 3: The control plane

**Code:** `internal/apiserver/`, `internal/scheduler/`, `internal/controller/`,
`cmd/mkube/server.go`, `test/e2e/`

### The picture

```
                 ┌────────────── mkube server ──────────────┐
  you / CLI ───▶ │  API server  ◀──▶  store (state.json)     │
                 │     ▲   ▲                                 │
                 │     │   └── scheduler   (binds pods)      │
                 │     └────── controllers (deployments,     │
                 │                          dead nodes)      │
                 └─────▲────────────────────────────────────┘
                       │ HTTP: heartbeats, "my pods?", status
          ┌────────────┼────────────┐
     mkube agent  mkube agent  mkube agent
       node1        node2        node3
```

The rule that makes this work: **every component talks only to the API
server**. The scheduler never calls an agent; the controller never calls the
scheduler. Each one reads state, writes state, and the others notice on their
next loop. You can kill and restart any one of them and the cluster carries
on from what is stored.

### Concept 1: The API server and its store

The API server is a plain REST API over JSON (`server.go`):

```
PUT  /api/deployments/web        create or update a Deployment
GET  /api/pods?node=node1        what the agent on node1 asks every 2s
PUT  /api/pods/web-x7k2p/binding what the scheduler writes
PUT  /api/pods/web-x7k2p/status  what the agent writes
```

Behind it is the **store** (`store.go`), our stand-in for **etcd**. It holds
everything in memory and writes a JSON snapshot to disk after every change.
The snapshot is written to a temporary file and then **renamed** over the old
one. A rename is atomic on Linux, so a crash can never leave a half-written
file. This write-then-rename pattern is used by almost every database and
editor.

The API server also **validates** everything on the way in: names must be DNS
labels (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`), which also stops tricks like a pod
named `../../etc`, unknown JSON fields are rejected, and request bodies are
capped at 1 MB. Validation at the front door means no other component has to
defend against bad data.

### Concept 2: Optimistic concurrency (resourceVersion)

Two people run `minictl scale web` at the same moment. Both read the
Deployment, both change it, both write it back. Without protection, the
second write silently erases the first. This is the **lost update** problem.

Kubernetes' answer, and ours: every object has a `resourceVersion` that
changes on every write. When you write, you send back the version you read.
If it no longer matches, someone else got there first, and the server answers
**409 Conflict**: "read it again and retry". No locks are held between read
and write, which is why this is called *optimistic* concurrency.
`TestOptimisticConcurrency` in `store_test.go` shows it.

Binding a pod uses the same idea in a simpler form: `BindPod` refuses to bind
a pod that already has a node, so if two schedulers race, exactly one wins.

### Concept 3: The scheduler (filter, then score)

Every second the scheduler (`scheduler.go`) finds pods with no `nodeName` and,
for each one:

1. **Filter** out nodes that can't take it: NotReady nodes, and nodes whose
   free CPU or memory (capacity minus the limits of pods already there) is too
   small.
2. **Score** the rest with **least-allocated**: the average share of CPU and
   memory that would still be free after placing the pod. Highest wins, which
   spreads pods evenly. Ties go to the first name, so results are repeatable.
3. **Bind**: write the node's name into the pod. That is all. The agent on
   that node sees it on its next sync and starts the container.

A pod that fits nowhere just stays `Pending` and is retried every second, the
same behaviour you see in Kubernetes when a cluster is full.

Note it counts *limits*, not actual usage. Real Kubernetes schedules on
*requests* (a guaranteed minimum) and enforces *limits* (a maximum). We use
one number for both to keep things simple; adding requests is an exercise.

### Concept 4: The deployment controller

You almost never create pods by hand in Kubernetes. You create a
**Deployment** that says "keep 3 copies of this pod running", and a controller
makes it so. Ours (`controller.go`) does, every second, for each Deployment:

1. Find its pods by **label** `deployment=<name>`. Labels are how Kubernetes
   objects find each other; there are no hard links between them.
2. Delete pods that **finished for good** (for example OOMKilled with
   `restartPolicy: Never`) so they get replaced.
3. **Too few?** Create pods from the template, named `<deployment>-<5
   random letters>`. (Ours skips vowels, as Kubernetes does, so a random name
   never spells a rude word.)
4. **Template changed?** Do a **rolling update** (below).
5. **Too many?** Delete extras, least useful first: unscheduled, then not
   ready, then the rest.

It only takes **one kind of step per pass**. The next pass, one second later,
sees the result and takes the next step. Small steps are easy to reason about
and safe to repeat.

When you delete a Deployment, its pods suddenly have an owner label pointing
at nothing. The controller deletes them: this is **garbage collection**.

### Concept 5: Rolling updates

Change the template (a new command, new env) and the controller replaces pods
one at a time, without ever dropping below the desired count:

- **maxSurge 1**: it may create one *extra* new pod first.
- **maxUnavailable 0**: it deletes an old pod only when, without it, there are
  still enough ready pods.

So with 3 replicas you go 3 old → 3 old + 1 new → 2 old + 1 new → 2 old + 2
new → ... → 3 new. If the new version crashes on start, the new pod never
becomes ready, the old pods are never deleted, and the rollout just stops. A
bad release can't take your service down. This is exactly what Kubernetes
does with its default settings.

### Concept 6: The node controller and self-healing

Each agent heartbeats every 2 seconds. If a node's last heartbeat is older
than 10 seconds, the node controller:

1. marks the node **NotReady**, so the scheduler stops placing pods there, and
2. **evicts** its pods by deleting them.

The deployment controller then sees it is short of pods and creates new ones,
and the scheduler puts them on healthy nodes. No component was told "node2
died"; each one just reacted to the state it saw. That is self-healing as an
*emergent* property of independent loops.

If node2 comes back, its agent asks "what are my pods?", gets an empty list,
and stops the orphaned containers it still has. Note that a *bare* pod (one
with no Deployment) on a dead node is simply gone, which is why you should
use Deployments.

### Concept 7: Polling vs. watching

All our components **poll**: they ask for the full list every 1 to 2 seconds.
Simple and robust, but with 5,000 nodes polling 100,000 pods it would melt the
API server. Real Kubernetes uses **watches**: a client lists once, then keeps
an HTTP connection open and the server streams every change ("pod web-x
modified, resourceVersion 1234"). Components keep a local cache
("informer") updated from the stream. It is the same level-triggered design,
just with far less traffic. Implementing a watch is a great exercise.

### How it was verified

- `store_test.go`: duplicate names, double binding, stale `resourceVersion`
  rejected, state survives a restart, validation rejects bad names and images.
- `scheduler_test.go`: filtering by readiness and capacity, spreading.
- `test/e2e/cluster_test.go` runs a **whole cluster in one test**: API server,
  scheduler, controllers and two agents over real HTTP. It creates a 4-replica
  Deployment and checks pods are spread 2/2, scales to 2, **kills the busier
  node** and checks its pods are replaced on the other node and the node is
  NotReady, does a rolling update, and deletes the Deployment. It runs once
  with fake containers and once with real Linux containers.
- A manual run of the real binaries: `mkube server` + `mkube agent`, a
  Deployment created with `curl`, two real `sleep` containers running,
  `hello from hello-5jkv6` read through the agent's log endpoint, and the
  containers gone after the agent stopped.

### Try it

```bash
make build && sudo ./scripts/make-rootfs.sh rootfs
sudo mkdir -p /var/lib/mkube/images && sudo ln -s $PWD/rootfs /var/lib/mkube/images/alpine
sudo ./bin/mkube server &
sudo ./bin/mkube agent -name node1 &
curl -X PUT localhost:6443/api/deployments/hello -d '{"name":"hello","replicas":2,
  "template":{"image":"alpine","command":["sh","-c","echo hi from $(hostname); sleep 1000"]}}'
curl -s localhost:6443/api/pods
```

Chapter 4 replaces those `curl` calls with a proper CLI.

### Exercises

1. **Requests vs. limits.** Add `Spec.Requests`, schedule on requests and
   enforce limits. Then you can overcommit a node, like real clusters do.
2. **Node selectors.** Add `Labels` to nodes and `NodeSelector` to pods, and
   filter nodes that don't match (e.g. `disk=ssd`).
3. **Watch API.** Add `GET /api/pods?watch=true` that streams changes as
   newline-separated JSON, and switch the agent to it.
4. **Leader election.** Run two `mkube server` processes for high
   availability. Only one scheduler may act at a time: store a "lease" object
   with a holder name and expiry, and use `resourceVersion` to take it over
   safely.

### Interview questions this chapter prepares you for

- What are the components of the Kubernetes control plane, and what does each
  do? (API server, etcd, scheduler, controller manager.)
- How does the scheduler pick a node? (Filter, then score.)
- What happens, step by step, when a node dies?
- How does a rolling update avoid downtime? (maxSurge, maxUnavailable,
  readiness.)
- What is `resourceVersion` for? (Optimistic concurrency, and resuming watches.)

---

## Chapter 4: minictl, a small kubectl

**Code:** `cmd/minictl/`, `internal/manifest/`, `examples/`

### What it does

```
minictl apply -f FILE                     create or update objects from YAML
minictl get pods|nodes|deployments [-o json]
minictl logs POD
minictl scale deployment NAME REPLICAS
minictl delete pod|deployment NAME
```

This is a real session on a two-node cluster, using the files in `examples/`:

```text
$ minictl apply -f examples/hello.yaml
deployment/hello created
$ minictl apply -f examples/oom.yaml
pod/memory-hog created
$ minictl apply -f examples/crash.yaml
pod/crasher created

$ minictl get pods
NAME          STATUS             RESTARTS   NODE    AGE
crasher       CrashLoopBackOff   3          node1   8s
hello-98z6d   Running            0          node1   7s
hello-h879b   Running            0          node2   7s
hello-vnt2w   Running            0          node1   7s
memory-hog    OOMKilled          0          node1   8s

$ minictl logs hello-98z6d
hello from hello-98z6d
still alive

# stop node2's agent, wait 15 seconds
$ minictl get nodes
NAME    STATUS     CPU     MEMORY   LAST HEARTBEAT
node1   Ready      2000m   1024Mi   1s ago
node2   NotReady   2000m   1024Mi   15s ago

$ minictl get pods
NAME          STATUS             RESTARTS   NODE    AGE
crasher       CrashLoopBackOff   5          node1   22s
hello-5f274   Running            0          node1   4s     <- replacement
hello-98z6d   Running            0          node1   21s
hello-vnt2w   Running            0          node1   21s
memory-hog    OOMKilled          0          node1   22s
```

Every line of that output comes from something built in chapters 1 to 3:
`OOMKilled` is the cgroup's `oom_kill` counter, `CrashLoopBackOff` and the
restart count come from the agent's backoff, and `hello-5f274` was created by
the deployment controller after the node controller evicted `hello-h879b`
from the dead node.

### Concept 1: The CLI is just another API client

`minictl` has no special powers. Every command is an HTTP request to the API
server, exactly like the scheduler or an agent makes. `kubectl` is the same:
run any kubectl command with `-v=8` and you will see the raw HTTP requests.
This is why you can manage Kubernetes from scripts, CI pipelines or your own
programs with no extra server-side work.

### Concept 2: Declarative vs. imperative

There are two ways to tell a system what to do:

- **Imperative**: "create a pod", "add 2 replicas", "delete that one". You
  describe the *steps*. `minictl scale` and `minictl delete` work this way.
- **Declarative**: "here is how things should look". You describe the *end
  state*, and the system works out the steps. `minictl apply -f` works this
  way: applying the same file twice is safe (the second time it just says
  `configured`), and changing `replicas: 3` to `5` in the file and applying
  again is how you scale.

Declarative files can be stored in Git, reviewed in pull requests, and
re-applied after a disaster to rebuild everything. That practice is called
**GitOps**, and it only works because the controllers (chapter 3) reconcile
toward desired state instead of executing commands.

### Concept 3: Manifests

`internal/manifest` turns YAML into API objects. The format is a simplified
Kubernetes manifest:

```yaml
kind: Deployment
name: hello
replicas: 3
template:
  image: alpine
  command: ["sh", "-c", "echo hello from $(hostname); sleep 3600"]
  limits: {cpu: 100m, memory: 32Mi}
```

Two details worth copying in your own tools:

- **Strict parsing.** `KnownFields(true)` makes a typo like `replica: 3` an
  error. Without it, YAML libraries silently ignore unknown keys, and you
  would get 1 replica and no idea why. Kubernetes added the same strictness
  ("field validation") in version 1.25 after years of exactly this confusion.
- **Human units.** `100m` and `32Mi` are parsed by the same `ParseCPU` and
  `ParseMemory` functions the runtime uses, so a quantity means the same thing
  everywhere.

Several objects can live in one file separated by `---`.

### Concept 4: Read-modify-write with retries

`minictl scale` reads the Deployment, changes `replicas`, and writes it back
with the `resourceVersion` it read. If the controller (which also writes the
Deployment's status) or another user wrote in between, the server answers
**409 Conflict**, and `scale` simply reads again and retries, up to 5 times.
This retry loop is the standard way to use optimistic concurrency (chapter 3).
In client-go, Kubernetes' Go library, it is literally called
`retry.RetryOnConflict`.

`apply`, on the other hand, deliberately sends no version: "make it look like
my file" should win over whatever is there.

### Concept 5: Logs come from the node

`minictl logs POD` asks the API server which node runs the pod and where that
node's agent listens (`Node.Address`, sent with every heartbeat), then fetches
the log from the agent directly (chapter 2). The API server never handles log
data.

### Concept 6: Making a CLI testable

`main()` is one line: `os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))`.
All the logic is in `run`, which takes arguments and writers and returns an
exit code. Tests call `run` directly and inspect the output as strings, with
no subprocesses. `main_test.go` starts a real control plane and an agent
(with fake containers) in the test process and walks through apply, get,
logs, scale and delete, plus error messages like `pod "nope" not found`.
`TestExamplesParse` applies every file in `examples/`, so an example can
never silently break.

### Exercises

1. **`minictl describe pod NAME`**: print every field of a pod, including the
   last exit code and message.
2. **`minictl get pods -w`**: refresh the table every second (or use the watch
   API if you did the chapter 3 exercise).
3. **Label selectors**: `minictl get pods -l deployment=hello`.
4. **`minictl apply` pruning**: delete Deployments that were created from a
   directory but are no longer in it (what `kubectl apply --prune` does).
5. **`minictl exec POD -- CMD`**: run a command inside a running container.
   You'll need the agent to call `setns()` on the container's namespaces,
   found under `/proc/<pid>/ns/`.

### Interview questions this chapter prepares you for

- What is the difference between `kubectl create` and `kubectl apply`?
- What does "declarative" mean in Kubernetes, and why does it matter for
  GitOps?
- `kubectl get pods` shows `OOMKilled`. What happened and what do you change?
  (The container hit its memory limit; raise `limits.memory` or fix the leak.)
- How does `kubectl logs` work under the hood?

---

## Chapter 5: Demo, CI, and presenting the project

**Code:** `scripts/demo.sh`, `.github/workflows/ci.yml`, `Makefile`, `README.md`

### The demo script

`sudo make demo` starts a control plane and **three agents on one machine**.
Each agent gets its own name, port and log directory, so to the control plane
they look like three separate machines. Then it walks through the story a
reviewer wants to see: pods spread across nodes, a node killed with `kill -9`,
pods replaced on the survivors, scaling, and an OOM kill.

Two shell techniques worth knowing:

- `trap cleanup EXIT` runs the cleanup function however the script ends:
  success, error (`set -e`) or Ctrl-C. Without it, a failed demo leaves
  servers running in the background.
- `mktemp -d` gives each run its own scratch directory, so runs never
  interfere with each other or with a real installation in `/var/lib/mkube`.

### What `kill -9` taught us: PDEATHSIG

When the demo kills node3's agent with `kill -9`, its containers die too, even
though the agent had no chance to stop them. That is because the runtime sets
`Pdeathsig: syscall.SIGKILL` (chapter 1, `container.go`): it asks the kernel
to kill the container's PID 1 when its parent dies, and killing PID 1 of a PID
namespace kills everything inside it.

That is a design choice with a trade-off. Real Kubernetes does the opposite:
containers are parented to a small "shim" process, so **restarting the kubelet
doesn't restart your containers**, which matters a lot during upgrades. A good
exercise is to remove PDEATHSIG and make the agent re-adopt running containers
on start-up (hint: store each container's PID in a file and find its cgroup).

### Continuous integration

`.github/workflows/ci.yml` runs on every push and pull request, in two jobs:

1. **Lint and unit tests** (no root): `gofmt` must report no unformatted files,
   `go vet` must pass, and all tests run with the race detector.
2. **Real containers** (with `sudo`): builds an Alpine rootfs and runs the
   runtime, agent and end-to-end tests with real namespaces and cgroups.

GitHub's Ubuntu runners use **cgroup v2**, while the machine this project was
developed on uses **cgroup v1**, so CI is also what proves both code paths in
`cgroups.go` work. The second job only runs if the first passes (`needs:
unit`), so cheap checks fail fast.

### How to present this project

**Resume bullets** (pick 2 or 3):

- Built a container orchestrator from scratch in Go, inspired by Kubernetes:
  container runtime, node agent, API server, scheduler, controllers and CLI
  (about 2,800 lines of Go plus 1,500 lines of tests, one dependency).
- Implemented Linux containers directly on namespaces, cgroups v1/v2 and
  `pivot_root`, enforcing CPU, memory and process limits with OOM detection.
- Designed level-triggered reconciliation loops for self-healing (node
  failure detection and pod rescheduling), zero-downtime rolling updates and
  exponential-backoff restarts.
- Added optimistic concurrency control with resource versions, and an
  end-to-end test suite that runs a full multi-node cluster in one process,
  in CI with real containers.

**The 2-minute story for interviews.** Interviewers love a bug story, and you
have a real one: *"My first `pivot_root` used a temporary `.oldroot`
directory. Unit tests passed. When I built the node agent, it started three
containers from the same image at once, and one failed with 'no such file or
directory'. All three were creating and deleting the same directory inside the
shared image. I switched to the `pivot_root(".", ".")` form that runc uses,
which needs no directory, and added an integration test that starts containers
in parallel."* It shows debugging, reading other people's code (runc), and
learning from a failure.

**Questions to be ready for:**

- "Why did you poll instead of using watches?" Simplicity first; explain the
  trade-off and how watches would work (chapter 3, concept 7).
- "What happens if the API server dies?" Running containers keep running; the
  agents keep retrying; nothing new is scheduled until it is back, and it
  reloads its state from `state.json`.
- "What would you add next?" Networking between pods on different nodes (a
  bridge plus routes or VXLAN), or watches, or requests vs. limits.

### Exercises

1. Record the demo with `asciinema rec` (or `vhs`) and add the GIF to the
   README. A moving demo gets far more attention than text.
2. Add a `Dockerfile`-free "image build": `minictl image import NAME
   rootfs.tar.gz` that unpacks a tarball into every node's image directory.
3. Add pod networking: create a bridge `mkube0`, give each container a
   `veth` pair and an IP, and let pods on one node reach each other.

---

## Chapter 6: Professional quality and the NASA/JPL rules

**Files:** `.golangci.yml`, `.github/`, `CONTRIBUTING.md`, `Makefile`,
`.pre-commit-config.yaml`, `scripts/check-coverage.sh`

### What the "Power of Ten" is

In 2006 Gerard Holzmann at NASA's Jet Propulsion Laboratory wrote ten rules
for safety-critical C code, the kind that flies on Mars rovers. The idea: a
small set of rules that a tool can check automatically is worth more than a
thick style guide nobody reads. The rules are for C, so each one has to be
translated into Go. This table is the translation used in this project, and
how each rule is enforced:

| # | JPL rule (for C) | What it means in this Go code | Enforced by |
|---|---|---|---|
| 1 | Simple control flow: no `goto`, no recursion | No recursion anywhere; complexity limits keep functions shallow | `gocyclo` ≤ 12, `gocognit` ≤ 20, `nestif` |
| 2 | Every loop has a fixed upper bound | Retry loops have a constant cap (`maxAttempts = 5`); service loops are bounded in *time*: they wait between passes and stop when their `context` is cancelled | Code review, comments at each loop |
| 3 | No dynamic memory after start-up | Go has a garbage collector, so the spirit is "bounded resources": request bodies capped at 1 MB (`MaxBytesReader`), manifest input at 10 MB, error bodies at 4 KB, and every network wait has a timeout | `gosec` (G112 Slowloris), `noctx` |
| 4 | A function fits on one printed page (~60 lines) | At most 60 lines and 40 statements | `funlen` |
| 5 | At least two assertions per function | Go has no `assert`; public entry points check their preconditions and return errors (`Validate`, `if cfg == nil`), and tests check invariants | Code review, tests |
| 6 | Declare data in the smallest scope | `if err := ...; err != nil`, no package-level mutable state except test hooks | `ineffassign`, `wastedassign`, `unused` |
| 7 | Check every return value; cast to `void` if ignored | Every error is checked. Ignoring one is written `_ = f.Close()` with a comment saying why it is safe | `errcheck`, `errorlint`, `bodyclose` |
| 8 | Limit the preprocessor | Go has no macros; the project uses no code generation and no build tags | (by construction) |
| 9 | Restrict pointers; no function pointers | The one use of `unsafe` (an `ioctl` in `rootfs.go`) is isolated and audited. Function values are used only for dependency injection | `gosec` G103 |
| 10 | Compile with all warnings on, zero warnings, use static analysers daily | `go vet` with all analysers, `staticcheck`, `revive`; CI fails on any finding | `golangci-lint` in CI and pre-commit |

### What applying the rules changed

Turning the linters on found **80 issues** in code that already passed all
its tests. Fixing them was not just cosmetic:

- **41 unchecked errors (rule 7).** Most were harmless (`Close` on a file only
  read from), but some were not: if the deferred `cgroup.Remove()` failed, the
  cgroup leaked silently, and a failed status update in the controller was
  ignored. Now each one is either handled or explicitly ignored with a reason.
- **`runtime.Run` and the agent's `runPod` were too long (rule 4).** Splitting
  them produced named steps (`initCommand`, `handshake`, `waitResult`,
  `runOnce`, `exitStatus`) that read like the explanation in chapter 1.
- **The deployment controller was the most complex function (rule 1).** It
  was split into `planDeployment`, a **pure function** that only decides what
  to do, and a small executor that does it. A pure function can be tested
  with plain data, so `plan_test.go` now checks eight rollout cases, including
  "a crashing new version stalls the rollout", without any HTTP or containers.
- **HTTP servers had no timeouts (rule 3).** A client that opens a connection
  and sends headers one byte a minute (a "Slowloris" attack) could exhaust
  the server. Every server now has read and idle timeouts, and every outgoing
  request carries a context with a deadline.
- **Files were created world-readable.** Logs and the state file are now
  `0600`, directories `0750`. The three exceptions (`/proc`, `/tmp`, `/dev`
  inside a container must be world-accessible) carry a `//nolint:gosec`
  comment explaining why.
- **The CLI's output errors.** Checking the error of every `fmt.Fprintf` would
  bury the logic. Instead `minictl` writes through a small `printer` that
  remembers the first error and skips later writes, and `run` checks it once.
  This "sticky error" pattern comes from Rob Pike's essay *Errors are values*.

### The tooling, and why each piece exists

| Tool | What it catches | When it runs |
|---|---|---|
| **golangci-lint** | 19 linters in one fast pass (the JPL table above) | pre-commit, CI `lint` job |
| **`go test -race`** | Data races between goroutines | CI `unit` job |
| **Integration tests as root** | Bugs only real namespaces and cgroups reveal (remember `.oldroot`) | CI `integration` job |
| **Coverage gate** | Untested code creeping in. The threshold is 65% because about a third of the code needs a real multi-machine setup or failure injection to reach | CI `integration` job |
| **govulncheck** | Known CVEs in dependencies *and the Go standard library*, only if your code actually calls the vulnerable function | CI `security` job |

> **What govulncheck caught on the first CI run.** CI built with Go 1.23.12,
> the version named in `go.mod`, and govulncheck failed: 38 known
> vulnerabilities in that release's standard library (`net/http`,
> `crypto/x509`, ...) were reachable from our code, for example through
> `http.Client.Do`. Our code was fine; the *toolchain* was out of date. The
> fix was to build with the latest stable Go (`go-version: stable` in the
> workflows) while keeping `go 1.23` in `go.mod` as the minimum language
> version. Lesson: your dependencies include the compiler and standard
> library, and they need patching too.
>
> Moving to the newest Go then broke two *tools*: govulncheck v1.1.4
> crashed parsing Go 1.27's standard library, and the prebuilt
> golangci-lint v2.5.0 could not read its export data. Both were upgraded
> (govulncheck v1.9.0, golangci-lint v2.15.0, now built from source with the
> job's own Go). The newer linter also found a real issue: the agent logged a
> pod name taken straight from an HTTP request, so a name containing a
> newline could forge log lines ("log injection"). Tools age like any other
> dependency, which is why Dependabot watches the workflows too.
| **CodeQL** | Deeper security flaws (injection, path traversal) by analysing data flow | Every PR and weekly |
| **Dependabot** | Outdated dependencies and GitHub Actions | Weekly pull requests |
| **pre-commit** | All of the above that is fast, before code even leaves your machine | Every `git commit` |

The CI jobs run in parallel where they can, and the slow root job only starts
once lint and unit tests pass (`needs: [lint, unit]`), so feedback on a typo
arrives in a minute, not five. `concurrency: cancel-in-progress` cancels the
old run when you push again to the same branch.

### The rest of a professional repository

- **LICENSE** (MIT): without one, nobody may legally use your code.
- **CONTRIBUTING.md**: how to set up, the coding standard, how to send a PR.
- **SECURITY.md**: how to report a vulnerability privately.
- **CHANGELOG.md** in the *Keep a Changelog* format, and **Semantic
  Versioning**: `MAJOR.MINOR.PATCH`, where MAJOR changes break users.
- **Release workflow**: pushing a tag like `v0.1.0` runs the tests, builds
  static Linux binaries for amd64 and arm64 with SHA-256 checksums, and
  publishes a GitHub release with the matching CHANGELOG section as notes.
- **Issue and PR templates** so reports arrive with the information needed,
  and a **CODEOWNERS** file so reviews are requested automatically.
- **Badges** in the README: a reviewer sees at a glance that CI is green.

### Exercises

1. Lower `funlen` to 40 lines and refactor what breaks. Does the code read
   better or worse? Where is the sweet spot?
2. Add `gomodguard` to forbid any new dependency without a review.
3. Turn on branch protection on GitHub: require the CI checks to pass and one
   review before merging to `main`.
4. Add fuzz testing (`go test -fuzz`) for `manifest.Parse` and `ParseMemory`:
   both take untrusted input.

### Interview questions this chapter prepares you for

- What is static analysis, and what can it catch that tests can't?
- How do you keep a codebase consistent across many contributors?
- Why might you set a coverage threshold below 100%?
- What is a supply-chain attack, and what in this repo defends against it?
  (Pinned tool versions, Dependabot, govulncheck, CodeQL, least-privilege
  `permissions:` in every workflow.)
