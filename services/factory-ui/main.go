// Command factory-ui serves the single web UI of the software factory and
// reverse-proxies the backend APIs it calls:
//
//	/api/orchestrator/*  ORCHESTRATOR_URL     /api/*
//	/api/agents/*        AGENT_REGISTRY_URL   /*  (the UI calls /api/agents/agents/...)
//	/api/policies/*      POLICY_REGISTRY_URL  /*  (the UI calls /api/policies/policies/...)
//	/api/skills/*        SKILL_REGISTRY_URL   /*  (the UI calls /api/skills/skills/...)
//	/api/blobs/*         BLOBSTORE_URL        /*  (reads; writes only tasks/<collection>/<file>,
//	                                              templates/tasks/, and interactive session files;
//	                                              deletes only task files and session files)
//	/api/agent/chat      ORCHESTRATOR_URL     /api/sessions/chat (POST only: one message of an interactive session)
//	/api/cluster/*       the Kubernetes API, read-only, see clusterProxy
//	/api/llm/v1/models   LLM_GATEWAY_URL      /v1/models (the model list only; the UI never calls a model)
//	/api/mcp-gateway/*   MCP_GATEWAY_URL      /*
//	/git/*               GIT_HOST_URL         /*  (clone, fetch, and push)
//
// Everything else is served from the embedded web directory.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFiles embed.FS

// backends names the proxy targets.
type backends struct {
	cluster                                                      *clusterProxy
	observability                                                *observabilityClient
	orchestrator, agents, policies, blobs, llm, mcp, evaluations string
	sqldb                                                        string
	skills                                                       string
	connectors                                                   string // the connector service: repositories and dev pods
	gitHost                                                      string // the demo Git server
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	handler, err := newHandler(&backends{
		orchestrator: env("ORCHESTRATOR_URL", "http://orchestrator:8080"),
		agents:       env("AGENT_REGISTRY_URL", "http://agent-registry:8080"),
		policies:     env("POLICY_REGISTRY_URL", "http://policy-registry:8080"),
		skills:       env("SKILL_REGISTRY_URL", "http://skill-registry:8080"),
		blobs:        env("BLOBSTORE_URL", "http://blobstore:8080"),
		llm:          env("LLM_GATEWAY_URL", "http://llm-gateway:8080"),
		mcp:          env("MCP_GATEWAY_URL", "http://mcp-gateway:8080"),
		evaluations:  env("EVALUATION_URL", "http://evaluation:8080"),
		sqldb:        env("SQLDB_URL", "http://sqldb:8080"),
		connectors:   env("CONNECTORS_URL", "http://connectors:8080"),
		gitHost:      env("GIT_HOST_URL", "http://git-host:8080"),
		cluster:      &clusterProxy{apiBase: inClusterAPI(), dir: serviceAccountDir, logger: logger},
		observability: loadObservability(validateObservabilityURL(env("OBSERVABILITY_URL", "http://openobserve:5080")),
			env("OBSERVABILITY_CONFIG", "/openobserve/config/openobserve.env"),
			validateObservabilityURL(env("LOG_COLLECTOR_URL", "http://log-collector:2020")),
			validateObservabilityURL(env("TRACING_HEALTH_URL", "http://telemetry-collector:13133")),
			validateObservabilityURL(env("TRACING_METRICS_URL", "http://telemetry-collector:8888/metrics"))),
	}, logger)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go seedDefaults(ctx, env("BLOBSTORE_URL", "http://blobstore:8080"), env("FACTORY_SEED_DIR", "/seed"), logger)
	result := make(chan error, 1)
	go func() {
		logger.Info("factory-ui listening", "address", server.Addr)
		result <- server.ListenAndServe()
	}()
	select {
	case err := <-result:
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

func newHandler(b *backends, logger *slog.Logger) (http.Handler, error) {
	web, err := fs.Sub(webFiles, "web")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	routes := []struct {
		prefix, target, replace string
		getOnly                 bool
	}{
		{"/api/orchestrator", b.orchestrator, "/api", false},
		{"/api/agents", b.agents, "", false},
		{"/api/policies", b.policies, "", false},
		{"/api/blobs", b.blobs, "", true},
	}
	for _, r := range routes {
		p, err := newProxy(r.prefix, r.target, r.replace, logger)
		if err != nil {
			return nil, err
		}
		if r.getOnly {
			p = getOnly(p)
		}
		mux.Handle(r.prefix+"/", p)
	}
	if b.evaluations != "" {
		p, err := newProxy("/api/evaluations", b.evaluations, "/api", logger)
		if err != nil {
			return nil, err
		}
		mux.Handle("/api/evaluations/", p)
	}
	if b.sqldb != "" {
		p, err := newProxy("/api/sqldb", b.sqldb, "/api", logger)
		if err != nil {
			return nil, err
		}
		mux.Handle("/api/sqldb/", getOnly(p))
	}
	if b.skills != "" {
		p, err := newProxy("/api/skills", b.skills, "", logger)
		if err != nil {
			return nil, err
		}
		mux.Handle("/api/skills/", p)
	}
	if b.connectors != "" {
		p, err := newProxy("/api/connectors", b.connectors, "/api", logger)
		if err != nil {
			return nil, err
		}
		mux.Handle("/api/connectors/", p)
	}
	if b.gitHost != "" {
		p, err := newProxy("/git", b.gitHost, "", logger)
		if err != nil {
			return nil, err
		}
		mux.Handle("/git/", p)
	}
	if b.mcp != "" {
		p, err := newProxy("/api/mcp-gateway", b.mcp, "", logger)
		if err != nil {
			return nil, err
		}
		mux.Handle("/api/mcp-gateway/", p)
	}
	if b.cluster != nil {
		mux.Handle("/api/cluster/", b.cluster)
	}
	if b.observability != nil {
		mux.Handle("/api/observability/", observabilityHandler(b.observability))
	}
	// The LLM gateway is reachable for its model list alone, which the Agents page shows with
	// each model's availability.
	if b.llm != "" {
		p, err := newProxy("/api/llm", b.llm, "", logger)
		if err != nil {
			return nil, err
		}
		mux.Handle("GET /api/llm/v1/models", p)
	}
	// Interactive chat goes through the orchestrator, which verifies the waiting session and
	// supplies the pinned agent settings. A turn may make several model and tool calls.
	if b.orchestrator != "" {
		p, err := newProxy("/api/agent", b.orchestrator, "/api/sessions", logger)
		if err != nil {
			return nil, err
		}
		mux.Handle("POST /api/agent/chat", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), chatTimeout)
			defer cancel()
			p.ServeHTTP(w, r.WithContext(ctx))
		}))
	}
	// The embedded files carry no modification time, so browsers would cache
	// them heuristically; ask them to revalidate on every load instead.
	static := http.FileServerFS(web)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// A vendored library is large and never changes under its version, so a request that
		// names the version may be cached.
		if strings.HasPrefix(r.URL.Path, "/vendor/") && r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		static.ServeHTTP(w, r)
	}))
	return mux, nil
}

