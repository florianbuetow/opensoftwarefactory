package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"opensoftwarefactory/orchestrator/pipeline"
)

// Workflows live in the blobstore, one object per version:
// workflows/<name>/v<N>/workflow.json, where the name may hold groups (ecc/ECC-plan). A
// directory that holds version directories is a workflow; any other directory is a group.
const (
	workflowPrefix = "workflows"
	workflowFile   = "workflow.json"
)

var versionDir = regexp.MustCompile(`^v([1-9][0-9]*)$`)

// workflowBlobs is the blobstore access the workflow store needs.
type workflowBlobs interface {
	pipeline.Store
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, prefix string) (dirs []string, objects []string, err error)
}

// workflowStore holds every stored version of every workflow. The orchestrator is the only
// writer, so it reads the blobstore once and keeps the configuration in memory.
type workflowStore struct {
	blobs  workflowBlobs
	mu     sync.Mutex
	config Config
}

// workflowID is the object ID of one stored workflow version.
func workflowID(name string, version int) string {
	return fmt.Sprintf("%s/%s/v%d/%s", workflowPrefix, name, version, workflowFile)
}

// openWorkflowStore reads every workflow version from the blobstore.
func openWorkflowStore(ctx context.Context, blobs workflowBlobs) (*workflowStore, error) {
	s := &workflowStore{blobs: blobs}
	if err := s.walk(ctx, ""); err != nil {
		return nil, err
	}
	pipeline.PinReferences(s.config.Workflows)
	if err := s.config.validate(); err != nil {
		return nil, fmt.Errorf("stored workflows: %w", err)
	}
	return s, nil
}

// openWorkflowStoreWithRetry waits up to wait for the blobstore to answer.
func openWorkflowStoreWithRetry(ctx context.Context, blobs workflowBlobs, logger *slog.Logger, wait time.Duration) (*workflowStore, error) {
	deadline := time.Now().Add(wait)
	for {
		s, err := openWorkflowStore(ctx, blobs)
		if err == nil || time.Now().After(deadline) {
			return s, err
		}
		logger.Warn("loading workflows failed, retrying", "error", err)
		time.Sleep(2 * time.Second)
	}
}

// walk reads the workflows under workflows/<path>.
func (s *workflowStore) walk(ctx context.Context, path string) error {
	prefix := workflowPrefix
	if path != "" {
		prefix += "/" + path
	}
	dirs, _, err := s.blobs.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("listing %s: %w", prefix, err)
	}
	for _, dir := range dirs {
		match := versionDir.FindStringSubmatch(dir)
		if match == nil {
			child := dir
			if path != "" {
				child = path + "/" + dir
			}
			if err := s.walk(ctx, child); err != nil {
				return err
			}
			continue
		}
		if path == "" {
			return fmt.Errorf("%s/%s: a version directory needs a workflow name", workflowPrefix, dir)
		}
		version, _ := strconv.Atoi(match[1])
		id := workflowID(path, version)
		data, err := s.blobs.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("reading %s: %w", id, err)
		}
		var def pipeline.Definition
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err = d.Decode(&def); err != nil {
			return fmt.Errorf("reading %s: %w", id, err)
		}
		// The path names the version; the object cannot move it elsewhere.
		def.Name, def.Version = path, version
		s.config.Workflows = append(s.config.Workflows, def)
	}
	return nil
}

// snapshot returns a copy of the configuration that the caller may change.
func (s *workflowStore) snapshot() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneConfig(s.config)
}

// update applies edit to a copy of the configuration and stores what changed: new and
// changed versions are written before removed ones are deleted, so a moved workflow is never
// lost. An edit that returns an error stores nothing.
func (s *workflowStore) update(ctx context.Context, edit func(c *Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := cloneConfig(s.config)
	if err != nil {
		return err
	}
	if err = edit(&next); err != nil {
		return err
	}
	before, err := encodeVersions(s.config)
	if err != nil {
		return err
	}
	after, err := encodeVersions(next)
	if err != nil {
		return err
	}
	for _, def := range next.Workflows {
		id := workflowID(def.Name, pipeline.DefinitionVersion(def))
		if old, ok := before[id]; ok && bytes.Equal(old, after[id]) {
			continue
		}
		if err = s.blobs.Put(ctx, id, after[id], pipeline.Meta{Creator: def.CreatedBy}); err != nil {
			return fmt.Errorf("storing %s: %w", id, err)
		}
	}
	for id := range before {
		if _, kept := after[id]; kept {
			continue
		}
		if err = s.blobs.Delete(ctx, id); err != nil {
			return fmt.Errorf("deleting %s: %w", id, err)
		}
	}
	s.config = next
	return nil
}

// encodeVersions returns every version of c as its stored object, keyed by object ID.
func encodeVersions(c Config) (map[string][]byte, error) {
	out := make(map[string][]byte, len(c.Workflows))
	for _, def := range c.Workflows {
		data, err := json.MarshalIndent(def, "", "  ")
		if err != nil {
			return nil, err
		}
		out[workflowID(def.Name, pipeline.DefinitionVersion(def))] = append(data, '\n')
	}
	return out, nil
}

func cloneConfig(c Config) (Config, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return Config{}, err
	}
	var out Config
	if err = json.Unmarshal(data, &out); err != nil {
		return Config{}, err
	}
	if out.Workflows == nil {
		out.Workflows = []pipeline.Definition{}
	}
	return out, nil
}
