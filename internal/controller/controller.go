// Package controller holds the control loops that keep the cluster healthy
// without anyone watching it:
//
//   - The node controller notices nodes that stopped sending heartbeats,
//     marks them NotReady and evicts their pods.
//   - The deployment controller keeps each Deployment at its replica count,
//     replaces pods that failed or were evicted, deletes extras, and rolls
//     out a new pod template one pod at a time.
//
// Like the node agent, each one is a level-triggered reconciliation loop:
// read the whole current state, compare it with the desired state, and
// take the smallest step that moves one toward the other.
package controller

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sort"
	"time"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
	"github.com/MITRAKER/mini-kubernetes/internal/client"
)

// Manager runs all controllers on one timer.
type Manager struct {
	API      *client.Client
	Interval time.Duration
	// NodeTimeout is how long a node may go without a heartbeat before it
	// is considered dead. Kubernetes uses 40 seconds by default.
	NodeTimeout time.Duration
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// Run reconciles every Interval until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	m.defaults()
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		if err := m.Once(ctx); err != nil && ctx.Err() == nil {
			log.Printf("controller: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) defaults() {
	if m.Interval == 0 {
		m.Interval = time.Second
	}
	if m.NodeTimeout == 0 {
		m.NodeTimeout = 10 * time.Second
	}
	if m.Now == nil {
		m.Now = time.Now
	}
}

// Once runs one pass of every controller.
func (m *Manager) Once(ctx context.Context) error {
	m.defaults()
	if err := m.reconcileNodes(ctx); err != nil {
		return err
	}
	return m.reconcileDeployments(ctx)
}

// ---- node controller ----

func (m *Manager) reconcileNodes(ctx context.Context) error {
	nodes, err := m.API.ListNodes(ctx)
	if err != nil {
		return err
	}
	dead, err := m.markDeadNodes(ctx, nodes)
	if err != nil || len(dead) == 0 {
		return err
	}
	return m.evictPods(ctx, dead)
}

// markDeadNodes marks nodes whose heartbeat is older than NodeTimeout as
// NotReady and returns the names of all such nodes.
func (m *Manager) markDeadNodes(ctx context.Context, nodes []api.Node) (map[string]bool, error) {
	dead := map[string]bool{}
	for _, n := range nodes {
		if m.Now().Sub(n.LastHeartbeat) <= m.NodeTimeout {
			continue
		}
		dead[n.Name] = true
		if !n.Ready {
			continue
		}
		log.Printf("node controller: node %s missed heartbeats for %s, marking NotReady", n.Name, m.NodeTimeout)
		n.Ready = false
		if err := m.API.PutNode(ctx, &n); err != nil {
			return nil, err
		}
	}
	return dead, nil
}

// evictPods deletes every still-running pod on a dead node. Pods owned by a
// Deployment are recreated by the deployment controller and scheduled onto
// a healthy node. A bare pod with no owner is simply gone, which is why you
// should almost never create bare pods in Kubernetes.
func (m *Manager) evictPods(ctx context.Context, dead map[string]bool) error {
	pods, err := m.API.ListPods(ctx, "")
	if err != nil {
		return err
	}
	for _, p := range pods {
		if dead[p.NodeName] && !p.Terminated() {
			if err := m.deletePod(ctx, p.Name, "node controller: evicting pod from dead node "+p.NodeName); err != nil {
				return err
			}
		}
	}
	return nil
}

// deletePod deletes a pod and logs why. A pod that is already gone is fine:
// someone else reached the same conclusion first.
func (m *Manager) deletePod(ctx context.Context, name, why string) error {
	log.Printf("%s: deleting pod %s", why, name)
	if err := m.API.DeletePod(ctx, name); err != nil && !errors.Is(err, client.ErrNotFound) {
		return err
	}
	return nil
}

// ---- deployment controller ----

func (m *Manager) reconcileDeployments(ctx context.Context) error {
	deps, err := m.API.ListDeployments(ctx)
	if err != nil {
		return err
	}
	pods, err := m.API.ListPods(ctx, "")
	if err != nil {
		return err
	}
	owned, orphans := groupByOwner(deps, pods)
	for _, name := range orphans {
		// Garbage collection: the Deployment was deleted, so its pods go too.
		if err := m.deletePod(ctx, name, "deployment controller: orphan"); err != nil {
			return err
		}
	}
	for _, d := range deps {
		if err := m.reconcileDeployment(ctx, d, owned[d.Name]); err != nil {
			log.Printf("deployment controller: %s: %v", d.Name, err)
		}
	}
	return nil
}

// groupByOwner sorts pods by the Deployment that owns them, and lists pods
// whose owner no longer exists.
func groupByOwner(deps []api.Deployment, pods []api.Pod) (map[string][]api.Pod, []string) {
	owned := map[string][]api.Pod{}
	for _, d := range deps {
		owned[d.Name] = nil
	}
	var orphans []string
	for _, p := range pods {
		owner := p.Labels[api.DeploymentLabel]
		if owner == "" {
			continue
		}
		if _, ok := owned[owner]; !ok {
			orphans = append(orphans, p.Name)
			continue
		}
		owned[owner] = append(owned[owner], p)
	}
	return owned, orphans
}

// plan is what the deployment controller decided to do in one pass.
type plan struct {
	status api.DeploymentStatus
	create int      // new pods to create from the template
	delete []string // pods to delete
	why    string   // log message explaining the deletions
}

// reconcileDeployment computes a plan and carries it out.
func (m *Manager) reconcileDeployment(ctx context.Context, d api.Deployment, pods []api.Pod) error {
	pl := planDeployment(d, pods)
	if err := m.API.UpdateDeploymentStatus(ctx, d.Name, pl.status); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	for _, name := range pl.delete {
		if err := m.deletePod(ctx, name, "deployment controller: "+d.Name+": "+pl.why); err != nil {
			return err
		}
	}
	for i := 0; i < pl.create; i++ { // bounded: create <= d.Replicas
		suffix, err := randomSuffix()
		if err != nil {
			return err
		}
		p := api.Pod{
			Name:   d.Name + "-" + suffix,
			Labels: map[string]string{api.DeploymentLabel: d.Name},
			Spec:   d.Template,
		}
		log.Printf("deployment controller: %s: creating pod %s", d.Name, p.Name)
		if err := m.API.CreatePod(ctx, &p); err != nil {
			return err
		}
	}
	return nil
}

// podSets splits a Deployment's pods by state.
type podSets struct {
	finished, alive, current, old []api.Pod
	ready                         int
}

func classify(d api.Deployment, pods []api.Pod) podSets {
	var s podSets
	for _, p := range pods {
		switch {
		case p.Terminated():
			s.finished = append(s.finished, p)
			continue
		case reflect.DeepEqual(p.Spec, d.Template):
			s.current = append(s.current, p)
		default:
			s.old = append(s.old, p)
		}
		s.alive = append(s.alive, p)
		if isReady(p) {
			s.ready++
		}
	}
	return s
}

// planDeployment decides the next step for a Deployment. It is a pure
// function of its inputs, which makes the controller's logic easy to test.
// It takes at most one kind of step per pass; the next pass sees the result.
func planDeployment(d api.Deployment, pods []api.Pod) plan {
	s := classify(d, pods)
	pl := plan{status: api.DeploymentStatus{Replicas: len(s.alive), ReadyReplicas: s.ready}}

	// 1. Pods that finished for good (for example OOMKilled with
	//    restartPolicy Never) are replaced, not kept.
	if len(s.finished) > 0 {
		pl.delete, pl.why = names(s.finished), "replacing finished pod"
		return pl
	}
	// 2. Create missing pods from the current template. During a rollout
	//    one extra pod is allowed ("maxSurge 1"), so a new pod can start
	//    before an old one is removed.
	maxTotal := d.Replicas
	if len(s.old) > 0 {
		maxTotal++
	}
	if pl.create = min(d.Replicas-len(s.current), maxTotal-len(s.alive)); pl.create > 0 {
		return pl
	}
	pl.create = 0
	// 3. Rolling update: remove one old pod, but only if we stay at full
	//    strength afterwards ("maxUnavailable 0"). If the new pods never
	//    become ready, for example because the new version crashes, the
	//    rollout stops here and the old pods keep serving.
	if len(s.old) > 0 {
		victim := byVictimOrder(s.old)[0]
		lost := 0
		if isReady(victim) {
			lost = 1
		}
		if s.ready-lost >= d.Replicas || len(s.alive) > d.Replicas+1 {
			pl.delete, pl.why = []string{victim.Name}, "rolling update"
		}
		return pl
	}
	// 4. Scale down: delete extras, the least useful first.
	if extra := len(s.current) - d.Replicas; extra > 0 {
		pl.delete, pl.why = names(byVictimOrder(s.current)[:extra]), "scaling down"
	}
	return pl
}

func names(pods []api.Pod) []string {
	out := make([]string, len(pods))
	for i, p := range pods {
		out[i] = p.Name
	}
	return out
}

func isReady(p api.Pod) bool {
	return p.Status.Phase == api.PodRunning && p.Status.Reason != "CrashLoopBackOff"
}

// byVictimOrder sorts pods so the one whose deletion hurts least comes
// first: unscheduled pods, then pods that are not ready, then the rest.
func byVictimOrder(pods []api.Pod) []api.Pod {
	rank := func(p api.Pod) int {
		switch {
		case p.NodeName == "":
			return 0
		case !isReady(p):
			return 1
		default:
			return 2
		}
	}
	sorted := append([]api.Pod(nil), pods...)
	sort.Slice(sorted, func(i, j int) bool {
		if rank(sorted[i]) != rank(sorted[j]) {
			return rank(sorted[i]) < rank(sorted[j])
		}
		return sorted[i].Name < sorted[j].Name
	})
	return sorted
}

// randomSuffix returns 5 random characters for a pod name. It leaves out
// vowels, as Kubernetes does, so a name never spells an accidental word.
func randomSuffix() (string, error) {
	const letters = "bcdfghjklmnpqrstvwxz2456789"
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random pod name: %w", err)
	}
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b), nil
}
