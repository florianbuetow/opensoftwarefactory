package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/temporal"
	"go.yaml.in/yaml/v3"
)

const (
	skillsService   = "skills"
	skillReadTool   = "read_skill"
	skillScriptTool = "run_skill_script"
)

var (
	skillIdentity = []string{"pod", "agent", "run", "version", "timeout_seconds"}
	skillVersion  = regexp.MustCompile(`^v[1-9]\d*$`)
)

// SkillEvidence names one skill version the agent carried and where its text is stored.
type SkillEvidence struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	SourceArtifact string `json:"source_artifact"`
}

type skillCalls struct {
	versions map[string]string
	timeout  int
}

type skillKey struct{}

func withSkills(ctx context.Context, versions map[string]string, timeout int) context.Context {
	return context.WithValue(ctx, skillKey{}, skillCalls{versions: versions, timeout: timeout})
}

func skillsOf(ctx context.Context) skillCalls {
	calls, _ := ctx.Value(skillKey{}).(skillCalls)
	return calls
}

func skillTools(ctx context.Context, configured []string) []string {
	selected := slices.DeleteFunc(slices.Clone(configured), func(ref string) bool {
		return strings.HasPrefix(ref, skillsService+"/")
	})
	if len(skillsOf(ctx).versions) != 0 {
		selected = append(selected, skillsService+"/"+skillReadTool, skillsService+"/"+skillScriptTool)
	}
	return selected
}

func (s *Service) loadSkillNote(ctx context.Context, versions map[string]string) (string, []SkillEvidence, error) {
	if len(versions) == 0 {
		return "", nil, nil
	}
	names := make([]string, 0, len(versions))
	for name := range versions {
		names = append(names, name)
	}
	slices.Sort(names)

	var note strings.Builder
	note.WriteString("\n\n## Skills\n\n")
	note.WriteString("You carry the skills listed below. Read a skill with skills__read_skill before you use it.\n")
	note.WriteString("Its text tells you which further files and scripts it has. Run a script with\n")
	note.WriteString("skills__run_skill_script.\n\n")
	evidence := make([]SkillEvidence, 0, len(names))
	for _, name := range names {
		version := versions[name]
		description, err := s.skillDescription(ctx, name, version)
		if err != nil {
			return "", nil, err
		}
		fmt.Fprintf(&note, "- %s: %s\n", name, description)
		evidence = append(evidence, SkillEvidence{
			Name: name, Version: version, SourceArtifact: "skills/" + name + "/" + version + "/SKILL.md",
		})
	}
	return strings.TrimSuffix(note.String(), "\n"), evidence, nil
}

func (s *Service) skillDescription(ctx context.Context, name, version string) (description string, err error) {
	ref := name + "@" + version
	if !validName(name) || !skillVersion.MatchString(version) {
		return "", temporal.NewNonRetryableApplicationError(fmt.Sprintf("Invalid pinned skill %q", ref), "InvalidSkill", nil)
	}
	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	endpoint := strings.TrimRight(s.SkillRegistryURL, "/") + "/skills/" + strings.Join(parts, "/") + "/" + version
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("skill registry: %w", err)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("skill registry: %w", err)
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode == http.StatusNotFound {
		return "", temporal.NewNonRetryableApplicationError(fmt.Sprintf("Skill %s was not found in the registry", ref), "SkillNotFound", nil)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("skill registry: reading %s: %s", ref, resp.Status)
	}
	var entry struct {
		Files map[string]string `json:"files"`
	}
	err = json.NewDecoder(resp.Body).Decode(&entry)
	if err != nil {
		return "", fmt.Errorf("skill registry: decoding %s: %w", ref, err)
	}
	root := strings.ReplaceAll(entry.Files["SKILL.md"], "\r\n", "\n")
	body, hasHeader := strings.CutPrefix(root, "---\n")
	header, _, hasEnd := strings.Cut(body+"\n", "\n---\n")
	var fields struct {
		Description string `yaml:"description"`
	}
	if !hasHeader || !hasEnd || yaml.Unmarshal([]byte(header), &fields) != nil || strings.TrimSpace(fields.Description) == "" {
		return "", temporal.NewNonRetryableApplicationError(fmt.Sprintf("Skill %s has invalid SKILL.md front matter", ref), "InvalidSkill", nil)
	}
	return strings.Join(strings.Fields(fields.Description), " "), nil
}

func setSkillArguments(ctx context.Context, tool string, args map[string]any) (string, error) {
	calls := skillsOf(ctx)
	name, _ := args["skill"].(string)
	version, carries := calls.versions[name]
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("factory.skill.name", name), attribute.String("factory.skill.version", version))
	if !carries {
		return fmt.Sprintf("Error: this agent does not carry skill %q. Choose a skill from the Skills section of the system prompt.", name), nil
	}
	pod := devPodOf(ctx)
	if tool == skillScriptTool && (pod == nil || pod.pod == "") {
		return "", temporal.NewNonRetryableApplicationError(
			"The workflow needs a devpod step before this step to run a skill script.", "SkillScriptNeedsDevPod", nil,
		)
	}
	if pod == nil {
		return "", fmt.Errorf("missing run identity for skill tool %q", tool)
	}
	args["pod"], args["agent"], args["run"] = pod.pod, pod.agent, pod.run
	args["version"], args["timeout_seconds"] = version, calls.timeout
	if tool == skillReadTool {
		// This argument belongs only to the script tool's input schema.
		delete(args, "timeout_seconds")
	}
	return "", nil
}

func skillWarning(route toolRoute, text string) string {
	if route.server != skillsService || route.name != skillScriptTool {
		return ""
	}
	var result struct {
		Warning string `json:"warning"`
	}
	if json.Unmarshal([]byte(text), &result) != nil {
		return ""
	}
	return result.Warning
}
