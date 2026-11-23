package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The types a mapping can read from a judge artifact.
const (
	mappingNumber = "number"
	mappingString = "string"
)

type extractionMapping struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Type string `json:"type"`
}
type extractionRequest struct {
	ProducerRunID     string              `json:"producer_run_id"`
	TargetRunID       string              `json:"target_run_id"`
	TargetExecutionID string              `json:"target_execution_id"`
	ArtifactID        string              `json:"artifact_id"`
	AgentRef          string              `json:"agent_ref"`
	MappingVersion    string              `json:"mapping_version"`
	Mappings          []extractionMapping `json:"mappings"`
}

func (s *service) extractMetrics(w http.ResponseWriter, r *http.Request) {
	var in extractionRequest
	if err := decodeStrictJSON(w, r, &in); err != nil {
		s.fail(w, 400, err)
		return
	}
	if in.ProducerRunID == "" || in.TargetRunID == "" || in.TargetExecutionID == "" || in.ArtifactID == "" || in.AgentRef == "" || in.MappingVersion == "" || len(in.Mappings) == 0 || len(in.Mappings) > 500 {
		s.fail(w, 422, errors.New("complete extraction identity and 1..500 mappings are required"))
		return
	}
	producerScope, scopeErr := s.executionForArtifact(r.Context(), in.ProducerRunID, in.ArtifactID)
	if scopeErr != nil {
		if errors.Is(scopeErr, errNotFound) {
			s.fail(w, 422, errors.New("artifact is not retained in the producer execution scope"))
		} else {
			s.dependencyFailure(w, scopeErr)
		}
		return
	}
	targetScope, scopeErr := s.executionScope(r.Context(), in.TargetExecutionID)
	if scopeErr != nil || targetScope.RunID != in.TargetRunID {
		if scopeErr != nil && !errors.Is(scopeErr, errNotFound) {
			s.dependencyFailure(w, scopeErr)
		} else {
			s.fail(w, 422, errors.New("target run and execution relationship is not retained"))
		}
		return
	}
	names := map[string]bool{}
	for _, m := range in.Mappings {
		if m.Path == "" || m.Name == "" || (m.Type != mappingNumber && m.Type != mappingString) || names[m.Name] {
			s.fail(w, 422, errors.New("mappings require unique names, RFC6901 paths, and number|string types"))
			return
		}
		if _, err := parsePointer(m.Path); err != nil {
			s.fail(w, 422, err)
			return
		}
		names[m.Name] = true
	}
	artifact, err := s.readObject(r.Context(), in.ArtifactID)
	if errors.Is(err, errNotFound) {
		s.fail(w, 404, errors.New("artifact not found"))
		return
	}
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	var producer struct {
		InputArtifact string `json:"input_artifact"`
	}
	if producerErr := s.getJSON(r.Context(), s.blobs, "/blobs/runs/"+in.ProducerRunID+"/run.json", &producer); producerErr != nil {
		if errors.Is(producerErr, errNotFound) {
			s.fail(w, 422, errors.New("producer run is not retained"))
		} else {
			s.dependencyFailure(w, producerErr)
		}
		return
	}
	if producer.InputArtifact == "" {
		s.fail(w, 422, errors.New("producer run does not retain immutable intake evidence"))
		return
	}
	var input any
	if inputErr := s.getJSON(r.Context(), s.blobs, "/blobs/"+producer.InputArtifact, &input); inputErr != nil {
		if errors.Is(inputErr, errNotFound) {
			s.fail(w, 422, errors.New("producer input is not retained"))
		} else {
			s.dependencyFailure(w, inputErr)
		}
		return
	}
	if !containsExecutionIdentity(input, in.TargetRunID, in.TargetExecutionID) {
		s.fail(w, 422, errors.New("producer input does not link to the target execution"))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(artifact.Data))
	decoder.UseNumber()
	var document any
	parseErr := decoder.Decode(&document)
	if parseErr == nil {
		var extra any
		if extraErr := decoder.Decode(&extra); extraErr != io.EOF {
			parseErr = errors.New("artifact contains multiple JSON values")
		}
	}
	generation, err := s.allocateGeneration(r.Context())
	if err != nil {
		s.dependencyFailure(w, err)
		return
	}
	now := artifact.Created.UTC()
	if now.IsZero() {
		s.fail(w, 503, errors.New("artifact source timestamp is unavailable"))
		return
	}
	records := make([]Measurement, 0, len(in.Mappings))
	created := 0
	for _, mapping := range in.Mappings {
		m := Measurement{SchemaVersion: 1, ID: hashID(in.ProducerRunID, in.ArtifactID, in.MappingVersion, mapping.Name), Kind: kindJudge, RunID: in.ProducerRunID, ExecutionID: producerScope.ExecutionID, ParentExecutionID: producerScope.ParentExecutionID, Workflow: producerScope.Workflow, WorkflowVersion: &producerScope.WorkflowVersion, Agent: in.AgentRef, OccurredAt: now, TargetRunID: in.TargetRunID, TargetExecutionID: in.TargetExecutionID, ProducerRunID: in.ProducerRunID, ArtifactID: in.ArtifactID, ArtifactRevision: artifact.Revision, MappingVersion: in.MappingVersion, MetricName: mapping.Name, Status: statusDerived, Missing: []string{}}
		if parseErr != nil {
			m.Status = statusInvalid
			m.Missing = []string{"artifact/invalid JSON: " + parseErr.Error()}
		} else if value, ok := pointerValue(document, mapping.Path); !ok {
			m.Status = statusMissing
			m.Missing = []string{mapping.Path + "/field missing"}
		} else if mapping.Type == mappingNumber {
			number, ok := value.(json.Number)
			switch {
			case !ok:
				m.Status = statusInvalid
				m.Missing = []string{mapping.Path + "/expected number"}
			case !exactNumber(number):
				m.Status = statusInvalid
				m.Missing = []string{mapping.Path + "/invalid number"}
			default:
				m.NumberValue = number
			}
		} else {
			value, ok := value.(string)
			if !ok {
				m.Status = statusInvalid
				m.Missing = []string{mapping.Path + "/expected string"}
			} else {
				m.TextValue = &value
			}
		}
		data, _ := json.Marshal(m)
		op := mutationOperation{SchemaVersion: 1, ID: uuid.NewString(), ObjectID: measurementObject(kindJudge, m.ID), MeasurementID: m.ID, Replacement: data, AffectedHour: now.Truncate(time.Hour), Generation: generation, Phase: opIntent, CreatedAt: now, UpdatedAt: now}
		opRevision, opErr := s.writeObject(r.Context(), operationPrefix+"/"+op.ID+".json", op, "*")
		err = opErr
		if err != nil {
			s.dependencyFailure(w, err)
			return
		}
		revision, err := s.writeObject(r.Context(), op.ObjectID, m, "*")
		switch {
		case errors.Is(err, errPrecondition):
			existing, e := s.loadMeasurement(r.Context(), kindJudge, m.ID)
			if e != nil {
				s.dependencyFailure(w, e)
				return
			}
			existingRevision := existing.Revision
			existing.Revision = ""
			if b, _ := json.Marshal(existing); !bytes.Equal(b, data) {
				op.Phase, op.UpdatedAt = opCancelled, time.Now().UTC()
				_, _ = s.writeObject(r.Context(), operationPrefix+"/"+op.ID+".json", op, opRevision)
				s.fail(w, 412, errors.New("stable extraction identity already contains different content"))
				return
			}
			m.Revision = existingRevision
			op.Phase = opCancelled
		case err != nil:
			s.dependencyFailure(w, err)
			return
		default:
			m.Revision = revision
			created++
		}
		if op.Phase != opCancelled {
			op.Phase = opProjectionPending
		}
		op.UpdatedAt = time.Now().UTC()
		_, _ = s.writeObject(r.Context(), operationPrefix+"/"+op.ID+".json", op, opRevision)
		records = append(records, m)
	}
	status := 200
	if created > 0 {
		status = 201
	}
	writeJSON(w, status, map[string]any{"measurements": records, "created": created, "artifact_revision": artifact.Revision})
}

