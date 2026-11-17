package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
	"github.com/MITRAKER/mini-kubernetes/internal/client"
	"github.com/MITRAKER/mini-kubernetes/internal/runtime"
)

// fakeAPI is a tiny in-memory stand-in for the API server, implementing only
// what the agent calls.
type fakeAPI struct {
	mu    sync.Mutex
	nodes map[string]api.Node
	pods  map[string]api.Pod
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{nodes: map[string]api.Node{}, pods: map[string]api.Pod{}}
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/nodes/{name}", func(_ http.ResponseWriter, r *http.Request) {
		var n api.Node
		json.NewDecoder(r.Body).Decode(&n)
		f.mu.Lock()
		f.nodes[n.Name] = n
		f.mu.Unlock()
	})
	mux.HandleFunc("GET /api/pods", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []api.Pod{}
		for _, p := range f.pods {
			if p.NodeName == r.URL.Query().Get("node") {
				out = append(out, p)
			}
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("PUT /api/pods/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		var s api.PodStatus
		json.NewDecoder(r.Body).Decode(&s)
		f.mu.Lock()
		defer f.mu.Unlock()
		p, ok := f.pods[r.PathValue("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		p.Status = s
		f.pods[p.Name] = p
	})
	return mux
}

func (f *fakeAPI) pod(name string) api.Pod {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pods[name]
}

func (f *fakeAPI) put(p api.Pod) {
	f.mu.Lock()
	f.pods[p.Name] = p
	f.mu.Unlock()
}

func (f *fakeAPI) del(name string) {
	f.mu.Lock()
	delete(f.pods, name)
	f.mu.Unlock()
}

// fakeRunner pretends to run containers: it records starts, writes a log
// line and returns the scripted result, or blocks until cancelled.
type fakeRunner struct {
	mu     sync.Mutex
	starts map[string]int
	result func(id string) (runtime.Result, bool) // bool: block until cancelled
}

func (r *fakeRunner) run(ctx context.Context, cfg *runtime.Config, stdio runtime.IO) (runtime.Result, error) {
	r.mu.Lock()
	r.starts[cfg.ID]++
	r.mu.Unlock()
	fmt.Fprintf(stdio.Stdout, "hello from %s\n", cfg.ID)
	res, block := r.result(cfg.ID)
	if block {
		<-ctx.Done()
		return runtime.Result{ExitCode: 137}, nil
	}
	return res, nil
}

func (r *fakeRunner) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts[id]
}

func setup(t *testing.T, result func(string) (runtime.Result, bool)) (*Agent, *fakeAPI, *fakeRunner) {
	t.Helper()
	fapi := newFakeAPI()
	srv := httptest.NewServer(fapi.handler())
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "images", "alpine"), 0o755)
	fr := &fakeRunner{starts: map[string]int{}, result: result}
	a := &Agent{
		Name: "node1", API: client.New(srv.URL), Run: fr.run,
		ImageDir: filepath.Join(dir, "images"), LogDir: filepath.Join(dir, "logs"),
		SyncInterval: 20 * time.Millisecond, BackoffBase: 10 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
	}
	return a, fapi, fr
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func pod(name string, policy api.RestartPolicy) api.Pod {
	return api.Pod{Name: name, NodeName: "node1", Spec: api.PodSpec{
		Image: "alpine", Command: []string{"sh"}, RestartPolicy: policy,
	}}
}

func TestHeartbeat(t *testing.T) {
	a, fapi, _ := setup(t, nil)
	if err := a.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := fapi.nodes["node1"]
	if !n.Ready || time.Since(n.LastHeartbeat) > time.Second {
		t.Errorf("node not registered as ready: %+v", n)
	}
}

func TestRunsAndStopsPods(t *testing.T) {
	a, fapi, fr := setup(t, func(string) (runtime.Result, bool) { return runtime.Result{}, true })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Start(ctx)

	fapi.put(pod("web", api.RestartAlways))
	eventually(t, "pod running", func() bool { return fapi.pod("web").Status.Phase == api.PodRunning })
	if fr.count("web") != 1 {
		t.Errorf("started %d times, want 1", fr.count("web"))
	}

	// Pods bound to other nodes are not ours.
	other := pod("elsewhere", api.RestartAlways)
	other.NodeName = "node2"
	fapi.put(other)

	fapi.del("web")
	eventually(t, "pod stopped", func() bool { return len(a.Running()) == 0 })
	if fr.count("elsewhere") != 0 {
		t.Error("ran a pod bound to another node")
	}
}

func TestRestartPolicies(t *testing.T) {
	exit := map[string]int{"job-ok": 0, "job-bad": 1, "crash": 1, "never": 3}
	a, fapi, fr := setup(t, func(id string) (runtime.Result, bool) {
		return runtime.Result{ExitCode: exit[id]}, false
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Start(ctx)

	fapi.put(pod("job-ok", api.RestartOnFailure))
	fapi.put(pod("job-bad", api.RestartOnFailure))
	fapi.put(pod("crash", api.RestartAlways))
	fapi.put(pod("never", api.RestartNever))

	eventually(t, "job-ok succeeded", func() bool { return fapi.pod("job-ok").Status.Phase == api.PodSucceeded })
	eventually(t, "never failed", func() bool { return fapi.pod("never").Status.Phase == api.PodFailed })
	eventually(t, "crash restarted 3 times", func() bool { return fapi.pod("crash").Status.Restarts >= 3 })
	eventually(t, "job-bad restarted", func() bool { return fapi.pod("job-bad").Status.Restarts >= 2 })

	if r := fapi.pod("crash").Status.Reason; r != "CrashLoopBackOff" {
		t.Errorf("crash reason = %q, want CrashLoopBackOff", r)
	}
	if n := fr.count("job-ok"); n != 1 {
		t.Errorf("job-ok ran %d times, want 1", n)
	}
	if s := fapi.pod("never").Status; s.ExitCode != 3 || s.Reason != "Error" {
		t.Errorf("never status = %+v", s)
	}
}

func TestOOMKilledReason(t *testing.T) {
	a, fapi, _ := setup(t, func(string) (runtime.Result, bool) {
		return runtime.Result{ExitCode: 137, OOMKilled: true}, false
	})
	fapi.put(pod("hog", api.RestartNever))
	a.Sync(context.Background())
	eventually(t, "hog failed", func() bool { return fapi.pod("hog").Status.Phase == api.PodFailed })
	if r := fapi.pod("hog").Status.Reason; r != "OOMKilled" {
		t.Errorf("reason = %q, want OOMKilled", r)
	}
}

func TestMissingImage(t *testing.T) {
	a, fapi, fr := setup(t, nil)
	p := pod("bad", api.RestartAlways)
	p.Spec.Image = "nginx"
	fapi.put(p)
	a.Sync(context.Background())
	eventually(t, "bad failed", func() bool { return fapi.pod("bad").Status.Reason == "InvalidSpec" })
	if fr.count("bad") != 0 {
		t.Error("runner called for a missing image")
	}
}

func TestLogsEndpoint(t *testing.T) {
	a, fapi, _ := setup(t, func(string) (runtime.Result, bool) { return runtime.Result{}, false })
	fapi.put(pod("hello", api.RestartNever))
	a.Sync(context.Background())
	eventually(t, "hello done", func() bool { return fapi.pod("hello").Status.Phase == api.PodSucceeded })

	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/logs/hello")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "hello from hello\n" {
		t.Errorf("logs = %q", b)
	}
}
