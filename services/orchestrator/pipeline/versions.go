package pipeline

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// workflowRefPattern matches a workflow reference: a workflow name, or a name pinned to a
// version such as team-build@v2 or ecc/ECC-plan@v1.
var workflowRefPattern = regexp.MustCompile(`^` + workflowNameExpr + `(@v[1-9][0-9]*)?$`)

// ParseWorkflowRef splits a workflow reference into name and version: "team-build@v2" is
// ("team-build", 2). A bare name has version 0, which means the newest version. ok is false
// when the reference is neither.
func ParseWorkflowRef(ref string) (name string, version int, ok bool) {
	if !workflowRefPattern.MatchString(ref) {
		return "", 0, false
	}
	name, pinned, found := strings.Cut(ref, "@v")
	if !found {
		return name, 0, true
	}
	version, err := strconv.Atoi(pinned)
	if err != nil {
		return "", 0, false
	}
	return name, version, true
}

// WorkflowRef joins a workflow name and a version into a reference such as team-build@v2.
// Version 0 gives the bare name.
func WorkflowRef(name string, version int) string {
	if version <= 0 {
		return name
	}
	return fmt.Sprintf("%s@v%d", name, version)
}

// DefinitionVersion is the version of a definition. A definition without a version is v1.
func DefinitionVersion(d *Definition) int {
	if d.Version < 1 {
		return 1
	}
	return d.Version
}

// FindDefinition returns the definition a reference names: the pinned version, or the
// newest version for a bare name.
func FindDefinition(definitions []Definition, ref string) (Definition, bool) {
	name, version, ok := ParseWorkflowRef(ref)
	if !ok {
		return Definition{}, false
	}
	var found *Definition
	for i := range definitions {
		d := &definitions[i]
		if d.Name != name {
			continue
		}
		v := DefinitionVersion(d)
		if version > 0 && v == version {
			return *d, true
		}
		if version == 0 && (found == nil || v > DefinitionVersion(found)) {
			found = d
		}
	}
	if found == nil {
		return Definition{}, false
	}
	return *found, true
}

// Newest returns the newest version of every workflow, in the order the workflows first appear.
func Newest(definitions []Definition) []Definition {
	index := map[string]int{}
	var newest []Definition
	for j := range definitions {
		d := &definitions[j]
		i, known := index[d.Name]
		if !known {
			index[d.Name] = len(newest)
			newest = append(newest, *d)
			continue
		}
		if DefinitionVersion(d) > DefinitionVersion(&newest[i]) {
			newest[i] = *d
		}
	}
	return newest
}

// PinReferences rewrites every bare workflow reference of the definitions to the newest
// version of the workflow it names. A reference to an unknown workflow stays as it is.
func PinReferences(definitions []Definition) {
	pin := func(ref string) string {
		name, version, ok := ParseWorkflowRef(ref)
		if !ok || version > 0 {
			return ref
		}
		newest, found := FindDefinition(definitions, name)
		if !found {
			return ref
		}
		return WorkflowRef(name, DefinitionVersion(&newest))
	}
	for _, d := range definitions {
		for i := range d.Steps {
			s := &d.Steps[i]
			if s.Workflow != "" {
				s.Workflow = pin(s.Workflow)
			}
			for j, ref := range s.Workflows {
				s.Workflows[j] = pin(ref)
			}
		}
	}
}

// NestedDefinitions returns every workflow version the definition reaches through the
// workflow references of its steps and team members, each once, in the order they are
// found. A reference that names no stored version is skipped.
func NestedDefinitions(definitions []Definition, root *Definition) []Definition {
	seen := map[string]bool{WorkflowRef(root.Name, DefinitionVersion(root)): true}
	var nested []Definition
	var visit func(d *Definition)
	visit = func(d *Definition) {
		for _, ref := range nestedWorkflows(d) {
			child, ok := FindDefinition(definitions, ref[1])
			if !ok {
				continue
			}
			key := WorkflowRef(child.Name, DefinitionVersion(&child))
			if seen[key] {
				continue
			}
			seen[key] = true
			nested = append(nested, child)
			visit(&child)
		}
	}
	visit(root)
	return nested
}

// AgentRefs returns every agent reference of the definitions' steps, each once, in the
// order they are found.
func AgentRefs(definitions ...Definition) []string {
	seen := map[string]bool{}
	var refs []string
	add := func(ref string) {
		if ref != "" && !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	for i := range definitions {
		steps := definitions[i].Steps
		for j := range steps {
			add(steps[j].Agent)
			for _, ref := range steps[j].Agents {
				add(ref)
			}
		}
	}
	return refs
}
