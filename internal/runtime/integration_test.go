package runtime_test

// These tests start real containers, so they need root and a root
// filesystem. Run them with:
//
//	./scripts/make-rootfs.sh /tmp/mk-rootfs
//	sudo MKUBE_ROOTFS=/tmp/mk-rootfs go test ./...

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildMkube compiles the CLI, because the runtime re-executes its own
// binary to start a container and the test binary has no "__init" mode.
func buildMkube(t *testing.T) (bin, rootfs string) {
	t.Helper()
	rootfs = os.Getenv("MKUBE_ROOTFS")
	if os.Geteuid() != 0 || rootfs == "" {
		t.Skip("needs root and MKUBE_ROOTFS")
	}
	bin = filepath.Join(t.TempDir(), "mkube")
	out, err := exec.Command("go", "build", "-o", bin, "../../cmd/mkube").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin, rootfs
}

func mkube(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), code
}

func TestIsolation(t *testing.T) {
	bin, rootfs := buildMkube(t)
	out, code := mkube(t, bin, "run", "-name", "iso", "-rootfs", rootfs, "-net", "--",
		"sh", "-c", `echo "$(hostname) $$"; ls /proc | grep -c '^[0-9]'; cat /proc/net/dev`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	lines := strings.Split(out, "\n")
	if lines[0] != "iso 1" {
		t.Errorf("want hostname iso and PID 1, got %q", lines[0])
	}
	if strings.Contains(out, "eth0") {
		t.Errorf("network namespace should only have lo:\n%s", out)
	}
}

func TestExitCode(t *testing.T) {
	bin, rootfs := buildMkube(t)
	if _, code := mkube(t, bin, "run", "-name", "exit", "-rootfs", rootfs, "--", "sh", "-c", "exit 7"); code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}

func TestMemoryLimit(t *testing.T) {
	bin, rootfs := buildMkube(t)
	out, code := mkube(t, bin, "run", "-name", "oom", "-rootfs", rootfs, "-memory", "32Mi", "--",
		"sh", "-c", `x=$(head -c 200000000 /dev/zero | tr "\0" a)`)
	if code != 137 || !strings.Contains(out, "OOMKilled") {
		t.Errorf("want OOMKilled exit 137, got %d: %s", code, out)
	}
}

func TestPidsLimit(t *testing.T) {
	bin, rootfs := buildMkube(t)
	out, _ := mkube(t, bin, "run", "-name", "pids", "-rootfs", rootfs, "-pids", "16", "--",
		"sh", "-c", "i=0; while [ $i -lt 32 ]; do sleep 1 & i=$((i+1)); done; wait")
	if !strings.Contains(out, "fork") {
		t.Errorf("want fork failure, got: %s", out)
	}
}
