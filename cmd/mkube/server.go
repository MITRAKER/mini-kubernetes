package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MITRAKER/mini-kubernetes/internal/apiserver"
	"github.com/MITRAKER/mini-kubernetes/internal/client"
	"github.com/MITRAKER/mini-kubernetes/internal/controller"
	"github.com/MITRAKER/mini-kubernetes/internal/scheduler"
)

// runServer starts the whole control plane in one process: the API server,
// the scheduler and the controllers. In real Kubernetes these are separate
// programs (kube-apiserver, kube-scheduler, kube-controller-manager); k3s
// bundles them like this. They still talk to each other only through the
// HTTP API, so splitting them later would need no code changes.
func runServer(args []string) int {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	listen := fs.String("listen", ":6443", "address for the API server")
	data := fs.String("data", "/var/lib/mkube/state.json", `cluster state file ("" keeps state in memory only)`)
	nodeTimeout := fs.Duration("node-timeout", 10*time.Second, "how long a node may miss heartbeats before its pods are moved")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	store, err := apiserver.NewStore(*data)
	if err != nil {
		log.Print(err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *listen)
	if err != nil {
		log.Print(err)
		return 1
	}
	srv := newHTTPServer("", (&apiserver.Server{Store: store}).Handler())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("api server: %v", err)
			stop()
		}
	}()

	self := client.New("http://" + loopback(ln.Addr()))
	go (&scheduler.Scheduler{API: self}).Run(ctx)
	go (&controller.Manager{API: self, NodeTimeout: *nodeTimeout}).Run(ctx)

	log.Printf("control plane listening on %s (state: %s)", ln.Addr(), *data)
	<-ctx.Done()
	if err := shutdown(srv); err != nil {
		log.Printf("api server shutdown: %v", err)
		return 1
	}
	return 0
}

// loopback turns a listen address like [::]:6443 into 127.0.0.1:6443 so the
// in-process scheduler and controllers can reach the API server.
func loopback(a net.Addr) string {
	_, port, _ := net.SplitHostPort(a.String())
	return net.JoinHostPort("127.0.0.1", port)
}
