package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/workspace"
)

// connectWorkspaceMCP stands up the workspace MCP server over an in-memory
// transport and returns a connected client session for calling tools.
func connectWorkspaceMCP(t *testing.T, ws *workspace.Workspace) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := newWorkspaceMCPServer(ws)
	serverT, clientT := mcp.NewInMemoryTransports()

	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestWorkspaceMCPListsFourTools(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	cs := connectWorkspaceMCP(t, ws)

	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range lt.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{"workspace_remember", "workspace_recall", "workspace_note_append", "workspace_list_outputs"} {
		if !got[want] {
			t.Errorf("missing tool %q (have %v)", want, got)
		}
	}
}

func TestWorkspaceMCPRememberThenRecall(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	cs := connectWorkspaceMCP(t, ws)

	if txt, isErr := callText(t, cs, "workspace_remember", map[string]any{"key": "topic", "value": "AI"}); isErr {
		t.Fatalf("remember errored: %s", txt)
	}
	txt, isErr := callText(t, cs, "workspace_recall", map[string]any{"key": "topic"})
	if isErr {
		t.Fatalf("recall errored: %s", txt)
	}
	if txt != "AI" {
		t.Errorf("recall got %q, want AI", txt)
	}
}

func TestWorkspaceMCPRecallAllWhenKeyOmitted(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	cs := connectWorkspaceMCP(t, ws)
	_, _ = callText(t, cs, "workspace_remember", map[string]any{"key": "a", "value": "1"})
	_, _ = callText(t, cs, "workspace_remember", map[string]any{"key": "b", "value": "2"})

	txt, _ := callText(t, cs, "workspace_recall", map[string]any{})
	if !strings.Contains(txt, "a = 1") || !strings.Contains(txt, "b = 2") {
		t.Errorf("recall-all got %q", txt)
	}
}

func TestWorkspaceMCPRecallMissingKeyIsNotAnError(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	cs := connectWorkspaceMCP(t, ws)
	txt, isErr := callText(t, cs, "workspace_recall", map[string]any{"key": "ghost"})
	if isErr {
		t.Errorf("recall of a missing key should not be a tool error; got %q", txt)
	}
	if !strings.Contains(txt, "no value remembered") {
		t.Errorf("expected a not-found message, got %q", txt)
	}
}

func TestWorkspaceMCPNoteAppendAndListOutputs(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	cs := connectWorkspaceMCP(t, ws)

	if txt, isErr := callText(t, cs, "workspace_note_append", map[string]any{"note": "found something"}); isErr {
		t.Fatalf("note_append errored: %s", txt)
	}
	// note_append creates notes.md → it should show in list_outputs, while
	// the internal .agentctl-workspace.db must NOT.
	txt, isErr := callText(t, cs, "workspace_list_outputs", map[string]any{})
	if isErr {
		t.Fatalf("list_outputs errored: %s", txt)
	}
	if !strings.Contains(txt, "notes.md") {
		t.Errorf("list_outputs should include notes.md; got %q", txt)
	}
	if strings.Contains(txt, workspace.DBFileName) {
		t.Errorf("list_outputs leaked the internal DB file; got %q", txt)
	}
}

// k8sBinding is a minimal valid kubernetes RuntimeBinding for exercising
// the --workspace K8s rejection (we never reach the backend, so the
// image/secret values just need to validate).
const k8sBinding = `apiVersion: agent-controller.dev/v1alpha1
kind: RuntimeBinding
metadata:
  name: k8s-test
spec:
  selector:
    runtimeType: local
    capabilities: {}
  target:
    type: kubernetes
    kubernetes:
      namespace: default
      image: ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.5
      secretRef:
        name: anthropic-creds
        keys: [ANTHROPIC_API_KEY]
`

func TestInjectWorkspaceMCPServerAppendsEntry(t *testing.T) {
	dir := t.TempDir()
	spec := &adl.CompiledSpec{}
	if err := injectWorkspaceMCPServer(spec, dir); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if len(spec.MCPServers) != 1 {
		t.Fatalf("expected 1 mcpServer, got %d", len(spec.MCPServers))
	}
	s := spec.MCPServers[0]
	if s.Name != workspaceMCPServerName {
		t.Errorf("name = %q", s.Name)
	}
	if s.Transport != "stdio" || s.Lifecycle != "eager" {
		t.Errorf("transport/lifecycle = %q/%q", s.Transport, s.Lifecycle)
	}
	if s.Command == "" {
		t.Error("command should be the agentctl executable path")
	}
	// args: __workspace-mcp --workspace <absdir>
	if len(s.Args) != 3 || s.Args[0] != "__workspace-mcp" || s.Args[1] != "--workspace" {
		t.Errorf("unexpected args: %v", s.Args)
	}
	if !filepath.IsAbs(s.Args[2]) {
		t.Errorf("workspace arg should be absolute, got %q", s.Args[2])
	}
	// The dir + its DB should now exist (eager open).
	if _, err := os.Stat(filepath.Join(dir, workspace.DBFileName)); err != nil {
		t.Errorf("workspace db not created: %v", err)
	}
}

