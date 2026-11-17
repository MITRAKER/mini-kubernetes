// Package api holds the objects the cluster talks about: Nodes and Pods.
// They are plain structs serialized as JSON, the same way Kubernetes objects
// are, and every component (API server, scheduler, agent, CLI) shares them.
package api

import "time"

// Node is a machine that runs pods. The node agent creates it on start-up
// and refreshes LastHeartbeat every few seconds; the control plane treats a
// node whose heartbeat is too old as dead.
type Node struct {
	Name string `json:"name"`
	// Address is where the node agent's own HTTP server listens (for logs).
	Address       string    `json:"address"`
	Capacity      Resources `json:"capacity"`
	LastHeartbeat time.Time `json:"lastHeartbeat"`
	Ready         bool      `json:"ready"`
}

// Resources is an amount of CPU and memory.
type Resources struct {
	// MilliCPU is thousandths of a core: 500 is half a core.
	MilliCPU int64 `json:"milliCPU"`
	// Memory is in bytes.
	Memory int64 `json:"memory"`
}

// RestartPolicy says what the node agent does when a pod's process exits.
type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "Always"    // restart whatever the exit code (long-running services)
	RestartOnFailure RestartPolicy = "OnFailure" // restart only on non-zero exit (batch jobs)
	RestartNever     RestartPolicy = "Never"
)

// PodSpec is the desired state of a pod, written by a user or a controller.
type PodSpec struct {
	// Image names a root filesystem in the node's image directory.
	Image         string        `json:"image"`
	Command       []string      `json:"command"`
	Env           []string      `json:"env,omitempty"`
	Limits        Resources     `json:"limits"`
	RestartPolicy RestartPolicy `json:"restartPolicy,omitempty"`
}

// PodPhase is the high-level state of a pod.
type PodPhase string

const (
	PodPending   PodPhase = "Pending"   // accepted, not running yet
	PodRunning   PodPhase = "Running"   // the process is running
	PodSucceeded PodPhase = "Succeeded" // exited 0 and will not restart
	PodFailed    PodPhase = "Failed"    // exited non-zero and will not restart
)

// PodStatus is the observed state of a pod, written by the node agent.
type PodStatus struct {
	Phase    PodPhase `json:"phase"`
	ExitCode int      `json:"exitCode"`
	// Reason is a short machine-friendly cause, e.g. "OOMKilled" or
	// "CrashLoopBackOff".
	Reason   string     `json:"reason,omitempty"`
	Message  string     `json:"message,omitempty"`
	Restarts int        `json:"restarts"`
	Started  *time.Time `json:"started,omitempty"`
}

// Pod is the smallest thing the cluster runs: one container.
type Pod struct {
	Name string `json:"name"`
	// ResourceVersion changes on every write. Sending back the version you
	// read lets the API server reject your update if someone else changed
	// the object in between (optimistic concurrency).
	ResourceVersion int64 `json:"resourceVersion,omitempty"`
	// Created is set by the API server when the pod is created.
	Created time.Time `json:"created"`
	// Labels are free-form tags. Controllers use them to find the pods
	// they own.
	Labels map[string]string `json:"labels,omitempty"`
	Spec   PodSpec           `json:"spec"`
	// NodeName is empty until the scheduler binds the pod to a node.
	NodeName string    `json:"nodeName,omitempty"`
	Status   PodStatus `json:"status"`
}

// Deployment keeps a number of identical pods running. Its controller
// creates pods labelled "deployment=<name>" from Template until Replicas of them
// are alive, and deletes extras.
type Deployment struct {
	Name            string           `json:"name"`
	ResourceVersion int64            `json:"resourceVersion,omitempty"`
	Created         time.Time        `json:"created"`
	Replicas        int              `json:"replicas"`
	Template        PodSpec          `json:"template"`
	Status          DeploymentStatus `json:"status"`
}

// DeploymentStatus is what the controller last observed.
type DeploymentStatus struct {
	Replicas      int `json:"replicas"`      // pods that exist
	ReadyReplicas int `json:"readyReplicas"` // pods that are Running
}

// DeploymentLabel is the label key that ties a pod to the Deployment that
// owns it.
const DeploymentLabel = "deployment"

// Terminated reports whether a pod's process has finished for good.
func (p *Pod) Terminated() bool {
	return p.Status.Phase == PodSucceeded || p.Status.Phase == PodFailed
}
