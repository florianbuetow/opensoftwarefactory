// The connector service keeps the Git repositories agents may work on, runs the dev pods that
// hold a clone of one, checks every agent tool call against the rules that apply to it, and
// publishes the work as a pushed branch and a pull request.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// config holds the settings read from the environment.
type config struct {
	podNamespace     string        // namespace of the dev pods
	factoryNamespace string        // namespace of the factory services
	image            string        // image of both containers of a dev pod
	pullPolicy       string        // image pull policy of a dev pod
	sweep            time.Duration // how often idle pods are looked for
	idleOverride     time.Duration // when > 0, replaces every repository's idle_minutes
	holdTTL          time.Duration // when > 0, a hold ends this long after its last renewal
	startTimeout     time.Duration // how long a new pod may take to answer
	poll             time.Duration // pause between two looks at a starting pod
	mcpURL           string        // URL the MCP gateway uses for the service devpod
	registerRetry    time.Duration // pause between two registration attempts
}

type service struct {
	cfg        config
	blobs      blobClient
	kube       cluster
	registry   string
	mcpGateway string
	githubAPI  string
	http       *http.Client // calls to factory services and GitHub
	podHTTP    *http.Client // calls into dev pods; the time limit comes from the context
	log        *slog.Logger
	podURL     func(ip string, port int) string
	resolve    func(host string) ([]string, error)
	now        func() time.Time

	mu       sync.Mutex // guards everything below; held while a pod record is saved
	loaded   bool
	pods     map[string]*devPod
	conns    map[string]podConn
	commands map[string]cachedCommands
	createMu sync.Mutex // one creation decides at a time whether a named pod exists
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	shutdown, err := initTelemetry(context.Background())
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()
	cfg := configFromEnv()
	kube, err := newKubeClient(env("KUBERNETES_API_URL", "https://kubernetes.default.svc"), cfg.podNamespace)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)}
	s := newService(cfg, log, client, kube)
	s.blobs = blobClient{base: strings.TrimRight(env("BLOBSTORE_URL", "http://blobstore:8080"), "/"), http: client}
	s.registry = strings.TrimRight(env("AGENT_REGISTRY_URL", "http://agent-registry:8080"), "/")
	s.mcpGateway = strings.TrimRight(env("MCP_GATEWAY_URL", "http://mcp-gateway:8080"), "/")
	s.githubAPI = strings.TrimRight(env("GITHUB_API_URL", "https://api.github.com"), "/")
	seedRepositories, err := readRepositorySeeds(env("CONNECTOR_SEED_FILE", "/seed/repositories.json"))
	if err != nil {
		return fmt.Errorf("reading repository seeds: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.loadUntilDone(ctx)
	go s.registerUntilDone(ctx)
	go s.sweepLoop(ctx)
	if len(seedRepositories) > 0 {
		go s.seedUntilDone(ctx, seedRepositories)
	}

	server := &http.Server{Addr: ":8080", Handler: otelhttp.NewHandler(s.routes(), "connectors"), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { log.Info("connectors listening", "address", server.Addr); done <- server.ListenAndServe() }()
	select {
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(drain)
	}
	return nil
}

func newService(cfg config, log *slog.Logger, client *http.Client, kube cluster) *service {
	return &service{
		cfg: cfg, kube: kube, http: client, log: log,
		podHTTP:  &http.Client{},
		podURL:   func(ip string, port int) string { return "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) },
		resolve:  net.LookupHost,
		now:      time.Now,
		pods:     map[string]*devPod{},
		conns:    map[string]podConn{},
		commands: map[string]cachedCommands{},
	}
}

func configFromEnv() config {
	factory := os.Getenv("FACTORY_NAMESPACE")
	if factory == "" {
		if data, err := os.ReadFile(serviceAccountDir + "/namespace"); err == nil {
			factory = strings.TrimSpace(string(data))
		}
	}
	if factory == "" {
		factory = "opensoftwarefactory"
	}
	return config{
		podNamespace:     env("DEVPOD_NAMESPACE", "opensoftwarefactory-devpods"),
		factoryNamespace: factory,
		image:            env("DEVPOD_IMAGE", "opensoftwarefactory-devpod:local"),
		pullPolicy:       env("DEVPOD_IMAGE_PULL_POLICY", "Never"),
		sweep:            seconds("DEVPOD_SWEEP_SECONDS", 60),
		idleOverride:     seconds("DEVPOD_IDLE_SECONDS", 0),
		holdTTL:          seconds("DEVPOD_HOLD_SECONDS", 0),
		startTimeout:     seconds("DEVPOD_START_SECONDS", 180),
		poll:             time.Second,
		mcpURL:           env("DEVPOD_MCP_URL", "http://tool-gateway:8080/mcp"),
		registerRetry:    5 * time.Second,
	}
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// seconds reads a number of seconds, which may have a fraction, from the environment.
func seconds(k string, d float64) time.Duration {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil && v >= 0 {
		d = v
	}
	return time.Duration(d * float64(time.Second))
}

func (s *service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/git/repositories", s.listRepositories)
	mux.HandleFunc("GET /api/git/repositories/{name}", s.getRepository)
	mux.HandleFunc("PUT /api/git/repositories/{name}", s.putRepository)
	mux.HandleFunc("DELETE /api/git/repositories/{name}", s.deleteRepository)
	mux.HandleFunc("GET /api/devpods", s.listPods)
	mux.HandleFunc("POST /api/devpods", s.createPod)
	mux.HandleFunc("GET /api/devpods/{id}", s.getPod)
	mux.HandleFunc("DELETE /api/devpods/{id}", s.deletePod)
	mux.HandleFunc("POST /api/devpods/{id}/hold", s.holdPod)
	mux.HandleFunc("POST /api/devpods/{id}/release", s.releasePod)
	mux.HandleFunc("POST /api/devpods/{id}/exec", s.execPod)
	mux.HandleFunc("GET /api/devpods/{id}/files", s.viewFiles)
	mux.HandleFunc("GET /api/devpods/{id}/file", s.viewFile)
	mux.HandleFunc("GET /api/devpods/{id}/log", s.viewLog)
	mux.HandleFunc("GET /api/devpods/{id}/diff", s.viewDiff)
	mux.HandleFunc("POST /api/devpods/{id}/tool", s.tool)
	mux.HandleFunc("GET /api/devpods/{id}/publish-requests", s.listPublishRequests)
	mux.HandleFunc("POST /api/devpods/{id}/publish-requests/{n}/decision", s.decidePublishRequest)
	return s.whenLoaded(mux)
}

// whenLoaded refuses the API until the pod records were read from the blob store, so that a
// request never acts on a state that misses a pod.
func (s *service) whenLoaded(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		loaded := s.loaded
		s.mu.Unlock()
		if !loaded && r.URL.Path != "/healthz" {
			s.fail(w, 503, errors.New("the connector service is still reading its dev pods from the blob store; try again"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *service) loadUntilDone(ctx context.Context) {
	for {
		err := s.load(ctx)
		if err == nil {
			return
		}
		s.log.Warn("reading dev pods failed; trying again", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// httpError is an error that carries the HTTP status to answer with.
type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

func statusError(status int, format string, args ...any) error {
	return httpError{status, fmt.Sprintf(format, args...)}
}

func (s *service) fail(w http.ResponseWriter, status int, err error) {
	var he httpError
	if errors.As(err, &he) {
		status = he.status
	}
	s.log.Warn("connectors request failed", "status", status, "error", err)
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, out any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		return statusError(400, "the request body cannot be read: %v", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if err = json.Unmarshal(data, out); err != nil {
		return statusError(400, "the request body is not valid JSON: %v", err)
	}
	return nil
}
