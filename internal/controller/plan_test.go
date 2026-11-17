package controller

import (
	"reflect"
	"testing"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
)

var (
	v1 = api.PodSpec{Image: "alpine", Command: []string{"v1"}}
	v2 = api.PodSpec{Image: "alpine", Command: []string{"v2"}}
)

func pod(name string, spec api.PodSpec, phase api.PodPhase, reason string) api.Pod {
	return api.Pod{Name: name, NodeName: "n1", Spec: spec, Status: api.PodStatus{Phase: phase, Reason: reason}}
}

func running(name string, spec api.PodSpec) api.Pod { return pod(name, spec, api.PodRunning, "") }

func TestPlanDeployment(t *testing.T) {
	dep := func(replicas int, spec api.PodSpec) api.Deployment {
		return api.Deployment{Name: "web", Replicas: replicas, Template: spec}
	}
	cases := []struct {
		name       string
		d          api.Deployment
		pods       []api.Pod
		wantCreate int
		wantDelete []string
	}{
		{"scale up from zero", dep(3, v1), nil, 3, nil},
		{"steady state does nothing", dep(2, v1), []api.Pod{running("a", v1), running("b", v1)}, 0, nil},
		{"scale down removes unscheduled first", dep(1, v1),
			[]api.Pod{running("a", v1), {Name: "b", Spec: v1}}, 0, []string{"b"}},
		{"finished pods are replaced", dep(2, v1),
			[]api.Pod{running("a", v1), pod("b", v1, api.PodFailed, "OOMKilled")}, 0, []string{"b"}},
		{"rollout starts with one surge pod", dep(2, v2),
			[]api.Pod{running("a", v1), running("b", v1)}, 1, nil},
		{"rollout waits while the new pod is not ready", dep(2, v2),
			[]api.Pod{running("a", v1), running("b", v1), pod("c", v2, api.PodPending, "")}, 0, nil},
		{"rollout removes an old pod once the new one is ready", dep(2, v2),
			[]api.Pod{running("a", v1), running("b", v1), running("c", v2)}, 0, []string{"a"}},
		{"crashing new version stalls the rollout", dep(2, v2),
			[]api.Pod{running("a", v1), running("b", v1), pod("c", v2, api.PodRunning, "CrashLoopBackOff")}, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pl := planDeployment(tc.d, tc.pods)
			if pl.create != tc.wantCreate || !reflect.DeepEqual(pl.delete, tc.wantDelete) {
				t.Errorf("plan = create %d, delete %v; want create %d, delete %v",
					pl.create, pl.delete, tc.wantCreate, tc.wantDelete)
			}
		})
	}
}

func TestGroupByOwner(t *testing.T) {
	deps := []api.Deployment{{Name: "web"}}
	pods := []api.Pod{
		{Name: "web-1", Labels: map[string]string{api.DeploymentLabel: "web"}},
		{Name: "gone-1", Labels: map[string]string{api.DeploymentLabel: "gone"}},
		{Name: "bare"},
	}
	owned, orphans := groupByOwner(deps, pods)
	if len(owned["web"]) != 1 || !reflect.DeepEqual(orphans, []string{"gone-1"}) {
		t.Errorf("owned = %v, orphans = %v", owned, orphans)
	}
}

func TestRandomSuffix(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s, err := randomSuffix()
		if err != nil || len(s) != 5 {
			t.Fatalf("randomSuffix() = %q, %v", s, err)
		}
		seen[s] = true
	}
	if len(seen) < 95 {
		t.Errorf("only %d distinct suffixes in 100", len(seen))
	}
}
