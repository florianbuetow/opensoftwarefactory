package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"opensoftwarefactory/orchestrator/pipeline"
)

// Config is the content of config.yaml: every stored version of every workflow.
type Config struct {
	Workflows []pipeline.Definition `yaml:"workflows" json:"workflows"`
}

// parseConfig reads the configuration. A workflow without a version is v1, and a workflow
// reference without a version is pinned to the newest version of the workflow it names.
func parseConfig(data []byte) (Config, error) {
	var c Config
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil && err != io.EOF {
		return c, fmt.Errorf("invalid YAML configuration: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("configuration must contain one YAML document")
	}
	for i := range c.Workflows {
		if c.Workflows[i].Version == 0 {
			c.Workflows[i].Version = 1
		}
	}
	pipeline.PinReferences(c.Workflows)
	return c, c.validate()
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return parseConfig(data)
}

func saveConfig(path string, c Config) error {
	if err := c.validate(); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# Workflow configuration. Saved by the orchestrator web UI, which does not keep comments.\n")
	e := yaml.NewEncoder(&buf)
	e.SetIndent(2)
	if err := e.Encode(c); err != nil {
		return err
	}
	if err := e.Close(); err != nil {
		return err
	}
	return pipeline.WriteFileAtomic(path, buf.Bytes(), 0o644)
}

// seedWorkflows adds bundled workflow names that are wholly absent from the stored
// configuration. Existing names, including all their versions, always win. Seed files are
// read in name order and must each contain a valid workflow configuration.
func seedWorkflows(configPath, seedDir string) (int, error) {
	if seedDir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(seedDir)
	if err != nil {
		return 0, fmt.Errorf("reading workflow seed directory: %w", err)
	}
	stored, err := loadConfig(configPath)
	if err != nil {
		return 0, err
	}
	seeded := []pipeline.Definition{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension != ".json" && extension != ".yaml" && extension != ".yml" {
			continue
		}
		path := filepath.Join(seedDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, fmt.Errorf("reading workflow seed %s: %w", entry.Name(), err)
		}
		config, err := parseConfig(data)
		if err != nil {
			return 0, fmt.Errorf("workflow seed %s: %w", entry.Name(), err)
		}
		seeded = append(seeded, config.Workflows...)
	}

	existing := make(map[string]bool, len(stored.Workflows))
	for _, workflow := range stored.Workflows {
		existing[strings.ToLower(workflow.Name)] = true
	}
	addNames := map[string]bool{}
	for _, workflow := range seeded {
		key := strings.ToLower(workflow.Name)
		if !existing[key] {
			addNames[key] = true
		}
	}
	added := 0
	for _, workflow := range seeded {
		if addNames[strings.ToLower(workflow.Name)] {
			stored.Workflows = append(stored.Workflows, workflow)
			added++
		}
	}
	if added == 0 {
		return 0, nil
	}
	if err := stored.validate(); err != nil {
		return 0, fmt.Errorf("merging workflow seeds: %w", err)
	}
	if err := saveConfig(configPath, stored); err != nil {
		return 0, fmt.Errorf("saving workflow seeds: %w", err)
	}
	return added, nil
}

// validate checks every stored version on its own, that no two workflows have names that
// differ only in case, that no version is stored twice, and the references between them.
func (c Config) validate() error {
	names := map[string]string{}
	seen := map[string]bool{}
	for _, w := range c.Workflows {
		if err := pipeline.Validate(w); err != nil {
			return err
		}
		if w.Version < 1 {
			return fmt.Errorf("workflow %q: version must be 1 or higher", w.Name)
		}
		key := strings.ToLower(w.Name)
		if known, ok := names[key]; ok && known != w.Name {
			return fmt.Errorf("duplicate workflow name %q", w.Name)
		}
		names[key] = w.Name
		ref := pipeline.WorkflowRef(w.Name, w.Version)
		if seen[ref] {
			return fmt.Errorf("duplicate workflow name %q: version %d is stored twice", w.Name, w.Version)
		}
		seen[ref] = true
	}
	return pipeline.ValidateSet(c.Workflows)
}

// find returns the newest version of the named workflow, or the given version when it is
// above 0.
func (c Config) find(name string, version int) (pipeline.Definition, bool) {
	if version < 0 || strings.Contains(name, "@") {
		return pipeline.Definition{}, false
	}
	return pipeline.FindDefinition(c.Workflows, pipeline.WorkflowRef(name, version))
}

// versions lists the stored versions of the named workflow in ascending order.
func (c Config) versions(name string) []int {
	versions := []int{}
	for _, w := range c.Workflows {
		if w.Name == name {
			versions = append(versions, w.Version)
		}
	}
	sort.Ints(versions)
	return versions
}
