package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
	"github.com/MITRAKER/mini-kubernetes/internal/runtime"
)

// The runtime starts containers by re-executing /proc/self/exe with the
// "__init" argument. Under `go test`, /proc/self/exe is this test binary,
// so TestMain must also know how to become a container's init process.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == runtime.InitArg {
		if err := runtime.Init(); err != nil {
			fmt.Fprintln(os.Stderr, "init:", err)
			os.Exit(127)
		}
	}
	os.Exit(m.Run())
}

// TestRealContainers runs pods through the real runtime. Needs root:
//
//	sudo MKUBE_ROOTFS=/tmp/mk-rootfs go test ./internal/agent/
func TestRealContainers(t *testing.T) {
	rootfs := os.Getenv("MKUBE_ROOTFS")
	if os.Geteuid() != 0 || rootfs == "" {
		t.Skip("needs root and MKUBE_ROOTFS")
	}
	a, fapi, _ := setup(t, nil)
	a.Run = runtime.Run
	os.Remove(filepath.Join(a.ImageDir, "alpine"))
	if err := os.Symlink(rootfs, filepath.Join(a.ImageDir, "alpine")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Start(ctx)

	hello := pod("hello", api.RestartNever)
	hello.Spec.Command = []string{"sh", "-c", "echo hi from $(hostname)"}
	hog := pod("hog", api.RestartNever)
	hog.Spec.Command = []string{"sh", "-c", `x=$(head -c 200000000 /dev/zero | tr "\0" a)`}
	hog.Spec.Limits.Memory = 32 << 20
	server := pod("server", api.RestartAlways)
	server.Spec.Command = []string{"sleep", "1000"}
	for _, p := range []api.Pod{hello, hog, server} {
		fapi.put(p)
	}

	eventually(t, "hello succeeded", func() bool { return fapi.pod("hello").Status.Phase == api.PodSucceeded })
	logs, _ := os.ReadFile(a.logPath("hello"))
	if strings.TrimSpace(string(logs)) != "hi from hello" {
		t.Errorf("logs = %q", logs)
	}
	eventually(t, "hog OOMKilled", func() bool { return fapi.pod("hog").Status.Reason == "OOMKilled" })
	eventually(t, "server running", func() bool { return fapi.pod("server").Status.Phase == api.PodRunning })

	fapi.del("server")
	eventually(t, "server stopped", func() bool { return len(a.Running()) == 2 })
	eventually(t, "sleep process gone", func() bool {
		out, _ := exec.Command("pgrep", "-f", "sleep 1000").Output()
		return len(out) == 0
	})
}
