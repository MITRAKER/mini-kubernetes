package scheduler

import (
	"testing"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
)

func node(name string, cpu, mem int64, ready bool) api.Node {
	return api.Node{Name: name, Ready: ready, Capacity: api.Resources{MilliCPU: cpu, Memory: mem}}
}

func podNeeding(cpu, mem int64) api.Pod {
	return api.Pod{Spec: api.PodSpec{Limits: api.Resources{MilliCPU: cpu, Memory: mem}}}
}

func TestPickFilters(t *testing.T) {
	nodes := []api.Node{
		node("dead", 4000, 4<<30, false),
		node("small", 500, 4<<30, true),
		node("ok", 2000, 4<<30, true),
	}
	got, ok := Pick(podNeeding(1000, 1<<30), nodes, nil)
	if !ok || got != "ok" {
		t.Errorf("Pick = %q, %v; want ok", got, ok)
	}
	if _, ok := Pick(podNeeding(8000, 0), nodes, nil); ok {
		t.Error("placed a pod that fits nowhere")
	}
}

func TestPickSpreads(t *testing.T) {
	nodes := []api.Node{node("a", 2000, 2<<30, true), node("b", 2000, 2<<30, true)}
	used := map[string]api.Resources{"a": {MilliCPU: 1500, Memory: 1 << 30}}
	if got, _ := Pick(podNeeding(250, 256<<20), nodes, used); got != "b" {
		t.Errorf("Pick = %q, want the emptier node b", got)
	}
}

func TestPickCountsUsage(t *testing.T) {
	nodes := []api.Node{node("a", 1000, 1<<30, true)}
	used := map[string]api.Resources{"a": {MilliCPU: 800}}
	if _, ok := Pick(podNeeding(300, 0), nodes, used); ok {
		t.Error("overcommitted node a")
	}
}