func TestInjectWorkspaceMCPServerRejectsNameCollision(t *testing.T) {
	spec := &adl.CompiledSpec{
		MCPServers: []adl.MCPServer{{Name: workspaceMCPServerName, Transport: "stdio"}},
	}
	err := injectWorkspaceMCPServer(spec, t.TempDir())
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	if !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("error should explain the collision; got %q", err.Error())
	}
}

func TestInjectWorkspaceMCPServerRejectsURILikeValues(t *testing.T) {
	// codex pass 3 P3: reject file:/?-DSN forms BEFORE filepath.Abs, which
	// would otherwise prefix the cwd and bypass workspace.Open's guard.
	for _, bad := range []string{"file:/tmp/ws", "/tmp/ws?cache=shared"} {
		if err := injectWorkspaceMCPServer(&adl.CompiledSpec{}, bad); err == nil {
			t.Errorf("expected rejection for %q, got nil", bad)
		}
	}
}

func TestInjectWorkspaceMCPServerPreservesExistingServers(t *testing.T) {
	spec := &adl.CompiledSpec{
		MCPServers: []adl.MCPServer{{Name: "other", Transport: "stdio", Command: "x"}},
	}
	if err := injectWorkspaceMCPServer(spec, t.TempDir()); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if len(spec.MCPServers) != 2 || spec.MCPServers[0].Name != "other" {
		t.Errorf("existing servers not preserved: %+v", spec.MCPServers)
	}
}

func TestRunRejectsWorkspaceWithKubernetesTarget(t *testing.T) {
	// The K8s rejection lives in RunE (before backend selection). Exercise
	// it end-to-end: a kubernetes binding + --workspace must error before
	// any KubernetesBackend is constructed.
	spec := writeTemp(t, "agent.yaml", minimalAgentSpec)
	binding := writeTemp(t, "binding.yaml", k8sBinding)
	cmd := newRunCmd()
	cmd.SetArgs([]string{spec, "--binding", binding, "--workspace", t.TempDir()})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected --workspace + kubernetes target to error, got nil")
	}
	if !strings.Contains(err.Error(), "kubernetes") || !strings.Contains(err.Error(), "workspace") {
		t.Errorf("error should explain the k8s/workspace incompatibility; got %q", err.Error())
	}
}

func TestDeclaredBuiltinTools(t *testing.T) {
	spec := &adl.CompiledSpec{
		Tools: []adl.ResolvedRef{
			{Name: "bash", Builtin: true},
			{Name: "my-custom", Entrypoint: "/x/index.js"}, // not builtin
			{Name: "read", Builtin: true},
		},
	}
	got := declaredBuiltinTools(spec)
	if len(got) != 2 || got[0] != "bash" || got[1] != "read" {
		t.Errorf("got %v, want [bash read]", got)
	}
	// No builtins → nil.
	if b := declaredBuiltinTools(&adl.CompiledSpec{Tools: []adl.ResolvedRef{{Name: "c", Entrypoint: "/x"}}}); b != nil {
		t.Errorf("expected nil for no builtins, got %v", b)
	}
}

func TestRunRejectsEmptyWorkspaceFlag(t *testing.T) {
	// An explicit `--workspace ""` (a wrapper expanding an unset var) must
	// error, not silently run without the requested memory. Codex pass 5.
	spec := writeTemp(t, "agent.yaml", minimalAgentSpec)
	cmd := newRunCmd()
	cmd.SetArgs([]string{spec, "--workspace", ""})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for empty --workspace, got nil")
	}
	if !strings.Contains(err.Error(), "empty path") {
		t.Errorf("error should mention the empty path; got %q", err.Error())
	}
}

func TestWorkspaceMCPEmptyNoteIsToolError(t *testing.T) {
	ws, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	cs := connectWorkspaceMCP(t, ws)
	_, isErr := callText(t, cs, "workspace_note_append", map[string]any{"note": "   "})
	if !isErr {
		t.Error("an empty/whitespace note should be reported as a tool error")
	}
}
