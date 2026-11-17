package apiserver

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
)

func TestCreateAndBind(t *testing.T) {
	s, _ := NewStore("")
	s.PutNode(api.Node{Name: "n1"})
	p, err := s.CreatePod(api.Pod{Name: "web"})
	if err != nil || p.Status.Phase != api.PodPending || p.ResourceVersion == 0 {
		t.Fatalf("create: %+v %v", p, err)
	}
	if _, err := s.CreatePod(api.Pod{Name: "web"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate create: %v, want ErrExists", err)
	}
	if err := s.BindPod("web", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bind to missing node: %v", err)
	}
	if err := s.BindPod("web", "n1"); err != nil {
		t.Fatal(err)
	}
	// A second scheduler losing the race must not overwrite the binding.
	if err := s.BindPod("web", "n1"); !errors.Is(err, ErrConflict) {
		t.Errorf("double bind: %v, want ErrConflict", err)
	}
}

func TestOptimisticConcurrency(t *testing.T) {
	s, _ := NewStore("")
	d, _ := s.ApplyDeployment(api.Deployment{Name: "web", Replicas: 1})
	// Two clients read version d.ResourceVersion. The first write wins...
	a := d
	a.Replicas = 3
	if _, err := s.ApplyDeployment(a); err != nil {
		t.Fatal(err)
	}
	// ...and the second, still holding the old version, is rejected
	// instead of silently undoing the first.
	b := d
	b.Replicas = 5
	if _, err := s.ApplyDeployment(b); !errors.Is(err, ErrConflict) {
		t.Errorf("stale write: %v, want ErrConflict", err)
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := NewStore(path)
	s.CreatePod(api.Pod{Name: "web"})
	s.ApplyDeployment(api.Deployment{Name: "api", Replicas: 2})

	again, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.GetPod("web"); err != nil {
		t.Error("pod lost after restart")
	}
	if d, err := again.GetDeployment("api"); err != nil || d.Replicas != 2 {
		t.Error("deployment lost after restart")
	}
	// Versions keep increasing after a restart.
	p, _ := again.CreatePod(api.Pod{Name: "new"})
	if p.ResourceVersion <= 2 {
		t.Errorf("version went backwards: %d", p.ResourceVersion)
	}
}

func TestValidation(t *testing.T) {
	good := api.Pod{Name: "web-1", Spec: api.PodSpec{Image: "alpine", Command: []string{"sh"}}}
	if err := validatePod(&good); err != nil {
		t.Fatal(err)
	}
	bad := []api.Pod{
		{Name: "Web", Spec: good.Spec},
		{Name: "../x", Spec: good.Spec},
		{Name: "web", Spec: api.PodSpec{Image: "../etc", Command: []string{"sh"}}},
		{Name: "web", Spec: api.PodSpec{Image: "alpine"}},
		{Name: "web", NodeName: "n1", Spec: good.Spec},
		{Name: "web", Spec: api.PodSpec{Image: "alpine", Command: []string{"sh"}, RestartPolicy: "Sometimes"}},
	}
	for i, p := range bad {
		if err := validatePod(&p); err == nil {
			t.Errorf("case %d accepted: %+v", i, p)
		}
	}
}
