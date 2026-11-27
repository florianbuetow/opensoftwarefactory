package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

type fakeJetStream struct {
	jetstream.JetStream
	streamErr    error
	consumerErr  error
	deleteErr    error
	streams      map[string]*fakeStream
	consumers    map[string]*fakeConsumer
	deletedNames []string
	mu           sync.Mutex
}

func newFakeJetStream() *fakeJetStream {
	return &fakeJetStream{
		streams:   map[string]*fakeStream{},
		consumers: map[string]*fakeConsumer{},
	}
}

func (f *fakeJetStream) Stream(_ context.Context, name string) (jetstream.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	st, ok := f.streams[name]
	if !ok {
		return nil, jetstream.ErrStreamNotFound
	}
	return st, nil
}

func (f *fakeJetStream) Consumer(_ context.Context, _, name string) (jetstream.Consumer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consumerErr != nil {
		return nil, f.consumerErr
	}
	c, ok := f.consumers[name]
	if !ok {
		return nil, jetstream.ErrConsumerNotFound
	}
	return c, nil
}

func (f *fakeJetStream) DeleteConsumer(_ context.Context, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedNames = append(f.deletedNames, name)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.consumers[name]; !ok {
		return jetstream.ErrConsumerNotFound
	}
	delete(f.consumers, name)
	return nil
}

type fakeStream struct {
	jetstream.Stream
	infoErr      error
	createErr    error
	js           *fakeJetStream
	info         *jetstream.StreamInfo
	nextConsumer *fakeConsumer
	lastCreated  jetstream.ConsumerConfig
}

func (s *fakeStream) Info(_ context.Context, _ ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	if s.infoErr != nil {
		return nil, s.infoErr
	}
	return s.info, nil
}

func (s *fakeStream) CreateConsumer(_ context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) { //nolint:gocritic // permitted: required by third-party jetstream.Stream interface
	if s.createErr != nil {
		return nil, s.createErr
	}
	s.lastCreated = cfg
	c := s.nextConsumer
	if c == nil {
		c = &fakeConsumer{info: &jetstream.ConsumerInfo{}}
	}
	name := cfg.Durable
	if name == "" {
		name = cfg.Name
	}
	if s.js != nil && name != "" {
		s.js.mu.Lock()
		s.js.consumers[name] = c
		s.js.mu.Unlock()
	}
	return c, nil
}

type fakeConsumer struct {
	jetstream.Consumer
	info     *jetstream.ConsumerInfo
	infoErr  error
	fetchErr error
	batch    *fakeMessageBatch
}

func (c *fakeConsumer) Info(_ context.Context) (*jetstream.ConsumerInfo, error) {
	if c.infoErr != nil {
		return nil, c.infoErr
	}
	if c.info == nil {
		return &jetstream.ConsumerInfo{}, nil
	}
	return c.info, nil
}

func (c *fakeConsumer) Fetch(_ int, _ ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	if c.fetchErr != nil {
		return nil, c.fetchErr
	}
	if c.batch == nil {
		return newFakeMessageBatch(nil, nil), nil
	}
	return c.batch, nil
}

type fakeMessageBatch struct {
	jetstream.MessageBatch
	ch  chan jetstream.Msg
	err error
}

func newFakeMessageBatch(msgs []*fakeMsg, err error) *fakeMessageBatch {
	ch := make(chan jetstream.Msg, len(msgs))
	for _, m := range msgs {
		ch <- m
	}
	close(ch)
	return &fakeMessageBatch{ch: ch, err: err}
}

func (b *fakeMessageBatch) Messages() <-chan jetstream.Msg { return b.ch }
func (b *fakeMessageBatch) Error() error                   { return b.err }

type fakeMsg struct {
	jetstream.Msg
	metaErr      error
	doubleAckErr error
	data         []byte
	seq          uint64
	acked        bool
	doubleAcked  bool
	naked        bool
	termed       bool
}

func (m *fakeMsg) Data() []byte { return m.data }
func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	if m.metaErr != nil {
		return nil, m.metaErr
	}
	return &jetstream.MsgMetadata{Sequence: jetstream.SequencePair{Stream: m.seq}}, nil
}
func (m *fakeMsg) Ack() error                         { m.acked = true; return nil }
func (m *fakeMsg) DoubleAck(_ context.Context) error  { m.doubleAcked = true; return m.doubleAckErr }
func (m *fakeMsg) Nak() error                         { m.naked = true; return nil }
func (m *fakeMsg) NakWithDelay(_ time.Duration) error { m.naked = true; return nil }
func (m *fakeMsg) Term() error                        { m.termed = true; return nil }

// conditionalBlobs is a blob store test double that enforces ETag/If-Match/If-None-Match
// and supports fault injection.
type conditionalBlobs struct {
	objects map[string][]byte
	revs    map[string]string
	srv     *httptest.Server
	nextRev int
	mu      sync.Mutex
	failGet bool
	failPut bool
	failDel bool
	failLst bool
}

