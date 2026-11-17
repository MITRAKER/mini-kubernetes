// Package e2e runs a whole Mini Kubernetes cluster inside one test process:
// an API server, the scheduler, the controllers and two node agents, all
// talking over real HTTP. By default containers are faked; with root and
// MKUBE_ROOTFS set, TestClusterRealContainers uses real Linux containers.
package e2e

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// The real runtime re-executes /proc/self/exe, which here is this test
// binary, so it must be able to act as a container's init process.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == runtime.InitArg {
		if err := runtime.Init(); err != nil {
			fmt.Fprintln(os.Stderr, "init:", err)
			os.Exit(127)
		}
	}
	os.Exit(m.Run())
}

// fakeRun pretends to start a container that runs until it is stopped.
func fakeRun(ctx context.Context, _ *runtime.Config, _ runtime.IO) (runtime.Result, error) {
	<-ctx.Done()
	return runtime.Result{ExitCode: 137}, nil
}

type cluster struct {
	t      *testing.T
	api    *client.Client
	agents map[string]context.CancelFunc
}

func newCluster(t *testing.T, run agent.Runner, rootfs string) *cluster {
	store, _ := apiserver.NewStore("")
	srv := httptest.NewServer((&apiserver.Server{Store: store}).Handler())
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := &cluster{t: t, api: client.New(srv.URL), agents: map[string]context.CancelFunc{}}
	go (&scheduler.Scheduler{API: c.api, Interval: 50 * time.Millisecond}).Run(ctx)
	go (&controller.Manager{API: c.api, Interval: 50 * time.Millisecond, NodeTimeout: 500 * time.Millisecond}).Run(ctx)

	images := filepath.Join(t.TempDir(), "images")
	os.MkdirAll(images, 0o755)
	if rootfs != "" {
		os.Symlink(rootfs, filepath.Join(images, "alpine"))
	} else {
		os.MkdirAll(filepath.Join(images, "alpine"), 0o755)
	}
	for _, name := range []string{"node1", "node2"} {
		actx, acancel := context.WithCancel(ctx)
		a := &agent.Agent{
			Name: name, API: c.api, Run: run,
			Capacity: api.Resources{MilliCPU: 2000, Memory: 2 << 30},
			ImageDir: images, LogDir: filepath.Join(t.TempDir(), name),
			SyncInterval: 50 * time.Millisecond,
		}
		done := make(chan struct{})
		go func() { a.Start(actx); close(done) }()
		c.agents[name] = func() { acancel(); <-done }
	}
	t.Cleanup(func() {
		for _, stop := range c.agents {
			stop()
		}
	})
	return c
}

// running returns the Running pods of a deployment, by node.
func (c *cluster) running(dep string) map[string][]api.Pod {
	pods, err := c.api.ListPods(context.Background(), "")
	if err != nil {
		c.t.Fatal(err)
	}
	out := map[string][]api.Pod{}
	for _, p := range pods {
		if p.Labels[api.DeploymentLabel] == dep && p.Status.Phase == api.PodRunning {
			out[p.NodeName] = append(out[p.NodeName], p)
		}
	}
	return out
}

func (c *cluster) total(dep string) int {
	n := 0
	for _, ps := range c.running(dep) {
		n += len(ps)
	}
	return n
}

func (c *cluster) countAll(dep string) int {
	pods, _ := c.api.ListPods(context.Background(), "")
	n := 0
	for _, p := range pods {
		if p.Labels[api.DeploymentLabel] == dep {
			n++
		}
	}
	return n
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, what)
}

func scenario(t *testing.T, c *cluster, command []string, timeout time.Duration) {
	ctx := context.Background()
	dep := &api.Deployment{Name: "web", Replicas: 4, Template: api.PodSpec{
		Image: "alpine", Command: command,
		Limits: api.Resources{MilliCPU: 250, Memory: 64 << 20},
	}}
	if _, err := c.api.ApplyDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}

	t.Log("scale up: 4 replicas spread over both nodes")
	eventually(t, "4 running pods", timeout, func() bool { return c.total("web") == 4 })
	if r := c.running("web"); len(r["node1"]) != 2 || len(r["node2"]) != 2 {
		t.Errorf("pods not spread evenly: node1=%d node2=%d", len(r["node1"]), len(r["node2"]))
	}

	t.Log("scale down to 2")
	d, _ := c.api.GetDeployment(ctx, "web")
	d.Replicas = 2
	if _, err := c.api.ApplyDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	eventually(t, "2 pods left", timeout, func() bool { return c.countAll("web") == 2 && c.total("web") == 2 })

	// Kill the node holding the most pods, so the test always moves some.
	dead, alive := "node1", "node2"
	if r := c.running("web"); len(r["node2"]) > len(r["node1"]) {
		dead, alive = "node2", "node1"
	}
	moved := map[string]bool{}
	for _, p := range c.running("web")[dead] {
		moved[p.Name] = true
	}
	t.Logf("node failure: stop %s's agent (running %d pods), they must move to %s", dead, len(moved), alive)
	c.agents[dead]()
	eventually(t, "all pods on "+alive, timeout+2*time.Second, func() bool {
		r := c.running("web")
		return len(r[alive]) == 2 && len(r[dead]) == 0
	})
	for _, p := range c.running("web")[alive] {
		if moved[p.Name] {
			t.Errorf("pod %s should have been replaced, not kept", p.Name)
		}
	}
	nodes, _ := c.api.ListNodes(ctx)
	for _, n := range nodes {
		if n.Name == dead && n.Ready {
			t.Errorf("%s still marked Ready", dead)
		}
	}

	t.Log("rolling update: change the command")
	d, _ = c.api.GetDeployment(ctx, "web")
	d.Template.Env = []string{"VERSION=2"}
	if _, err := c.api.ApplyDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	eventually(t, "rollout finished", timeout, func() bool {
		pods := c.running("web")[alive]
		if len(pods) != 2 || c.countAll("web") != 2 {
			return false
		}
		for _, p := range pods {
			if len(p.Spec.Env) != 1 {
				return false
			}
		}
		return true
	})

	t.Log("delete the deployment: its pods are garbage-collected")
	if err := c.api.DeleteDeployment(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "no pods left", timeout, func() bool { return c.countAll("web") == 0 })
}

func TestCluster(t *testing.T) {
	c := newCluster(t, fakeRun, "")
	scenario(t, c, []string{"sleep", "1000"}, 3*time.Second)
}

func TestClusterRealContainers(t *testing.T) {
	rootfs := os.Getenv("MKUBE_ROOTFS")
	if os.Geteuid() != 0 || rootfs == "" {
		t.Skip("needs root and MKUBE_ROOTFS")
	}
	c := newCluster(t, runtime.Run, rootfs)
	scenario(t, c, []string{"sleep", "1000"}, 5*time.Second)
}
