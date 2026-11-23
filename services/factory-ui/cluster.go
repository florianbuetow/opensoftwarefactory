package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// serviceAccountDir holds the token, CA, and namespace Kubernetes mounts into every pod.
const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// logParams are the query parameters passed through to the pod log endpoint.
var logParams = []string{"container", "tailLines", "sinceSeconds", "previous", "timestamps"}

var podNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// clusterProxy is a read-only view of the Kubernetes API for the factory's namespace:
//
//	/api/cluster/nodes             /api/v1/nodes
//	/api/cluster/deployments       /apis/apps/v1/namespaces/<ns>/deployments
//	/api/cluster/pods              /api/v1/namespaces/<ns>/pods
//	/api/cluster/services          /api/v1/namespaces/<ns>/services
//	/api/cluster/events            /api/v1/namespaces/<ns>/events
//	/api/cluster/pods/{name}/log   /api/v1/namespaces/<ns>/pods/{name}/log, for a pod the namespace lists
type clusterProxy struct {
	logger  *slog.Logger
	client  *http.Client // built on first use, guarded by mu
	apiBase string       // https://host:port, empty outside a cluster
	dir     string       // service account directory
	mu      sync.Mutex
}

// inClusterAPI returns the API server URL from the environment Kubernetes sets.
func inClusterAPI() string {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return ""
	}
	return "https://" + net.JoinHostPort(host, port)
}

var errNotInCluster = errors.New("not running in a cluster")

func (c *clusterProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	resource, query, ok := clusterRoute(strings.TrimPrefix(r.URL.Path, "/api/cluster"), r.URL.Query())
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown cluster resource"})
		return
	}
	token, err := c.read("token")
	if c.apiBase == "" || errors.Is(err, fs.ErrNotExist) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": errNotInCluster.Error()})
		return
	}
	ns, nsErr := c.read("namespace")
	client, clientErr := c.httpClient()
	if err = errors.Join(err, nsErr, clientErr); err != nil {
		c.logger.Error("reading service account", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot read the service account"})
		return
	}
	resource = strings.ReplaceAll(resource, "{ns}", url.PathEscape(ns))
	if pod, isLog := podLogRoute(strings.TrimPrefix(r.URL.Path, "/api/cluster")); isLog {
		// The log is read under a name Kubernetes lists for the namespace, never under the
		// name the request carries.
		names, listErr := c.podNames(r.Context(), client, token, ns)
		if listErr != nil {
			c.logger.Error("listing pods failed", "error", listErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "kubernetes API unavailable"})
			return
		}
		listed := slices.Index(names, pod)
		if listed < 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("pods %q not found", pod)})
			return
		}
		resource = strings.ReplaceAll(resource, "{pod}", url.PathEscape(names[listed]))
	}
	target := c.apiBase + resource
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, http.NoBody)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json, */*")
	resp, err := client.Do(req)
	if err != nil {
		c.logger.Error("kubernetes request failed", "path", resource, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "kubernetes API unavailable"})
		return
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			c.logger.Error("closing kubernetes response", "path", resource, "error", closeErr)
		}
	}()
	if resp.StatusCode >= 400 {
		// Kubernetes errors are Status objects; hand the UI their message as {"error": ...}.
		var status struct {
			Message string `json:"message"`
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Unmarshal(body, &status) != nil || status.Message == "" {
			status.Message = resp.Status
		}
		writeJSON(w, resp.StatusCode, map[string]string{"error": status.Message})
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// podNames returns the names of the pods Kubernetes lists in the namespace.
func (c *clusterProxy) podNames(ctx context.Context, client *http.Client, token, ns string) (_ []string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/api/v1/namespaces/"+url.PathEscape(ns)+"/pods", http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pod list returned %s", resp.Status)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	err = json.NewDecoder(resp.Body).Decode(&list)
	if err != nil {
		return nil, fmt.Errorf("decoding pod list: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Metadata.Name)
	}
	return names, nil
}

// podLogRoute returns the pod a /pods/{name}/log path below /api/cluster asks for.
func podLogRoute(path string) (pod string, ok bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "pods" || parts[3] != "log" || !podNamePattern.MatchString(parts[2]) {
		return "", false
	}
	return parts[2], true
}

// clusterRoute maps a path below /api/cluster to a Kubernetes API path, with {ns}
// standing for the namespace and {pod} for the pod whose log is read, and keeps only the
// query parameters that path allows.
func clusterRoute(path string, q url.Values) (string, url.Values, bool) {
	switch path {
	case "/nodes":
		return "/api/v1/nodes", nil, true
	case "/deployments":
		return "/apis/apps/v1/namespaces/{ns}/deployments", nil, true
	case "/pods":
		return "/api/v1/namespaces/{ns}/pods", nil, true
	case "/services":
		return "/api/v1/namespaces/{ns}/services", nil, true
	case "/events":
		return "/api/v1/namespaces/{ns}/events", nil, true
	}
	if _, isLog := podLogRoute(path); !isLog {
		return "", nil, false
	}
	kept := url.Values{}
	for _, k := range logParams {
		if v, ok := q[k]; ok {
			kept[k] = v[:1]
		}
	}
	return "/api/v1/namespaces/{ns}/pods/{pod}/log", kept, true
}

// read returns a trimmed service account file. The token rotates, so it is read on every request.
// The read cannot leave the service account directory.
func (c *clusterProxy) read(name string) (_ string, err error) {
	root, err := os.OpenRoot(c.dir)
	if err != nil {
		return "", err
	}
	defer func() { err = closeAfter(err, root) }()
	data, err := root.ReadFile(name)
	return strings.TrimSpace(string(data)), err
}

// httpClient trusts the cluster CA; it is built once the CA file can be read.
func (c *clusterProxy) httpClient() (*http.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	pem, err := os.ReadFile(filepath.Join(c.dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("ca.crt holds no certificate")
	}
	c.client = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 4,
		},
	}
	return c.client, nil
}
