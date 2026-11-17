package manifest

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	objs, err := Parse([]byte(`
kind: Deployment
name: web
replicas: 3
template:
  image: alpine
  command: ["sh", "-c", "sleep 1000"]
  limits: {cpu: 250m, memory: 64Mi}
---
kind: Pod
name: once
spec:
  image: alpine
  command: [echo, hi]
  restartPolicy: Never
---
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("got %d objects, want 2", len(objs))
	}
	d := objs[0].Deployment
	if d == nil || d.Name != "web" || d.Replicas != 3 || d.Template.Limits.MilliCPU != 250 || d.Template.Limits.Memory != 64<<20 {
		t.Errorf("deployment = %+v", d)
	}
	p := objs[1].Pod
	if p == nil || p.Name != "once" || p.Spec.RestartPolicy != "Never" || p.Spec.Command[1] != "hi" {
		t.Errorf("pod = %+v", p)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"kind: Service\nname: x\n":                                      "unknown kind",
		"kind: Deployment\nname: x\nreplica: 3\ntemplate: {image: a}\n": "replica",
		"kind: Pod\nname: x\ntemplate: {image: a}\n":                    "needs a spec",
		"kind: Deployment\nname: x\ntemplate: {limits: {cpu: lots}}\n":  "invalid cpu",
		"kind: Pod\nspec: {image: a}\n":                                 "name is required",
		"":                                                              "no objects",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) error = %v, want it to mention %q", in, err, want)
		}
	}
}
