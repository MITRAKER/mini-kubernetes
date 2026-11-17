// Command mkube is the Mini Kubernetes container runtime CLI.
//
//	mkube run [flags] -- COMMAND [ARG...]
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/MITRAKER/mini-kubernetes/internal/runtime"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case runtime.InitArg:
		// We are the container's first process: set it up and exec the
		// user's command. Only returns on error.
		if err := runtime.Init(); err != nil {
			fmt.Fprintln(os.Stderr, "mkube init:", err)
			os.Exit(127)
		}
	case "run":
		os.Exit(run(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "mkube: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mkube - a tiny container runtime

Usage:
  mkube run [flags] -- COMMAND [ARG...]

Run "mkube run -h" for the flags.
`)
}

func run(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	id := fs.String("name", "", "container name (required)")
	rootfs := fs.String("rootfs", "", "directory with the container's filesystem (required)")
	hostname := fs.String("hostname", "", "hostname inside the container (default: the name)")
	memory := fs.String("memory", "", `memory limit, e.g. "64Mi" or "1Gi"`)
	cpu := fs.String("cpu", "", `CPU limit, e.g. "500m" (half a core) or "2"`)
	pids := fs.Int64("pids", 0, "maximum number of processes (0 = unlimited)")
	network := fs.Bool("net", false, "give the container its own empty network (loopback only)")
	var env multiFlag
	fs.Var(&env, "env", "environment variable KEY=VALUE (repeatable)")
	fs.Parse(args)

	mem, err := runtime.ParseMemory(*memory)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkube:", err)
		return 2
	}
	milli, err := runtime.ParseCPU(*cpu)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkube:", err)
		return 2
	}

	cfg := &runtime.Config{
		ID:          *id,
		RootFS:      *rootfs,
		Hostname:    *hostname,
		Command:     fs.Args(),
		Env:         env,
		MemoryBytes: mem,
		MilliCPU:    milli,
		MaxPids:     *pids,
		Network:     *network,
	}
	res, err := runtime.Run(cfg, runtime.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkube:", err)
		return 125
	}
	if res.OOMKilled {
		fmt.Fprintln(os.Stderr, "mkube: container was killed for exceeding its memory limit (OOMKilled)")
	}
	return res.ExitCode
}

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint(*m) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
