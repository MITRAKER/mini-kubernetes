// Package apiserver is the front door of the cluster. Every component, the
// scheduler, the controllers, the node agents and the CLI, reads and writes
// cluster state only through this HTTP API. None of them talk to each other
// directly. That one rule is what keeps Kubernetes' many parts decoupled.
package apiserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
)

// Server serves the REST API on top of a Store.
type Server struct {
	Store *Store
}

// names follow Kubernetes' rule for most objects: a DNS label.
var validName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n")) // a failed write means the client left; no one to tell
	})

	mux.HandleFunc("PUT /api/nodes/{name}", s.putNode)
	mux.HandleFunc("GET /api/nodes", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Store.ListNodes()) })

	mux.HandleFunc("POST /api/pods", s.createPod)
	mux.HandleFunc("GET /api/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.Store.ListPods(r.URL.Query().Get("node")))
	})
	mux.HandleFunc("GET /api/pods/{name}", func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Store.GetPod(r.PathValue("name"))
		respond(w, p, err)
	})
	mux.HandleFunc("PUT /api/pods/{name}/status", s.updatePodStatus)
	mux.HandleFunc("PUT /api/pods/{name}/binding", s.bindPod)
	mux.HandleFunc("DELETE /api/pods/{name}", func(w http.ResponseWriter, r *http.Request) {
		respond(w, nil, s.Store.DeletePod(r.PathValue("name")))
	})

	mux.HandleFunc("PUT /api/deployments/{name}", s.applyDeployment)
	mux.HandleFunc("GET /api/deployments", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Store.ListDeployments()) })
	mux.HandleFunc("GET /api/deployments/{name}", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.Store.GetDeployment(r.PathValue("name"))
		respond(w, d, err)
	})
	mux.HandleFunc("PUT /api/deployments/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		var st api.DeploymentStatus
		if !readJSON(w, r, &st) {
			return
		}
		respond(w, nil, s.Store.UpdateDeploymentStatus(r.PathValue("name"), st))
	})
	mux.HandleFunc("DELETE /api/deployments/{name}", func(w http.ResponseWriter, r *http.Request) {
		respond(w, nil, s.Store.DeleteDeployment(r.PathValue("name")))
	})
	return mux
}

func (s *Server) putNode(w http.ResponseWriter, r *http.Request) {
	var n api.Node
	if !readJSON(w, r, &n) {
		return
	}
	if n.Name != r.PathValue("name") || !validName.MatchString(n.Name) {
		http.Error(w, "invalid node name", http.StatusBadRequest)
		return
	}
	respond(w, nil, s.Store.PutNode(n))
}

func (s *Server) createPod(w http.ResponseWriter, r *http.Request) {
	var p api.Pod
	if !readJSON(w, r, &p) {
		return
	}
	if err := validatePod(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	created, err := s.Store.CreatePod(p)
	if err == nil {
		w.WriteHeader(http.StatusCreated)
	}
	respond(w, created, err)
}

func (s *Server) updatePodStatus(w http.ResponseWriter, r *http.Request) {
	var st api.PodStatus
	if !readJSON(w, r, &st) {
		return
	}
	respond(w, nil, s.Store.UpdatePodStatus(r.PathValue("name"), st))
}

func (s *Server) bindPod(w http.ResponseWriter, r *http.Request) {
	var b struct {
		NodeName string `json:"nodeName"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	respond(w, nil, s.Store.BindPod(r.PathValue("name"), b.NodeName))
}

func (s *Server) applyDeployment(w http.ResponseWriter, r *http.Request) {
	var d api.Deployment
	if !readJSON(w, r, &d) {
		return
	}
	if d.Name != r.PathValue("name") || !validName.MatchString(d.Name) || len(d.Name) > 50 {
		http.Error(w, "invalid deployment name (lowercase letters, digits and '-', at most 50 characters)", http.StatusBadRequest)
		return
	}
	if d.Replicas < 0 {
		http.Error(w, "replicas must not be negative", http.StatusBadRequest)
		return
	}
	if err := validateSpec(&d.Template); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	saved, err := s.Store.ApplyDeployment(d)
	respond(w, saved, err)
}

func validatePod(p *api.Pod) error {
	if !validName.MatchString(p.Name) {
		return fmt.Errorf("invalid pod name %q: use lowercase letters, digits and '-'", p.Name)
	}
	if p.NodeName != "" {
		return fmt.Errorf("nodeName is set by the scheduler, leave it empty")
	}
	return validateSpec(&p.Spec)
}

func validateSpec(s *api.PodSpec) error {
	if s.Image == "" || !validName.MatchString(s.Image) {
		return fmt.Errorf("invalid image %q", s.Image)
	}
	if len(s.Command) == 0 {
		return fmt.Errorf("command is required")
	}
	if s.Limits.MilliCPU < 0 || s.Limits.Memory < 0 {
		return fmt.Errorf("limits must not be negative")
	}
	switch s.RestartPolicy {
	case "", api.RestartAlways, api.RestartOnFailure, api.RestartNever:
	default:
		return fmt.Errorf("invalid restartPolicy %q", s.RestartPolicy)
	}
	return nil
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers are already sent, so the status can't change; record it.
		log.Printf("apiserver: write response: %v", err)
	}
}

// respond maps store errors to HTTP status codes, like the real API server:
// 404 Not Found, 409 Conflict for both "already exists" and stale versions.
func respond(w http.ResponseWriter, v any, err error) {
	switch {
	case err == nil:
		if v != nil {
			writeJSON(w, v)
		}
	case errors.Is(err, ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrExists), errors.Is(err, ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
