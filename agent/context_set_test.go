package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/contextdepth"
)

// testCallID is the tool call ID every validation error must name.
const testCallID = "call-1"

// validSet is the minimal legal set: one RAG group carrying one metadata-only
// alternative. Every validation row starts here and breaks exactly one rule.
func validSet() *ContextSet {
	return &ContextSet{
		Groups: []ContextGroup{{
			Desc: contextdepth.GroupDesc{
				Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "pkg/doc.go"},
				Rank:    1,
			},
			Alternatives: []ContextAlternative{{
				Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
					{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata},
				}},
				Content: "source pkg/doc.go (no summary)",
			}},
		}},
	}
}

// setWith returns validSet after mutate breaks (or extends) exactly one rule.
func setWith(mutate func(*ContextSet)) *ContextSet {
	s := validSet()
	mutate(s)
	return s
}

// groupsSet returns a set of n otherwise-valid groups with distinct IDs.
func groupsSet(n int) *ContextSet {
	s := validSet()
	proto := s.Groups[0]
	s.Groups = make([]ContextGroup, 0, n)
	for i := range n {
		g := proto
		g.Desc.Subject.ID = fmt.Sprintf("pkg/doc%d.go", i)
		s.Groups = append(s.Groups, g)
	}
	return s
}

// altsSet returns a one-group set holding n otherwise-valid alternatives.
func altsSet(n int) *ContextSet {
	s := validSet()
	proto := s.Groups[0].Alternatives[0]
	alts := make([]ContextAlternative, 0, n)
	for i := range n {
		a := proto
		a.Content = fmt.Sprintf("alternative %d", i)
		alts = append(alts, a)
	}
	s.Groups[0].Alternatives = alts
	return s
}

// testVerbatimRep is the descriptor component that licenses attribution.
var testVerbatimRep = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim}

func TestContextSetCloneIsolation(t *testing.T) {
	if got := (*ContextSet)(nil).clone(); got != nil {
		t.Fatalf("(*ContextSet)(nil).clone() = %+v, want nil", got)
	}

	s := validSet()
	// Alternative 0 gains a verbatim component and attribution; alternative 1
	// keeps nil attribution so the clone's nil handling is exercised too.
	s.Groups[0].Alternatives[0].Desc.Representations = append(s.Groups[0].Alternatives[0].Desc.Representations, testVerbatimRep)
	s.Groups[0].Alternatives[0].Attrib = &RetrievalAttribution{Sources: []RetrievedSource{
		{Source: "pkg/doc.go", StableKey: "k1", StartLine: 1, EndLine: 9, Score: 0.5},
	}}
	s.Groups[0].Alternatives = append(s.Groups[0].Alternatives, ContextAlternative{
		Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
			{Depth: contextdepth.DepthL1, Kind: contextdepth.RepresentationGenerated},
		}},
		Content: "overview",
	})

	c := s.clone()

	s.MinVerbatim = 7
	s.Groups[0].Desc.Subject.ID = "mutated-subject"
	s.Groups[0].Alternatives[0].Content = "mutated content"
	s.Groups[0].Alternatives[0].Desc.Representations[0].Kind = contextdepth.RepresentationVerbatim
	s.Groups[0].Alternatives[0].Attrib.Sources[0].Source = "mutated.go"
	s.Groups[0].Alternatives[0].Attrib.Sources[0].Score = 9.5

	// Expectations are the literals validSet and this test wrote, never values
	// re-read from s: a clone that shared state would agree with s either way.
	if got := c.MinVerbatim; got != 0 {
		t.Errorf("clone MinVerbatim = %d, want 0", got)
	}
	if got := len(c.Groups); got != 1 {
		t.Fatalf("clone groups = %d, want 1", got)
	}
	if got := c.Groups[0].Desc.Subject.ID; got != "pkg/doc.go" {
		t.Errorf("clone subject ID = %q, want %q", got, "pkg/doc.go")
	}
	if got := c.Groups[0].Desc.Rank; got != 1 {
		t.Errorf("clone rank = %d, want 1", got)
	}
	if got := len(c.Groups[0].Alternatives); got != 2 {
		t.Fatalf("clone alternatives = %d, want 2", got)
	}
	if got := c.Groups[0].Alternatives[0].Content; got != "source pkg/doc.go (no summary)" {
		t.Errorf("clone content = %q, want the original content", got)
	}
	if got := len(c.Groups[0].Alternatives[0].Desc.Representations); got != 2 {
		t.Fatalf("clone representations = %d, want 2", got)
	}
	if got := c.Groups[0].Alternatives[0].Desc.Representations[0].Kind; got != contextdepth.RepresentationMetadata {
		t.Errorf("clone representation[0].Kind = %v, want metadata", got)
	}
	if got := c.Groups[0].Alternatives[0].Desc.Representations[1]; got != testVerbatimRep {
		t.Errorf("clone representation[1] = %+v, want %+v", got, testVerbatimRep)
	}
	if c.Groups[0].Alternatives[0].Attrib == nil {
		t.Fatal("clone dropped attribution")
	}
	if got := len(c.Groups[0].Alternatives[0].Attrib.Sources); got != 1 {
		t.Fatalf("clone attribution sources = %d, want 1", got)
	}
	if got := c.Groups[0].Alternatives[0].Attrib.Sources[0]; got.Source != "pkg/doc.go" || got.Score != 0.5 {
		t.Errorf("clone attribution source = %+v, want the original source", got)
	}
	if got := c.Groups[0].Alternatives[1].Attrib; got != nil {
		t.Errorf("clone fabricated attribution %+v on an alternative that had none", got)
	}
}

