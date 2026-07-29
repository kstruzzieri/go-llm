package contextdepth

import "testing"

// Numeric values are an encoding contract with rag.Depth aliases and any
// serialized trace: None/Invalid=0, L0=1, L1=2, L2=3 (spec D2).
func TestDepthNumericValuesPinned(t *testing.T) {
	if DepthInvalid != 0 || DepthL0 != 1 || DepthL1 != 2 || DepthL2 != 3 {
		t.Fatalf("depth numeric values moved: invalid=%d l0=%d l1=%d l2=%d",
			DepthInvalid, DepthL0, DepthL1, DepthL2)
	}
}

func TestDepthValid(t *testing.T) {
	cases := []struct {
		d    Depth
		want bool
	}{
		{DepthInvalid, false},
		{DepthL0, true},
		{DepthL1, true},
		{DepthL2, true},
		{Depth(4), false},
		{Depth(255), false},
	}
	for _, tc := range cases {
		if got := tc.d.Valid(); got != tc.want {
			t.Errorf("Depth(%d).Valid() = %v, want %v", tc.d, got, tc.want)
		}
	}
}

func TestDepthString(t *testing.T) {
	cases := []struct {
		d    Depth
		want string
	}{
		{DepthInvalid, "invalid"},
		{DepthL0, "L0"},
		{DepthL1, "L1"},
		{DepthL2, "L2"},
		{Depth(9), "invalid"},
	}
	for _, tc := range cases {
		if got := tc.d.String(); got != tc.want {
			t.Errorf("Depth(%d).String() = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestRepresentationKindValidAndString(t *testing.T) {
	cases := []struct {
		k     RepresentationKind
		valid bool
		s     string
	}{
		{RepresentationInvalid, false, "invalid"},
		{RepresentationMetadata, true, "metadata"},
		{RepresentationGenerated, true, "generated"},
		{RepresentationCompact, true, "compact"},
		{RepresentationVerbatim, true, "verbatim"},
		{RepresentationKind(9), false, "invalid"},
	}
	for _, tc := range cases {
		if got := tc.k.Valid(); got != tc.valid {
			t.Errorf("Kind(%d).Valid() = %v, want %v", tc.k, got, tc.valid)
		}
		if got := tc.k.String(); got != tc.s {
			t.Errorf("Kind(%d).String() = %q, want %q", tc.k, got, tc.s)
		}
	}
}

// A3b (L0 orientation + L2 evidence) and A3c (L1 orientation + L2 evidence)
// must remain distinct descriptors while both report Depth() == L2 (spec D4).
func TestAlternativeDescDepthAndValid(t *testing.T) {
	a3b := AlternativeDesc{Representations: []RepresentationDesc{
		{Depth: DepthL0, Kind: RepresentationGenerated},
		{Depth: DepthL2, Kind: RepresentationVerbatim},
	}}
	a3c := AlternativeDesc{Representations: []RepresentationDesc{
		{Depth: DepthL0, Kind: RepresentationGenerated},
		{Depth: DepthL1, Kind: RepresentationGenerated},
		{Depth: DepthL2, Kind: RepresentationVerbatim},
	}}
	if !a3b.Valid() || !a3c.Valid() {
		t.Fatalf("legal descriptors reported invalid: a3b=%v a3c=%v", a3b.Valid(), a3c.Valid())
	}
	if a3b.Depth() != DepthL2 || a3c.Depth() != DepthL2 {
		t.Fatalf("Depth() = %v / %v, want L2 / L2", a3b.Depth(), a3c.Depth())
	}
	if len(a3b.Representations) == len(a3c.Representations) {
		t.Fatal("fixture bug: a3b and a3c should differ in component count")
	}

	// Max-depth component first, not last: distinguishes Depth() computing a
	// true maximum from one that merely returns the last (or first) element.
	outOfOrder := AlternativeDesc{Representations: []RepresentationDesc{
		{Depth: DepthL2, Kind: RepresentationVerbatim},
		{Depth: DepthL0, Kind: RepresentationMetadata},
	}}
	if !outOfOrder.Valid() {
		t.Fatal("legal descriptor reported invalid: outOfOrder")
	}
	if got := outOfOrder.Depth(); got != DepthL2 {
		t.Fatalf("outOfOrder.Depth() = %v, want L2", got)
	}

	malformed := []AlternativeDesc{
		{}, // empty components
		{Representations: []RepresentationDesc{{Depth: DepthInvalid, Kind: RepresentationMetadata}}}, // invalid depth
		{Representations: []RepresentationDesc{{Depth: DepthL0, Kind: RepresentationInvalid}}},       // invalid kind
		{Representations: []RepresentationDesc{
			{Depth: DepthL0, Kind: RepresentationMetadata},
			{Depth: DepthL2, Kind: RepresentationVerbatim},
			{Depth: DepthInvalid, Kind: RepresentationMetadata},
		}}, // malformed component last, not first
	}
	for i, m := range malformed {
		if m.Valid() {
			t.Errorf("malformed[%d].Valid() = true, want false", i)
		}
		if m.Depth() != DepthInvalid {
			t.Errorf("malformed[%d].Depth() = %v, want DepthInvalid", i, m.Depth())
		}
	}
}

func TestDomainConstants(t *testing.T) {
	if DomainRAG != "rag" || DomainConversation != "conversation" || DomainMemory != "memory" {
		t.Fatalf("domain vocabulary moved: %q %q %q", DomainRAG, DomainConversation, DomainMemory)
	}
}
