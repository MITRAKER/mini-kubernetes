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
func Run(ctx context.Context, cfg *Config, stdio IO) (res Result, err error) {
	if ctx == nil || cfg == nil { // precondition (JPL rule 5)
		return Result{}, errors.New("runtime.Run: ctx and cfg are required")
	}
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	if cfg.Hostname == "" {
		cfg.Hostname = cfg.ID
	}

	cg, err := NewCgroup(cfg.ID, cfg)
	if err != nil {
		return Result{}, err
	}
	// A cgroup that cannot be removed is a leak the operator must know
	// about, so its error is reported unless an earlier error wins.
	defer func() {
		if rmErr := cg.Remove(); rmErr != nil && err == nil {
			err = fmt.Errorf("remove cgroup: %w", rmErr)
		}
	}()

	r, w, err := os.Pipe()
	if err != nil {
		return Result{}, err
	}
	defer closeQuietly(w) // both ends are closed explicitly on the success path
	defer closeQuietly(r)

	cmd := initCommand(ctx, cfg, stdio, r)
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start container: %w", err)
	}
	if err := handshake(cmd, cg, w, r, cfg); err != nil {
		return Result{}, err
	}
	return waitResult(ctx, cmd, cg)
}

// initCommand prepares "/proc/self/exe __init" with the clone flags that put
// it in new namespaces.
func initCommand(ctx context.Context, cfg *Config, stdio IO, configPipe *os.File) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/proc/self/exe", InitArg)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr
	cmd.ExtraFiles = []*os.File{configPipe} // becomes fd 3 in the child
	// The init process gets no host environment; the command's own
	// environment travels in the config. GOMAXPROCS=1 keeps the Go
	// runtime's thread count low, because every thread counts against
	// the pids limit until init execs the command.
	cmd.Env = []string{"GOMAXPROCS=1"}

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
	return cmd
}

// handshake places the started child in its cgroup and only then sends it
// the config, which is its signal to continue. On failure the child is
// killed and reaped, so no process is ever left behind unlimited.
func handshake(cmd *exec.Cmd, cg *Cgroup, w, r *os.File, cfg *Config) error {
	// The child has its own copy of the read end now.
	if err := r.Close(); err != nil {
		return abort(cmd, fmt.Errorf("close pipe: %w", err))
	}
	if err := cg.AddProcess(cmd.Process.Pid); err != nil {
		return abort(cmd, err)
	}
	if err := json.NewEncoder(w).Encode(cfg); err != nil {
		return abort(cmd, fmt.Errorf("send config: %w", err))
	}
	if err := w.Close(); err != nil {
		return abort(cmd, fmt.Errorf("close pipe: %w", err))
	}
	return nil
}

// abort kills and reaps a child that failed to start properly. The kill and
// wait errors are deliberately ignored: the original cause is what matters,
// and the child is gone either way.
func abort(cmd *exec.Cmd, cause error) error {
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return cause
}

// waitResult waits for the container and turns how it ended into a Result.
func waitResult(ctx context.Context, cmd *exec.Cmd, cg *Cgroup) (Result, error) {
	err := cmd.Wait()
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

// closeQuietly closes a file whose close error carries no information,
// such as a pipe end that may already be closed. Ignoring it is explicit.
func closeQuietly(f *os.File) {
	_ = f.Close()
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
	if err := f.Close(); err != nil {
		return fmt.Errorf("close config pipe: %w", err)
	}

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

	path, env, err := resolveCommand(&cfg)
	if err != nil {
		return err
	}
	return syscall.Exec(path, cfg.Command, env)
}

// resolveCommand finds the command's binary inside the container, using the
// container's PATH (or a standard default), and returns the final
// environment.
func resolveCommand(cfg *Config) (string, []string, error) {
	env := cfg.Env
	if !hasEnv(env, "PATH") {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "PATH" {
			if err := os.Setenv("PATH", v); err != nil {
				return "", nil, fmt.Errorf("set PATH: %w", err)
			}
		}
	}
	path, err := exec.LookPath(cfg.Command[0])
	if err != nil {
		return "", nil, fmt.Errorf("find %q in container: %w", cfg.Command[0], err)
	}
	return path, env, nil
}

func hasEnv(env []string, key string) bool {
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k == key {
			return true
		}
	}
	return false
}
