// Package client talks to the Mini Kubernetes API server over HTTP+JSON.
//
// The API follows Kubernetes' REST shape:
//
//	PUT    /api/nodes/{name}              register or heartbeat a node
//	GET    /api/nodes                     list nodes
//	POST   /api/pods                      create a pod
//	GET    /api/pods?node={name}          list pods, optionally only one node's
//	GET    /api/pods/{name}               get one pod
//	PUT    /api/pods/{name}/status        update a pod's status
//	PUT    /api/pods/{name}/binding       assign a pod to a node
//	DELETE /api/pods/{name}               delete a pod
//	PUT    /api/deployments/{name}        create or update a deployment
//	GET    /api/deployments[/{name}]      list or get deployments
//	PUT    /api/deployments/{name}/status update a deployment's status
//	DELETE /api/deployments/{name}        delete a deployment
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/MITRAKER/mini-kubernetes/internal/api"
)

// Errors for the API server's answers that callers act on.
var (
	ErrNotFound = errors.New("not found") // 404
	// ErrConflict means the object already exists, or changed since you
	// read it. Read it again and retry.
	ErrConflict = errors.New("conflict") // 409
)

// Client is an API server client.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for the API server at base, e.g. "http://10.0.0.1:6443".
func New(base string) *Client {
	return &Client{base: base, http: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusConflict {
			return fmt.Errorf("%w: %s", ErrConflict, bytes.TrimSpace(msg))
		}
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// PutNode registers a node or refreshes its heartbeat.
func (c *Client) PutNode(ctx context.Context, n *api.Node) error {
	return c.do(ctx, http.MethodPut, "/api/nodes/"+url.PathEscape(n.Name), n, nil)
}

// ListNodes returns every node.
func (c *Client) ListNodes(ctx context.Context) ([]api.Node, error) {
	var out []api.Node
	return out, c.do(ctx, http.MethodGet, "/api/nodes", nil, &out)
}

// CreatePod creates a pod.
func (c *Client) CreatePod(ctx context.Context, p *api.Pod) error {
	return c.do(ctx, http.MethodPost, "/api/pods", p, nil)
}

// ListPods returns all pods, or only those bound to node when it is not empty.
func (c *Client) ListPods(ctx context.Context, node string) ([]api.Pod, error) {
	path := "/api/pods"
	if node != "" {
		path += "?node=" + url.QueryEscape(node)
	}
	var out []api.Pod
	return out, c.do(ctx, http.MethodGet, path, nil, &out)
}

// GetPod returns one pod.
func (c *Client) GetPod(ctx context.Context, name string) (*api.Pod, error) {
	var out api.Pod
	if err := c.do(ctx, http.MethodGet, "/api/pods/"+url.PathEscape(name), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdatePodStatus records what the node agent observed about a pod.
func (c *Client) UpdatePodStatus(ctx context.Context, name string, s api.PodStatus) error {
	return c.do(ctx, http.MethodPut, "/api/pods/"+url.PathEscape(name)+"/status", s, nil)
}

// BindPod assigns a pod to a node.
func (c *Client) BindPod(ctx context.Context, pod, node string) error {
	return c.do(ctx, http.MethodPut, "/api/pods/"+url.PathEscape(pod)+"/binding", map[string]string{"nodeName": node}, nil)
}

// DeletePod deletes a pod.
func (c *Client) DeletePod(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/pods/"+url.PathEscape(name), nil, nil)
}

// ApplyDeployment creates or updates a deployment.
func (c *Client) ApplyDeployment(ctx context.Context, d *api.Deployment) (*api.Deployment, error) {
	var out api.Deployment
	if err := c.do(ctx, http.MethodPut, "/api/deployments/"+url.PathEscape(d.Name), d, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDeployment returns one deployment.
func (c *Client) GetDeployment(ctx context.Context, name string) (*api.Deployment, error) {
	var out api.Deployment
	if err := c.do(ctx, http.MethodGet, "/api/deployments/"+url.PathEscape(name), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListDeployments returns every deployment.
func (c *Client) ListDeployments(ctx context.Context) ([]api.Deployment, error) {
	var out []api.Deployment
	return out, c.do(ctx, http.MethodGet, "/api/deployments", nil, &out)
}

// UpdateDeploymentStatus records what the controller observed.
func (c *Client) UpdateDeploymentStatus(ctx context.Context, name string, s api.DeploymentStatus) error {
	return c.do(ctx, http.MethodPut, "/api/deployments/"+url.PathEscape(name)+"/status", s, nil)
}

// DeleteDeployment deletes a deployment; its controller then removes its pods.
func (c *Client) DeleteDeployment(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/deployments/"+url.PathEscape(name), nil, nil)
}
