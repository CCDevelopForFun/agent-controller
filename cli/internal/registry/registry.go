// Package registry resolves tool/extension/MCP/skill manifests by name.
//
// MVP scope: scans a root directory laid out as
//   <root>/tools/<name>/manifest.yaml
//   <root>/extensions/<name>/manifest.yaml
// Post-MVP, the registry will also resolve installed manifests from
// ~/.agent-controller/registry/ — same shape, different root.
//
// Every manifest is validated against schemas/manifest.v1.json during
// scan. Invalid manifests cause Scan to return an error and a nil index —
// surfacing the schema authority at compile time, not at first run.
package registry

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"
)

//go:embed schemas/manifest.v1.json
var manifestSchemaBytes []byte

func compileManifestSchema() (*jsonschema.Schema, error) {
	var doc any
	if err := json.Unmarshal(manifestSchemaBytes, &doc); err != nil {
		return nil, fmt.Errorf("decode embedded manifest schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("manifest.v1.json", doc); err != nil {
		return nil, fmt.Errorf("add manifest schema: %w", err)
	}
	return c.Compile("manifest.v1.json")
}

type Manifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

type Metadata struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Owner       string `json:"owner,omitempty"`
	Description string `json:"description,omitempty"`
}

type Spec struct {
	Entrypoint          string         `json:"entrypoint"`
	InputSchema         map[string]any `json:"inputSchema,omitempty"`
	OutputSchema        map[string]any `json:"outputSchema,omitempty"`
	ConfigSchema        map[string]any `json:"configSchema,omitempty"`
	Hooks               []string       `json:"hooks,omitempty"`
	RiskLevel           string         `json:"riskLevel,omitempty"`
	RequiredPermissions []string       `json:"requiredPermissions,omitempty"`
	SupportsDryRun      bool           `json:"supportsDryRun,omitempty"`
}

// ManifestIndex stores manifests keyed by (kind, name).
type ManifestIndex struct {
	entries map[string]Manifest
}

func key(kind, name string) string { return kind + "/" + name }

// Lookup returns the manifest for (kind, name), if any.
func (i *ManifestIndex) Lookup(kind, name string) (Manifest, bool) {
	m, ok := i.entries[key(kind, name)]
	return m, ok
}

// Scan walks <root>/tools, <root>/extensions, <root>/skills, and <root>/agents
// and returns an index of every manifest found.
//
// Tools and extensions: each directory must contain manifest.yaml, validated
// against the embedded manifest schema. The first invalid manifest aborts the
// scan with a descriptive error. Entrypoint paths are rewritten to absolute
// paths anchored at the manifest's directory.
//
// Skills: each directory under <root>/skills must contain SKILL.md. No
// manifest.yaml is required; the registry synthesises a Manifest with
// Kind="Skill" and Entrypoint pointing to the SKILL.md file. The directory
// name is used as the skill name. This matches Pi's loadSkillsFromDir
// discovery rule (a directory containing SKILL.md is treated as a skill root).
//
// Agents (Subagent kind): each <root>/agents/<slug>.md file is treated as an
// agent definition. No manifest.yaml is required. The registry synthesises a
// Manifest with Kind="Subagent" and Entrypoint pointing to the .md file. This
// matches Pi's subagent extension discovery rule (discoverAgents reads .md
// files from a directory).
func Scan(root string) (*ManifestIndex, error) {
	schema, err := compileManifestSchema()
	if err != nil {
		return nil, err
	}
	idx := &ManifestIndex{entries: map[string]Manifest{}}
	for _, sub := range []string{"tools", "extensions"} {
		dir := filepath.Join(root, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			mPath := filepath.Join(dir, e.Name(), "manifest.yaml")
			data, err := os.ReadFile(mPath)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("read %s: %w", mPath, err)
			}

			// Validate against the embedded schema first. Use a generic
			// map so additionalProperties/required violations surface
			// before we lose information by unmarshaling into typed Go
			// struct fields.
			var raw map[string]any
			if err := yaml.Unmarshal(data, &raw); err != nil {
				return nil, fmt.Errorf("parse %s: %w", mPath, err)
			}
			if err := schema.Validate(raw); err != nil {
				return nil, fmt.Errorf("invalid manifest %s: %w", mPath, err)
			}

			var m Manifest
			if err := yaml.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("decode %s: %w", mPath, err)
			}
			abs, err := filepath.Abs(filepath.Join(dir, e.Name(), m.Spec.Entrypoint))
			if err != nil {
				return nil, fmt.Errorf("resolve entrypoint %s: %w", mPath, err)
			}
			m.Spec.Entrypoint = abs
			idx.entries[key(m.Kind, m.Metadata.Name)] = m
		}
	}

	// Scan skills: <root>/skills/<name>/SKILL.md — no manifest.yaml required.
	skillsDir := filepath.Join(root, "skills")
	skillEntries, err := os.ReadDir(skillsDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", skillsDir, err)
		}
	} else {
		for _, e := range skillEntries {
			if !e.IsDir() {
				continue
			}
			skillMD := filepath.Join(skillsDir, e.Name(), "SKILL.md")
			if _, statErr := os.Stat(skillMD); statErr != nil {
				// No SKILL.md — not a skill root; skip.
				continue
			}
			abs, err := filepath.Abs(skillMD)
			if err != nil {
				return nil, fmt.Errorf("resolve skill path %s: %w", skillMD, err)
			}
			name := e.Name()
			m := Manifest{
				APIVersion: "agent-controller.dev/v1alpha1",
				Kind:       "Skill",
				Metadata:   Metadata{Name: name},
				Spec:       Spec{Entrypoint: abs},
			}
			idx.entries[key("Skill", name)] = m
		}
	}

	// Scan agents: <root>/agents/<slug>.md — loose .md files, no manifest.yaml required.
	// Each file is a subagent definition with YAML frontmatter (name, description, tools, model).
	agentsDir := filepath.Join(root, "agents")
	agentEntries, err := os.ReadDir(agentsDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", agentsDir, err)
		}
	} else {
		for _, e := range agentEntries {
			if e.IsDir() {
				continue
			}
			if filepath.Ext(e.Name()) != ".md" {
				continue
			}
			agentMD := filepath.Join(agentsDir, e.Name())
			abs, err := filepath.Abs(agentMD)
			if err != nil {
				return nil, fmt.Errorf("resolve agent path %s: %w", agentMD, err)
			}
			// Derive the agent name from the filename without extension.
			// e.g. "sql-explorer.md" → "sql-explorer"
			name := e.Name()[:len(e.Name())-len(filepath.Ext(e.Name()))]
			m := Manifest{
				APIVersion: "agent-controller.dev/v1alpha1",
				Kind:       "Subagent",
				Metadata:   Metadata{Name: name},
				Spec:       Spec{Entrypoint: abs},
			}
			idx.entries[key("Subagent", name)] = m
		}
	}

	return idx, nil
}
