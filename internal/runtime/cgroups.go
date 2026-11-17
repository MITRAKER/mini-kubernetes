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
// does not move any process into it yet; call AddProcess for that. On error
// it removes whatever it created, so the caller has nothing to clean up.
func NewCgroup(id string, c *Config) (*Cgroup, error) {
	if c == nil || id == "" { // precondition (JPL rule 5)
		return nil, errors.New("NewCgroup: id and config are required")
	}
	name := "mkube-" + id
	limits := cgroupV1Limits(c)
	cg := &Cgroup{}
	var err error
	if isCgroupV2() {
		err = cg.setupV2(name, c)
	} else {
		err = cg.setupV1(name, limits)
	}
	if err != nil {
		if rmErr := cg.Remove(); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		return nil, err
	}
	return cg, nil
}

func (cg *Cgroup) mkdir(dir string) error {
	if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create cgroup %s: %w", dir, err)
	}
	cg.dirs = append(cg.dirs, dir)
	return nil
}

func (cg *Cgroup) setupV2(name string, c *Config) error {
	// Child cgroups can only use controllers their parent enables in
	// cgroup.subtree_control. Ask for the ones we need. The error is
	// ignored on purpose: they may already be on, or the host may not
	// offer one, and a missing limit file below reports that precisely.
	_ = writeFile(cgroupRoot, "cgroup.subtree_control", "+cpu +memory +pids")

	dir := filepath.Join(cgroupRoot, name)
	if err := cg.mkdir(dir); err != nil {
		return err
	}
	var files []limitFile
	if c.MemoryBytes > 0 {
		files = append(files, limitFile{"memory.max", strconv.FormatInt(c.MemoryBytes, 10), true},
			// Disable swap so the memory limit is a hard limit (optional:
			// absent when the kernel has no swap accounting).
			limitFile{"memory.swap.max", "0", false})
	}
	if c.MilliCPU > 0 {
		files = append(files, limitFile{"cpu.max", fmt.Sprintf("%d %d", cpuQuota(c.MilliCPU), cpuPeriodUs), true})
	}
	if c.MaxPids > 0 {
		files = append(files, limitFile{"pids.max", strconv.FormatInt(c.MaxPids, 10), true})
	}
	return writeLimits(dir, files)
}

// limitFile is one value to write into a cgroup file.
type limitFile struct {
	name, value string
	required    bool // optional files may not exist on every kernel
}

// cgroupV1Limits lists the files to write per v1 controller.
func cgroupV1Limits(c *Config) map[string][]limitFile {
	limits := map[string][]limitFile{"memory": nil, "cpu": nil, "pids": nil}
	if c.MemoryBytes > 0 {
		v := strconv.FormatInt(c.MemoryBytes, 10)
		limits["memory"] = []limitFile{{"memory.limit_in_bytes", v, true},
			// memsw (memory+swap) only exists when swap accounting is on.
			{"memory.memsw.limit_in_bytes", v, false}}
	}
	if c.MilliCPU > 0 {
		limits["cpu"] = []limitFile{{"cpu.cfs_period_us", strconv.Itoa(cpuPeriodUs), true},
			{"cpu.cfs_quota_us", strconv.FormatInt(cpuQuota(c.MilliCPU), 10), true}}
	}
	if c.MaxPids > 0 {
		limits["pids"] = []limitFile{{"pids.max", strconv.FormatInt(c.MaxPids, 10), true}}
	}
	return limits
}

func (cg *Cgroup) setupV1(name string, limits map[string][]limitFile) error {
	// Create every controller's directory, in a fixed order, even with no
	// limit, so the process is always accounted for.
	for _, controller := range []string{"memory", "cpu", "pids"} {
		dir := filepath.Join(cgroupRoot, controller, name)
		if err := cg.mkdir(dir); err != nil {
			return err
		}
		if err := writeLimits(dir, limits[controller]); err != nil {
			return err
		}
	}
	return nil
}

func writeLimits(dir string, files []limitFile) error {
	for _, f := range files {
		err := writeFile(dir, f.name, f.value)
		if err != nil && f.required {
			return err
		}
	}
	return nil
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
			b, err := os.ReadFile(filepath.Join(dir, f)) //nolint:gosec // G304: dir is a cgroup this package created
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
	// Cgroup files are owned by root and read by the kernel; 0600 is enough.
	if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0o600); err != nil {
		return fmt.Errorf("write %s=%s: %w", file, value, err)
	}
	return nil
}
