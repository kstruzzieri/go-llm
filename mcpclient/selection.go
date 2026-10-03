package mcpclient

import (
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// validateSelection rejects a selection that could never name exactly one
// admitted tool. Errors identify entries by position, never by text.
func validateSelection(s Server) error {
	if len(s.tools) > maxToolsPerServer {
		return fmt.Errorf("mcpclient: tool selection exceeds %d names", maxToolsPerServer)
	}
	seen := make(map[string]bool, len(s.tools))
	for i, name := range s.tools {
		if _, ok := composeName(s.Alias, name); !ok {
			return fmt.Errorf("mcpclient: tool selection entry %d is not a valid tool name", i+1)
		}
		if seen[name] {
			return fmt.Errorf("mcpclient: tool selection entry %d repeats a name", i+1)
		}
		seen[name] = true
	}
	return nil
}

// selectRemote returns the admitted remote tools named by names, in server
// listing order, and the selected names the admitted listing lacks, in
// selection order. Remote names are unique in a validated catalog.
func selectRemote(remote []*gomcp.Tool, names []string) ([]*gomcp.Tool, []string) {
	want := make(map[string]bool, len(names))
	for _, name := range names {
		want[name] = true
	}
	out := make([]*gomcp.Tool, 0, len(names))
	for _, rt := range remote {
		if want[rt.Name] {
			out = append(out, rt)
			delete(want, rt.Name)
		}
	}
	var missing []string
	for _, name := range names {
		if want[name] {
			missing = append(missing, name)
		}
	}
	return out, missing
}
