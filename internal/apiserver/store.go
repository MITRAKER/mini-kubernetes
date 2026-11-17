package apiserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
)

// Errors returned by the store. The HTTP layer maps them to status codes.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	// ErrConflict means the caller's ResourceVersion is stale: someone else
	// wrote the object after the caller read it.
	ErrConflict = errors.New("conflict: object was modified, read it again and retry")
)

// Store holds the whole cluster state. It plays the role etcd plays in
// Kubernetes: the single source of truth that every other component reads
// and writes through the API server.
//
// It keeps everything in memory behind one mutex and, when a path is given,
// writes a JSON snapshot to disk after every change, so the cluster
// survives a restart of the server.
type Store struct {
	mu   sync.Mutex
	path string
	data state
}

type state struct {
	Version     int64                     `json:"version"`
	Nodes       map[string]api.Node       `json:"nodes"`
	Pods        map[string]api.Pod        `json:"pods"`
	Deployments map[string]api.Deployment `json:"deployments"`
}

// NewStore loads the snapshot at path if it exists. An empty path keeps
// state in memory only.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path, data: state{
		Nodes: map[string]api.Node{}, Pods: map[string]api.Pod{}, Deployments: map[string]api.Deployment{},
	}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return s, nil
}

// save must be called with s.mu held. It writes to a temporary file and
// renames it over the old one: rename is atomic, so a crash mid-write
// can never leave a half-written snapshot.
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) nextVersion() int64 {
	s.data.Version++
	return s.data.Version
}

// ---- nodes ----

// PutNode creates or replaces a node.
func (s *Store) PutNode(n api.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Nodes[n.Name] = n
	return s.save()
}

// ListNodes returns all nodes sorted by name.
func (s *Store) ListNodes() []api.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]api.Node, 0, len(s.data.Nodes))
	for _, n := range s.data.Nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---- pods ----

// CreatePod stores a new pod in phase Pending.
func (s *Store) CreatePod(p api.Pod) (api.Pod, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Pods[p.Name]; ok {
		return p, ErrExists
	}
	p.ResourceVersion = s.nextVersion()
	p.Status = api.PodStatus{Phase: api.PodPending}
	s.data.Pods[p.Name] = p
	return p, s.save()
}

// GetPod returns one pod.
func (s *Store) GetPod(name string) (api.Pod, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Pods[name]
	if !ok {
		return p, ErrNotFound
	}
	return p, nil
}

// ListPods returns pods sorted by name, filtered by node when node is not
// empty.
func (s *Store) ListPods(node string) []api.Pod {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []api.Pod{}
	for _, p := range s.data.Pods {
		if node == "" || p.NodeName == node {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// UpdatePodStatus replaces a pod's status. Only the node agent calls this,
// and it is the only writer of status, so no version check is needed.
func (s *Store) UpdatePodStatus(name string, st api.PodStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Pods[name]
	if !ok {
		return ErrNotFound
	}
	p.Status = st
	p.ResourceVersion = s.nextVersion()
	s.data.Pods[name] = p
	return s.save()
}

// BindPod assigns a pod to a node. Check and write happen under one lock,
// so if two schedulers race to bind the same pod, exactly one wins and the
// other gets ErrConflict.
func (s *Store) BindPod(name, node string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data.Pods[name]
	if !ok {
		return ErrNotFound
	}
	if _, ok := s.data.Nodes[node]; !ok {
		return fmt.Errorf("node %q: %w", node, ErrNotFound)
	}
	if p.NodeName != "" {
		return fmt.Errorf("pod already bound to %s: %w", p.NodeName, ErrConflict)
	}
	p.NodeName = node
	p.ResourceVersion = s.nextVersion()
	s.data.Pods[name] = p
	return s.save()
}

// DeletePod removes a pod.
func (s *Store) DeletePod(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Pods[name]; !ok {
		return ErrNotFound
	}
	delete(s.data.Pods, name)
	return s.save()
}

// ---- deployments ----

// ApplyDeployment creates a deployment or updates its desired state.
// If d.ResourceVersion is set it must match the stored one.
func (s *Store) ApplyDeployment(d api.Deployment) (api.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.data.Deployments[d.Name]
	if exists {
		if d.ResourceVersion != 0 && d.ResourceVersion != old.ResourceVersion {
			return old, ErrConflict
		}
		d.Status = old.Status // status is owned by the controller
	}
	d.ResourceVersion = s.nextVersion()
	s.data.Deployments[d.Name] = d
	return d, s.save()
}

// UpdateDeploymentStatus replaces a deployment's status.
func (s *Store) UpdateDeploymentStatus(name string, st api.DeploymentStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.Deployments[name]
	if !ok {
		return ErrNotFound
	}
	if d.Status == st {
		return nil // nothing changed; skip the disk write
	}
	d.Status = st
	d.ResourceVersion = s.nextVersion()
	s.data.Deployments[name] = d
	return s.save()
}

// GetDeployment returns one deployment.
func (s *Store) GetDeployment(name string) (api.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data.Deployments[name]
	if !ok {
		return d, ErrNotFound
	}
	return d, nil
}

// ListDeployments returns all deployments sorted by name.
func (s *Store) ListDeployments() []api.Deployment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []api.Deployment{}
	for _, d := range s.data.Deployments {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DeleteDeployment removes a deployment. Its pods are cleaned up by the
// controller, which notices they no longer have an owner.
func (s *Store) DeleteDeployment(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Deployments[name]; !ok {
		return ErrNotFound
	}
	delete(s.data.Deployments, name)
	return s.save()
}
