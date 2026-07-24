package adl

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestADLSchemaEmbedInSync asserts that the language-neutral root schema at
// `<repo>/schemas/adl.v1alpha1.json` matches the byte-identical copy embedded
// in this package via `//go:embed schemas/adl.v1alpha1.json` (see validator.go).
//
// Closes debt #2 — the two copies historically had to be synced by hand. If
// this test fails, copy the root schema over the embedded copy:
//
//	cp schemas/adl.v1alpha1.json cli/internal/adl/schemas/adl.v1alpha1.json
//
// The root copy is the source of truth: it lives next to the manifest schema,
// is the file external consumers will discover, and is the one we link to in
// docs. The embedded copy exists so the CLI binary is self-contained.
func TestADLSchemaEmbedInSync(t *testing.T) {
	// Repo root is three levels up from cli/internal/adl/.
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	rootPath := filepath.Join(repoRoot, "schemas", "adl.v1alpha1.json")
	rootBytes, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatalf("read root schema %s: %v", rootPath, err)
	}

	// adlSchemaBytes is the //go:embed'd copy declared in validator.go in
	// this package. We compare byte-for-byte; any whitespace or formatting
	// change in the root must be mirrored exactly so jsonschema.Compile
	// behaves identically on either path.
	if !bytes.Equal(rootBytes, adlSchemaBytes) {
		t.Errorf(
			"ADL schema drift: schemas/adl.v1alpha1.json and cli/internal/adl/schemas/adl.v1alpha1.json differ.\n"+
				"Resolve by copying the root file over the embedded copy:\n"+
				"  cp schemas/adl.v1alpha1.json cli/internal/adl/schemas/adl.v1alpha1.json\n"+
				"(The root copy is the source of truth; never edit the embedded copy directly.)",
		)
	}
}

// v0.3.2: same drift check for the RuntimeBinding schema. The two copies
// (root + embedded under cli/internal/adl/schemas/) must stay byte-identical
// for the same reasons as the ADL schema check above.
func TestRuntimeBindingSchemaEmbedInSync(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	rootPath := filepath.Join(repoRoot, "schemas", "runtimebinding.v1alpha1.json")
	rootBytes, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatalf("read root schema %s: %v", rootPath, err)
	}
	if !bytes.Equal(rootBytes, runtimeBindingSchemaBytes) {
		t.Errorf(
			"RuntimeBinding schema drift: schemas/runtimebinding.v1alpha1.json and cli/internal/adl/schemas/runtimebinding.v1alpha1.json differ.\n"+
				"Resolve by copying the root file over the embedded copy:\n"+
				"  cp schemas/runtimebinding.v1alpha1.json cli/internal/adl/schemas/runtimebinding.v1alpha1.json\n"+
				"(The root copy is the source of truth; never edit the embedded copy directly.)",
		)
	}
}
