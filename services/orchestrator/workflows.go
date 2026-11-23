package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"opensoftwarefactory/orchestrator/pipeline"
)

// workflowView is a workflow version as the API returns it: the definition and the list of
// stored versions of that workflow.
type workflowView struct {
	pipeline.Definition
	Versions []int `json:"versions"`
}

func (c Config) view(d *pipeline.Definition) workflowView {
	return workflowView{Definition: *d, Versions: c.versions(d.Name)}
}

// statusError is an error with the HTTP status it is answered with. Workflows names the
// workflows that stand in the way, for a conflict.
type statusError struct {
	err       error
	workflows []string
	status    int
}

func (e *statusError) Error() string { return e.err.Error() }

func refuse(status int, format string, args ...any) *statusError {
	return &statusError{status: status, err: fmt.Errorf(format, args...)}
}

// change applies edit to the stored workflows and stores the result. An invalid result is
// refused with 422 and nothing is stored. The change is not tied to the request, so a client
// that goes away does not leave it half stored.
func (s *server) change(edit func(c *Config) error) error {
	return s.workflows.update(context.Background(), func(c *Config) error {
		if err := edit(c); err != nil {
			return err
		}
		if err := c.validate(); err != nil {
			return &statusError{status: http.StatusUnprocessableEntity, err: err}
		}
		return nil
	})
}

