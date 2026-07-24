package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
)

// kindKeywords is the set of positional "kind" prefixes recognised by
// `agentctl install <kind> <name>...`.  They are semantic sugar that signals
// intent without changing what `pi install` actually does — each name is
// prefixed with "npm:" (if not already) and dispatched to the normal handler.
var kindKeywords = map[string]bool{
	"extension": true,
	"plugin":    true,
	"skill":     true,
	"tool":      true,
	"mcp":       true,
}

// resolvePackageArgs handles the optional kind-prefix syntax:
//
//	agentctl install extension pi-mcp-extension
//	agentctl install plugin  foo bar
//
// If args[0] is a kind keyword, every subsequent arg is treated as a bare
// package name and prefixed with "npm:" when it doesn't already carry a
// scheme.  Otherwise args is returned unchanged (the raw npm:... path).
func resolvePackageArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	if !kindKeywords[args[0]] {
		return args
	}
	names := args[1:]
	packages := make([]string, 0, len(names))
	for _, name := range names {
		if strings.Contains(name, ":") {
			// Already has a scheme (e.g. npm:foo) — keep as-is.
			packages = append(packages, name)
		} else {
			packages = append(packages, "npm:"+name)
		}
	}
	return packages
}

// NewInstallCmd returns the `agentctl install` cobra command.
func NewInstallCmd() *cobra.Command {
	var fromFile string
	var piBinOverride string

	c := &cobra.Command{
		Use:          "install [extension|plugin|skill|tool|mcp <name>... | npm:<name>...]",
		Short:        "Install npm packages via pi install",
		SilenceUsage: true,
		Long: `Install one or more npm packages using the pi coding-agent binary.

Kind keywords (extension, plugin, skill, tool, mcp) are semantic sugar: they
prefix the package name(s) with "npm:" and dispatch to the same handler.

Examples:
  agentctl install extension pi-mcp-extension
  agentctl install plugin pi-mcp-extension
  agentctl install npm:pi-mcp-extension
  agentctl install npm:pi-mcp-extension npm:some-other-package
  agentctl install --from examples/with-installs.yaml
  agentctl install --pi-bin /path/to/pi npm:pi-mcp-extension`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Mutual exclusion: --from and positional args.
			if fromFile != "" && len(args) > 0 {
				return fmt.Errorf("--from and positional package arguments are mutually exclusive")
			}
			if fromFile == "" && len(args) == 0 {
				return fmt.Errorf("provide --from <adl.yaml> or at least one npm:<name> argument")
			}

			piBin, err := resolvePiBin(piBinOverride)
			if err != nil {
				return err
			}

			var packages []string
			if fromFile != "" {
				packages, err = installsFromADL(fromFile)
				if err != nil {
					return err
				}
				if len(packages) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "no installs declared in spec.installs — nothing to do")
					return nil
				}
			} else {
				packages = resolvePackageArgs(args)
				if len(packages) == 0 {
					return fmt.Errorf("kind keyword %q given but no package names follow it", args[0])
				}
			}

			return runInstalls(piBin, packages)
		},
	}
	c.Flags().StringVar(&fromFile, "from", "", "ADL YAML file to read spec.installs[] from")
	c.Flags().StringVar(&piBinOverride, "pi-bin", "", "path to pi binary (overrides PI_BIN env var and auto-detection)")
	return c
}

// resolvePiBin resolves the path to the pi binary using the following precedence:
//  1. --pi-bin flag (override passed in)
//  2. PI_BIN env var
//  3. <cwd>/runtime/node_modules/.bin/pi
//  4. System pi on PATH
//  5. Error with remediation hint
func resolvePiBin(override string) (string, error) {
	if override != "" {
		return override, nil
	}

	if env := os.Getenv("PI_BIN"); env != "" {
		return env, nil
	}

	wd, _ := os.Getwd()
	local := filepath.Join(wd, "runtime", "node_modules", ".bin", "pi")
	if _, err := os.Stat(local); err == nil {
		return local, nil
	}

	if path, err := exec.LookPath("pi"); err == nil {
		return path, nil
	}

	return "", fmt.Errorf(
		"pi binary not found\n\n" +
			"Tried:\n" +
			"  1. --pi-bin flag\n" +
			"  2. PI_BIN environment variable\n" +
			"  3. <cwd>/runtime/node_modules/.bin/pi\n" +
			"  4. 'pi' on system PATH\n\n" +
			"Remediation: install @earendil-works/pi-coding-agent, set PI_BIN, or use --pi-bin",
	)
}

// installsFromADL parses and validates the ADL YAML at path and returns
// the spec.installs[] array.
func installsFromADL(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	doc, err := adl.Parse(data)
	if err != nil {
		return nil, err
	}
	v, err := adl.NewValidator()
	if err != nil {
		return nil, err
	}
	if err := v.Validate(doc); err != nil {
		return nil, err
	}
	// As of v0.3.2 the validator accepts kind: RuntimeBinding too. Gate
	// install --from to Agent kinds only — RuntimeBindings don't carry
	// spec.installs[]. Codex pass 1 of slice 3.2 caught the Agent-only
	// regression at the compile/run site; mirror the check here.
	if kind, _ := doc["kind"].(string); kind != "Agent" {
		return nil, fmt.Errorf(
			"agentctl install --from requires kind: Agent (got kind: %q from %s); "+
				"RuntimeBinding documents do not carry spec.installs[]",
			kind, path,
		)
	}

	spec, _ := doc["spec"].(map[string]any)
	rawInstalls, _ := spec["installs"].([]any)
	var packages []string
	for _, raw := range rawInstalls {
		if pkg, ok := raw.(string); ok {
			packages = append(packages, pkg)
		}
	}
	return packages, nil
}

// runInstalls runs `pi install <pkg>` for each package sequentially.
// It streams pi's stdout/stderr directly to our own. After attempting all
// installs it aggregates any failures and returns a combined error.
func runInstalls(piBin string, packages []string) error {
	var failed []string
	for _, pkg := range packages {
		fmt.Printf("==> pi install %s\n", pkg)
		cmd := exec.Command(piBin, "install", pkg)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "error: pi install %s failed: %v\n", pkg, err)
			failed = append(failed, pkg)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d install(s) failed: %v", len(failed), failed)
	}
	return nil
}