func TestValidateContextSet(t *testing.T) {
	// wantMsg pins the rule each error names AND its indexing. "wantErr plus a
	// prefix" cannot tell two rules apart: deleting the conversation branch
	// makes that set fall through to the generic unknown-domain error, and
	// stripping "group %d" from a message loses the indexing the spec requires
	// — both stay green without these substrings.
	tests := []struct {
		name    string
		set     *ContextSet
		wantErr bool
		wantMsg []string
	}{
		{name: "valid", set: validSet()},
		{name: "memory domain", set: setWith(func(s *ContextSet) {
			s.Groups[0].Desc.Subject.Domain = contextdepth.DomainMemory
		})},
		{name: "positive MinVerbatim", set: setWith(func(s *ContextSet) { s.MinVerbatim = 2 })},
		{
			name:    "negative MinVerbatim",
			set:     setWith(func(s *ContextSet) { s.MinVerbatim = -1 }),
			wantErr: true,
			wantMsg: []string{"MinVerbatim must be >= 0"},
		},
		{
			name:    "zero groups",
			set:     setWith(func(s *ContextSet) { s.Groups = nil }),
			wantErr: true,
			wantMsg: []string{"zero groups"},
		},
		{
			name:    "blank ID",
			set:     setWith(func(s *ContextSet) { s.Groups[0].Desc.Subject.ID = "" }),
			wantErr: true,
			wantMsg: []string{"group 0", "blank subject ID"},
		},
		{
			name:    "unknown domain",
			set:     setWith(func(s *ContextSet) { s.Groups[0].Desc.Subject.Domain = "x" }),
			wantErr: true,
			wantMsg: []string{"group 0", `unknown domain "x"`},
		},
		{
			name:    "blank domain",
			set:     setWith(func(s *ContextSet) { s.Groups[0].Desc.Subject.Domain = "" }),
			wantErr: true,
			wantMsg: []string{"group 0", `unknown domain ""`},
		},
		{
			name: "conversation domain reserved",
			set: setWith(func(s *ContextSet) {
				s.Groups[0].Desc.Subject.Domain = contextdepth.DomainConversation
			}),
			wantErr: true,
			// Rejected AS assembler-owned, not merely rejected: the generic
			// unknown-domain message would satisfy every other expectation.
			wantMsg: []string{"group 0", `domain "conversation" is assembler-owned`},
		},
		{
			name:    "empty alternatives",
			set:     setWith(func(s *ContextSet) { s.Groups[0].Alternatives = nil }),
			wantErr: true,
			wantMsg: []string{"group 0 (pkg/doc.go)", "no alternatives"},
		},
		{
			name: "invalid desc",
			set: setWith(func(s *ContextSet) {
				s.Groups[0].Alternatives[0].Desc.Representations = nil
			}),
			wantErr: true,
			wantMsg: []string{"group 0 (pkg/doc.go)", "alternative 0", "invalid descriptor"},
		},
		{
			name:    "empty content",
			set:     setWith(func(s *ContextSet) { s.Groups[0].Alternatives[0].Content = "" }),
			wantErr: true,
			wantMsg: []string{"group 0 (pkg/doc.go)", "alternative 0", "empty content"},
		},
		{name: "max groups", set: groupsSet(256)},
		{
			name:    "too many groups",
			set:     groupsSet(257),
			wantErr: true,
			wantMsg: []string{"257 groups exceeds limit 256"},
		},
		{name: "max alternatives", set: altsSet(64)},
		{
			name:    "too many alternatives",
			set:     altsSet(65),
			wantErr: true,
			wantMsg: []string{"group 0 (pkg/doc.go)", "65 alternatives exceeds limit 64"},
		},
		{
			name: "attrib on non-verbatim",
			set: setWith(func(s *ContextSet) {
				s.Groups[0].Alternatives[0].Attrib = &RetrievalAttribution{Sources: []RetrievedSource{{Source: "pkg/doc.go"}}}
			}),
			wantErr: true,
			wantMsg: []string{"group 0 (pkg/doc.go)", "alternative 0", "attribution on a non-verbatim alternative"},
		},
		{name: "attrib on verbatim ok", set: setWith(func(s *ContextSet) {
			s.Groups[0].Alternatives[0].Desc.Representations = []contextdepth.RepresentationDesc{testVerbatimRep}
			s.Groups[0].Alternatives[0].Attrib = &RetrievalAttribution{Sources: []RetrievedSource{{Source: "pkg/doc.go"}}}
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateContextSet(testCallID, tt.set)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("validateContextSet() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateContextSet() = nil, want an error")
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "agent: context set") || !strings.Contains(msg, testCallID) {
				t.Errorf("error %q must be package-prefixed and name call %q", msg, testCallID)
			}
			for _, want := range tt.wantMsg {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q must contain %q", msg, want)
				}
			}
		})
	}
}