// exactNumber reports whether a JSON number can be held as an exact fraction:
// its power of ten, once the fraction digits are counted in, must stay within
// maxDecimalExponent. Zero qualifies whatever its exponent. The number is
// judged from its text and never expanded, so an absurd exponent costs nothing.
func exactNumber(number json.Number) bool {
	mantissa, exponent, hasExponent := strings.Cut(strings.ToLower(number.String()), "e")
	var scale int64
	if hasExponent {
		parsed, err := strconv.ParseInt(exponent, 10, 64)
		if err != nil {
			return false
		}
		scale = parsed
	}
	if strings.Trim(mantissa, "-0.") == "" {
		return true
	}
	_, fraction, _ := strings.Cut(mantissa, ".")
	scale -= int64(len(fraction))
	return scale >= -maxDecimalExponent && scale <= maxDecimalExponent
}

func containsExecutionIdentity(value any, runID, executionID string) bool {
	switch node := value.(type) {
	case map[string]any:
		if node["run_id"] == runID && node["execution_id"] == executionID {
			return true
		}
		for _, child := range node {
			if containsExecutionIdentity(child, runID, executionID) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if containsExecutionIdentity(child, runID, executionID) {
				return true
			}
		}
	}
	return false
}

func parsePointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("mapping path must be an RFC6901 JSON pointer")
	}
	parts := strings.Split(pointer[1:], "/")
	for i, p := range parts {
		var b strings.Builder
		for j := 0; j < len(p); j++ {
			if p[j] != '~' {
				b.WriteByte(p[j])
				continue
			}
			if j+1 >= len(p) || (p[j+1] != '0' && p[j+1] != '1') {
				return nil, errors.New("invalid RFC6901 escape")
			}
			j++
			if p[j] == '0' {
				b.WriteByte('~')
			} else {
				b.WriteByte('/')
			}
		}
		parts[i] = b.String()
	}
	return parts, nil
}

func pointerValue(document any, pointer string) (any, bool) {
	parts, err := parsePointer(pointer)
	if err != nil {
		return nil, false
	}
	value := document
	for _, part := range parts {
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[part]
			if !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			value = v[i]
		default:
			return nil, false
		}
	}
	return value, true
}
