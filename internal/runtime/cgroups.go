package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroupRoot is where the kernel exposes the cgroup filesystem.
var cgroupRoot = "/sys/fs/cgroup"

// cpuPeriodUs is the CFS scheduling period. In every period the container
// may use at most quota microseconds of CPU time. 100ms is the kernel default.
const cpuPeriodUs = 100000

// Cgroup is the set of cgroup directories created for one container. On a
// cgroup v2 host this is a single directory; on a v1 host it is one directory
// per controller (memory, cpu, pids).
type Cgroup struct {
	dirs []string
}

// isCgroupV2 reports whether the host uses the unified (v2) hierarchy. On v2
// the root directory itself contains cgroup.controllers.
func isCgroupV2() bool {
	_, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers"))
	return err == nil
}

// cpuQuota converts millicores to a CFS quota in microseconds per period.
func cpuQuota(milliCPU int64) int64 {
	return milliCPU * cpuPeriodUs / 1000
}

// NewCgroup creates the cgroup for a container and writes its limits. It
// does not move any process into it yet; call AddProcess for that.
func NewCgroup(id string, c *Config) (*Cgroup, error) {
	name := "mkube-" + id
	if isCgroupV2() {
		return newCgroupV2(name, c)
	}
	return newCgroupV1(name, c)
}

func newCgroupV2(name string, c *Config) (*Cgroup, error) {
	// Child cgroups can only use controllers their parent enables in
	// cgroup.subtree_control. Ask for the ones we need; ignore errors
	// because they may already be on, or the host may not offer them.
	_ = os.WriteFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0o644)

	dir := filepath.Join(cgroupRoot, name)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create cgroup: %w", err)
	}
	cg := &Cgroup{dirs: []string{dir}}
	if c.MemoryBytes > 0 {
		if err := writeFile(dir, "memory.max", strconv.FormatInt(c.MemoryBytes, 10)); err != nil {
			return cg, err
		}
		// Disable swap so the memory limit is a hard limit.
		_ = writeFile(dir, "memory.swap.max", "0")
	}
	if c.MilliCPU > 0 {
		v := fmt.Sprintf("%d %d", cpuQuota(c.MilliCPU), cpuPeriodUs)
		if err := writeFile(dir, "cpu.max", v); err != nil {
			return cg, err
		}
	}
	if c.MaxPids > 0 {
		if err := writeFile(dir, "pids.max", strconv.FormatInt(c.MaxPids, 10)); err != nil {
			return cg, err
		}
	}
	return cg, nil
}

func newCgroupV1(name string, c *Config) (*Cgroup, error) {
	cg := &Cgroup{}
	mk := func(controller string) (string, error) {
		dir := filepath.Join(cgroupRoot, controller, name)
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create %s cgroup: %w", controller, err)
		}
		cg.dirs = append(cg.dirs, dir)
		return dir, nil
	}
	// Always create every controller's directory so that the process is
	// accounted for even when no limit is set.
	mem, err := mk("memory")
	if err != nil {
		return cg, err
	}
	cpu, err := mk("cpu")
	if err != nil {
		return cg, err
	}
	pids, err := mk("pids")
	if err != nil {
		return cg, err
	}
	if c.MemoryBytes > 0 {
		if err := writeFile(mem, "memory.limit_in_bytes", strconv.FormatInt(c.MemoryBytes, 10)); err != nil {
			return cg, err
		}
		// Swap is limited through memsw, which only exists when swap
		// accounting is enabled.
		_ = writeFile(mem, "memory.memsw.limit_in_bytes", strconv.FormatInt(c.MemoryBytes, 10))
	}
	if c.MilliCPU > 0 {
		if err := writeFile(cpu, "cpu.cfs_period_us", strconv.Itoa(cpuPeriodUs)); err != nil {
			return cg, err
		}
		if err := writeFile(cpu, "cpu.cfs_quota_us", strconv.FormatInt(cpuQuota(c.MilliCPU), 10)); err != nil {
			return cg, err
		}
	}
	if c.MaxPids > 0 {
		if err := writeFile(pids, "pids.max", strconv.FormatInt(c.MaxPids, 10)); err != nil {
			return cg, err
		}
	}
	return cg, nil
}

// AddProcess moves a process (and its future children) into the cgroup.
func (cg *Cgroup) AddProcess(pid int) error {
	for _, dir := range cg.dirs {
		if err := writeFile(dir, "cgroup.procs", strconv.Itoa(pid)); err != nil {
			return err
		}
	}
	return nil
}

// OOMKilled reports whether the kernel killed a process in this cgroup for
// exceeding its memory limit.
func (cg *Cgroup) OOMKilled() bool {
	for _, dir := range cg.dirs {
		for _, f := range []string{"memory.events", "memory.oom_control"} {
			b, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(b), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 2 && fields[0] == "oom_kill" && fields[1] != "0" {
					return true
				}
			}
		}
	}
	return false
}

// Remove deletes the cgroup directories. The kernel only allows this once
// every process in the cgroup has exited.
func (cg *Cgroup) Remove() error {
	var errs []error
	for _, dir := range cg.dirs {
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func writeFile(dir, file, value string) error {
	if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s=%s: %w", file, value, err)
	}
	return nil
}
