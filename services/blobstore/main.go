// Command blobstore stores artifacts as plain files, each with a metadata
// record (creator, trace ID, creation time), and serves them over HTTP:
//
//	PUT    /blobs/<objectId>   write the request body; X-Creator and X-Trace-Id become metadata
//	GET    /blobs/<objectId>   stream the stored bytes; metadata is returned in headers
//	DELETE /blobs/<objectId>   remove the object and its metadata
//	GET    /meta/<objectId>    the metadata record as JSON
//	GET    /list/<prefix>      the immediate child directories and objects of a prefix
//	GET    /trace/<traceId>    every object created under a trace ID, oldest first
//
// Objects live under <data>/objects and metadata under <data>/meta. <data>/traces
// indexes the objects of each trace ID, so a trace lookup reads only its own entries:
// <data>/traces/<sha256 of the trace ID>/<sha256 of the object ID> holds the object ID.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	maxObjectBytes         = 256 << 20
	maxHeaderValue         = 256
	headerCreator          = "X-Creator"
	headerTrace            = "X-Trace-Id"
	headerCreated          = "X-Created"
	headerEstimatedInput   = "X-Estimated-Input-Tokens"
	headerEstimatedOutput  = "X-Estimated-Output-Tokens"
	headerProcessingTime   = "X-Processing-Time-Ms"
	headerEstimationMethod = "X-Estimation-Method"
	// traceIndexMarker exists in the trace index once it covers every stored object.
	traceIndexMarker = ".indexed"
)

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Meta describes one stored object.
type Meta struct {
	ID                    string    `json:"id"`
	Size                  int64     `json:"size"`
	Created               time.Time `json:"created"`
	Revision              string    `json:"revision,omitempty"`
	Creator               string    `json:"creator,omitempty"`
	TraceID               string    `json:"trace_id,omitempty"`
	EstimatedInputTokens  *int64    `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens *int64    `json:"estimated_output_tokens,omitempty"`
	ProcessingTimeMS      *int64    `json:"processing_time_ms,omitempty"`
	EstimationMethod      string    `json:"estimation_method,omitempty"`
}

// Listing is the content of one prefix: its child directories and objects.
type Listing struct {
	Prefix      string   `json:"prefix"`
	Directories []string `json:"directories"`
	Objects     []Meta   `json:"objects"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	dataPath := flag.String("data", "/data", "directory that holds the objects and metadata")
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	shutdownTelemetry, err := initTelemetry(context.Background(), "blobstore")
	if err != nil {
		return fmt.Errorf("initializing telemetry: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(ctx); err != nil {
			logger.Warn("telemetry shutdown failed", "error", err)
		}
	}()
	handler, err := newHandler(*dataPath, logger)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: *addr, Handler: otelhttp.NewHandler(handler, "blobstore"), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() {
		logger.Info("blobstore listening", "address", server.Addr, "data", *dataPath)
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

func newHandler(root string, logger *slog.Logger) (http.Handler, error) {
	s := store{objects: filepath.Join(root, "objects"), meta: filepath.Join(root, "meta"), traces: filepath.Join(root, "traces"),
		transactions: filepath.Join(root, "transactions"), logger: logger, mu: &sync.Mutex{}, tx: &transactionState{}, indexed: &atomic.Bool{}}
	for _, dir := range []string{s.objects, s.meta, s.traces, s.transactions} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	if err := s.recoverTransactions(); err != nil {
		return nil, fmt.Errorf("recovering blob transactions: %w", err)
	}
	if err := s.ensureLegacyRevisions(); err != nil {
		return nil, fmt.Errorf("assigning revisions to existing blobs: %w", err)
	}
	// Objects stored before the trace index existed are indexed once, in the background;
	// until then a trace lookup scans all metadata as it always did. An empty store has
	// nothing to scan and is marked at once.
	if _, err := os.Stat(filepath.Join(s.traces, traceIndexMarker)); err == nil {
		s.indexed.Store(true)
	} else if stored, _ := os.ReadDir(s.meta); len(stored) == 0 {
		s.buildTraceIndex()
	} else {
		go s.buildTraceIndex()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("PUT /blobs/{id...}", s.put)
	mux.HandleFunc("GET /blobs/{id...}", s.get)
	mux.HandleFunc("DELETE /blobs/{id...}", s.delete)
	mux.HandleFunc("GET /meta/{id...}", s.getMeta)
	mux.HandleFunc("GET /list/{prefix...}", s.list)
	mux.HandleFunc("GET /trace/{trace}", s.trace)
	return mux, nil
}

type store struct {
	objects      string
	meta         string
	traces       string
	transactions string
	logger       *slog.Logger
	// tx serialises object mutations and keeps object bytes and metadata consistent
	// for readers. It is deliberately separate from the trace-index mutex.
	tx *transactionState
	// mu serialises every change to a metadata record and its trace index entry,
	// so concurrent writes under one trace ID cannot lose entries.
	mu *sync.Mutex
	// indexed reports that the trace index covers every stored object.
	indexed *atomic.Bool
}

// validID reports whether id is a non-empty object ID that cannot escape the root.
func validID(id string) bool {
	if id == "" || (len(id) > 512 && !strings.HasPrefix(id, "skills/")) {
		return false
	}
	parts := strings.Split(id, "/")
	for _, seg := range parts {
		if seg == "." || seg == ".." || !segmentPattern.MatchString(seg) || (strings.HasPrefix(seg, ".") && parts[0] != "skills") {
			return false
		}
	}
	return true
}

// paths maps an object ID to its object file and metadata file.
func (s store) paths(id string) (object, meta string, ok bool) {
	if !validID(id) {
		return "", "", false
	}
	rel := filepath.FromSlash(id)
	if strings.HasPrefix(id, "skills/") {
		// A skill can contain both `config` and `config.json/example.txt`. Their metadata
		// cannot share the ordinary filename-plus-extension layout.
		key := sha256.Sum256([]byte(id))
		return filepath.Join(s.objects, rel), filepath.Join(s.meta, ".skill-files", hex.EncodeToString(key[:])+".json"), true
	}
	return filepath.Join(s.objects, rel), filepath.Join(s.meta, rel+".json"), true
}

func headerValue(r *http.Request, name string) (string, bool) {
	v := strings.TrimSpace(r.Header.Get(name))
	if len(v) > maxHeaderValue {
		return "", false
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f {
			return "", false
		}
	}
	return v, true
}

func (s store) put(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, _, ok := s.paths(id)
	if !ok {
		http.Error(w, "invalid object id", http.StatusBadRequest)
		return
	}
	creator, okCreator := headerValue(r, headerCreator)
	trace, okTrace := headerValue(r, headerTrace)
	inputTokens, okInput := optionalNonnegativeInt64Header(r, headerEstimatedInput)
	outputTokens, okOutput := optionalNonnegativeInt64Header(r, headerEstimatedOutput)
	processingTime, okProcessing := optionalNonnegativeInt64Header(r, headerProcessingTime)
	method, okMethod := headerValue(r, headerEstimationMethod)
	if !okCreator || !okTrace || !okInput || !okOutput || !okProcessing || !okMethod {
		http.Error(w, "invalid metadata header", http.StatusBadRequest)
		return
	}
	condition, err := parsePutCondition(r)
	if err != nil {
		http.Error(w, "invalid conditional header", http.StatusBadRequest)
		return
	}
	m := Meta{ID: id, Created: time.Now().UTC(), Creator: creator, TraceID: trace,
		EstimatedInputTokens: inputTokens, EstimatedOutputTokens: outputTokens, ProcessingTimeMS: processingTime, EstimationMethod: method}
	revision, status, err := s.commitPut(id, r.Body, m, condition)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("ETag", quoteETag(revision))
	w.WriteHeader(http.StatusNoContent)
}

func optionalNonnegativeInt64Header(r *http.Request, name string) (*int64, bool) {
	raw, ok := headerValue(r, name)
	if !ok || raw == "" {
		return nil, ok
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return nil, false
	}
	return &value, true
}

func (s store) get(w http.ResponseWriter, r *http.Request) {
	s.tx.mu.RLock()
	locked := true
	defer func() {
		if locked {
			s.tx.mu.RUnlock()
		}
	}()
	if s.tx.err != nil {
		s.fail(w, s.tx.err)
		return
	}
	id := r.PathValue("id")
	object, _, ok := s.paths(id)
	if !ok {
		http.Error(w, "invalid object id", http.StatusBadRequest)
		return
	}
	f, err := os.Open(object)
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "object not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "object not found", http.StatusNotFound)
		return
	}
	m := s.readMeta(id, info)
	// The open file descriptor and copied metadata form one committed snapshot.
	// Renames/unlinks cannot change these bytes; a slow download need not block writes.
	s.tx.mu.RUnlock()
	locked = false
	if m.Revision != "" {
		w.Header().Set("ETag", quoteETag(m.Revision))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set(headerCreator, m.Creator)
	w.Header().Set(headerTrace, m.TraceID)
	w.Header().Set(headerCreated, m.Created.Format(time.RFC3339Nano))
	setOptionalInt64ResponseHeader(w, headerEstimatedInput, m.EstimatedInputTokens)
	setOptionalInt64ResponseHeader(w, headerEstimatedOutput, m.EstimatedOutputTokens)
	setOptionalInt64ResponseHeader(w, headerProcessingTime, m.ProcessingTimeMS)
	if m.EstimationMethod != "" {
		w.Header().Set(headerEstimationMethod, m.EstimationMethod)
	}
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func setOptionalInt64ResponseHeader(w http.ResponseWriter, name string, value *int64) {
	if value != nil {
		w.Header().Set(name, strconv.FormatInt(*value, 10))
	}
}

func (s store) delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, _, ok := s.paths(id)
	if !ok {
		http.Error(w, "invalid object id", http.StatusBadRequest)
		return
	}
	status, err := s.commitDelete(id)
	if status != 0 {
		http.Error(w, "object not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pruneEmpty removes dir and its parents while they are empty, stopping at root.
func pruneEmpty(dir, root string) {
	for dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)) {
		if os.Remove(dir) != nil { // not empty, or already gone
			return
		}
		parent := filepath.Dir(dir)
		_ = syncDir(parent)
		dir = parent
	}
}

func (s store) getMeta(w http.ResponseWriter, r *http.Request) {
	s.tx.mu.RLock()
	defer s.tx.mu.RUnlock()
	if s.tx.err != nil {
		s.fail(w, s.tx.err)
		return
	}
	id := r.PathValue("id")
	object, _, ok := s.paths(id)
	if !ok {
		http.Error(w, "invalid object id", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(object)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && info.IsDir()) {
		http.Error(w, "object not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, s.readMeta(id, info))
}

// readMeta loads an object's metadata, falling back to the file's own attributes.
func (s store) readMeta(id string, info fs.FileInfo) Meta {
	m := Meta{ID: id, Size: info.Size(), Created: info.ModTime().UTC()}
	_, metaPath, ok := s.paths(id)
	if !ok {
		return m
	}
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return m
	}
	var stored Meta
	if json.Unmarshal(data, &stored) == nil && stored.ID == id {
		return stored
	}
	return m
}

// list returns the immediate children of a prefix: directories and objects.
func (s store) list(w http.ResponseWriter, r *http.Request) {
	s.tx.mu.RLock()
	defer s.tx.mu.RUnlock()
	if s.tx.err != nil {
		s.fail(w, s.tx.err)
		return
	}
	prefix := strings.TrimSuffix(r.PathValue("prefix"), "/")
	if prefix != "" && !validID(prefix) {
		http.Error(w, "invalid prefix", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.objects, filepath.FromSlash(prefix))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "prefix not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	out := Listing{Prefix: prefix, Directories: []string{}, Objects: []Meta{}}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") && prefix != "skills" && !strings.HasPrefix(prefix, "skills/") {
			continue
		}
		id := name
		if prefix != "" {
			id = prefix + "/" + name
		}
		if s.isUnpublishedStage(id, e) {
			continue
		}
		if e.IsDir() {
			out.Directories = append(out.Directories, name)
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out.Objects = append(out.Objects, s.readMeta(id, info))
	}
	sort.Strings(out.Directories)
	sort.Slice(out.Objects, func(i, j int) bool { return out.Objects[i].ID < out.Objects[j].ID })
	writeJSON(w, out)
}

// isUnpublishedStage identifies legacy upload files created beside an object before
// staging moved to the private transactions directory. A real dot-prefixed object has
// metadata and remains visible.
func (s store) isUnpublishedStage(id string, entry fs.DirEntry) bool {
	if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".upload-") {
		return false
	}
	_, metaPath, ok := s.paths(id)
	if !ok {
		return true
	}
	_, err := os.Stat(metaPath)
	return errors.Is(err, fs.ErrNotExist)
}

// trace returns every object whose metadata carries the trace ID, oldest first.
func (s store) trace(w http.ResponseWriter, r *http.Request) {
	s.tx.mu.RLock()
	defer s.tx.mu.RUnlock()
	if s.tx.err != nil {
		s.fail(w, s.tx.err)
		return
	}
	trace := r.PathValue("trace")
	if trace == "" || len(trace) > maxHeaderValue {
		http.Error(w, "invalid trace id", http.StatusBadRequest)
		return
	}
	lookup := s.scanTrace
	if s.indexed.Load() {
		lookup = s.indexedTrace
	}
	objects, err := lookup(trace)
	if err != nil {
		s.fail(w, err)
		return
	}
	sort.Slice(objects, func(i, j int) bool {
		if !objects[i].Created.Equal(objects[j].Created) {
			return objects[i].Created.Before(objects[j].Created)
		}
		return objects[i].ID < objects[j].ID
	})
	writeJSON(w, map[string]any{"trace_id": trace, "objects": objects})
}

// indexedTrace reads the index entries of one trace and the metadata they point to.
func (s store) indexedTrace(trace string) ([]Meta, error) {
	objects := []Meta{}
	dir := s.traceDir(trace)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return objects, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var id string
		if json.Unmarshal(data, &id) != nil {
			continue
		}
		m, found, stale := s.tracedMeta(trace, id)
		if stale {
			// Check again under the lock: a PUT may have written the record in between.
			s.mu.Lock()
			if m, found, stale = s.tracedMeta(trace, id); stale {
				s.dropTraceEntry(trace, id)
			}
			s.mu.Unlock()
		}
		if found {
			objects = append(objects, m)
		}
	}
	return objects, nil
}

// tracedMeta loads the metadata an index entry points to. The entry is stale when
// the object is gone or now belongs to another trace; an unreadable record is neither.
func (s store) tracedMeta(trace, id string) (m Meta, found, stale bool) {
	_, metaPath, ok := s.paths(id)
	if !ok {
		return m, false, true
	}
	data, err := os.ReadFile(metaPath)
	if errors.Is(err, fs.ErrNotExist) {
		return m, false, true
	}
	if err != nil || json.Unmarshal(data, &m) != nil {
		return m, false, false
	}
	if m.ID != id || m.TraceID != trace {
		return m, false, true
	}
	return m, true, false
}

// scanTrace finds the objects of one trace by reading every metadata record. It
// answers only while the index of previously stored objects is still being built.
func (s store) scanTrace(trace string) ([]Meta, error) {
	objects := []Meta{}
	err := filepath.WalkDir(s.meta, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var m Meta
		if json.Unmarshal(data, &m) == nil && m.TraceID == trace {
			objects = append(objects, m)
		}
		return nil
	})
	return objects, err
}

// buildTraceIndex indexes the objects stored before the index existed. The marker is
// written only after a complete scan, so an interrupted build starts over at the next
// start; entries are idempotent, and writes during the scan index themselves.
func (s store) buildTraceIndex() {
	started := time.Now()
	indexed := 0
	err := filepath.WalkDir(s.meta, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) { // deleted during the scan
			return nil
		}
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var m Meta
		if json.Unmarshal(data, &m) != nil || m.TraceID == "" {
			return nil
		}
		if _, metaPath, ok := s.paths(m.ID); !ok || metaPath != path {
			return nil
		}
		indexed++
		return writeJSONAtomic(s.traceEntry(m.TraceID, m.ID), m.ID)
	})
	if err == nil {
		err = writeJSONAtomic(filepath.Join(s.traces, traceIndexMarker), time.Now().UTC())
	}
	if err != nil {
		s.logger.Error("trace index build failed, trace lookups keep scanning all metadata", "error", err)
		return
	}
	s.indexed.Store(true)
	s.logger.Info("trace index built", "objects", indexed, "duration_ms", time.Since(started).Milliseconds())
}

// hashName turns a trace or object ID into a fixed-length file name, so slashes and
// long IDs are safe in the index.
func hashName(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// traceDir is the index directory of one trace ID.
func (s store) traceDir(trace string) string {
	return filepath.Join(s.traces, hashName(trace))
}

// traceEntry is the index entry of one object under a trace ID; its content is the object ID.
func (s store) traceEntry(trace, id string) string {
	return filepath.Join(s.traceDir(trace), hashName(id))
}

// storedTrace returns the trace ID of the metadata record at metaPath, or "" without one.
func storedTrace(metaPath string) string {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return ""
	}
	var m Meta
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	return m.TraceID
}

// writeMeta stores an object's metadata and moves its index entry to the record's
// trace ID. The entry is written first: a failure in between leaves a stale entry,
// which a trace lookup skips and removes, never an object missing from its trace.
func (s store) writeMeta(metaPath string, m Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := storedTrace(metaPath)
	if m.TraceID != "" {
		if err := ensureDirDurable(filepath.Dir(s.traceEntry(m.TraceID, m.ID)), s.traces); err != nil {
			return err
		}
		if err := writeJSONAtomic(s.traceEntry(m.TraceID, m.ID), m.ID); err != nil {
			return err
		}
	}
	if err := ensureDirDurable(filepath.Dir(metaPath), s.meta); err != nil {
		return err
	}
	if err := writeJSONAtomic(metaPath, m); err != nil {
		return err
	}
	if previous != "" && previous != m.TraceID {
		s.dropTraceEntry(previous, m.ID)
	}
	return nil
}

// removeMeta deletes an object's metadata and its index entry.
func (s store) removeMeta(id, metaPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := storedTrace(metaPath)
	if err := os.Remove(metaPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := syncNearestDir(filepath.Dir(metaPath), s.meta); err != nil {
		return err
	}
	if previous != "" {
		s.dropTraceEntry(previous, id)
	}
	return nil
}

// dropTraceEntry removes one index entry, and the trace's directory once it is empty.
// The caller holds s.mu, so no entry is being added to that directory meanwhile.
func (s store) dropTraceEntry(trace, id string) {
	entry := s.traceEntry(trace, id)
	if err := os.Remove(entry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.logger.Warn("removing trace index entry failed", "error", err)
		return
	}
	dir := filepath.Dir(entry)
	_ = syncNearestDir(dir, s.traces)
	if os.Remove(dir) == nil { // fails while other entries remain
		_ = syncDir(s.traces)
	}
}

func (s store) fail(w http.ResponseWriter, err error) {
	s.logger.Error("request failed", "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONAtomic writes v to path through a temporary file and a rename.
func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".meta-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
