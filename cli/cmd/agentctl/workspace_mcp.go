package main

// Slice 7.5 — the workspace memory MCP server.
//
// `agentctl run --workspace <dir>` injects an mcpServers entry that runs
// THIS hidden subcommand as a stdio MCP server. Because the memory tools
// are exposed over MCP (not as harness-specific native tools), the same
// workspace works on the Pi adapter AND the opencode adapter — both
// connect to MCP servers from spec.mcpServers. agentctl is both the
// orchestratable task and its own workspace server, so there's no extra
// binary to install and the server is always version-matched to the CLI.
//
// The subcommand name is dunder-prefixed and Hidden because it's an
// internal wiring detail, not a user-facing command.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/workspace"
)

const workspaceMCPServerName = "agentctl-workspace"

// declaredBuiltinTools returns the names of Pi-builtin tools (bash, read,
// edit, write) the spec declares. Used to warn that --workspace's injected
// MCP server will cause the Pi adapter to suppress these (see the caveat
// in newRunCmd). Returns nil when none are declared.
func declaredBuiltinTools(spec *adl.CompiledSpec) []string {
	var builtins []string
	for _, tr := range spec.Tools {
		if tr.Builtin {
			builtins = append(builtins, tr.Name)
		}
	}
	return builtins
}

// injectWorkspaceMCPServer adds the workspace memory MCP server to
// spec.MCPServers (slice 7.5). The injected entry runs agentctl itself
// (`<self> __workspace-mcp --workspace <absdir>`) over stdio, so the four
// memory tools reach whichever adapter the run uses.
//
// It opens the workspace once up front to fail fast on a bad dir (and to
// create it), then closes that handle — the spawned MCP server process
// opens its own. The injected command is the ABSOLUTE path to the current
// executable so it resolves regardless of the adapter subprocess's PATH.
func injectWorkspaceMCPServer(spec *adl.CompiledSpec, workspaceDir string) error {
	// Validate the RAW flag value before filepath.Abs (codex pass 3 of
	// slice 7.5): Abs prefixes the cwd onto a `file:`/`?`-DSN value, which
	// would slip past workspace.Open's guard (it only sees the absolutized
	// path) and create a nested `<cwd>/file:/...` workspace instead of
	// failing. Reject the SQLite-URI-looking forms up front.
	if strings.HasPrefix(workspaceDir, "file:") || strings.Contains(workspaceDir, "?") {
		return fmt.Errorf(
			"--workspace %q is not a plain directory path (no `file:` prefix or `?`)", workspaceDir)
	}
	absDir, err := filepath.Abs(workspaceDir)
	if err != nil {
		return fmt.Errorf("--workspace: resolve absolute path: %w", err)
	}
	// Validate/create the workspace eagerly so a bad path fails before the
	// agent runs, not mid-session when the model first calls a tool.
	ws, err := workspace.Open(absDir)
	if err != nil {
		return fmt.Errorf("--workspace: %w", err)
	}
	_ = ws.Close()

	// The reserved server name is ours; refuse to collide with a
	// user-declared mcpServers entry rather than silently shadow it.
	for _, s := range spec.MCPServers {
		if s.Name == workspaceMCPServerName {
			return fmt.Errorf(
				"--workspace conflicts with an mcpServers entry named %q in the spec; "+
					"rename that server", workspaceMCPServerName)
		}
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("--workspace: locate agentctl executable: %w", err)
	}

	spec.MCPServers = append(spec.MCPServers, adl.MCPServer{
		Name:      workspaceMCPServerName,
		Transport: "stdio",
		// eager so the tools are advertised to the model at session start
		// (lazy would only connect on first use, after which discovery is
		// too late for the model to know the tools exist).
		Lifecycle: "eager",
		Command:   self,
		Args:      []string{"__workspace-mcp", "--workspace", absDir},
	})
	return nil
}

func newWorkspaceMCPCmd() *cobra.Command {
	var workspaceDir string
	c := &cobra.Command{
		Use:    "__workspace-mcp",
		Short:  "(internal) stdio MCP server exposing workspace memory tools",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if workspaceDir == "" {
				return fmt.Errorf("__workspace-mcp: --workspace <dir> is required")
			}
			ws, err := workspace.Open(workspaceDir)
			if err != nil {
				return fmt.Errorf("open workspace: %w", err)
			}
			defer ws.Close()

			server := newWorkspaceMCPServer(ws)
			// Run over stdio until the MCP client (the runtime adapter)
			// disconnects. cmd.Context() is canceled on SIGINT/SIGTERM by
			// cobra, so a parent teardown stops the server cleanly.
			return server.Run(cmd.Context(), &mcp.StdioTransport{})
		},
	}
	c.Flags().StringVar(&workspaceDir, "workspace", "",
		"workspace directory whose SQLite store backs the memory tools")
	return c
}

