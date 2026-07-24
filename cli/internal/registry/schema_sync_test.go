package registry

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestManifestSchemaEmbedInSync asserts that the language-neutral root schema
// at `<repo>/schemas/manifest.v1.json` matches the byte-identical copy
// embedded in this package via `//go:embed schemas/manifest.v1.json` (see
// registry.go).
//
// Closes debt #2 — the two copies historically had to be synced by hand. If
// this test fails, copy the root schema over the embedded copy:
//
//	cp schemas/manifest.v1.json cli/internal/registry/schemas/manifest.v1.json
//
// The root copy is the source of truth.
func TestManifestSchemaEmbedInSync(t *testing.T) {
	// Repo root is three levels up from cli/internal/registry/.
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	rootPath := filepath.Join(repoRoot, "schemas", "manifest.v1.json")
	rootBytes, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatalf("read root schema %s: %v", rootPath, err)
	}

	if !bytes.Equal(rootBytes, manifestSchemaBytes) {
		t.Errorf(
			"manifest schema drift: schemas/manifest.v1.json and cli/internal/registry/schemas/manifest.v1.json differ.\n"+
				"Resolve by copying the root file over the embedded copy:\n"+
				"  cp schemas/manifest.v1.json cli/internal/registry/schemas/manifest.v1.json\n"+
				"(The root copy is the source of truth; never edit the embedded copy directly.)",
		)
	}
}
