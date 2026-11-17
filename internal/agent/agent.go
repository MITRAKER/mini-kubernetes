// Package agent is the node agent, Mini Kubernetes' version of the kubelet.
//
// One agent runs on every node. It never decides *what* should run; the
// control plane does that by binding pods to nodes. The agent only makes
// its own machine match what the API server says, over and over:
//
//	loop every SyncInterval:
//	    send a heartbeat
//	    desired := pods bound to this node
//	    start every desired pod that is not running here
//	    stop every running pod that is no longer desired
//
// This "observe, compare, act" loop is called reconciliation, and it is the
// central idea of Kubernetes.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
	"github.com/MITRAKER/mini-kubernetes/internal/client"
	"github.com/MITRAKER/mini-kubernetes/internal/runtime"
)

// Runner starts a container and blocks until it exits or ctx is cancelled.
// runtime.Run satisfies it; tests use a fake.
type Runner func(ctx context.Context, cfg *runtime.Config, stdio runtime.IO) (runtime.Result, error)

// Agent is a node agent.
type Agent struct {
	Name     string
	Address  string // where this agent's HTTP server is reachable
	Capacity api.Resources
	API      *client.Client
	Run      Runner
	// ImageDir holds one root filesystem per image: ImageDir/<image>/.
	ImageDir string
	// LogDir receives one log file per pod.
	LogDir       string
	SyncInterval time.Duration
	// BackoffBase is the first restart delay; it doubles up to BackoffMax.
	BackoffBase, BackoffMax time.Duration

	mu      sync.Mutex
	workers map[string]*worker
}

// worker owns one pod's container on this node.
type worker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Start runs the sync loop until ctx is cancelled, then stops every pod.
func (a *Agent) Start(ctx context.Context) {
	a.defaults()
	t := time.NewTicker(a.SyncInterval)
	defer t.Stop()
	for {
		if err := a.Sync(ctx); err != nil && ctx.Err() == nil {
			log.Printf("agent %s: sync: %v", a.Name, err)
		}
		select {
		case <-ctx.Done():
			a.stopAll()
			return
		case <-t.C:
		}
	}
}

func (a *Agent) defaults() {
	if a.SyncInterval == 0 {
		a.SyncInterval = 2 * time.Second
	}
	if a.BackoffBase == 0 {
		a.BackoffBase = time.Second
	}
	if a.BackoffMax == 0 {
		a.BackoffMax = 30 * time.Second
	}
	a.mu.Lock()
	if a.workers == nil {
		a.workers = map[string]*worker{}
	}
	a.mu.Unlock()
}

// Sync does one round of heartbeat and reconciliation.
func (a *Agent) Sync(ctx context.Context) error {
	a.defaults()
	err := a.API.PutNode(ctx, &api.Node{
		Name: a.Name, Address: a.Address, Capacity: a.Capacity,
		LastHeartbeat: time.Now(), Ready: true,
	})
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	pods, err := a.API.ListPods(ctx, a.Name)
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	desired := map[string]bool{}
	for _, p := range pods {
		desired[p.Name] = true
		if _, running := a.workers[p.Name]; running {
			continue
		}
		// A pod that already finished stays finished, even if this agent
		// restarted and has forgotten about it.
		if p.Status.Phase == api.PodSucceeded || p.Status.Phase == api.PodFailed {
			continue
		}
		a.startWorker(p)
	}
	for name, w := range a.workers {
		if !desired[name] {
			log.Printf("agent %s: pod %s was deleted, stopping it", a.Name, name)
			w.cancel()
			delete(a.workers, name)
		}
	}
	return nil
}

// Running returns the names of the pods this agent is managing.
func (a *Agent) Running() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var names []string
	for n := range a.workers {
		names = append(names, n)
	}
	return names
}

func (a *Agent) stopAll() {
	a.mu.Lock()
	ws := a.workers
	a.workers = map[string]*worker{}
	a.mu.Unlock()
	for _, w := range ws {
		w.cancel()
		<-w.done
	}
}

// startWorker must be called with a.mu held.
func (a *Agent) startWorker(p api.Pod) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &worker{cancel: cancel, done: make(chan struct{})}
	a.workers[p.Name] = w
	go func() {
		defer close(w.done)
		a.runPod(ctx, p)
	}()
}

