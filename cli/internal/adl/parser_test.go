package adl

import (
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParseValidYAML(t *testing.T) {
	doc, err := Parse(readFixture(t, "valid_hello.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc["apiVersion"] != "agent-controller.dev/v1alpha1" {
		t.Errorf("apiVersion = %v, want agent-controller.dev/v1alpha1", doc["apiVersion"])
	}
	if doc["kind"] != "Agent" {
		t.Errorf("kind = %v, want Agent", doc["kind"])
	}
}

func TestParseInvalidYAMLReturnsError(t *testing.T) {
	_, err := Parse(readFixture(t, "invalid_yaml.yaml"))
	if err == nil {
		t.Fatalf("Parse: expected error on malformed YAML, got nil")
	}
}
