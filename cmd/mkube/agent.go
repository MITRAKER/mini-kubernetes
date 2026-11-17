package main

import (
	"bufio"
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/MITRAKER/mini-kubernetes/internal/agent"
	"github.com/MITRAKER/mini-kubernetes/internal/api"
	"github.com/MITRAKER/mini-kubernetes/internal/client"
	"github.com/MITRAKER/mini-kubernetes/internal/runtime"
)

func runAgent(args []string) int {
	host, _ := os.Hostname()
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	name := fs.String("name", host, "node name")
	server := fs.String("server", "http://127.0.0.1:6443", "API server URL")
	listen := fs.String("listen", ":10250", "address for the agent's own HTTP server (logs)")
	advertise := fs.String("advertise", "", "URL other components use to reach this agent (default http://<name><listen>)")
	cpu := fs.String("cpu", "", "CPU capacity to offer, e.g. 2 or 1500m (default: all cores)")
	memory := fs.String("memory", "", "memory capacity to offer, e.g. 2Gi (default: all RAM)")
	images := fs.String("images", "/var/lib/mkube/images", "directory with one root filesystem per image")
	logs := fs.String("logs", "/var/lib/mkube/logs", "directory for pod logs")
	fs.Parse(args)

	capacity := api.Resources{MilliCPU: int64(goruntime.NumCPU()) * 1000, Memory: hostMemory()}
	var err error
	if *cpu != "" {
		if capacity.MilliCPU, err = runtime.ParseCPU(*cpu); err != nil {
			log.Fatal(err)
		}
	}
	if *memory != "" {
		if capacity.Memory, err = runtime.ParseMemory(*memory); err != nil {
			log.Fatal(err)
		}
	}
	addr := *advertise
	if addr == "" {
		addr = "http://" + *name + *listen
	}

	a := &agent.Agent{
		Name: *name, Address: addr, Capacity: capacity,
		API: client.New(*server), Run: runtime.Run,
		ImageDir: *images, LogDir: *logs,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: *listen, Handler: a.Handler()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("agent http: %v", err)
		}
	}()
	log.Printf("agent %s: offering %dm CPU and %d MiB memory, API server %s", *name, capacity.MilliCPU, capacity.Memory>>20, *server)
	a.Start(ctx) // returns after ctx is cancelled and all pods are stopped
	srv.Shutdown(context.Background())
	log.Printf("agent %s: stopped", *name)
	return 0
}

// hostMemory reads MemTotal from /proc/meminfo.
func hostMemory() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(fields[1], 10, 64)
			return kb * 1024
		}
	}
	return 0
}
