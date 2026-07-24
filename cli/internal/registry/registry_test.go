package registry

import (
	"path/filepath"
	"testing"
)

func TestScanFindsToolAndExtension(t *testing.T) {
	// Use the registry_good fixture (no bad manifest in the tree).
	root, err := filepath.Abs("testdata/registry_good")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	tool, ok := idx.Lookup("Tool", "get_time")
	if !ok {
		t.Fatalf("Lookup Tool/get_time: not found")
	}
	if tool.Metadata.Name != "get_time" {
		t.Errorf("name = %s, want get_time", tool.Metadata.Name)
	}
	if !filepath.IsAbs(tool.Spec.Entrypoint) {
		t.Errorf("entrypoint should be resolved to absolute path, got %s", tool.Spec.Entrypoint)
	}
	if _, ok := idx.Lookup("Extension", "audit-log"); !ok {
		t.Errorf("Lookup Extension/audit-log: not found")
	}
}

func TestScanFindsSkill(t *testing.T) {
	root, err := filepath.Abs("testdata/registry_good")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	skill, ok := idx.Lookup("Skill", "example-time-skill")
	if !ok {
		t.Fatalf("Lookup Skill/example-time-skill: not found")
	}
	if skill.Kind != "Skill" {
		t.Errorf("kind = %s, want Skill", skill.Kind)
	}
	if skill.Metadata.Name != "example-time-skill" {
		t.Errorf("name = %s, want example-time-skill", skill.Metadata.Name)
	}
	if !filepath.IsAbs(skill.Spec.Entrypoint) {
		t.Errorf("entrypoint should be absolute, got %s", skill.Spec.Entrypoint)
	}
	if filepath.Base(skill.Spec.Entrypoint) != "SKILL.md" {
		t.Errorf("entrypoint should point to SKILL.md, got %s", skill.Spec.Entrypoint)
	}
}

func TestScanFindsSubagent(t *testing.T) {
	root, err := filepath.Abs("testdata/registry_good")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	agent, ok := idx.Lookup("Subagent", "sql-explorer")
	if !ok {
		t.Fatalf("Lookup Subagent/sql-explorer: not found")
	}
	if agent.Kind != "Subagent" {
		t.Errorf("kind = %s, want Subagent", agent.Kind)
	}
	if agent.Metadata.Name != "sql-explorer" {
		t.Errorf("name = %s, want sql-explorer", agent.Metadata.Name)
	}
	if !filepath.IsAbs(agent.Spec.Entrypoint) {
		t.Errorf("entrypoint should be absolute, got %s", agent.Spec.Entrypoint)
	}
	if filepath.Base(agent.Spec.Entrypoint) != "sql-explorer.md" {
		t.Errorf("entrypoint should point to sql-explorer.md, got %s", agent.Spec.Entrypoint)
	}
}

func TestLookupMissingReturnsFalse(t *testing.T) {
	root, _ := filepath.Abs("testdata/registry")
	idx, err := Scan(root)
	if err == nil {
		// bad_tool fixture is invalid; if Scan ever stops returning an
		// error for it we need to revisit Step 4's validation logic.
		t.Fatalf("expected validation error from bad_tool manifest")
	}
	if idx != nil {
		t.Errorf("expected nil index on validation failure")
	}
}

func TestLookupMissingAfterRemovingBadFixture(t *testing.T) {
	// Repeat the success-path lookup but without the bad fixture in the tree.
	root, _ := filepath.Abs("testdata/registry_good")
	idx, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if _, ok := idx.Lookup("Tool", "no_such_tool"); ok {
		t.Errorf("expected not found")
	}
}
