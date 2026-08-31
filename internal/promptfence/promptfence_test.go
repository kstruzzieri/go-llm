package promptfence

import (
	"strings"
	"testing"
)

func TestNewIsUnguessablePerRender(t *testing.T) {
	seen := make(map[string]bool, 128)
	for i := 0; i < 128; i++ {
		id := New().ID()
		if len(id) != idLen {
			t.Fatalf("id %q has length %d, want %d", id, len(id), idLen)
		}
		if seen[id] {
			t.Fatalf("id %q repeated across renders; a reused fence is forgeable", id)
		}
		seen[id] = true
	}
}

func TestFenceMarkersCarryTheID(t *testing.T) {
	f := New()
	open, closing, lead := f.Open("EVIDENCE"), f.Close("EVIDENCE"), f.Lead("E1")
	for name, got := range map[string]string{"Open": open, "Close": closing, "Lead": lead} {
		if !strings.Contains(got, f.ID()) {
			t.Errorf("%s() = %q, must carry the fence id %q or it is forgeable", name, got, f.ID())
		}
	}
	if open == closing {
		t.Error("Open and Close must differ or content can forge the terminator")
	}
	// The open marker is what tells the model the region is data, not instructions.
	if !strings.Contains(strings.ToLower(open), "untrusted") {
		t.Errorf("Open() = %q, want the untrusted-data spotlight clause", open)
	}
}

// TestMarkersOccupyExactlyOneLine keeps a marker from being split across lines,
// which would let content sit at a line start inside the region.
func TestMarkersOccupyExactlyOneLine(t *testing.T) {
	f := New()
	for name, got := range map[string]string{
		"Open": f.Open("EVIDENCE"), "Close": f.Close("EVIDENCE"), "Lead": f.Lead("E1"),
	} {
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("%s() = %q, must not contain a line break", name, got)
		}
	}
}
