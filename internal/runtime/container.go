package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// InitArg is the hidden subcommand the runtime re-executes itself with to
// become the container's first process. The CLI must route it to Init.
const InitArg = "__init"

// Result is how a container ended.
type Result struct {
	ExitCode  int
	OOMKilled bool
}

// IO holds the streams a container's process is wired to.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run starts a container and waits for it to exit.
//
// It works in two halves, because Go cannot safely call unshare() in a
// running, multi-threaded program:
//
//  1. Here, in the parent, we start a copy of our own binary
//     (/proc/self/exe __init) with clone flags that put it in new
//     namespaces, and place it in the container's cgroup.
//  2. That child runs Init, which sets up the hostname and filesystem from
//     inside the namespaces and then execs the user's command.
//
// The config travels to the child through a pipe. The child blocks reading
// it, which doubles as a "go" signal: we only send it once the child is in
// its cgroup, so the user's command can never run unlimited.
func Run(ctx context.Context, cfg *Config, stdio IO) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	if cfg.Hostname == "" {
		cfg.Hostname = cfg.ID
	}

	cg, err := NewCgroup(cfg.ID, cfg)
	if err != nil {
		if cg != nil {
			_ = cg.Remove()
		}
		return Result{}, err
	}
	defer cg.Remove()

	r, w, err := os.Pipe()
	if err != nil {
		return Result{}, err
	}
	defer r.Close()
	defer w.Close()

	cmd := exec.CommandContext(ctx, "/proc/self/exe", InitArg)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr
	cmd.ExtraFiles = []*os.File{r} // becomes fd 3 in the child
	cmd.Env = []string{}

	flags := uintptr(syscall.CLONE_NEWUTS | // own hostname
		syscall.CLONE_NEWPID | // own process tree: the command is PID 1
		syscall.CLONE_NEWNS | // own mount table
		syscall.CLONE_NEWIPC) // own shared memory and semaphores
	if cfg.Network {
		flags |= syscall.CLONE_NEWNET // own network stack
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: flags,
		// If the runtime dies, take the container with it.
		Pdeathsig: syscall.SIGKILL,
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start container: %w", err)
	}
	r.Close()

	if err := cg.AddProcess(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Result{}, err
	}
	if err := json.NewEncoder(w).Encode(cfg); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Result{}, fmt.Errorf("send config: %w", err)
	}
	w.Close()

	err = cmd.Wait()
	res := Result{OOMKilled: cg.OOMKilled()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitCode(exitErr.ProcessState)
	case ctx.Err() != nil:
		res.ExitCode = 137
	default:
		return res, err
	}
	return res, nil
}

// exitCode follows the shell convention: a process killed by signal N
// reports 128+N.
func exitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// Init runs inside the new namespaces as the container's first process. It
// never returns on success: it replaces itself with the user's command.
func Init() error {
	f := os.NewFile(3, "config")
	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	f.Close()

	if err := syscall.Sethostname([]byte(cfg.Hostname)); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	if err := setupRootFS(cfg.RootFS); err != nil {
		return err
	}
	if cfg.Network {
		if err := loopbackUp(); err != nil {
			return fmt.Errorf("bring up loopback: %w", err)
		}
	}

	env := cfg.Env
	if !hasEnv(env, "PATH") {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "PATH" {
			os.Setenv("PATH", v)
		}
	}
	path, err := exec.LookPath(cfg.Command[0])
	if err != nil {
		return fmt.Errorf("find %q in container: %w", cfg.Command[0], err)
	}
	return syscall.Exec(path, cfg.Command, env)
}

func hasEnv(env []string, key string) bool {
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k == key {
			return true
		}
	}
	return false
}