// runPod runs one pod's container, restarting it according to its
// RestartPolicy, until it finishes for good or ctx is cancelled.
//
// This loop has no fixed iteration bound by design: a service is restarted
// for as long as it is wanted. It is bounded in time instead: every pass
// waits at least BackoffBase, and cancelling ctx ends it (see JPL rule 2 in
// LEARNING.md).
func (a *Agent) runPod(ctx context.Context, p api.Pod) {
	status := p.Status
	backoff := a.BackoffBase
	for ctx.Err() == nil {
		ran, ok := a.runOnce(ctx, p, &status)
		if !ok || ctx.Err() != nil {
			return
		}
		if !shouldRestart(p.Spec.RestartPolicy, status.ExitCode) {
			status.Phase = api.PodSucceeded
			if status.ExitCode != 0 {
				status.Phase = api.PodFailed
			}
			a.report(ctx, p.Name, status)
			return
		}
		// A container that ran for a good while before exiting is not
		// crash-looping, so start counting from the beginning again.
		if ran > 2*a.BackoffMax {
			backoff = a.BackoffBase
		}
		status.Restarts++
		if status.ExitCode != 0 {
			status.Message = fmt.Sprintf("last exit: %s (code %d); restarting in %s", status.Reason, status.ExitCode, backoff)
			status.Reason = "CrashLoopBackOff"
		}
		a.report(ctx, p.Name, status)
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, a.BackoffMax)
	}
}

// runOnce starts the pod's container once and records how it ended in
// status. It returns how long the container ran, and false if the pod
// cannot be run at all.
func (a *Agent) runOnce(ctx context.Context, p api.Pod, status *api.PodStatus) (time.Duration, bool) {
	cfg, logFile, err := a.containerConfig(p)
	if err != nil {
		a.report(ctx, p.Name, api.PodStatus{Phase: api.PodFailed, Reason: "InvalidSpec", Message: err.Error(), Restarts: status.Restarts})
		return 0, false
	}
	start := time.Now()
	status.Phase, status.Reason, status.Message, status.Started = api.PodRunning, "", "", &start
	a.report(ctx, p.Name, *status)

	res, runErr := a.Run(ctx, cfg, runtime.IO{Stdin: nil, Stdout: logFile, Stderr: logFile})
	if err := logFile.Close(); err != nil {
		log.Printf("agent %s: close log of %s: %v", a.Name, p.Name, err)
	}
	status.ExitCode, status.Reason, status.Message = exitStatus(res, runErr)
	return time.Since(start), true
}

// exitStatus turns a runtime result into an exit code, reason and message.
func exitStatus(res runtime.Result, err error) (code int, reason, message string) {
	switch {
	case err != nil:
		return 128, "StartError", err.Error()
	case res.OOMKilled:
		return res.ExitCode, "OOMKilled", ""
	case res.ExitCode == 0:
		return 0, "Completed", ""
	default:
		return res.ExitCode, "Error", ""
	}
}

// sleep waits for d, and returns false early if ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func shouldRestart(policy api.RestartPolicy, exitCode int) bool {
	switch policy {
	case api.RestartNever:
		return false
	case api.RestartOnFailure:
		return exitCode != 0
	default: // Always, and the default when unset, like Kubernetes
		return true
	}
}

func (a *Agent) containerConfig(p api.Pod) (*runtime.Config, *os.File, error) {
	if p.Spec.Image == "" || filepath.Base(p.Spec.Image) != p.Spec.Image {
		return nil, nil, fmt.Errorf("invalid image %q", p.Spec.Image)
	}
	rootfs := filepath.Join(a.ImageDir, p.Spec.Image)
	if _, err := os.Stat(rootfs); err != nil {
		return nil, nil, fmt.Errorf("image %q not found on node %s", p.Spec.Image, a.Name)
	}
	if err := os.MkdirAll(a.LogDir, 0o750); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(a.logPath(p.Name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	cfg := &runtime.Config{
		ID:          p.Name,
		RootFS:      rootfs,
		Command:     p.Spec.Command,
		Env:         p.Spec.Env,
		MemoryBytes: p.Spec.Limits.Memory,
		MilliCPU:    p.Spec.Limits.MilliCPU,
		MaxPids:     256,
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	return cfg, f, nil
}

func (a *Agent) logPath(pod string) string {
	return filepath.Join(a.LogDir, pod+".log")
}

func (a *Agent) report(ctx context.Context, pod string, s api.PodStatus) {
	if err := a.API.UpdatePodStatus(ctx, pod, s); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("agent %s: update status of %s: %v", a.Name, pod, err)
	}
}

// Handler serves the agent's own small HTTP API, used by the CLI to read
// logs straight from the node (the API server never stores logs, just as in
// Kubernetes, where `kubectl logs` is proxied to the kubelet).
//
//	GET /healthz
//	GET /logs/{pod}
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		// A failed write means the client went away; there is no one to tell.
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /logs/{pod}", func(w http.ResponseWriter, r *http.Request) {
		pod := r.PathValue("pod")
		if filepath.Base(pod) != pod {
			http.Error(w, "bad pod name", http.StatusBadRequest)
			return
		}
		f, err := os.Open(a.logPath(pod)) //nolint:gosec // G304: pod is a single path element, checked above
		if err != nil {
			http.Error(w, "no logs for pod "+pod, http.StatusNotFound)
			return
		}
		defer func() { _ = f.Close() }() // read-only: close cannot lose data
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := io.Copy(w, f); err != nil {
			// The pod name comes from the request, so it is not logged: a
			// crafted name containing newlines could forge log lines.
			log.Printf("agent %s: send logs: %v", a.Name, err)
		}
	})
	return mux
}