// --- tool I/O types (jsonschema struct tags drive the MCP input schema) ---

type rememberArgs struct {
	Key   string `json:"key" jsonschema:"the key to store the value under"`
	Value string `json:"value" jsonschema:"the value to remember for this key"`
}

type recallArgs struct {
	Key string `json:"key,omitempty" jsonschema:"the key to recall; omit or leave empty to list every remembered key and value"`
}

type noteArgs struct {
	Note string `json:"note" jsonschema:"the note text to append to the workspace journal (notes.md)"`
}

// listOutputsArgs has no fields — the tool takes no input. A struct (vs
// `any`) keeps the inferred schema an object, as the MCP spec requires.
type listOutputsArgs struct{}

// newWorkspaceMCPServer builds the MCP server and registers the four
// workspace tools against ws. Factored out so tests can register the
// tools without standing up a stdio transport.
func newWorkspaceMCPServer(ws *workspace.Workspace) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    workspaceMCPServerName,
		Version: version,
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "workspace_remember",
		Description: "Store a key/value pair in the shared workspace memory so a " +
			"later step in this workflow can recall it. Overwrites any existing value for the key.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in rememberArgs) (*mcp.CallToolResult, any, error) {
		if err := ws.Remember(ctx, in.Key, in.Value); err != nil {
			return toolError(err), nil, nil
		}
		return toolText(fmt.Sprintf("remembered %q", in.Key)), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "workspace_recall",
		Description: "Recall a value previously stored in the shared workspace memory. " +
			"Pass a key to get its value, or omit the key to list everything remembered.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in recallArgs) (*mcp.CallToolResult, any, error) {
		if in.Key == "" {
			all, err := ws.RecallAll(ctx)
			if err != nil {
				return toolError(err), nil, nil
			}
			if len(all) == 0 {
				return toolText("(workspace memory is empty)"), nil, nil
			}
			var b strings.Builder
			for _, kv := range all {
				fmt.Fprintf(&b, "%s = %s\n", kv.Key, kv.Value)
			}
			return toolText(strings.TrimRight(b.String(), "\n")), nil, nil
		}
		val, found, err := ws.Recall(ctx, in.Key)
		if err != nil {
			return toolError(err), nil, nil
		}
		if !found {
			return toolText(fmt.Sprintf("(no value remembered for %q)", in.Key)), nil, nil
		}
		return toolText(val), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "workspace_note_append",
		Description: "Append a timestamped note to the shared workspace journal (notes.md). " +
			"Use it to leave freeform progress/findings for later steps to read.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in noteArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.Note) == "" {
			return toolError(fmt.Errorf("note is empty")), nil, nil
		}
		if err := ws.AppendNote(in.Note); err != nil {
			return toolError(err), nil, nil
		}
		return toolText("noted"), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "workspace_list_outputs",
		Description: "List the output files in the shared workspace directory — the results " +
			"prior workflow steps wrote (e.g. via --output-file) plus the notes journal.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ listOutputsArgs) (*mcp.CallToolResult, any, error) {
		outs, err := ws.ListOutputs()
		if err != nil {
			return toolError(err), nil, nil
		}
		if len(outs) == 0 {
			return toolText("(no outputs in the workspace yet)"), nil, nil
		}
		var b strings.Builder
		for _, o := range outs {
			if o.IsDir {
				fmt.Fprintf(&b, "%s/\n", o.Name)
			} else {
				fmt.Fprintf(&b, "%s (%d bytes)\n", o.Name, o.Size)
			}
		}
		return toolText(strings.TrimRight(b.String(), "\n")), nil, nil
	})

	return server
}

// toolText wraps a plain string as a successful MCP tool result.
func toolText(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// toolError returns a tool result flagged as an error (IsError) carrying
// the message as text. We surface failures THROUGH the tool result rather
// than as a Go error so the model sees a usable message and can adapt,
// instead of the whole MCP call faulting.
func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}