func newConditionalBlobs(t *testing.T) *conditionalBlobs {
	t.Helper()
	b := &conditionalBlobs{
		objects: map[string][]byte{},
		revs:    map[string]string{},
	}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if prefix, ok := strings.CutPrefix(r.URL.Path, "/list/"); ok {
			if b.failLst {
				http.Error(w, "list failed", 500)
				return
			}
			objects := []map[string]string{}
			for id := range b.objects {
				if strings.HasPrefix(id, prefix+"/") {
					objects = append(objects, map[string]string{"id": id})
				}
			}
			if len(objects) == 0 {
				http.Error(w, "not found", 404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"objects": objects})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/blobs/")
		switch r.Method {
		case http.MethodGet:
			if b.failGet {
				http.Error(w, "get failed", 500)
				return
			}
			data, ok := b.objects[id]
			if !ok {
				http.Error(w, "not found", 404)
				return
			}
			w.Header().Set("ETag", `"`+b.revs[id]+`"`)
			_, _ = w.Write(data)
		case http.MethodPut:
			if b.failPut {
				http.Error(w, "put failed", 500)
				return
			}
			if r.Header.Get("If-None-Match") == "*" {
				if _, exists := b.objects[id]; exists {
					http.Error(w, "exists", http.StatusPreconditionFailed)
					return
				}
			}
			if im := r.Header.Get("If-Match"); im != "" {
				want := strings.Trim(im, `"`)
				if cur, exists := b.revs[id]; !exists || cur != want {
					http.Error(w, "stale", http.StatusPreconditionFailed)
					return
				}
			}
			data, _ := io.ReadAll(r.Body)
			b.nextRev++
			rev := fmt.Sprintf("rev-%d", b.nextRev)
			b.objects[id] = data
			b.revs[id] = rev
			w.Header().Set("ETag", `"`+rev+`"`)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			if b.failDel {
				http.Error(w, "delete failed", 500)
				return
			}
			delete(b.objects, id)
			delete(b.revs, id)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func newIntakeService(t *testing.T, blobs *conditionalBlobs, js *fakeJetStream, orchURL string) (*service, http.Handler) {
	t.Helper()
	cfg := config{registerRetry: time.Millisecond}
	s := newService(&cfg, slog.New(slog.DiscardHandler), http.DefaultClient, nil)
	s.blobs = blobClient{base: blobs.srv.URL, http: http.DefaultClient}
	s.js = js
	s.eventStream = "FACTORY_WORKFLOW_EVENTS"
	s.orchestratorURL = orchURL
	s.loaded = true
	routes := s.routes()
	return s, routes
}

func doReq(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, r))
	return rec
}

func TestIntakeBindingCRUDAndStatus(t *testing.T) {
	blobs := newConditionalBlobs(t)
	js := newFakeJetStream()
	js.streams["FACTORY_WORKFLOW_EVENTS"] = &fakeStream{
		js:   js,
		info: &jetstream.StreamInfo{State: jetstream.StreamState{FirstSeq: 1, LastSeq: 5, Msgs: 5}},
	}
	s, h := newIntakeService(t, blobs, js, "")

	// Empty list
	rec := doReq(t, h, http.MethodGet, "/api/intake/bindings", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty list: %d %s", rec.Code, rec.Body.String())
	}

	// Not found get & status
	if rec = doReq(t, h, http.MethodGet, "/api/intake/bindings/missing", ""); rec.Code != 404 {
		t.Fatalf("get missing = %d", rec.Code)
	}
	if rec = doReq(t, h, http.MethodGet, "/api/intake/bindings/missing/status", ""); rec.Code != 404 {
		t.Fatalf("status missing = %d", rec.Code)
	}

	// Bad JSON & validation error
	if rec = doReq(t, h, http.MethodPut, "/api/intake/bindings/b1", "{bad"); rec.Code != 400 {
		t.Fatalf("bad json = %d", rec.Code)
	}
	if rec = doReq(t, h, http.MethodPut, "/api/intake/bindings/b1", `{"name":""}`); rec.Code != 422 {
		t.Fatalf("invalid binding = %d", rec.Code)
	}

	// Create valid binding
	validBody := `{"name":"Score","source":"factory","source_workflows":["apps/a"],"outcomes":["completed"],"workflow":"judges/b","workflow_version":1,"enabled":true}`
	rec = doReq(t, h, http.MethodPut, "/api/intake/bindings/b1", validBody)
	if rec.Code != 200 {
		t.Fatalf("create b1 = %d %s", rec.Code, rec.Body.String())
	}
	var created IntakeBinding
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Revision == "" {
		t.Fatalf("created=%+v err=%v", created, err)
	}

	// Get & List
	rec = doReq(t, h, http.MethodGet, "/api/intake/bindings/b1", "")
	if rec.Code != 200 {
		t.Fatalf("get b1 = %d", rec.Code)
	}
	rec = doReq(t, h, http.MethodGet, "/api/intake/bindings", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"b1"`) {
		t.Fatalf("list b1 = %d %s", rec.Code, rec.Body.String())
	}

	// Cycle detection on second binding
	cycleBody := `{"name":"Loop","source":"factory","source_workflows":["judges/b"],"outcomes":["completed"],"workflow":"apps/a","workflow_version":1,"enabled":true}`
	if rec = doReq(t, h, http.MethodPut, "/api/intake/bindings/b2", cycleBody); rec.Code != 422 {
		t.Fatalf("cycle = %d %s", rec.Code, rec.Body.String())
	}

	// Stale revision update -> 412
	if rec = doReq(t, h, http.MethodPut, "/api/intake/bindings/b1", validBody); rec.Code != 412 {
		t.Fatalf("stale update = %d", rec.Code)
	}

	// Valid update with revision
	updateBody := fmt.Sprintf(`{"revision":%q,"name":"Score v2","source":"factory","source_workflows":["apps/a"],"outcomes":["completed"],"workflow":"judges/b","workflow_version":2,"enabled":false}`, created.Revision)
	rec = doReq(t, h, http.MethodPut, "/api/intake/bindings/b1", updateBody)
	if rec.Code != 200 {
		t.Fatalf("update b1 = %d %s", rec.Code, rec.Body.String())
	}

	// Status with pending events from consumer
	js.consumers[durableBindingName("b1")].info = &jetstream.ConsumerInfo{NumPending: 3, NumAckPending: 2}
	s.setBindingError("b1", errors.New("boom"))
	rec = doReq(t, h, http.MethodGet, "/api/intake/bindings/b1/status", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var st bindingStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || st.Pending != 5 || st.LastError != "boom" {
		t.Fatalf("status=%+v err=%v", st, err)
	}

	// Delete binding (consumer exists, then again when consumer already gone -> still 204)
	if rec = doReq(t, h, http.MethodDelete, "/api/intake/bindings/b1", ""); rec.Code != 204 {
		t.Fatalf("delete b1 = %d", rec.Code)
	}
	if rec = doReq(t, h, http.MethodDelete, "/api/intake/bindings/b1", ""); rec.Code != 204 {
		t.Fatalf("delete b1 again = %d", rec.Code)
	}

	// POST creates a binding with a server-generated UUIDv7 id and refuses a caller-supplied id.
	if rec = doReq(t, h, http.MethodPost, "/api/intake/bindings", "{bad"); rec.Code != 400 {
		t.Fatalf("post bad json = %d", rec.Code)
	}
	if rec = doReq(t, h, http.MethodPost, "/api/intake/bindings", `{"id":"client-id","name":"Score","source":"factory","source_workflows":["apps/a"],"outcomes":["completed"],"workflow":"judges/b","workflow_version":1,"enabled":true}`); rec.Code != 400 {
		t.Fatalf("post with caller id = %d", rec.Code)
	}
	rec = doReq(t, h, http.MethodPost, "/api/intake/bindings", validBody)
	if rec.Code != 201 {
		t.Fatalf("post binding = %d %s", rec.Code, rec.Body.String())
	}
	var posted IntakeBinding
	if err := json.Unmarshal(rec.Body.Bytes(), &posted); err != nil || posted.ID == "" || posted.Revision == "" {
		t.Fatalf("posted=%+v err=%v", posted, err)
	}
	if rec := doReq(t, h, http.MethodDelete, "/api/intake/bindings/"+posted.ID, ""); rec.Code != 204 {
		t.Fatalf("delete posted = %d", rec.Code)
	}
}

func TestIntakeBindingErrorPaths(t *testing.T) {
	blobs := newConditionalBlobs(t)
	js := newFakeJetStream()
	js.streams["FACTORY_WORKFLOW_EVENTS"] = &fakeStream{
		js:   js,
		info: &jetstream.StreamInfo{State: jetstream.StreamState{LastSeq: 0}},
	}
	_, h := newIntakeService(t, blobs, js, "")

	// Corrupt stored JSON -> get 500, list 503
	blobs.mu.Lock()
	blobs.objects[bindingObject("bad")] = []byte("{not-json")
	blobs.revs[bindingObject("bad")] = "r1"
	blobs.mu.Unlock()
	if rec := doReq(t, h, http.MethodGet, "/api/intake/bindings/bad", ""); rec.Code != 500 {
		t.Fatalf("corrupt get = %d", rec.Code)
	}
	if rec := doReq(t, h, http.MethodGet, "/api/intake/bindings", ""); rec.Code != 503 {
		t.Fatalf("corrupt list = %d", rec.Code)
	}
	blobs.mu.Lock()
	delete(blobs.objects, bindingObject("bad"))
	blobs.mu.Unlock()

	// Blob get/list/delete failures -> 503
	blobs.failGet = true
	if rec := doReq(t, h, http.MethodGet, "/api/intake/bindings/x", ""); rec.Code != 503 {
		t.Fatalf("get 503 = %d", rec.Code)
	}
	if rec := doReq(t, h, http.MethodGet, "/api/intake/bindings/x/status", ""); rec.Code != 503 {
		t.Fatalf("status 503 = %d", rec.Code)
	}
	blobs.failGet = false

	blobs.failLst = true
	validBody := `{"name":"N","source":"factory","source_workflows":["a/b"],"outcomes":["completed"],"workflow":"c/d","workflow_version":1}`
	if rec := doReq(t, h, http.MethodPut, "/api/intake/bindings/x", validBody); rec.Code != 503 {
		t.Fatalf("put loadBindings 503 = %d", rec.Code)
	}
	blobs.failLst = false

	// Provision failure -> 503
	js.streamErr = errors.New("nats down")
	if rec := doReq(t, h, http.MethodPut, "/api/intake/bindings/x", validBody); rec.Code != 503 {
		t.Fatalf("provision 503 = %d", rec.Code)
	}
	js.streamErr = nil

	// Put failure rolls back provisioned consumer
	blobs.failPut = true
	if rec := doReq(t, h, http.MethodPut, "/api/intake/bindings/x", validBody); rec.Code != 503 {
		t.Fatalf("put 503 = %d", rec.Code)
	}
	blobs.failPut = false

	// Delete blob failure & consumer delete failure
	blobs.failDel = true
	if rec := doReq(t, h, http.MethodDelete, "/api/intake/bindings/x", ""); rec.Code != 503 {
		t.Fatalf("delete blob 503 = %d", rec.Code)
	}
	blobs.failDel = false
	js.deleteErr = errors.New("nats delete failed")
	if rec := doReq(t, h, http.MethodDelete, "/api/intake/bindings/x", ""); rec.Code != 503 {
		t.Fatalf("delete consumer 503 = %d", rec.Code)
	}
}

func TestIntakeConsumeAndSubmitBinding(t *testing.T) {
	blobs := newConditionalBlobs(t)
	js := newFakeJetStream()
	var orchStatus int
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(orchStatus)
		_, _ = w.Write([]byte(`{"error":"msg"}`))
	}))
	t.Cleanup(orch.Close)

	s, _ := newIntakeService(t, blobs, js, orch.URL)

	// Store two bindings: one enabled, one disabled.
	enabled := IntakeBinding{ID: "en", Name: "En", Source: "factory", SourceWorkflows: []string{"apps/a"}, Outcomes: []string{"completed"}, Workflow: "judges/b", WorkflowVersion: 1, Enabled: true}
	disabled := IntakeBinding{ID: "dis", Name: "Dis", Source: "factory", SourceWorkflows: []string{"apps/a"}, Outcomes: []string{"completed"}, Workflow: "judges/b", WorkflowVersion: 1, Enabled: false}
	enRaw, _ := json.Marshal(enabled)
	disRaw, _ := json.Marshal(disabled)
	blobs.objects[bindingObject("en")] = enRaw
	blobs.revs[bindingObject("en")] = "r1"
	blobs.objects[bindingObject("dis")] = disRaw
	blobs.revs[bindingObject("dis")] = "r1"

	badMsg := &fakeMsg{data: []byte("{bad"), seq: 1}
	nonMatch, _ := json.Marshal(factoryEvent{SchemaVersion: 1, Type: "workflow.finished", Workflow: "apps/a", Status: "failed"})
	nonMatchMsg := &fakeMsg{data: nonMatch, seq: 2}
	match, _ := json.Marshal(factoryEvent{ID: "e1", SchemaVersion: 1, Type: "workflow.finished", Workflow: "apps/a", Status: "completed"})
	matchMsg := &fakeMsg{data: match, seq: 3}

	enConsumer := &fakeConsumer{
		info:  &jetstream.ConsumerInfo{NumPending: 1, NumAckPending: 0},
		batch: newFakeMessageBatch([]*fakeMsg{badMsg, nonMatchMsg, matchMsg}, nil),
	}
	disConsumer := &fakeConsumer{info: &jetstream.ConsumerInfo{NumPending: 4, NumAckPending: 1}}
	js.consumers[durableBindingName("en")] = enConsumer
	js.consumers[durableBindingName("dis")] = disConsumer

	// Happy path submit (200)
	orchStatus = 200
	s.consumeBindings(t.Context())
	if !badMsg.termed || !nonMatchMsg.acked || !matchMsg.doubleAcked {
		t.Fatalf("flags termed=%v acked=%v doubleAcked=%v", badMsg.termed, nonMatchMsg.acked, matchMsg.doubleAcked)
	}
	s.intakeMu.Lock()
	enSt, disSt := s.intakeStatus["en"], s.intakeStatus["dis"]
	s.intakeMu.Unlock()
	if enSt.LastAccepted.IsZero() || enSt.Pending != 1 || disSt.Pending != 5 {
		t.Fatalf("en=%+v dis=%+v", enSt, disSt)
	}

	// Submit 404 & 500 & unreachable
	orchStatus = 404
	failMsg := &fakeMsg{data: match, seq: 4}
	enConsumer.batch = newFakeMessageBatch([]*fakeMsg{failMsg}, nil)
	s.consumeBindings(t.Context())
	if !failMsg.naked {
		t.Fatal("expected NakWithDelay on 404")
	}

	orchStatus = 500
	if err := s.submitBinding(t.Context(), &enabled, &factoryEvent{ID: "e2"}, match); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("500 err = %v", err)
	}

	// Batch error & fetch error & loadBindings error
	enConsumer.batch = newFakeMessageBatch(nil, errors.New("batch boom"))
	if err := s.consumeBinding(t.Context(), &enabled); err == nil {
		t.Fatal("expected batch error")
	}
	enConsumer.fetchErr = errors.New("fetch boom")
	if err := s.consumeBinding(t.Context(), &enabled); err == nil {
		t.Fatal("expected fetch error")
	}
	enConsumer.fetchErr = nil

	// runIntake exits cleanly when context is already canceled
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	blobs.failLst = true
	s.runIntake(ctx)
	blobs.failLst = false

	// submitBinding unreachable & bad URL
	s.orchestratorURL = "http://127.0.0.1:1"
	if err := s.submitBinding(t.Context(), &enabled, &factoryEvent{ID: "e3"}, match); err == nil {
		t.Fatal("expected unreachable error")
	}
	s.orchestratorURL = "://bad-url"
	if err := s.submitBinding(t.Context(), &enabled, &factoryEvent{ID: "e3"}, match); err == nil {
		t.Fatal("expected bad url error")
	}
}

func TestValidateBindingAndCyclesEdgeCases(t *testing.T) {
	base := IntakeBinding{ID: "ok", Name: "Ok", Source: "factory", SourceWorkflows: []string{"a/b"}, Outcomes: []string{"completed"}, Workflow: "c/d", WorkflowVersion: 1}
	cases := []func(*IntakeBinding){
		func(b *IntakeBinding) { b.ID = "../bad" },
		func(b *IntakeBinding) { b.Name = "   " },
		func(b *IntakeBinding) { b.Source = "other" },
		func(b *IntakeBinding) { b.Workflow = "bad workflow!" },
		func(b *IntakeBinding) { b.Outcomes = nil },
		func(b *IntakeBinding) { b.SourceWorkflows = []string{"bad name!"} },
		func(b *IntakeBinding) { b.Outcomes = []string{"unknown"} },
	}
	for i, mutate := range cases {
		b := base
		mutate(&b)
		if err := validateBinding(&b); err == nil {
			t.Errorf("case %d accepted invalid binding %+v", i, b)
		}
	}
	// bindingMatches wrong schema & wrong outcome
	if bindingMatches(&base, &factoryEvent{SchemaVersion: 2, Type: "workflow.finished", Workflow: "a/b", Status: "completed"}) {
		t.Fatal("wrong schema matched")
	}
	if bindingMatches(&base, &factoryEvent{SchemaVersion: 1, Type: "workflow.finished", Workflow: "a/b", Status: "failed"}) {
		t.Fatal("wrong outcome matched")
	}
	// createsCycle diamond (revisits node) + same ID different revision skipped
	existing := []IntakeBinding{
		{ID: "b1", Revision: "old", SourceWorkflows: []string{"a/x"}, Workflow: "a/y"},
		{ID: "b2", SourceWorkflows: []string{"a/y"}, Workflow: "a/z"},
		{ID: "b3", SourceWorkflows: []string{"a/y"}, Workflow: "a/w"},
		{ID: "b4", SourceWorkflows: []string{"a/w"}, Workflow: "a/z"},
	}
	if createsCycle(existing, &IntakeBinding{ID: "b1", Revision: "new", SourceWorkflows: []string{"a/x"}, Workflow: "a/y"}) {
		t.Fatal("acyclic diamond flagged as cycle")
	}
}

func TestEventViewerSessions(t *testing.T) {
	blobs := newConditionalBlobs(t)
	js := newFakeJetStream()
	stream := &fakeStream{
		js:   js,
		info: &jetstream.StreamInfo{State: jetstream.StreamState{FirstSeq: 1, LastSeq: 10, Msgs: 10}},
	}
	js.streams["FACTORY_WORKFLOW_EVENTS"] = stream
	s, h := newIntakeService(t, blobs, js, "")

	// Create session errors: bad JSON, stream error, info error, create error
	if rec := doReq(t, h, http.MethodPost, "/api/events/sessions", "{bad"); rec.Code != 400 {
		t.Fatalf("bad json = %d", rec.Code)
	}
	js.streamErr = errors.New("down")
	if rec := doReq(t, h, http.MethodPost, "/api/events/sessions", `{}`); rec.Code != 503 {
		t.Fatalf("stream err = %d", rec.Code)
	}
	js.streamErr = nil
	stream.infoErr = errors.New("info err")
	if rec := doReq(t, h, http.MethodPost, "/api/events/sessions", `{}`); rec.Code != 503 {
		t.Fatalf("info err = %d", rec.Code)
	}
	stream.infoErr = nil
	stream.createErr = errors.New("create err")
	if rec := doReq(t, h, http.MethodPost, "/api/events/sessions", `{}`); rec.Code != 503 {
		t.Fatalf("create err = %d", rec.Code)
	}
	stream.createErr = nil

	// Empty stream -> DeliverAllPolicy
	stream.info = &jetstream.StreamInfo{State: jetstream.StreamState{Msgs: 0}}
	rec := doReq(t, h, http.MethodPost, "/api/events/sessions", `{}`)
	if rec.Code != 201 || stream.lastCreated.DeliverPolicy != jetstream.DeliverAllPolicy {
		t.Fatalf("empty stream: %d policy=%v", rec.Code, stream.lastCreated.DeliverPolicy)
	}

	// Non-empty stream with StartSequence and StartTime
	stream.info = &jetstream.StreamInfo{State: jetstream.StreamState{FirstSeq: 1, LastSeq: 10, Msgs: 10}}
	rec = doReq(t, h, http.MethodPost, "/api/events/sessions", `{"start_sequence":5,"query":"alpha"}`)
	if rec.Code != 201 || stream.lastCreated.OptStartSeq != 5 {
		t.Fatalf("start_sequence: %d seq=%d", rec.Code, stream.lastCreated.OptStartSeq)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rec = doReq(t, h, http.MethodPost, "/api/events/sessions", fmt.Sprintf(`{"start_time":%q}`, now))
	if rec.Code != 201 || stream.lastCreated.DeliverPolicy != jetstream.DeliverByStartTimePolicy {
		t.Fatalf("start_time: %d policy=%v", rec.Code, stream.lastCreated.DeliverPolicy)
	}
	var created viewerResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Fetch: unknown session (404), bad limit (400)
	if rec = doReq(t, h, http.MethodGet, "/api/events/sessions/nope", ""); rec.Code != 404 {
		t.Fatalf("missing session = %d", rec.Code)
	}
	if rec = doReq(t, h, http.MethodGet, "/api/events/sessions/"+created.ID+"?limit=0", ""); rec.Code != 400 {
		t.Fatalf("bad limit = %d", rec.Code)
	}

	// Fetch with messages: metadata error, query filter, limit cap (stops and naks remaining)
	ev1, _ := json.Marshal(factoryEvent{ID: "e1", Workflow: "apps/alpha", Status: "completed"})
	ev2, _ := json.Marshal(factoryEvent{ID: "e2", Workflow: "apps/beta", Status: "completed"})
	ev3, _ := json.Marshal(factoryEvent{ID: "e3", Workflow: "apps/alpha", Status: "completed"})
	mErr := &fakeMsg{data: ev1, metaErr: errors.New("no meta")}
	mSkip := &fakeMsg{data: ev2, seq: 2}
	mKeep := &fakeMsg{data: ev1, seq: 3}
	mExtra := &fakeMsg{data: ev3, seq: 4}
	js.consumers[created.ID].batch = newFakeMessageBatch([]*fakeMsg{mErr, mSkip, mKeep, mExtra}, nil)

	rec = doReq(t, h, http.MethodGet, "/api/events/sessions/"+created.ID+"?limit=1&query=alpha", "")
	if rec.Code != 200 {
		t.Fatalf("fetch = %d %s", rec.Code, rec.Body.String())
	}
	var fetched viewerResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &fetched)
	if len(fetched.Events) != 1 || !fetched.HasMore || fetched.NextSequence != 4 || !mErr.naked || !mSkip.acked || !mKeep.acked || !mExtra.naked {
		t.Fatalf("fetched=%+v flags err=%v skip=%v keep=%v extra=%v", fetched, mErr.naked, mSkip.acked, mKeep.acked, mExtra.naked)
	}

	// Fetch error & batch error
	js.consumers[created.ID].batch = newFakeMessageBatch(nil, errors.New("batch fail"))
	if rec = doReq(t, h, http.MethodGet, "/api/events/sessions/"+created.ID, ""); rec.Code != 503 {
		t.Fatalf("batch fail = %d", rec.Code)
	}
	js.consumers[created.ID].fetchErr = errors.New("fetch fail")
	if rec = doReq(t, h, http.MethodGet, "/api/events/sessions/"+created.ID, ""); rec.Code != 503 {
		t.Fatalf("fetch fail = %d", rec.Code)
	}
	js.consumers[created.ID].fetchErr = nil

	// Delete session: not found (404), delete error (503), success (204)
	if rec = doReq(t, h, http.MethodDelete, "/api/events/sessions/nope", ""); rec.Code != 404 {
		t.Fatalf("delete missing = %d", rec.Code)
	}
	js.deleteErr = errors.New("del fail")
	if rec = doReq(t, h, http.MethodDelete, "/api/events/sessions/"+created.ID, ""); rec.Code != 503 {
		t.Fatalf("delete 503 = %d", rec.Code)
	}
	js.deleteErr = nil
	// Re-add to test 204 delete
	s.viewerMu.Lock()
	s.viewers["v-del"] = &viewerSession{name: "v-del", lastUsed: time.Now()}
	s.viewerMu.Unlock()
	js.consumers["v-del"] = &fakeConsumer{}
	if rec = doReq(t, h, http.MethodDelete, "/api/events/sessions/v-del", ""); rec.Code != 204 {
		t.Fatalf("delete 204 = %d", rec.Code)
	}

	// runViewerCleanup cancels cleanly
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.runViewerCleanup(ctx)
}

type failingCloser struct{}

func (failingCloser) Close() error { return errors.New("close failed") }

func TestBlobClientAndHelpers(t *testing.T) {
	if err := closeAfter(nil, failingCloser{}); err == nil {
		t.Fatal("expected close error")
	}

	blobs := newConditionalBlobs(t)
	client := blobClient{base: blobs.srv.URL, http: http.DefaultClient}

	// putConditionalJSON: create, conflict, update, conflict, server error, marshal error
	rev, err := client.putConditionalJSON(t.Context(), "k1", map[string]string{"a": "b"}, "", true)
	if err != nil || rev == "" {
		t.Fatalf("create rev=%q err=%v", rev, err)
	}
	if _, err = client.putConditionalJSON(t.Context(), "k1", map[string]string{"a": "b"}, "", true); !errors.Is(err, errPrecondition) {
		t.Fatalf("duplicate create err=%v", err)
	}
	rev2, err := client.putConditionalJSON(t.Context(), "k1", map[string]string{"a": "c"}, rev, false)
	if err != nil || rev2 == "" {
		t.Fatalf("update rev=%q err=%v", rev2, err)
	}
	if _, err = client.putConditionalJSON(t.Context(), "k1", map[string]string{"a": "d"}, rev, false); !errors.Is(err, errPrecondition) {
		t.Fatalf("stale update err=%v", err)
	}
	if _, err = client.putConditionalJSON(t.Context(), "k1", func() {}, "", true); err == nil {
		t.Fatal("expected marshal error")
	}
	blobs.failPut = true
	if _, err := client.putConditionalJSON(t.Context(), "k2", map[string]string{}, "", true); err == nil {
		t.Fatal("expected 500 putConditionalJSON error")
	}
	if err := client.put(t.Context(), "k2", []byte("x"), "text/plain", "c", "trace-1"); err == nil {
		t.Fatal("expected 500 put error")
	}
	blobs.failPut = false

	blobs.failGet = true
	if _, _, err := client.getWithRevision(t.Context(), "k1"); err == nil {
		t.Fatal("expected 500 get error")
	}
	blobs.failGet = false

	blobs.failDel = true
	if err := client.delete(t.Context(), "k1"); err == nil {
		t.Fatal("expected 500 delete error")
	}
	blobs.failDel = false

	blobs.failLst = true
	if _, err := client.list(t.Context(), "k"); err == nil {
		t.Fatal("expected 500 list error")
	}
	blobs.failLst = false

	// Unreachable blob store
	down := blobClient{base: "http://127.0.0.1:1", http: http.DefaultClient}
	if _, _, err := down.getWithRevision(t.Context(), "k1"); err == nil {
		t.Fatal("expected unreachable get")
	}
	if _, err := down.putConditionalJSON(t.Context(), "k1", map[string]string{}, "", true); err == nil {
		t.Fatal("expected unreachable putConditionalJSON")
	}
	if err := down.put(t.Context(), "k1", []byte("x"), "text/plain", "c", ""); err == nil {
		t.Fatal("expected unreachable put")
	}
	if err := down.delete(t.Context(), "k1"); err == nil {
		t.Fatal("expected unreachable delete")
	}
	if _, err := down.list(t.Context(), "k"); err == nil {
		t.Fatal("expected unreachable list")
	}
	badURL := blobClient{base: "://bad", http: http.DefaultClient}
	if _, err := badURL.newRequest(t.Context(), http.MethodGet, "/x", http.NoBody); err == nil {
		t.Fatal("expected bad url error")
	}
}

func TestMainHelpersTelemetryMCPAndSeed(t *testing.T) {
	// env & positiveInt
	t.Setenv("TEST_CONNECTORS_ENV", "custom")
	if got := env("TEST_CONNECTORS_ENV", "def"); got != "custom" {
		t.Fatalf("env = %q", got)
	}
	t.Setenv("TEST_CONNECTORS_INT", "42")
	if got := positiveInt("TEST_CONNECTORS_INT", 10); got != 42 {
		t.Fatalf("positiveInt = %d", got)
	}

	// connectorNATSOptions
	t.Setenv("NATS_USER", "")
	t.Setenv("NATS_PASSWORD", "")
	if opts, err := connectorNATSOptions(); err != nil || len(opts) != 3 {
		t.Fatalf("no auth opts=%d err=%v", len(opts), err)
	}
	t.Setenv("NATS_USER", "u")
	if _, err := connectorNATSOptions(); err == nil {
		t.Fatal("expected mismatch error")
	}
	t.Setenv("NATS_PASSWORD", "p")
	if opts, err := connectorNATSOptions(); err != nil || len(opts) != 4 {
		t.Fatalf("with auth opts=%d err=%v", len(opts), err)
	}

	// initTelemetry: noop and configured
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	shutdown, err := initTelemetry(t.Context())
	if err != nil || shutdown(t.Context()) != nil {
		t.Fatalf("noop telemetry err=%v", err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:4318/v1/traces")
	shutdown, err = initTelemetry(t.Context())
	if err != nil {
		t.Fatalf("otlp telemetry err=%v", err)
	}
	_ = shutdown(t.Context())

	// waitForEventStream: valid, invalid config, canceled while missing
	js := newFakeJetStream()
	validCfg := jetstream.StreamConfig{
		Storage: jetstream.FileStorage, Retention: jetstream.LimitsPolicy, Discard: jetstream.DiscardNew,
		Replicas: 1, MaxConsumers: -1, MaxMsgs: -1, MaxBytes: -1, MaxMsgSize: -1,
		Subjects: []string{"factory.workflow.started", "factory.workflow.finished"},
	}
	js.streams["S"] = &fakeStream{info: &jetstream.StreamInfo{Config: validCfg}}
	if err := waitForEventStream(t.Context(), js, "S", slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("waitForEventStream valid = %v", err)
	}
	badCfg := validCfg
	badCfg.Replicas = 3
	js.streams["BAD"] = &fakeStream{info: &jetstream.StreamInfo{Config: badCfg}}
	if err := waitForEventStream(t.Context(), js, "BAD", slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("expected invalid stream config error")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitForEventStream(ctx, js, "MISSING", slog.New(slog.DiscardHandler)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// loadUntilDone, seedUntilDone, registerSkillsUntilDone, registerFactoryUntilDone
	blobs := newConditionalBlobs(t)
	var mcpCalls int
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mcpCalls++
		if r.Method == http.MethodGet {
			if mcpCalls <= 2 {
				w.WriteHeader(500)
				return
			}
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(mcpSrv.Close)

	s, _ := newIntakeService(t, blobs, js, "")
	s.mcpGateway = mcpSrv.URL
	s.cfg.mcpURL = "http://tool-gateway:8080/mcp"
	s.cfg.registerRetry = time.Millisecond
	s.loadUntilDone(t.Context())
	s.seedUntilDone(t.Context(), []repository{{Name: "demo", Kind: kindPlain, URL: "http://git:8080/demo.git", DefaultBranch: "main", IdleMinutes: 30, Credential: credentialNone}})
	s.registerSkillsUntilDone(t.Context())
	s.registerFactoryUntilDone(t.Context())

	// Repository URL helpers
	if h, p, err := repositoryHost("git@github.com:org/repo.git"); err != nil || h != "github.com" || p != "22" {
		t.Fatalf("scp host=%q port=%q err=%v", h, p, err)
	}
	if h, p, err := repositoryHost("ssh://git.example.com/org/repo.git"); err != nil || h != "git.example.com" || p != "22" {
		t.Fatalf("ssh host=%q port=%q err=%v", h, p, err)
	}
	if _, _, err := repositoryHost("ftp://example.com/repo.git"); err == nil {
		t.Fatal("expected ftp scheme error")
	}
	if o, n, err := githubRepository("git@github.com:org/repo.git"); err != nil || o != "org" || n != "repo" {
		t.Fatalf("github scp owner=%q name=%q err=%v", o, n, err)
	}
}
