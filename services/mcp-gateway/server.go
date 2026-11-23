package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"sync"
	"time"
)

type entry struct {
	upstream upstream
	config   Config
}
type gateway struct {
	log     *slog.Logger
	entries map[string]*entry
	invalid map[string]string
	store   blobStore
	changes sync.Mutex
	mu      sync.RWMutex
}

// closeResponse closes an upstream response body. A handler cannot return the
// close error, so it is logged.
func (g *gateway) closeResponse(resp *http.Response, service string) {
	if err := resp.Body.Close(); err != nil {
		g.log.Error("closing upstream response failed", "service", service, "error", err)
	}
}

func newGateway(store blobStore, log *slog.Logger) *gateway {
	return &gateway{store: store, log: log, entries: map[string]*entry{}, invalid: map[string]string{}}
}

func (g *gateway) load(ctx context.Context) error {
	names, err := g.store.names(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := validateName(name); err != nil {
			g.invalid[name] = err.Error()
			g.log.Warn("configuration unavailable", "service", name, "error", err)
			continue
		}
		c, err := g.store.load(ctx, name)
		if err != nil {
			g.invalid[name] = err.Error()
			g.log.Warn("configuration unavailable", "service", name, "error", err)
			continue
		}
		u, err := newUpstream(&c.MCPConfig)
		if err != nil {
			return err
		}
		g.entries[name] = &entry{config: c, upstream: u}
	}
	return nil
}

func (g *gateway) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range g.entries {
		_ = e.upstream.Close()
	}
}

func (g *gateway) get(name string) *entry { g.mu.RLock(); defer g.mu.RUnlock(); return g.entries[name] }

func (g *gateway) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /api/services", g.list)
	mux.HandleFunc("GET /api/services/{name}", g.read)
	mux.HandleFunc("PUT /api/services/{name}", g.save)
	mux.HandleFunc("DELETE /api/services/{name}", g.remove)
	mux.HandleFunc("POST /api/services/{name}/test-connection", g.testConnection)
	mux.HandleFunc("POST /mcp/{name}", g.proxy)
	mux.HandleFunc("DELETE /mcp/{name}", g.closeSession)
	mux.HandleFunc("GET /mcp/{name}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) })
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (g *gateway) list(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name      string `json:"name"`
		Transport string `json:"transport,omitempty"`
		Error     string `json:"error,omitempty"`
	}
	items := []item{}
	g.mu.RLock()
	for name, e := range g.entries {
		items = append(items, item{Name: name, Transport: e.config.MCPConfig.Transport})
	}
	for name, err := range g.invalid {
		items = append(items, item{Name: name, Error: err})
	}
	g.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	writeJSON(w, 200, map[string]any{"services": items})
}

func (g *gateway) read(w http.ResponseWriter, r *http.Request) {
	if e := g.get(r.PathValue("name")); e != nil {
		writeJSON(w, 200, e.config)
		return
	}
	g.mu.RLock()
	reason, ok := g.invalid[r.PathValue("name")]
	g.mu.RUnlock()
	if ok {
		apiError(w, 422, fmt.Errorf("configuration unavailable: %s", reason))
		return
	}
	apiError(w, 404, errMissing)
}

func (g *gateway) save(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateName(name); err != nil {
		apiError(w, 400, err)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		apiError(w, 400, err)
		return
	}
	c, err := decodeConfig(data)
	if err != nil {
		apiError(w, 400, err)
		return
	}
	g.changes.Lock()
	defer g.changes.Unlock()
	if err = g.store.save(r.Context(), name, &c); err != nil {
		apiError(w, 502, err)
		return
	}
	old := g.get(name)
	var u upstream
	if old != nil && reflect.DeepEqual(old.config.MCPConfig, c.MCPConfig) {
		u = old.upstream
	} else {
		u, err = newUpstream(&c.MCPConfig)
		if err != nil {
			apiError(w, 400, err)
			return
		}
	}
	g.mu.Lock()
	g.entries[name] = &entry{config: c, upstream: u}
	delete(g.invalid, name)
	g.mu.Unlock()
	if old != nil && old.upstream != u {
		_ = old.upstream.Close()
	}
	writeJSON(w, 200, c)
}

func (g *gateway) remove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateName(name); err != nil {
		apiError(w, 400, err)
		return
	}
	g.changes.Lock()
	defer g.changes.Unlock()
	if err := g.store.remove(r.Context(), name); err != nil {
		apiError(w, 502, err)
		return
	}
	g.mu.Lock()
	old := g.entries[name]
	delete(g.entries, name)
	delete(g.invalid, name)
	g.mu.Unlock()
	if old != nil {
		_ = old.upstream.Close()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *gateway) testConnection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	e := g.get(name)
	if e == nil {
		apiError(w, 404, errMissing)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	session := ""
	defer func() {
		if session == "" {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		resp, err := e.upstream.Delete(cleanup, http.Header{"MCP-Protocol-Version": []string{protocolVersion}, "Mcp-Session-Id": []string{session}})
		if err == nil && resp != nil {
			g.closeResponse(resp, name)
		}
	}()
	for _, method := range []string{methodInitialize, methodToolsList} {
		payload := initializeRequest("connection-test")
		if method == methodToolsList {
			payload, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "connection-test", "method": methodToolsList, "params": map[string]any{}})
		}
		headers := protocolHeaders(method == methodInitialize)
		if session != "" {
			headers.Set("Mcp-Session-Id", session)
		}
		resp, err := e.upstream.RoundTrip(ctx, payload, headers)
		if err != nil {
			apiError(w, 502, err)
			return
		}
		if method == methodInitialize {
			session = resp.Header.Get("Mcp-Session-Id")
		}
		result, err := readRPCResponse(resp, json.RawMessage(`"connection-test"`))
		g.closeResponse(resp, name)
		if err == nil && method == methodInitialize {
			err = validateInitialize(result)
		} else if err == nil {
			_, err = toolList(result)
		}
		if err != nil {
			apiError(w, 502, err)
			return
		}
		if method == methodInitialize {
			notification := initializedNotification()
			headers := protocolHeaders(false)
			if session != "" {
				headers.Set("Mcp-Session-Id", session)
			}
			initialized, err := e.upstream.RoundTrip(ctx, notification, headers)
			if err != nil {
				apiError(w, 502, err)
				return
			}
			g.closeResponse(initialized, name)
			if initialized.StatusCode != http.StatusAccepted {
				apiError(w, 502, fmt.Errorf("upstream did not accept initialized notification"))
				return
			}
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
