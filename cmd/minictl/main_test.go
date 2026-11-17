package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MITRAKER/mini-kubernetes/internal/agent"
	"github.com/MITRAKER/mini-kubernetes/internal/api"
	"github.com/MITRAKER/mini-kubernetes/internal/apiserver"
	"github.com/MITRAKER/mini-kubernetes/internal/client"
	"github.com/MITRAKER/mini-kubernetes/internal/controller"
	"github.com/MITRAKER/mini-kubernetes/internal/runtime"
	"github.com/MITRAKER/mini-kubernetes/internal/scheduler"
)

// startCluster runs a control plane and one agent with fake containers that
// print a line and then run until stopped.
func startCluster(t *testing.T) string {
	store, _ := apiserver.NewStore("")
	srv := httptest.NewServer((&apiserver.Server{Store: store}).Handler())
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	apiClient := client.New(srv.URL)
	go (&scheduler.Scheduler{API: apiClient, Interval: 20 * time.Millisecond}).Run(ctx)
	go (&controller.Manager{API: apiClient, Interval: 20 * time.Millisecond}).Run(ctx)

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "images", "alpine"), 0o755)
	a := &agent.Agent{
		Name: "node1", API: apiClient, Capacity: api.Resources{MilliCPU: 4000, Memory: 4 << 30},
		ImageDir: filepath.Join(dir, "images"), LogDir: filepath.Join(dir, "logs"),
		SyncInterval: 20 * time.Millisecond,
		Run: func(ctx context.Context, cfg *runtime.Config, io runtime.IO) (runtime.Result, error) {
			fmt.Fprintf(io.Stdout, "hello from %s\n", cfg.ID)
			<-ctx.Done()
			return runtime.Result{ExitCode: 137}, nil
		},
	}
	agentSrv := httptest.NewServer(a.Handler())
	t.Cleanup(agentSrv.Close)
	a.Address = agentSrv.URL
	done := make(chan struct{})
	go func() { a.Start(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return srv.URL
}

func ctl(t *testing.T, server string, args ...string) (string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(append([]string{"-server", server}, args...), &out, &errOut)
	return out.String() + errOut.String(), code
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWorkflow(t *testing.T) {
	s := startCluster(t)

	out, code := ctl(t, s, "apply", "-f", "../../examples/hello.yaml")
	if code != 0 || out != "deployment/hello created\n" {
		t.Fatalf("apply: %d %q", code, out)
	}
	if out, _ := ctl(t, s, "apply", "-f", "../../examples/hello.yaml"); out != "deployment/hello configured\n" {
		t.Errorf("second apply: %q", out)
	}

	eventually(t, "3/3 ready", func() bool {
		out, _ := ctl(t, s, "get", "deploy")
		return strings.Contains(out, "hello   3/3")
	})

	out, _ = ctl(t, s, "get", "pods")
	if strings.Count(out, "Running") != 3 || !strings.Contains(out, "NAME") {
		t.Errorf("get pods:\n%s", out)
	}
	pod := strings.Fields(strings.Split(out, "\n")[1])[0]

	out, code = ctl(t, s, "logs", pod)
	if code != 0 || out != "hello from "+pod+"\n" {
		t.Errorf("logs: %d %q", code, out)
	}

	if out, code := ctl(t, s, "scale", "deployment", "hello", "1"); code != 0 {
		t.Fatalf("scale: %s", out)
	}
	eventually(t, "1 pod", func() bool {
		out, _ := ctl(t, s, "get", "pods")
		return strings.Count(out, "\n") == 2
	})

	if out, _ := ctl(t, s, "get", "nodes"); !strings.Contains(out, "node1   Ready    4000m   4096Mi") {
		t.Errorf("get nodes:\n%s", out)
	}

	if out, code := ctl(t, s, "delete", "deployment", "hello"); code != 0 || out != "deployment/hello deleted\n" {
		t.Errorf("delete: %d %q", code, out)
	}
	eventually(t, "no pods", func() bool {
		out, _ := ctl(t, s, "get", "pods")
		return strings.Count(out, "\n") == 1
	})
}

func TestErrors(t *testing.T) {
	s := startCluster(t)
	cases := map[string][]string{
		`pod "nope" not found`:      {"logs", "nope"},
		`deployment "x" not found`:  {"delete", "deploy", "x"},
		"unknown resource type":     {"get", "services"},
		"unknown command":           {"frobnicate"},
		"replicas must be a number": {"scale", "deploy", "x", "lots"},
	}
	for want, args := range cases {
		out, code := ctl(t, s, args...)
		if code == 0 || !strings.Contains(out, want) {
			t.Errorf("%v: code %d, output %q, want it to contain %q", args, code, out, want)
		}
	}
}

func TestExamplesParse(t *testing.T) {
	files, _ := filepath.Glob("../../examples/*.yaml")
	if len(files) == 0 {
		t.Fatal("no examples found")
	}
	s := startCluster(t)
	for _, f := range files {
		if out, code := ctl(t, s, "apply", "-f", f); code != 0 {
			t.Errorf("%s: %s", f, out)
		}
	}
}
