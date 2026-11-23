package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	maxViewerRows  = 1000
	maxViewerBytes = 4 << 20
)

type viewerSession struct {
	name, query string
	consumer    jetstream.Consumer
	lastUsed    time.Time
}

type createViewerRequest struct {
	StartSequence *uint64    `json:"start_sequence,omitempty"`
	StartTime     *time.Time `json:"start_time,omitempty"`
	Query         string     `json:"query,omitempty"`
}

type viewerResponse struct {
	ID                  string         `json:"id,omitempty"`
	SessionID           string         `json:"session_id,omitempty"`
	Events              []factoryEvent `json:"events"`
	NextSequence        uint64         `json:"next_sequence"`
	HasMore             bool           `json:"has_more"`
	StreamFirstSequence uint64         `json:"stream_first_sequence"`
	StreamLastSequence  uint64         `json:"stream_last_sequence"`
}

func (s *service) createEventSession(w http.ResponseWriter, r *http.Request) {
	var req createViewerRequest
	if err := readJSON(w, r, &req); err != nil {
		s.fail(w, 400, err)
		return
	}
	stream, err := s.js.Stream(r.Context(), s.eventStream)
	if err != nil {
		s.fail(w, 503, err)
		return
	}
	info, err := stream.Info(r.Context())
	if err != nil {
		s.fail(w, 503, err)
		return
	}
	start := viewerStartSequence(info.State.FirstSeq, info.State.LastSeq, info.State.Msgs)
	name := "viewer-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	cfg := jetstream.ConsumerConfig{
		Name: name, AckPolicy: jetstream.AckExplicitPolicy, InactiveThreshold: 10 * time.Minute,
		FilterSubjects: []string{"factory.workflow.started", "factory.workflow.finished"}, DeliverPolicy: jetstream.DeliverByStartSequencePolicy, OptStartSeq: start,
	}
	if req.StartSequence != nil {
		cfg.OptStartSeq = *req.StartSequence
	}
	if req.StartTime != nil {
		cfg.DeliverPolicy, cfg.OptStartTime, cfg.OptStartSeq = jetstream.DeliverByStartTimePolicy, req.StartTime, 0
	}
	if info.State.Msgs == 0 && req.StartSequence == nil && req.StartTime == nil {
		cfg.DeliverPolicy, cfg.OptStartSeq = jetstream.DeliverAllPolicy, 0
	}
	consumer, err := stream.CreateConsumer(r.Context(), cfg)
	if err != nil {
		s.fail(w, 503, err)
		return
	}
	session := &viewerSession{name: name, query: req.Query, consumer: consumer, lastUsed: time.Now()}
	s.viewerMu.Lock()
	s.viewers[name] = session
	s.viewerMu.Unlock()
	writeJSON(w, http.StatusCreated, viewerResponse{
		ID: name, SessionID: name, Events: []factoryEvent{}, NextSequence: cfg.OptStartSeq,
		HasMore: info.State.Msgs > 0, StreamFirstSequence: info.State.FirstSeq, StreamLastSequence: info.State.LastSeq,
	})
}

func viewerStartSequence(first, last, count uint64) uint64 {
	if count > maxViewerRows {
		return last - maxViewerRows + 1
	}
	return first
}

func (s *service) fetchEventSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.viewerMu.Lock()
	session := s.viewers[id]
	if session != nil {
		session.lastUsed = time.Now()
	}
	s.viewerMu.Unlock()
	if session == nil {
		s.fail(w, 404, errors.New("viewer session not found"))
		return
	}
	limit := maxViewerRows
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.fail(w, 400, errors.New("limit must be positive"))
			return
		}
		if n < limit {
			limit = n
		}
	}
	query := session.query
	if values, ok := r.URL.Query()["query"]; ok {
		query = values[len(values)-1]
	}
	stream, err := s.js.Stream(r.Context(), s.eventStream)
	if err != nil {
		s.fail(w, 503, err)
		return
	}
	info, err := stream.Info(r.Context())
	if err != nil {
		s.fail(w, 503, err)
		return
	}
	fetchCount := limit
	if query != "" {
		fetchCount = maxViewerRows
	}
	batch, err := session.consumer.Fetch(fetchCount, jetstream.FetchMaxWait(150*time.Millisecond))
	if err != nil {
		s.fail(w, 503, err)
		return
	}
	out := viewerResponse{Events: []factoryEvent{}, StreamFirstSequence: info.State.FirstSeq, StreamLastSequence: info.State.LastSeq}
	bytesUsed := 0
	stopped := false
	for msg := range batch.Messages() {
		if stopped {
			_ = msg.Nak()
			continue
		}
		meta, metaErr := msg.Metadata()
		if metaErr != nil {
			_ = msg.Nak()
			continue
		}
		var event factoryEvent
		if json.Unmarshal(msg.Data(), &event) == nil && (query == "" || strings.Contains(strings.ToLower(string(msg.Data())), strings.ToLower(query))) {
			if bytesUsed+len(msg.Data()) > maxViewerBytes {
				stopped = true
				_ = msg.Nak()
				continue
			}
			if len(out.Events) < limit {
				out.Events = append(out.Events, event)
				bytesUsed += len(msg.Data())
			}
		}
		_ = msg.Ack()
		out.NextSequence = meta.Sequence.Stream + 1
		if len(out.Events) >= limit {
			stopped = true
		}
	}
	if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) {
		s.fail(w, 503, err)
		return
	}
	out.HasMore = out.NextSequence != 0 && out.NextSequence <= info.State.LastSeq
	writeJSON(w, 200, out)
}

func (s *service) runViewerCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			var expired []string
			s.viewerMu.Lock()
			for id, session := range s.viewers {
				if now.Sub(session.lastUsed) >= 10*time.Minute {
					expired = append(expired, id)
					delete(s.viewers, id)
				}
			}
			s.viewerMu.Unlock()
			for _, id := range expired {
				_ = s.js.DeleteConsumer(ctx, s.eventStream, id)
			}
		}
	}
}

func (s *service) deleteEventSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.viewerMu.Lock()
	session := s.viewers[id]
	delete(s.viewers, id)
	s.viewerMu.Unlock()
	if session == nil {
		s.fail(w, 404, errors.New("viewer session not found"))
		return
	}
	if err := s.js.DeleteConsumer(r.Context(), s.eventStream, session.name); err != nil {
		s.fail(w, 503, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
