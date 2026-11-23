// Command migrate-workflow-groups moves workflows from a config.yaml, where names have no
// group, into the blobstore, where each version is workflows/<group>/<name>/v<N>/workflow.json.
// It is run by hand, once per file:
//
//	go run ./cmd/migrate-workflow-groups -config config/config.yaml              # print the result
//	go run ./cmd/migrate-workflow-groups -config config/config.yaml -blob-url URL  # store it
//
// ECC-* goes to ecc/, ex01-* to ex09-* to examples/, EGM-* to egm/, and every other name
// without a group to default/. References to renamed workflows are rewritten. A version that
// is already stored is left alone and reported, so running the command again is safe.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

import "opensoftwarefactory/orchestrator/pipeline"

var example = regexp.MustCompile(`^ex0[1-9]-`)

// groupFor is the group a workflow without one moves to.
func groupFor(name string) string {
	switch {
	case strings.HasPrefix(name, "ECC-"):
		return "ecc"
	case strings.HasPrefix(name, "EGM-"):
		return "egm"
	case example.MatchString(name):
		return "examples"
	default:
		return "default"
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "", "config.yaml to migrate")
	blobURL := flag.String("blob-url", "", "blobstore /blobs URL to store the result in; without it the result is printed")
	flag.Parse()
	if *configPath == "" {
		return errors.New("-config is required")
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	workflows, err := migrate(data)
	if err != nil {
		return err
	}
	if *blobURL == "" {
		out, err := json.MarshalIndent(map[string]any{"workflows": workflows}, "", "  ")
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(append(out, '\n'))
		return err
	}
	store := pipeline.HTTPStore{BaseURL: strings.TrimRight(*blobURL, "/"), Client: http.DefaultClient}
	return storeAll(context.Background(), store, workflows, os.Stdout)
}

// migrate parses a configuration as the orchestrator did and moves every workflow without a
// group into one.
func migrate(data []byte) ([]pipeline.Definition, error) {
	var c struct {
		Workflows []pipeline.Definition `yaml:"workflows"`
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil && err != io.EOF {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	for i := range c.Workflows {
		if c.Workflows[i].Version == 0 {
			c.Workflows[i].Version = 1
		}
	}
	pipeline.PinReferences(c.Workflows)

	renamed := map[string]string{}
	for _, w := range c.Workflows {
		if !strings.Contains(w.Name, "/") {
			renamed[w.Name] = groupFor(w.Name) + "/" + w.Name
		}
	}
	rename := func(ref string) string {
		if name, version, ok := pipeline.ParseWorkflowRef(ref); ok && renamed[name] != "" {
			return pipeline.WorkflowRef(renamed[name], version)
		}
		return ref
	}
	for i := range c.Workflows {
		w := &c.Workflows[i]
		if to := renamed[w.Name]; to != "" {
			w.Name = to
		}
		// Team channels and member positions name a workflow member by the last segment of
		// its name, which the group does not change.
		for j := range w.Steps {
			step := &w.Steps[j]
			step.Workflow = rename(step.Workflow)
			for k, ref := range step.Workflows {
				step.Workflows[k] = rename(ref)
			}
		}
	}
	for i := range c.Workflows {
		if err := pipeline.Validate(&c.Workflows[i]); err != nil {
			return nil, err
		}
	}
	if err := pipeline.ValidateSet(c.Workflows); err != nil {
		return nil, err
	}
	return c.Workflows, nil
}

// storeAll writes every version that is not stored yet.
func storeAll(ctx context.Context, store pipeline.HTTPStore, workflows []pipeline.Definition, out io.Writer) error {
	for i := range workflows {
		w := &workflows[i]
		id := fmt.Sprintf("workflows/%s/v%d/workflow.json", w.Name, pipeline.DefinitionVersion(w))
		_, err := store.Get(ctx, id)
		if err == nil {
			if _, err = fmt.Fprintf(out, "kept    %s (already stored)\n", id); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, pipeline.ErrNotFound) {
			return fmt.Errorf("reading %s: %w", id, err)
		}
		data, err := json.MarshalIndent(w, "", "  ")
		if err != nil {
			return err
		}
		if err = store.Put(ctx, id, append(data, '\n'), pipeline.Meta{Creator: w.CreatedBy}); err != nil {
			return fmt.Errorf("storing %s: %w", id, err)
		}
		if _, err = fmt.Fprintf(out, "stored  %s\n", id); err != nil {
			return err
		}
	}
	return nil
}
