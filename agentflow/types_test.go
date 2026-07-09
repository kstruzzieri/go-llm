package agentflow

import "testing"

func TestCommandError_Message(t *testing.T) {
	e := &CommandError{Cmd: "lock-plan", Exit: 1, Errors: []StructuredError{{Code: "schema", Message: "bad"}}}
	if got := e.Error(); got == "" || !contains(got, "lock-plan") || !contains(got, "bad") {
		t.Fatalf("Error() = %q", got)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
