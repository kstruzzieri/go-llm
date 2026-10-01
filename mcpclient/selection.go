package mcpclient

import (
	"errors"
	"fmt"
)

// validateSelection rejects a selection that could never name exactly one
// admitted tool. Errors identify entries by position, never by text.
func validateSelection(s Server) error {
	if len(s.tools) > maxToolsPerServer {
		return errors.New("mcpclient: tool selection exceeds 128 names")
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
