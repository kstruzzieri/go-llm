package mcpclient

import (
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioTransport(t *testing.T) {
	tr, err := StdioServer("fs", []string{"echo", "hi"}).transport()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := tr.(*gomcp.CommandTransport); !ok {
		t.Fatalf("want *CommandTransport, got %T", tr)
	}
}

func TestStdioEmptyCommand(t *testing.T) {
	if _, err := StdioServer("fs", nil).transport(); err == nil {
		t.Fatal("empty command must error")
	}
}

func TestHTTPTransport(t *testing.T) {
	tr, err := HTTPServer("api", "https://h/mcp").transport()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	sc, ok := tr.(*gomcp.StreamableClientTransport)
	if !ok {
		t.Fatalf("want *StreamableClientTransport, got %T", tr)
	}
	if !sc.DisableStandaloneSSE {
		t.Fatal("MVP must disable the standalone SSE stream (request/response only)")
	}
	if sc.Endpoint != "https://h/mcp" {
		t.Fatalf("endpoint %q", sc.Endpoint)
	}
}

func TestHTTPEmptyEndpoint(t *testing.T) {
	if _, err := HTTPServer("api", "").transport(); err == nil {
		t.Fatal("empty endpoint must error")
	}
}
