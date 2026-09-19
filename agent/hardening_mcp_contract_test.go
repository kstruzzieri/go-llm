package agent_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/mcpclient"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func runMCPTrustContract(t *testing.T) {
	t.Run("ZT-603_#432_MCP_description_catalog_trust", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		pins, err := mcpclient.NewPinStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		srv := gomcp.NewServer(&gomcp.Implementation{Name: "contract", Version: "1"}, nil)
		var invokes atomic.Int32
		set := func(description string) {
			srv.AddTool(&gomcp.Tool{Name: "read", Description: description, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
				invokes.Add(1)
				return &gomcp.CallToolResult{}, nil
			})
		}
		set("Read\nfile")
		httpServer := httptest.NewServer(gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, nil))
		t.Cleanup(httpServer.Close)
		server := mcpclient.HTTPServer("fs", httpServer.URL)
		impl := mcpclient.Implementation{Name: "contract", Version: "1"}
		connect := func(wantTools int, requirePinned bool) (*mcpclient.Manager, []error) {
			t.Helper()
			mgr, warns, err := mcpclient.Connect(t.Context(), impl, []mcpclient.Server{server}, mcpclient.ConnectOptions{Pins: pins, RequirePinned: requirePinned})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = mgr.Close() })
			if len(mgr.Tools()) != wantTools {
				t.Fatalf("tools=%d, want %d; %v", len(mgr.Tools()), wantTools, warns)
			}
			return mgr, warns
		}
		mgr, warns := connect(1, false)
		if len(warns) != 1 || mgr.Tools()[0].Spec().Description != "Read file" || mgr.Tools()[0].(agent.OriginTool).Origin() != agent.OriginForeign {
			t.Fatalf("normalization/provenance: %v %v", mgr.Tools(), warns)
		}
		if err := mgr.Close(); err != nil {
			t.Fatal(err)
		}
		set("Read\nfiles")
		mgr, warns = connect(0, true)
		var rejection *mcpclient.AdmissionError
		if len(warns) != 1 || !errors.As(warns[0], &rejection) || rejection.Reason != "catalog_changed" || rejection.PinnedDigest != "sha256:d076c0d77d90e89d7022d158c501320cb52ff0acfb18157506f397b195feb99e" || rejection.CandidateDigest != "sha256:5ed6cdea197afcbc274e95c7b9eb7fa76263b49fb300dd5a43409054e2ae9bf3" {
			t.Fatalf("mismatch=%v", warns)
		}
		if err := mgr.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := mcpclient.Approve(t.Context(), impl, server, pins, "sha256:d076c0d77d90e89d7022d158c501320cb52ff0acfb18157506f397b195feb99e"); err == nil {
			t.Fatal("stale digest approved")
		}
		approval, err := mcpclient.Approve(t.Context(), impl, server, pins, "sha256:5ed6cdea197afcbc274e95c7b9eb7fa76263b49fb300dd5a43409054e2ae9bf3")
		if err != nil {
			t.Fatal(err)
		}
		if approval.Diff.String() != "changed: mcp__fs__read (description)" {
			t.Fatalf("approved diff=%s", approval.Diff)
		}
		mgr, warns = connect(1, true)
		if len(warns) != 0 || mgr.Tools()[0].Spec().Description != "Read files" {
			t.Fatalf("approved catalog: %v", warns)
		}
		if err := mgr.Close(); err != nil {
			t.Fatal(err)
		}
		if invokes.Load() != 0 {
			t.Fatal("catalog admission invoked tools")
		}
		for range srv.Sessions() {
			t.Fatal("catalog admission left session open")
		}
	})
}
