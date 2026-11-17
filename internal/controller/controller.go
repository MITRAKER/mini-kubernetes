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
	dead := map[string]bool{}
	for _, n := range nodes {
		if m.Now().Sub(n.LastHeartbeat) <= m.NodeTimeout {
			continue
		}
		dead[n.Name] = true
		if n.Ready {
			log.Printf("node controller: node %s missed heartbeats for %s, marking NotReady", n.Name, m.NodeTimeout)
			n.Ready = false
			if err := m.API.PutNode(ctx, &n); err != nil {
				return err
			}
		}
	}
	if len(dead) == 0 {
		return nil
	}
	// Evict: delete every still-running pod on a dead node. Pods owned by a
	// Deployment are recreated by the deployment controller and scheduled
	// onto a healthy node. A bare pod with no owner is simply gone, which
	// is why you should almost never create bare pods in Kubernetes.
	pods, err := m.API.ListPods(ctx, "")
	if err != nil {
		return err
	}
	for _, p := range pods {
		if dead[p.NodeName] && !p.Terminated() {
			log.Printf("node controller: evicting pod %s from dead node %s", p.Name, p.NodeName)
			if err := m.API.DeletePod(ctx, p.Name); err != nil && err != client.ErrNotFound {
				return err
			}
		}
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
	owned := map[string][]api.Pod{}
	exists := map[string]bool{}
	for _, d := range deps {
		exists[d.Name] = true
	}
	for _, p := range pods {
		owner := p.Labels[api.DeploymentLabel]
		if owner == "" {
			continue
		}
		if !exists[owner] {
			// Garbage collection: the Deployment was deleted, so its pods go too.
			log.Printf("deployment controller: deleting orphan pod %s", p.Name)
			if err := m.API.DeletePod(ctx, p.Name); err != nil && err != client.ErrNotFound {
				return err
			}
			continue
		}
		owned[owner] = append(owned[owner], p)
	}
	for _, d := range deps {
		if err := m.reconcileDeployment(ctx, d, owned[d.Name]); err != nil {
			log.Printf("deployment controller: %s: %v", d.Name, err)
		}
	}
	return nil
}

// reconcileDeployment takes at most one kind of step per pass. Small steps
// are easier to reason about, and the next pass sees their result.
func (m *Manager) reconcileDeployment(ctx context.Context, d api.Deployment, pods []api.Pod) error {
	// 1. Pods that finished for good (for example OOMKilled with
	//    restartPolicy Never) are replaced, not kept.
	var alive, current, old []api.Pod
	for _, p := range pods {
		if p.Terminated() {
			log.Printf("deployment controller: %s: replacing finished pod %s (%s)", d.Name, p.Name, p.Status.Reason)
			if err := m.API.DeletePod(ctx, p.Name); err != nil && err != client.ErrNotFound {
				return err
			}
			continue
		}
		alive = append(alive, p)
		if reflect.DeepEqual(p.Spec, d.Template) {
			current = append(current, p)
		} else {
			old = append(old, p)
		}
	}

	ready := 0
	for _, p := range alive {
		if isReady(p) {
			ready++
		}
	}
	defer m.API.UpdateDeploymentStatus(ctx, d.Name, api.DeploymentStatus{Replicas: len(alive), ReadyReplicas: ready})

	// 2. Create missing pods from the current template. During a rollout we
	//    allow one extra pod ("maxSurge 1"), so a new pod can start before
	//    an old one is removed.
	maxTotal := d.Replicas
	if len(old) > 0 {
		maxTotal++
	}
	toCreate := min(d.Replicas-len(current), maxTotal-len(alive))
	for i := 0; i < toCreate; i++ {
		p := api.Pod{
			Name:   d.Name + "-" + randomSuffix(),
			Labels: map[string]string{api.DeploymentLabel: d.Name},
			Spec:   d.Template,
		}
		log.Printf("deployment controller: %s: creating pod %s", d.Name, p.Name)
		if err := m.API.CreatePod(ctx, &p); err != nil {
			return err
		}
	}
	if toCreate > 0 {
		return nil
	}

	// 3. Rolling update: remove one old pod, but only if we stay at full
	//    strength afterwards ("maxUnavailable 0"). If the new pods never
	//    become ready, for example because the new version crashes, the
	//    rollout simply stops here and the old pods keep serving.
	if len(old) > 0 {
		victim := byVictimOrder(old)[0]
		lost := 0
		if isReady(victim) {
			lost = 1
		}
		if ready-lost >= d.Replicas || len(alive) > d.Replicas+1 {
			log.Printf("deployment controller: %s: rolling update, deleting old pod %s", d.Name, victim.Name)
			return m.API.DeletePod(ctx, victim.Name)
		}
		return nil
	}

	// 4. Scale down: delete extras, the least useful first.
	if extra := len(current) - d.Replicas; extra > 0 {
		for _, victim := range byVictimOrder(current)[:extra] {
			log.Printf("deployment controller: %s: scaling down, deleting pod %s", d.Name, victim.Name)
			if err := m.API.DeletePod(ctx, victim.Name); err != nil && err != client.ErrNotFound {
				return err
			}
		}
	}
	return nil
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

func randomSuffix() string {
	const letters = "bcdfghjklmnpqrstvwxz2456789" // no vowels: avoids accidental words, like Kubernetes
	b := make([]byte, 5)
	rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}