// newProxy forwards requests under prefix to target, replacing prefix with
// replace in the path.
func newProxy(prefix, target, replace string, logger *slog.Logger) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid backend URL %q", target)
	}
	base := strings.TrimRight(u.EscapedPath(), "/")
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			escaped := base + replace + strings.TrimPrefix(pr.In.URL.EscapedPath(), prefix)
			path, err := url.PathUnescape(escaped)
			if err != nil {
				path = escaped
			}
			pr.Out.URL.Scheme, pr.Out.URL.Host = u.Scheme, u.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, escaped
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = ""
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("proxy request failed", "path", r.URL.Path, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "backend unavailable: " + prefix})
		},
	}, nil
}

// writablePrefixes are the only blobstore paths the UI may write: predefined
// task inputs and task templates.
var writablePrefixes = []string{"/api/blobs/blobs/tasks/", "/api/blobs/blobs/templates/tasks/"}

// chatTimeout bounds one interactive chat message. The agent service allows a turn ten
// minutes; the proxy waits a little longer so the agent's own answer arrives.
var chatTimeout = 11 * time.Minute

// sessionFile matches the files of an interactive session, the only objects under
// user-sessions/ the UI may write or delete: user-sessions/<trace>/<step...>/files/<name>.
// Chat logs and completion records stay read-only.
var sessionFile = regexp.MustCompile(`^/api/blobs/blobs/user-sessions(/[A-Za-z0-9_-][A-Za-z0-9._-]*){2,}/files/[A-Za-z0-9][A-Za-z0-9._-]{0,99}\.(md|txt|json)$`)

// getOnly keeps the blobstore read-only from the UI, except for PUTs of
// objects under writablePrefixes and PUTs and DELETEs of session files.
func getOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upload := false
		for _, p := range writablePrefixes {
			if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, p) && len(r.URL.Path) > len(p) {
				upload = true
			}
		}
		// A task always belongs to a collection: tasks/<collection>/<file>.
		if upload && strings.HasPrefix(r.URL.Path, writablePrefixes[0]) {
			rest := strings.TrimPrefix(r.URL.Path, writablePrefixes[0])
			if i := strings.IndexByte(rest, '/'); i <= 0 || i == len(rest)-1 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a task must be stored in a collection: tasks/<collection>/<file>"})
				return
			}
		}
		// Task files may also be deleted; everything else stays read-only.
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, writablePrefixes[0]) {
			rest := strings.TrimPrefix(r.URL.Path, writablePrefixes[0])
			if i := strings.IndexByte(rest, '/'); i > 0 && i < len(rest)-1 {
				upload = true
			}
		}
		if (r.Method == http.MethodPut || r.Method == http.MethodDelete) && sessionFile.MatchString(r.URL.Path) {
			upload = true
		}
		if r.Method != http.MethodGet && !upload {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// closeAfter closes c and adds a failed Close to the error the caller returns.
func closeAfter(err error, c io.Closer) error {
	if closeErr := c.Close(); closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return err
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
