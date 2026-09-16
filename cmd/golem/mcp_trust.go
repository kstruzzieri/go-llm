package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/kstruzzieri/go-llm/mcpclient"
)

// openMCPPins keeps filesystem diagnostics from revealing credential-bearing
// paths. Detailed remote definitions are only rendered by explicit inspection.
func openMCPPins(root string) (*mcpclient.PinStore, error) {
	pins, err := mcpclient.NewPinStore(root)
	if err != nil {
		return nil, errors.New("mcp: pin store unavailable; check -root and the user data directory")
	}
	return pins, nil
}

func runMCPTrust(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "inspect" && args[0] != "approve") {
		return errors.New("usage: golem mcp inspect|approve [-root .] -mcp-stdio 'alias=command args'|-mcp-http 'alias=https://endpoint' [-digest sha256:…]")
	}
	action := args[0]
	fs := flag.NewFlagSet("golem mcp "+action, flag.ContinueOnError)
	// flag parser errors may echo arbitrary supplied values, including secrets.
	fs.SetOutput(io.Discard)
	root := fs.String("root", ".", "workspace root")
	var stdio, httpFlags stringSliceFlag
	fs.Var(&stdio, "mcp-stdio", "one explicitly aliased stdio server")
	fs.Var(&httpFlags, "mcp-http", "one explicitly aliased HTTP server")
	digest := ""
	if action == "approve" {
		fs.StringVar(&digest, "digest", "", "exact candidate digest from inspect")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fs.PrintDefaults()
			return flag.ErrHelp
		}
		return errors.New("mcp: invalid command flags")
	}
	if strings.TrimSpace(*root) == "" {
		return errors.New("mcp: -root must not be empty")
	}
	if fs.NArg() != 0 || len(stdio)+len(httpFlags) != 1 {
		return errors.New("mcp: exactly one -mcp-stdio or -mcp-http server is required")
	}
	spec := ""
	if len(stdio) == 1 {
		spec = stdio[0]
	} else {
		spec = httpFlags[0]
	}
	if alias, _ := splitAlias(strings.TrimSpace(spec)); alias == "" {
		return errors.New("mcp: explicit alias= is required; use the alias from the startup notice")
	}
	servers, err := parseMCPServers(stdio, httpFlags)
	if err != nil {
		return errors.New("mcp: invalid server specification")
	}
	pins, err := openMCPPins(*root)
	if err != nil {
		return err
	}
	var inspection *mcpclient.Inspection
	if action == "inspect" {
		inspection, err = mcpclient.Inspect(ctx, mcpClientImpl(), servers[0], pins)
	} else {
		inspection, err = mcpclient.Approve(ctx, mcpClientImpl(), servers[0], pins, digest)
	}
	if err != nil {
		return err
	}
	if action == "inspect" {
		_, err = fmt.Fprint(stdout, inspection.String())
		return err
	}
	line := fmt.Sprintf("mcp: approved server %q %s", inspection.Alias, inspection.CandidateDigest)
	if diff := inspection.Diff.String(); diff != "" {
		line += "; " + diff
	}
	_, err = fmt.Fprintln(stderr, line)
	return err
}

// connectMCP opens the workspace store before any provider preparation. A store
// failure blocks each alias; ordinary REPL startup can retain its local tools.
func connectMCP(ctx context.Context, root string, servers []mcpclient.Server, requirePinned bool) (*mcpclient.Manager, []error, error) {
	if len(servers) == 0 {
		return nil, nil, nil
	}
	pins, err := openMCPPins(root)
	if err != nil {
		failures := make([]error, 0, len(servers))
		for _, server := range servers {
			failures = append(failures, &mcpclient.AdmissionError{Alias: server.Alias, Reason: "pin_unavailable"})
		}
		return nil, failures, nil
	}
	return mcpclient.Connect(ctx, mcpClientImpl(), servers, mcpclient.ConnectOptions{Pins: pins, RequirePinned: requirePinned})
}

func mcpBlockedAliases(warnings []error) []string {
	var blocked []string
	for _, warning := range warnings {
		var rejection *mcpclient.AdmissionError
		if errors.As(warning, &rejection) {
			blocked = append(blocked, fmt.Sprintf("%s (%s)", rejection.Alias, rejection.Reason))
		}
	}
	return blocked
}
