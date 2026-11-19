// Package runtime is a tiny container runtime built directly on Linux
// primitives: namespaces for isolation, cgroups for resource limits and
// pivot_root for a private filesystem. It is the bottom layer of Mini
// Kubernetes, the piece the node agent will call to start a pod's container.
package runtime

import (
	"fmt"
	"strconv"
	"strings"
)

// Config describes one container to run.
type Config struct {
	// ID names the container. It is used for the cgroup directory and as
	// the default hostname.
	ID string
	// RootFS is a directory holding the container's filesystem, for
	// example an unpacked Alpine minirootfs.
	RootFS string
	// Hostname seen inside the container. Defaults to ID.
	Hostname string
	// Command and its arguments, resolved inside the container's RootFS.
	Command []string
	// Env is the environment passed to Command.
	Env []string
	// MemoryBytes caps memory use. 0 means unlimited.
	MemoryBytes int64
	// MilliCPU caps CPU time, in thousandths of a core (500 = half a core).
	// 0 means unlimited. This is the same unit Kubernetes uses ("500m").
	MilliCPU int64
	// MaxPids caps how many processes the container can create. 0 means
	// unlimited. This stops fork bombs.
	MaxPids int64
	// Network gives the container its own, empty network namespace (only a
	// loopback interface). When false it shares the host network.
	Network bool
}

// Validate checks that the config can be run.
func (c *Config) Validate() error {
	if c.ID == "" {
		return fmt.Errorf("container id is required")
	}
	if strings.ContainsAny(c.ID, "/. ") {
		return fmt.Errorf("container id %q must not contain '/', '.' or spaces", c.ID)
	}
	if c.RootFS == "" {
		return fmt.Errorf("rootfs is required")
	}
	if len(c.Command) == 0 {
		return fmt.Errorf("a command is required")
	}
	if c.MemoryBytes < 0 || c.MilliCPU < 0 || c.MaxPids < 0 {
		return fmt.Errorf("limits must not be negative")
	}
	if c.MaxPids > 0 && c.MaxPids < MinPids {
		return fmt.Errorf("pids limit %d is below the minimum of %d", c.MaxPids, MinPids)
	}
	return nil
}

// MinPids is the smallest process limit a container may have. The pids
// controller counts threads, and the Go init process that sets up the
// container already runs several before it execs the real command. A
// smaller limit would make the Go runtime itself fail to start a thread
// ("pthread_create failed") instead of failing the command's fork.
const MinPids = 16

// ParseMemory turns a Kubernetes-style quantity such as "64Mi", "1Gi",
// "500k" or "1048576" into bytes.
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30},
		{"k", 1000}, {"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(s, u.suffix), 10, 64)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("invalid memory quantity %q", s)
			}
			return n * u.mult, nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid memory quantity %q", s)
	}
	return n, nil
}

// ParseCPU turns a Kubernetes-style CPU quantity such as "500m", "0.5" or
// "2" into millicores.
func ParseCPU(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "m") {
		n, err := strconv.ParseInt(strings.TrimSuffix(s, "m"), 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid cpu quantity %q", s)
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid cpu quantity %q", s)
	}
	return int64(f*1000 + 0.5), nil
}
