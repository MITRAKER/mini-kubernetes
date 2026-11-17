package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MITRAKER/mini-kubernetes/internal/agent"
	"github.com/MITRAKER/mini-kubernetes/internal/api"
	"github.com/MITRAKER/mini-kubernetes/internal/client"
	"github.com/MITRAKER/mini-kubernetes/internal/runtime"
)

func runAgent(args []string) int {
	host, err := os.Hostname()
	if err != nil {
		host = "node"
	}
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	name := fs.String("name", host, "node name")
	server := fs.String("server", "http://127.0.0.1:6443", "API server URL")
	listen := fs.String("listen", ":10250", "address for the agent's own HTTP server (logs)")
	advertise := fs.String("advertise", "", "URL other components use to reach this agent (default http://<name><listen>)")
	cpu := fs.String("cpu", "", "CPU capacity to offer, e.g. 2 or 1500m (default: all cores)")
	memory := fs.String("memory", "", "memory capacity to offer, e.g. 2Gi (default: all RAM)")
	images := fs.String("images", "/var/lib/mkube/images", "directory with one root filesystem per image")
	logs := fs.String("logs", "/var/lib/mkube/logs", "directory for pod logs")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	capacity, err := nodeCapacity(*cpu, *memory)
	if err != nil {
		log.Print(err)
		return 2
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

	srv := newHTTPServer(*listen, a.Handler())
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("agent http: %v", err)
			stop()
		}
	}()
	log.Printf("agent %s: offering %dm CPU and %d MiB memory, API server %s", *name, capacity.MilliCPU, capacity.Memory>>20, *server)
	a.Start(ctx) // returns after ctx is cancelled and all pods are stopped
	if err := shutdown(srv); err != nil {
		log.Printf("agent http shutdown: %v", err)
	}
	log.Printf("agent %s: stopped", *name)
	return 0
}

// nodeCapacity is what the node offers: the flags if given, else the whole
// machine.
func nodeCapacity(cpu, memory string) (api.Resources, error) {
	c := api.Resources{MilliCPU: int64(goruntime.NumCPU()) * 1000, Memory: hostMemory()}
	var err error
	if cpu != "" {
		if c.MilliCPU, err = runtime.ParseCPU(cpu); err != nil {
			return c, err
		}
	}
	if memory != "" {
		if c.Memory, err = runtime.ParseMemory(memory); err != nil {
			return c, err
		}
	}
	return c, nil
}

// newHTTPServer returns a server with timeouts, so a slow or malicious
// client cannot hold connections open forever (JPL rule 2/3: every wait is
// bounded).
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// shutdown stops a server, giving open requests 5 seconds to finish.
func shutdown(srv *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// hostMemory reads MemTotal from /proc/meminfo.
func hostMemory() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }() // read-only: close cannot lose data
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
