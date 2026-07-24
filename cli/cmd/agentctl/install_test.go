package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeFakePi creates a fake "pi" shell script in dir.
// The script:
//   - prints "fake-pi install <pkg>" to stdout
//   - exits 1 if the package name contains "fail"
//   - otherwise exits 0
//
// Returns the path to the script.
func makeFakePi(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "pi")
	content := `#!/bin/sh
# fake pi: install <pkg>
# exit 1 if pkg contains "fail", else exit 0
pkg="$2"
echo "fake-pi install $pkg"
case "$pkg" in
  *fail*) exit 1 ;;
  *)      exit 0 ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	return script
}

// makeInstallsADL writes a minimal ADL YAML with the given installs list to
// a temp file and returns its path.
func makeInstallsADL(t *testing.T, dir string, installs []string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(`apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata:
  name: test-agent
spec:
  model:
    provider: anthropic
    name: claude-sonnet-4-20250514
  task: test
  tools: []
  installs:
`)
	for _, pkg := range installs {
		sb.WriteString("    - ")
		sb.WriteString(pkg)
		sb.WriteString("\n")
	}
	sb.WriteString("  runtime:\n    type: local\n")

	path := filepath.Join(dir, "test.yaml")
	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		t.Fatalf("write ADL yaml: %v", err)
	}
	return path
}

func TestInstallPositionalArgs(t *testing.T) {
	dir := t.TempDir()
	piBin := makeFakePi(t, dir)

	cmd := NewInstallCmd()
	cmd.SetArgs([]string{"--pi-bin", piBin, "npm:pkg-a", "npm:pkg-b"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}
}

func TestInstallFromADL(t *testing.T) {
	dir := t.TempDir()
	piBin := makeFakePi(t, dir)
	adlPath := makeInstallsADL(t, dir, []string{"npm:pkg-x", "npm:pkg-y"})

	cmd := NewInstallCmd()
	cmd.SetArgs([]string{"--pi-bin", piBin, "--from", adlPath})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}
}

func TestInstallFailedPackageReturnsError(t *testing.T) {
	dir := t.TempDir()
	piBin := makeFakePi(t, dir)

	cmd := NewInstallCmd()
	// "npm:will-fail" contains "fail" → fake pi exits 1
	cmd.SetArgs([]string{"--pi-bin", piBin, "npm:will-fail"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	// SilenceErrors so cobra doesn't print the error; we check it ourselves.
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected error for failing install, got nil\noutput: %s", out.String())
	}
	if !strings.Contains(err.Error(), "npm:will-fail") {
		t.Errorf("error %q should mention the failed package", err.Error())
	}
}

func TestInstallMissingPiBinaryReturnsError(t *testing.T) {
	dir := t.TempDir()
	nonexistent := filepath.Join(dir, "nonexistent-pi")

	cmd := NewInstallCmd()
	cmd.SetArgs([]string{"--pi-bin", nonexistent, "npm:pkg-a"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected error for missing pi binary, got nil")
	}
	// The error originates from exec.Command.Run() since the file doesn't exist.
	// We just verify we got an error.
}

func TestInstallMutualExclusionError(t *testing.T) {
	dir := t.TempDir()
	piBin := makeFakePi(t, dir)
	adlPath := makeInstallsADL(t, dir, []string{"npm:pkg-x"})

	cmd := NewInstallCmd()
	cmd.SetArgs([]string{"--pi-bin", piBin, "--from", adlPath, "npm:extra-pkg"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected error when both --from and positional args are provided")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q should mention 'mutually exclusive'", err.Error())
	}
}

func TestInstallNoArgsError(t *testing.T) {
	cmd := NewInstallCmd()
	cmd.SetArgs([]string{})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected error when no args provided")
	}
}

func TestResolvePiBinOverride(t *testing.T) {
	dir := t.TempDir()
	piBin := makeFakePi(t, dir)

	got, err := resolvePiBin(piBin)
	if err != nil {
		t.Fatalf("resolvePiBin: %v", err)
	}
	if got != piBin {
		t.Errorf("resolvePiBin: got %q, want %q", got, piBin)
	}
}

func TestResolvePiBinEnvVar(t *testing.T) {
	dir := t.TempDir()
	piBin := makeFakePi(t, dir)

	t.Setenv("PI_BIN", piBin)

	got, err := resolvePiBin("")
	if err != nil {
		t.Fatalf("resolvePiBin: %v", err)
	}
	if got != piBin {
		t.Errorf("resolvePiBin: got %q, want %q", got, piBin)
	}
}

// ---------------------------------------------------------------------------
// resolvePackageArgs unit tests
// ---------------------------------------------------------------------------

func TestResolvePackageArgs_NoKeyword(t *testing.T) {
	// Raw npm: args pass through unchanged.
	args := []string{"npm:pkg-a", "npm:pkg-b"}
	got := resolvePackageArgs(args)
	if len(got) != 2 || got[0] != "npm:pkg-a" || got[1] != "npm:pkg-b" {
		t.Errorf("unexpected result: %v", got)
	}
}

func TestResolvePackageArgs_ExtensionKeyword(t *testing.T) {
	got := resolvePackageArgs([]string{"extension", "pi-mcp-extension"})
	if len(got) != 1 || got[0] != "npm:pi-mcp-extension" {
		t.Errorf("expected [npm:pi-mcp-extension], got %v", got)
	}
}

func TestResolvePackageArgs_PluginKeyword(t *testing.T) {
	got := resolvePackageArgs([]string{"plugin", "pi-mcp-extension"})
	if len(got) != 1 || got[0] != "npm:pi-mcp-extension" {
		t.Errorf("expected [npm:pi-mcp-extension], got %v", got)
	}
}

func TestResolvePackageArgs_MultipleNames(t *testing.T) {
	got := resolvePackageArgs([]string{"extension", "foo", "bar"})
	if len(got) != 2 || got[0] != "npm:foo" || got[1] != "npm:bar" {
		t.Errorf("expected [npm:foo npm:bar], got %v", got)
	}
}

func TestResolvePackageArgs_AlreadyPrefixed(t *testing.T) {
	// "npm:foo" already has a scheme — must not be double-prefixed.
	got := resolvePackageArgs([]string{"extension", "npm:foo"})
	if len(got) != 1 || got[0] != "npm:foo" {
		t.Errorf("expected [npm:foo], got %v", got)
	}
}

func TestResolvePackageArgs_EmptyKeyword(t *testing.T) {
	// Kind keyword with no following names produces empty slice.
	got := resolvePackageArgs([]string{"extension"})
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// End-to-end command tests using the kind-keyword syntax
// ---------------------------------------------------------------------------

// capturePI is a helper that creates a fake pi that records the arguments it
// was invoked with by writing them to a file in dir.
func capturePI(t *testing.T, dir string) (piBin, recordFile string) {
	t.Helper()
	recordFile = filepath.Join(dir, "calls.txt")
	piBin = filepath.Join(dir, "pi")
	script := `#!/bin/sh
