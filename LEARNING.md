# Learning Mini Kubernetes

This file grows with the project. Each chapter explains one milestone: what
was built, why it was designed that way, the concepts behind it, and an
exercise so you can rebuild or extend it yourself.

- [Chapter 1: A container runtime from scratch](#chapter-1-a-container-runtime-from-scratch)

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
3. `pivot_root(rootfs, rootfs/.oldroot)`: the rootfs is now `/`.
4. Mount a fresh **`/proc`**. Because we are in a new PID namespace, it shows
   only the container's processes. This is why `ps` inside shows 3 processes.
5. Mount a small **`/dev`** with only safe devices (`null`, `zero`, `urandom`,
   ...), so the container cannot touch the host's disks.
6. **Unmount `/.oldroot`**. The host filesystem is now unreachable.

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
