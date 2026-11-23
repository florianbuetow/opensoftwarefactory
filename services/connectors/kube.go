package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// podInfo is what the connector service reads of a pod.
type podInfo struct {
	Name     string
	Phase    string
	IP       string
	Deleting bool
}

// cluster is the part of Kubernetes the connector service uses, all of it in the namespace
// of the dev pods. Deleting something that does not exist is not an error.
type cluster interface {
	CreatePod(ctx context.Context, pod map[string]any) error
	Pod(ctx context.Context, name string) (podInfo, error) // errNotFound when missing
	Pods(ctx context.Context, labelSelector string) ([]podInfo, error)
	DeletePod(ctx context.Context, name string) error
	ApplySecret(ctx context.Context, name string, labels, data map[string]string) error
	Secret(ctx context.Context, name string) (map[string]string, error) // errNotFound when missing
	DeleteSecret(ctx context.Context, name string) error
	ApplyNetworkPolicy(ctx context.Context, name string, policy map[string]any) error
	DeleteNetworkPolicy(ctx context.Context, name string) error
}

// kubeClient talks to the Kubernetes REST API with the service account of the pod it runs in.
type kubeClient struct {
	http      *http.Client
	base      string
	namespace string
	tokenFile string
}

// newKubeClient trusts the cluster's certificate authority when the service account provides
// one. Outside a cluster the client is still created; its calls then fail with a clear error.
func newKubeClient(base, namespace string) (*kubeClient, error) {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("the default HTTP transport is a %T, not a *http.Transport", http.DefaultTransport)
	}
	transport := defaultTransport.Clone()
	if ca, err := os.ReadFile(serviceAccountDir + "/ca.crt"); err == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("the service account's ca.crt holds no certificate")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &kubeClient{
		base:      strings.TrimRight(base, "/"),
		namespace: namespace,
		tokenFile: serviceAccountDir + "/token",
		http:      &http.Client{Timeout: 30 * time.Second, Transport: transport},
	}, nil
}

func (k *kubeClient) core(resource string) string {
	return "/api/v1/namespaces/" + url.PathEscape(k.namespace) + "/" + resource
}

func (k *kubeClient) policies() string {
	return "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(k.namespace) + "/networkpolicies"
}

// do sends one request. The token is read on every call because Kubernetes replaces it
// regularly. A nil out drops the answer.
func (k *kubeClient) do(ctx context.Context, method, path string, in, out any) (code int, err error) {
	var body io.Reader
	if in != nil {
		encoded, marshalErr := json.Marshal(in)
		if marshalErr != nil {
			return 0, marshalErr
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if k.tokenFile != "" {
		token, readErr := os.ReadFile(k.tokenFile)
		if readErr != nil {
			return 0, fmt.Errorf("the Kubernetes service account token cannot be read: %w", readErr)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("the Kubernetes API is not reachable: %w", err)
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode/100 != 2 {
		var status struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &status)
		if status.Message == "" {
			status.Message = resp.Status
		}
		return resp.StatusCode, fmt.Errorf("Kubernetes API %s %s: %s", method, path, status.Message)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

type kubePod struct {
	Metadata struct {
		Name              string `json:"name"`
		DeletionTimestamp string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase string `json:"phase"`
		PodIP string `json:"podIP"`
	} `json:"status"`
}

func (p kubePod) info() podInfo {
	return podInfo{Name: p.Metadata.Name, Phase: p.Status.Phase, IP: p.Status.PodIP, Deleting: p.Metadata.DeletionTimestamp != ""}
}

func (k *kubeClient) CreatePod(ctx context.Context, pod map[string]any) error {
	_, err := k.do(ctx, http.MethodPost, k.core("pods"), pod, nil)
	return err
}

func (k *kubeClient) Pod(ctx context.Context, name string) (podInfo, error) {
	var pod kubePod
	status, err := k.do(ctx, http.MethodGet, k.core("pods/"+url.PathEscape(name)), nil, &pod)
	if status == http.StatusNotFound {
		return podInfo{}, errNotFound
	}
	return pod.info(), err
}

func (k *kubeClient) Pods(ctx context.Context, labelSelector string) ([]podInfo, error) {
	var list struct {
		Items []kubePod `json:"items"`
	}
	if _, err := k.do(ctx, http.MethodGet, k.core("pods")+"?labelSelector="+url.QueryEscape(labelSelector), nil, &list); err != nil {
		return nil, err
	}
	pods := make([]podInfo, 0, len(list.Items))
	for _, item := range list.Items {
		pods = append(pods, item.info())
	}
	return pods, nil
}

func (k *kubeClient) delete(ctx context.Context, path string) error {
	status, err := k.do(ctx, http.MethodDelete, path, nil, nil)
	if status == http.StatusNotFound {
		return nil
	}
	return err
}

func (k *kubeClient) DeletePod(ctx context.Context, name string) error {
	return k.delete(ctx, k.core("pods/"+url.PathEscape(name))+"?gracePeriodSeconds=5")
}

// apply creates an object and replaces it when it exists already.
func (k *kubeClient) apply(ctx context.Context, collection, name string, object map[string]any) error {
	status, err := k.do(ctx, http.MethodPost, collection, object, nil)
	if status == http.StatusConflict {
		_, err = k.do(ctx, http.MethodPut, collection+"/"+url.PathEscape(name), object, nil)
	}
	return err
}

func (k *kubeClient) ApplySecret(ctx context.Context, name string, labels, data map[string]string) error {
	encoded := map[string]string{}
	for key, value := range data {
		encoded[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	return k.apply(ctx, k.core("secrets"), name, map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": name, "namespace": k.namespace, "labels": labels},
		"data":     encoded,
	})
}

func (k *kubeClient) Secret(ctx context.Context, name string) (map[string]string, error) {
	var secret struct {
		Data map[string]string `json:"data"`
	}
	status, err := k.do(ctx, http.MethodGet, k.core("secrets/"+url.PathEscape(name)), nil, &secret)
	if status == http.StatusNotFound {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	data := map[string]string{}
	for key, value := range secret.Data {
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("secret %s: key %s is not base64", name, key)
		}
		data[key] = string(decoded)
	}
	return data, nil
}

func (k *kubeClient) DeleteSecret(ctx context.Context, name string) error {
	return k.delete(ctx, k.core("secrets/"+url.PathEscape(name)))
}

func (k *kubeClient) ApplyNetworkPolicy(ctx context.Context, name string, policy map[string]any) error {
	return k.apply(ctx, k.policies(), name, policy)
}

func (k *kubeClient) DeleteNetworkPolicy(ctx context.Context, name string) error {
	return k.delete(ctx, k.policies()+"/"+url.PathEscape(name))
}
