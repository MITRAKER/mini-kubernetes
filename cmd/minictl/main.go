// Command minictl is the Mini Kubernetes command-line client, a small
// kubectl. Like kubectl, it is just another API client: everything it does
// is an HTTP request to the API server (or, for logs, to a node agent).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
	"github.com/MITRAKER/mini-kubernetes/internal/client"
	"github.com/MITRAKER/mini-kubernetes/internal/manifest"
)

const usage = `minictl - control a Mini Kubernetes cluster

Usage:
  minictl apply -f FILE                     create or update objects from YAML
  minictl get pods|nodes|deployments [-o json]
  minictl logs POD                          print a pod's output
  minictl scale deployment NAME REPLICAS    change the replica count
  minictl delete pod|deployment NAME

Global flags (before the command):
  -server URL   API server (default $MKUBE_SERVER or http://127.0.0.1:6443)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without os.Exit, so tests can call it.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("minictl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	def := os.Getenv("MKUBE_SERVER")
	if def == "" {
		def = "http://127.0.0.1:6443"
	}
	server := fs.String("server", def, "API server URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	c := &cli{api: client.New(*server), out: stdout, ctx: context.Background()}
	cmd, rest := fs.Arg(0), fs.Args()[1:]
	var err error
	switch cmd {
	case "apply":
		err = c.apply(rest)
	case "get":
		err = c.get(rest)
	case "logs":
		err = c.logs(rest)
	case "scale":
		err = c.scale(rest)
	case "delete":
		err = c.delete(rest)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		err = fmt.Errorf("unknown command %q (run minictl help)", cmd)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

type cli struct {
	api *client.Client
	out io.Writer
	ctx context.Context
}

// kind accepts the same short names kubectl does.
func kind(s string) (string, error) {
	switch s {
	case "pod", "pods", "po":
		return "pod", nil
	case "node", "nodes", "no":
		return "node", nil
	case "deployment", "deployments", "deploy":
		return "deployment", nil
	}
	return "", fmt.Errorf("unknown resource type %q (want pods, nodes or deployments)", s)
}

func (c *cli) apply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	file := fs.String("f", "", "YAML file, or - for stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("apply needs -f FILE")
	}
	var data []byte
	var err error
	if *file == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(*file)
	}
	if err != nil {
		return err
	}
	objs, err := manifest.Parse(data)
	if err != nil {
		return err
	}
	for _, o := range objs {
		switch {
		case o.Deployment != nil:
			// apply is declarative: "make it look like this file". It
			// overwrites whatever is there, so it sends no resourceVersion.
			_, getErr := c.api.GetDeployment(c.ctx, o.Deployment.Name)
			if _, err := c.api.ApplyDeployment(c.ctx, o.Deployment); err != nil {
				return err
			}
			verb := "configured"
			if errors.Is(getErr, client.ErrNotFound) {
				verb = "created"
			}
			fmt.Fprintf(c.out, "deployment/%s %s\n", o.Deployment.Name, verb)
		case o.Pod != nil:
			if _, err := c.api.GetPod(c.ctx, o.Pod.Name); err == nil {
				fmt.Fprintf(c.out, "pod/%s unchanged (pods can't be edited; delete it first, or use a Deployment)\n", o.Pod.Name)
				continue
			}
			if err := c.api.CreatePod(c.ctx, o.Pod); err != nil {
				return err
			}
			fmt.Fprintf(c.out, "pod/%s created\n", o.Pod.Name)
		}
	}
	return nil
}

func (c *cli) get(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("get needs a resource type: pods, nodes or deployments")
	}
	k, err := kind(args[0])
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	output := fs.String("o", "", "output format: json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	var v any
	switch k {
	case "pod":
		v, err = c.api.ListPods(c.ctx, "")
	case "node":
		v, err = c.api.ListNodes(c.ctx)
	case "deployment":
		v, err = c.api.ListDeployments(c.ctx)
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}

	tw := tabwriter.NewWriter(c.out, 0, 0, 3, ' ', 0)
	defer tw.Flush()
	now := time.Now()
	switch items := v.(type) {
	case []api.Pod:
		fmt.Fprintln(tw, "NAME\tSTATUS\tRESTARTS\tNODE\tAGE")
		for _, p := range items {
			node := p.NodeName
			if node == "" {
				node = "<none>"
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", p.Name, podStatus(p), p.Status.Restarts, node, age(now, p.Created))
		}
	case []api.Node:
		fmt.Fprintln(tw, "NAME\tSTATUS\tCPU\tMEMORY\tLAST HEARTBEAT")
		for _, n := range items {
			status := "Ready"
			if !n.Ready {
				status = "NotReady"
			}
			fmt.Fprintf(tw, "%s\t%s\t%dm\t%dMi\t%s ago\n", n.Name, status, n.Capacity.MilliCPU, n.Capacity.Memory>>20, age(now, n.LastHeartbeat))
		}
	case []api.Deployment:
		fmt.Fprintln(tw, "NAME\tREADY\tAGE")
		for _, d := range items {
			fmt.Fprintf(tw, "%s\t%d/%d\t%s\n", d.Name, d.Status.ReadyReplicas, d.Replicas, age(now, d.Created))
		}
	}
	return nil
}

// podStatus shows the most useful single word, like kubectl's STATUS column:
// the reason (OOMKilled, CrashLoopBackOff...) when there is one, else the phase.
func podStatus(p api.Pod) string {
	switch {
	case p.Status.Reason == "CrashLoopBackOff", p.Terminated() && p.Status.Reason != "":
		return p.Status.Reason
	case p.Status.Phase == "":
		return string(api.PodPending)
	}
	return string(p.Status.Phase)
}

func age(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d"
}

// logs fetches output straight from the node agent that runs the pod. The
// API server only tells us which node that is and where its agent listens.
func (c *cli) logs(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: minictl logs POD")
	}
	pod, err := c.api.GetPod(c.ctx, args[0])
	if err != nil {
		return notFound(err, "pod", args[0])
	}
	if pod.NodeName == "" {
		return fmt.Errorf("pod %s is not scheduled yet", pod.Name)
	}
	nodes, err := c.api.ListNodes(c.ctx)
	if err != nil {
		return err
	}
	var addr string
	for _, n := range nodes {
		if n.Name == pod.NodeName {
			addr = n.Address
		}
	}
	if addr == "" {
		return fmt.Errorf("node %s has no address", pod.NodeName)
	}
	resp, err := http.Get(strings.TrimRight(addr, "/") + "/logs/" + url.PathEscape(pod.Name))
	if err != nil {
		return fmt.Errorf("reach node %s: %w", pod.NodeName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("node %s: %s", pod.NodeName, strings.TrimSpace(string(msg)))
	}
	_, err = io.Copy(c.out, resp.Body)
	return err
}

// scale is a read-modify-write, so it uses optimistic concurrency: it sends
// back the resourceVersion it read, and if someone else changed the
// deployment in between, the server answers 409 and we simply try again.
func (c *cli) scale(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: minictl scale deployment NAME REPLICAS")
	}
	if k, err := kind(args[0]); err != nil || k != "deployment" {
		return fmt.Errorf("only deployments can be scaled")
	}
	n, err := strconv.Atoi(args[2])
	if err != nil || n < 0 {
		return fmt.Errorf("replicas must be a number >= 0")
	}
	for attempt := 0; attempt < 5; attempt++ {
		d, err := c.api.GetDeployment(c.ctx, args[1])
		if err != nil {
			return notFound(err, "deployment", args[1])
		}
		d.Replicas = n
		_, err = c.api.ApplyDeployment(c.ctx, d)
		if err == nil {
			fmt.Fprintf(c.out, "deployment/%s scaled to %d\n", d.Name, n)
			return nil
		}
		if !errors.Is(err, client.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("deployment %s kept changing; try again", args[1])
}

func (c *cli) delete(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: minictl delete pod|deployment NAME")
	}
	k, err := kind(args[0])
	if err != nil {
		return err
	}
	switch k {
	case "pod":
		err = c.api.DeletePod(c.ctx, args[1])
	case "deployment":
		err = c.api.DeleteDeployment(c.ctx, args[1])
	default:
		return fmt.Errorf("nodes can't be deleted; stop their agent instead")
	}
	if err != nil {
		return notFound(err, k, args[1])
	}
	fmt.Fprintf(c.out, "%s/%s deleted\n", k, args[1])
	return nil
}

func notFound(err error, kind, name string) error {
	if errors.Is(err, client.ErrNotFound) {
		return fmt.Errorf("%s %q not found", kind, name)
	}
	return err
}
