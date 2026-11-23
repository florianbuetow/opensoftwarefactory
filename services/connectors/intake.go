package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const bindingPrefix = "intake-bindings"

var (
	intakeIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	workflowNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)
)

// IntakeBinding starts a pinned workflow version when a source workflow finishes with one of
// the named outcomes.
type IntakeBinding struct {
	ID              string   `json:"id"`
	Revision        string   `json:"revision"`
	Name            string   `json:"name"`
	Source          string   `json:"source"`
	SourceWorkflows []string `json:"source_workflows"`
	Workflow        string   `json:"workflow"`
	Description     string   `json:"description,omitempty"`
	Outcomes        []string `json:"outcomes"`
	WorkflowVersion int      `json:"workflow_version"`
	Enabled         bool     `json:"enabled"`
}

type bindingStatus struct {
	LastAccepted time.Time `json:"last_accepted,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	Pending      uint64    `json:"pending"`
}

func bindingObject(id string) string { return bindingPrefix + "/" + id + ".json" }

// pendingEvents counts the events a consumer still owes its binding: those not delivered yet
// and those delivered but not acknowledged.
func pendingEvents(info *jetstream.ConsumerInfo) uint64 {
	pending := info.NumPending
	if info.NumAckPending > 0 {
		pending += uint64(info.NumAckPending)
	}
	return pending
}

func validateBinding(b *IntakeBinding) error {
	if !intakeIDPattern.MatchString(b.ID) {
		return errors.New("id must be a safe non-empty identifier")
	}
	if strings.TrimSpace(b.Name) == "" {
		return errors.New("name is required")
	}
	if b.Source != "factory" {
		return errors.New(`source must be "factory"`)
	}
	if !workflowNamePattern.MatchString(b.Workflow) || b.WorkflowVersion < 1 {
		return errors.New("workflow must be a full grouped name and workflow_version must be positive")
	}
	if len(b.SourceWorkflows) == 0 || len(b.Outcomes) == 0 {
		return errors.New("source_workflows and outcomes must be explicit non-empty filters")
	}
	for _, name := range b.SourceWorkflows {
		if !workflowNamePattern.MatchString(name) {
			return fmt.Errorf("invalid source workflow %q", name)
		}
	}
	validOutcome := map[string]bool{"completed": true, "failed": true, "cancelled": true, "blocked": true, "terminated": true, "timed_out": true}
	for _, outcome := range b.Outcomes {
		if !validOutcome[outcome] {
			return fmt.Errorf("invalid outcome %q", outcome)
		}
	}
	return nil
}

func (s *service) loadBindings(ctx context.Context) ([]IntakeBinding, error) {
	ids, err := s.blobs.list(ctx, bindingPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]IntakeBinding, 0, len(ids))
	for _, id := range ids {
		data, revision, err := s.blobs.getWithRevision(ctx, id)
		if err != nil {
			return nil, err
		}
		var b IntakeBinding
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, err
		}
		b.Revision = revision
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func createsCycle(bindings []IntakeBinding, candidate *IntakeBinding) bool {
	edges := map[string][]string{}
	add := func(b *IntakeBinding) {
		if b.ID == candidate.ID && b.ID != "" && b.Revision != candidate.Revision {
			return
		}
		for _, source := range b.SourceWorkflows {
			edges[source] = append(edges[source], b.Workflow)
		}
	}
	for i := range bindings {
		add(&bindings[i])
	}
	add(candidate)
	for _, source := range candidate.SourceWorkflows {
		seen := map[string]bool{}
		var visit func(string) bool
		visit = func(at string) bool {
			if at == source {
				return true
			}
			if seen[at] {
				return false
			}
			seen[at] = true
			for _, next := range edges[at] {
				if visit(next) {
					return true
				}
			}
			return false
		}
		if visit(candidate.Workflow) {
			return true
		}
	}
	return false
}

func (s *service) listIntakeBindings(w http.ResponseWriter, r *http.Request) {
	bindings, err := s.loadBindings(r.Context())
	if err != nil {
		s.fail(w, 503, err)
		return
	}
	writeJSON(w, 200, bindings)
}

func (s *service) getIntakeBinding(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, revision, err := s.blobs.getWithRevision(r.Context(), bindingObject(id))
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("binding not found"))
		return
	}
	if err != nil {
		s.fail(w, 503, err)
		return
	}
	var b IntakeBinding
	if json.Unmarshal(data, &b) != nil {
		s.fail(w, 500, errors.New("stored binding is invalid"))
		return
	}
	b.Revision = revision
	writeJSON(w, 200, b)
}

func (s *service) putIntakeBinding(w http.ResponseWriter, r *http.Request) {
	s.bindingMu.Lock()
	defer s.bindingMu.Unlock()
	id := r.PathValue("id")
	var b IntakeBinding
	if err := readJSON(w, r, &b); err != nil {
		s.fail(w, 400, err)
		return
	}
	b.ID = id
	if err := validateBinding(&b); err != nil {
		s.fail(w, 422, err)
		return
	}
	all, err := s.loadBindings(r.Context())
	if err != nil {
		s.fail(w, 503, err)
		return
	}
	var current *IntakeBinding
	filtered := make([]IntakeBinding, 0, len(all))
	for i := range all {
		if all[i].ID == id {
			current = &all[i]
		} else {
			filtered = append(filtered, all[i])
		}
	}
	if createsCycle(filtered, &b) {
		s.fail(w, 422, errors.New("binding creates an automatic workflow trigger cycle"))
		return
	}
	create := current == nil
	if !create && (b.Revision == "" || b.Revision != current.Revision) {
		s.fail(w, 412, errors.New("binding revision is stale"))
		return
	}
	requestedRevision := b.Revision
	b.Revision = ""
	provisioned := false
	if create {
		if err = s.provisionBindingConsumer(r.Context(), id); err != nil {
			s.fail(w, 503, fmt.Errorf("provisioning binding checkpoint: %w", err))
			return
		}
		provisioned = true
	}
	revision, err := s.blobs.putConditionalJSON(r.Context(), bindingObject(id), b, requestedRevision, create)
	if errors.Is(err, errPrecondition) {
		if provisioned {
			_ = s.js.DeleteConsumer(r.Context(), s.eventStream, durableBindingName(id))
		}
		s.fail(w, 412, errors.New("binding revision is stale"))
		return
	}
	if err != nil {
		if provisioned {
			_ = s.js.DeleteConsumer(r.Context(), s.eventStream, durableBindingName(id))
		}
		s.fail(w, 503, err)
		return
	}
	b.Revision = revision
	writeJSON(w, 200, b)
}

func (s *service) deleteIntakeBinding(w http.ResponseWriter, r *http.Request) {
	s.bindingMu.Lock()
	defer s.bindingMu.Unlock()
	if err := s.blobs.delete(r.Context(), bindingObject(r.PathValue("id"))); err != nil {
		s.fail(w, 503, err)
		return
	}
	if err := s.js.DeleteConsumer(r.Context(), s.eventStream, durableBindingName(r.PathValue("id"))); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
		s.fail(w, 503, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *service) intakeBindingStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, err := s.blobs.getWithRevision(r.Context(), bindingObject(id)); errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("binding not found"))
		return
	} else if err != nil {
		s.fail(w, 503, err)
		return
	}
	s.intakeMu.Lock()
	status := s.intakeStatus[id]
	s.intakeMu.Unlock()
	if consumer, err := s.js.Consumer(r.Context(), s.eventStream, durableBindingName(id)); err == nil {
		if info, infoErr := consumer.Info(r.Context()); infoErr == nil {
			status.Pending = pendingEvents(info)
		}
	}
	writeJSON(w, 200, status)
}

func durableBindingName(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "factory-intake-" + hex.EncodeToString(sum[:8])
}

func (s *service) runIntake(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.consumeBindings(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *service) consumeBindings(ctx context.Context) {
	bindings, err := s.loadBindings(ctx)
	if err != nil {
		s.log.Warn("loading intake bindings failed", "error", err)
		return
	}
	for i := range bindings {
		b := &bindings[i]
		if err := s.provisionBindingConsumer(ctx, b.ID); err != nil {
			s.setBindingError(b.ID, err)
			continue
		}
		if !b.Enabled {
			if consumer, err := s.js.Consumer(ctx, s.eventStream, durableBindingName(b.ID)); err == nil {
				if info, infoErr := consumer.Info(ctx); infoErr == nil {
					s.intakeMu.Lock()
					st := s.intakeStatus[b.ID]
					st.Pending = pendingEvents(info)
					s.intakeStatus[b.ID] = st
					s.intakeMu.Unlock()
				}
			}
			continue
		}
		if err := s.consumeBinding(ctx, b); err != nil {
			s.setBindingError(b.ID, err)
		}
	}
}

func (s *service) consumeBinding(ctx context.Context, b *IntakeBinding) error {
	consumer, err := s.js.Consumer(ctx, s.eventStream, durableBindingName(b.ID))
	if err != nil {
		return err
	}
	batch, err := consumer.Fetch(32, jetstream.FetchMaxWait(100*time.Millisecond))
	if err != nil {
		return err
	}
	for msg := range batch.Messages() {
		var event factoryEvent
		if json.Unmarshal(msg.Data(), &event) != nil {
			_ = msg.Term()
			continue
		}
		if !bindingMatches(b, &event) {
			_ = msg.Ack()
			continue
		}
		if err := s.submitBinding(ctx, b, &event, msg.Data()); err != nil {
			_ = msg.NakWithDelay(time.Second)
			return err
		}
		if err := msg.DoubleAck(ctx); err != nil {
			return err
		}
		s.setBindingAccepted(b.ID)
	}
	if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) {
		return err
	}
	if info, err := consumer.Info(ctx); err == nil {
		s.intakeMu.Lock()
		st := s.intakeStatus[b.ID]
		st.Pending = pendingEvents(info)
		s.intakeStatus[b.ID] = st
		s.intakeMu.Unlock()
	}
	return nil
}

func (s *service) provisionBindingConsumer(ctx context.Context, id string) error {
	name := durableBindingName(id)
	if _, err := s.js.Consumer(ctx, s.eventStream, name); err == nil {
		return nil
	} else if !errors.Is(err, jetstream.ErrConsumerNotFound) {
		return err
	}
	stream, err := s.js.Stream(ctx, s.eventStream)
	if err != nil {
		return err
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return err
	}
	start := info.State.LastSeq + 1
	if start == 0 {
		start = 1
	}
	_, err = stream.CreateConsumer(ctx, bindingConsumerConfig(id, start))
	return err
}

func bindingConsumerConfig(id string, start uint64) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{Durable: durableBindingName(id), AckPolicy: jetstream.AckExplicitPolicy, AckWait: 30 * time.Second, FilterSubjects: []string{"factory.workflow.started", "factory.workflow.finished"}, DeliverPolicy: jetstream.DeliverByStartSequencePolicy, OptStartSeq: start}
}

func (s *service) setBindingError(id string, err error) {
	s.intakeMu.Lock()
	defer s.intakeMu.Unlock()
	st := s.intakeStatus[id]
	st.LastError = err.Error()
	s.intakeStatus[id] = st
}

func (s *service) setBindingAccepted(id string) {
	s.intakeMu.Lock()
	defer s.intakeMu.Unlock()
	st := s.intakeStatus[id]
	st.LastError = ""
	st.LastAccepted = time.Now().UTC()
	s.intakeStatus[id] = st
}
