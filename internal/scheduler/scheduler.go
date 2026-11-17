// Package scheduler decides which node each new pod runs on.
//
// It works in two phases, the same as the Kubernetes scheduler:
//
//  1. Filter: drop nodes that cannot run the pod at all (not ready, or not
//     enough free CPU or memory).
//  2. Score: rank the nodes that are left and pick the best one.
//
// It then "binds" the pod by writing the node's name into pod.NodeName. That
// is all scheduling is: the node agent on that node notices the binding on
// its next sync and starts the container.
package scheduler

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
	"github.com/MITRAKER/mini-kubernetes/internal/client"
)

// Scheduler binds unscheduled pods to nodes.
type Scheduler struct {
	API      *client.Client
	Interval time.Duration
}

// Run schedules pods every Interval until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	if s.Interval == 0 {
		s.Interval = time.Second
	}
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		if err := s.Once(ctx); err != nil && ctx.Err() == nil {
			log.Printf("scheduler: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once makes one scheduling pass over all pending pods.
func (s *Scheduler) Once(ctx context.Context) error {
	nodes, err := s.API.ListNodes(ctx)
	if err != nil {
		return err
	}
	pods, err := s.API.ListPods(ctx, "")
	if err != nil {
		return err
	}

	// Work out what is already allocated on each node from the pods bound
	// to it. Finished pods no longer use anything.
	used := map[string]api.Resources{}
	var pending []api.Pod
	for _, p := range pods {
		switch {
		case p.NodeName == "":
			pending = append(pending, p)
		case !p.Terminated():
			u := used[p.NodeName]
			u.MilliCPU += p.Spec.Limits.MilliCPU
			u.Memory += p.Spec.Limits.Memory
			used[p.NodeName] = u
		}
	}

	for _, p := range pending {
		node, ok := Pick(p, nodes, used)
		if !ok {
			// Leave it Pending; capacity may free up or a node may join.
			continue
		}
		if err := s.API.BindPod(ctx, p.Name, node); err != nil {
			log.Printf("scheduler: bind %s to %s: %v", p.Name, node, err)
			continue
		}
		log.Printf("scheduler: bound pod %s to node %s", p.Name, node)
		// Count this pod right away so the next pod in this pass sees the
		// node's reduced free capacity.
		u := used[node]
		u.MilliCPU += p.Spec.Limits.MilliCPU
		u.Memory += p.Spec.Limits.Memory
		used[node] = u
	}
	return nil
}

// Pick chooses a node for pod, or returns false when none fits.
func Pick(pod api.Pod, nodes []api.Node, used map[string]api.Resources) (string, bool) {
	type candidate struct {
		name  string
		score float64
	}
	var fits []candidate
	for _, n := range nodes {
		// Filter.
		if !n.Ready {
			continue
		}
		u := used[n.Name]
		freeCPU := n.Capacity.MilliCPU - u.MilliCPU
		freeMem := n.Capacity.Memory - u.Memory
		if pod.Spec.Limits.MilliCPU > freeCPU || pod.Spec.Limits.Memory > freeMem {
			continue
		}
		// Score: "least allocated". Prefer the node that would have the
		// largest share of its CPU and memory still free after placing
		// this pod. This spreads load evenly across the cluster.
		fits = append(fits, candidate{n.Name, (frac(freeCPU-pod.Spec.Limits.MilliCPU, n.Capacity.MilliCPU) +
			frac(freeMem-pod.Spec.Limits.Memory, n.Capacity.Memory)) / 2})
	}
	if len(fits) == 0 {
		return "", false
	}
	// Highest score wins; break ties by name so the result is predictable.
	sort.Slice(fits, func(i, j int) bool {
		if fits[i].score != fits[j].score {
			return fits[i].score > fits[j].score
		}
		return fits[i].name < fits[j].name
	})
	return fits[0].name, true
}

func frac(free, capacity int64) float64 {
	if capacity <= 0 {
		return 0
	}
	return float64(free) / float64(capacity)
}