# Append "install <pkg>" to the record file, then succeed.
echo "install $2" >> ` + recordFile + `
exit 0
`
	if err := os.WriteFile(piBin, []byte(script), 0755); err != nil {
		t.Fatalf("write capture pi: %v", err)
	}
	return piBin, recordFile
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestInstallExtensionKeyword_SinglePackage(t *testing.T) {
	dir := t.TempDir()
	piBin, rec := capturePI(t, dir)

	cmd := NewInstallCmd()
	cmd.SetArgs([]string{"--pi-bin", piBin, "extension", "pi-mcp-extension"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	lines := readLines(t, rec)
	if len(lines) != 1 {
		t.Fatalf("expected 1 pi call, got %d: %v", len(lines), lines)
	}
	if lines[0] != "install npm:pi-mcp-extension" {
		t.Errorf("expected 'install npm:pi-mcp-extension', got %q", lines[0])
	}
}

func TestInstallExtensionKeyword_MultiplePackages(t *testing.T) {
	dir := t.TempDir()
	piBin, rec := capturePI(t, dir)

	cmd := NewInstallCmd()
	cmd.SetArgs([]string{"--pi-bin", piBin, "extension", "foo", "bar"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	lines := readLines(t, rec)
	if len(lines) != 2 {
		t.Fatalf("expected 2 pi calls, got %d: %v", len(lines), lines)
	}
	if lines[0] != "install npm:foo" {
		t.Errorf("call 0: expected 'install npm:foo', got %q", lines[0])
	}
	if lines[1] != "install npm:bar" {
		t.Errorf("call 1: expected 'install npm:bar', got %q", lines[1])
	}
}

func TestInstallExtensionKeyword_AlreadyPrefixed(t *testing.T) {
	dir := t.TempDir()
	piBin, rec := capturePI(t, dir)

	cmd := NewInstallCmd()
	cmd.SetArgs([]string{"--pi-bin", piBin, "extension", "npm:foo"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	lines := readLines(t, rec)
	if len(lines) != 1 || lines[0] != "install npm:foo" {
		t.Errorf("expected [install npm:foo], got %v", lines)
	}
}

func TestInstallPluginKeyword(t *testing.T) {
	dir := t.TempDir()
	piBin, rec := capturePI(t, dir)

	cmd := NewInstallCmd()
	cmd.SetArgs([]string{"--pi-bin", piBin, "plugin", "pi-mcp-extension"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\noutput: %s", err, out.String())
	}

	lines := readLines(t, rec)
	if len(lines) != 1 || lines[0] != "install npm:pi-mcp-extension" {
		t.Errorf("expected [install npm:pi-mcp-extension], got %v", lines)
	}
}

func TestInstallKeywordWithNoNameReturnsError(t *testing.T) {
	dir := t.TempDir()
	piBin := makeFakePi(t, dir)

	cmd := NewInstallCmd()
	cmd.SetArgs([]string{"--pi-bin", piBin, "extension"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when kind keyword has no package names")
	}
}