// failChange answers the error of a change.
func (s *server) failChange(w http.ResponseWriter, err error) {
	var refused *statusError
	if !errors.As(err, &refused) {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	body := map[string]any{"error": refused.Error()}
	if refused.workflows != nil {
		body["workflows"] = refused.workflows
	}
	writeJSON(w, refused.status, body)
}

// getWorkflows lists the newest version of every workflow.
func (s *server) getWorkflows(w http.ResponseWriter, _ *http.Request) {
	c, err := s.config()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	views := []workflowView{}
	newest := pipeline.Newest(c.Workflows)
	for i := range newest {
		views = append(views, c.view(&newest[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": views})
}

// getWorkflowVersion returns one stored version of a workflow.
func (s *server) getWorkflowVersion(w http.ResponseWriter, r *http.Request) {
	c, err := s.config()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	version, err := strconv.Atoi(r.PathValue("version"))
	def, ok := c.find(r.PathValue("name"), version)
	if err != nil || version < 1 || !ok {
		s.fail(w, http.StatusNotFound, errors.New("unknown workflow version"))
		return
	}
	writeJSON(w, http.StatusOK, c.view(&def))
}

func descriptionHasLineBreaks(description string) bool {
	return strings.ContainsAny(description, "\r\n\v\f\u0085\u2028\u2029")
}

// patchWorkflowDescription updates only the description of one stored workflow version.
// Descriptions are version-specific, but editing one does not create a new workflow version.
func (s *server) patchWorkflowDescription(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || version < 1 {
		s.fail(w, http.StatusNotFound, errors.New("unknown workflow version"))
		return
	}
	var body struct {
		Description *string `json:"description"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	d.DisallowUnknownFields()
	if err = d.Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if err = d.Decode(&struct{}{}); err != io.EOF {
		s.fail(w, http.StatusBadRequest, errors.New("request body must contain one JSON object"))
		return
	}
	if body.Description == nil {
		s.fail(w, http.StatusBadRequest, errors.New("description is required"))
		return
	}
	if descriptionHasLineBreaks(*body.Description) {
		s.fail(w, http.StatusBadRequest, errors.New("description must not contain line breaks"))
		return
	}

	var stored workflowView
	err = s.change(func(c *Config) error {
		for i := range c.Workflows {
			def := &c.Workflows[i]
			if def.Name == r.PathValue("name") && def.Version == version {
				def.Description = *body.Description
				stored = c.view(def)
				return nil
			}
		}
		return refuse(http.StatusNotFound, "unknown workflow version")
	})
	if err != nil {
		s.failChange(w, err)
		return
	}
	s.logger.Info("workflow description updated", "workflow", stored.Name, "version", stored.Version)
	writeJSON(w, http.StatusOK, stored)
}

// postWorkflowVersion stores the body as the next version of the workflow in the path. The
// orchestrator sets the name, version, time, and creator; the body's values are ignored.
func (s *server) postWorkflowVersion(w http.ResponseWriter, r *http.Request) {
	var body workflowView
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	def := body.Definition
	if descriptionHasLineBreaks(def.Description) {
		s.fail(w, http.StatusBadRequest, errors.New("description must not contain line breaks"))
		return
	}
	def.Name = r.PathValue("name")
	def.Created = time.Now().UTC().Format(time.RFC3339)
	def.CreatedBy = strings.TrimSpace(r.Header.Get("X-Creator"))
	var stored workflowView
	err := s.change(func(c *Config) error {
		def.Version = 1
		if newest, ok := c.find(def.Name, 0); ok {
			def.Version = newest.Version + 1
		}
		c.Workflows = append(c.Workflows, def)
		// A reference without a version names the newest version at the time of saving.
		pipeline.PinReferences(c.Workflows)
		stored = c.view(&c.Workflows[len(c.Workflows)-1])
		return nil
	})
	if err != nil {
		s.failChange(w, err)
		return
	}
	s.logger.Info("workflow version saved", "workflow", stored.Name, "version", stored.Version)
	writeJSON(w, http.StatusCreated, stored)
}

// renameWorkflow renames every version of a workflow and rewrites the references to it in
// every stored version of every workflow. This is the one change to stored versions.
func (s *server) renameWorkflow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	old, renamed := r.PathValue("name"), body.Name
	var stored workflowView
	err := s.change(func(c *Config) error {
		candidate, ok := c.find(old, 0)
		if !ok {
			return refuse(http.StatusNotFound, "unknown workflow")
		}
		// The stored versions are valid, so this checks the new name.
		candidate.Name = renamed
		if err := pipeline.Validate(&candidate); err != nil {
			return &statusError{status: http.StatusUnprocessableEntity, err: err}
		}
		for _, other := range c.Workflows {
			if other.Name != old && strings.EqualFold(other.Name, renamed) {
				return refuse(http.StatusConflict, "a workflow named %q exists", other.Name)
			}
		}
		rename := func(ref string) string {
			if name, version, ok := pipeline.ParseWorkflowRef(ref); ok && name == old {
				return pipeline.WorkflowRef(renamed, version)
			}
			return ref
		}
		for i := range c.Workflows {
			def := &c.Workflows[i]
			if def.Name == old {
				def.Name = renamed
			}
			for j := range def.Steps {
				step := &def.Steps[j]
				step.Workflow = rename(step.Workflow)
				member := false
				for k, ref := range step.Workflows {
					step.Workflows[k] = rename(ref)
					member = member || step.Workflows[k] != ref
				}
				if !member {
					continue
				}
				// The workflow is a team member under the last segment of its name, which
				// channels and positions use; moving it to another group leaves that alone.
				from, to := pipeline.MemberName(old), pipeline.MemberName(renamed)
				if from == to {
					continue
				}
				for _, channel := range step.Channels {
					for k := 0; k < len(channel) && k < 2; k++ {
						if channel[k] == from {
							channel[k] = to
						}
					}
				}
				if position, ok := step.MemberPositions[from]; ok {
					delete(step.MemberPositions, from)
					step.MemberPositions[to] = position
				}
			}
		}
		newest, _ := c.find(renamed, 0)
		stored = c.view(&newest)
		return nil
	})
	if err != nil {
		s.failChange(w, err)
		return
	}
	s.logger.Info("workflow renamed", "from", old, "to", renamed)
	writeJSON(w, http.StatusOK, stored)
}

// deleteWorkflow removes every version of a workflow. It refuses while the newest version of
// another workflow runs it, directly or through the versions it pins.
func (s *server) deleteWorkflow(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.change(func(c *Config) error {
		if _, ok := c.find(name, 0); !ok {
			return refuse(http.StatusNotFound, "unknown workflow")
		}
		users := []string{}
		heads := pipeline.Newest(c.Workflows)
		for i := range heads {
			newest := &heads[i]
			if newest.Name == name {
				continue
			}
			for _, nested := range pipeline.NestedDefinitions(c.Workflows, newest) {
				if nested.Name == name {
					users = append(users, newest.Name)
					break
				}
			}
		}
		if len(users) > 0 {
			sort.Strings(users)
			conflict := refuse(http.StatusConflict, "workflow %q is used by: %s", name, strings.Join(users, ", "))
			conflict.workflows = users
			return conflict
		}
		kept := c.Workflows[:0]
		for _, def := range c.Workflows {
			if def.Name != name {
				kept = append(kept, def)
			}
		}
		c.Workflows = kept
		return nil
	})
	if err != nil {
		s.failChange(w, err)
		return
	}
	s.logger.Info("workflow deleted", "workflow", name)
	w.WriteHeader(http.StatusNoContent)
}
