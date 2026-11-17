// Package manifest reads the YAML files that `minictl apply` takes and turns
// them into API objects.
//
// The format is a simplified version of Kubernetes manifests:
//
//	kind: Deployment
//	name: web
//	replicas: 3
//	template:
//	  image: alpine
//	  command: ["sh", "-c", "echo hello; sleep 3600"]
//	  env: ["GREETING=hi"]
//	  limits: {cpu: 250m, memory: 64Mi}
//	  restartPolicy: Always
//	---
//	kind: Pod
//	name: once
//	spec:
//	  image: alpine
//	  command: ["echo", "hi"]
//	  restartPolicy: Never
//
// Several objects can share one file, separated by "---".
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
	"github.com/MITRAKER/mini-kubernetes/internal/runtime"
)

// Object is one parsed manifest: exactly one of Pod or Deployment is set.
type Object struct {
	Pod        *api.Pod
	Deployment *api.Deployment
}

type limitsDoc struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

type specDoc struct {
	Image         string    `yaml:"image"`
	Command       []string  `yaml:"command"`
	Env           []string  `yaml:"env"`
	Limits        limitsDoc `yaml:"limits"`
	RestartPolicy string    `yaml:"restartPolicy"`
}

type doc struct {
	Kind     string            `yaml:"kind"`
	Name     string            `yaml:"name"`
	Labels   map[string]string `yaml:"labels"`
	Replicas *int              `yaml:"replicas"`
	Template *specDoc          `yaml:"template"`
	Spec     *specDoc          `yaml:"spec"`
}

// Parse reads every object in a YAML stream.
func Parse(data []byte) ([]Object, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // a typo like "replica:" is an error, not silently ignored
	var out []Object
	for i := 1; ; i++ {
		var d doc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if d.Kind == "" && d.Name == "" {
			continue // empty document, e.g. a trailing "---"
		}
		obj, err := convert(d)
		if err != nil {
			return nil, fmt.Errorf("document %d (%s %q): %w", i, d.Kind, d.Name, err)
		}
		out = append(out, obj)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no objects found")
	}
	return out, nil
}

func convert(d doc) (Object, error) {
	if d.Name == "" {
		return Object{}, fmt.Errorf("name is required")
	}
	switch d.Kind {
	case "Deployment":
		if d.Template == nil || d.Spec != nil {
			return Object{}, fmt.Errorf("a Deployment needs a template (and no spec)")
		}
		spec, err := convertSpec(*d.Template)
		if err != nil {
			return Object{}, err
		}
		replicas := 1
		if d.Replicas != nil {
			replicas = *d.Replicas
		}
		return Object{Deployment: &api.Deployment{Name: d.Name, Replicas: replicas, Template: spec}}, nil
	case "Pod":
		if d.Spec == nil || d.Template != nil || d.Replicas != nil {
			return Object{}, fmt.Errorf("a Pod needs a spec (and no template or replicas)")
		}
		spec, err := convertSpec(*d.Spec)
		if err != nil {
			return Object{}, err
		}
		return Object{Pod: &api.Pod{Name: d.Name, Labels: d.Labels, Spec: spec}}, nil
	default:
		return Object{}, fmt.Errorf("unknown kind %q (want Deployment or Pod)", d.Kind)
	}
}

func convertSpec(s specDoc) (api.PodSpec, error) {
	cpu, err := runtime.ParseCPU(s.Limits.CPU)
	if err != nil {
		return api.PodSpec{}, err
	}
	mem, err := runtime.ParseMemory(s.Limits.Memory)
	if err != nil {
		return api.PodSpec{}, err
	}
	return api.PodSpec{
		Image: s.Image, Command: s.Command, Env: s.Env,
		Limits:        api.Resources{MilliCPU: cpu, Memory: mem},
		RestartPolicy: api.RestartPolicy(s.RestartPolicy),
	}, nil
}
