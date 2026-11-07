package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/ollama"
)

// positive tokens as the catalog/runtime vocabulary writes them, in two cases.
var listingTokens = []string{
	"chat", "generate", "stream", "embed", "tool_call", "thinking", "insert",
	"completion", "tools", "embedding",
}

func TestOllamaListingFacts_AllAliasesMatchExistingPositiveMapping(t *testing.T) {
	for _, tok := range listingTokens {
		for _, spelled := range []string{tok, strings.ToUpper(tok)} {
			caps, _ := ListingFactMasks(OllamaListingFacts(ModelInfo{Capabilities: []string{spelled}}))
			if want := parseCaps([]string{spelled}); caps != want {
				t.Errorf("token %q: listing facts say %v, parseCaps says %v", spelled, caps, want)
			}
		}
	}
	// completion includes generate; pinned literally, not derived.
	caps, _ := ListingFactMasks(OllamaListingFacts(ModelInfo{Capabilities: []string{"completion"}}))
	if caps != CapChat|CapGenerate|CapStream {
		t.Errorf("completion = %v", caps)
	}
}

func TestOllamaListingFacts_TemplateAndMissingArray(t *testing.T) {
	facts := OllamaListingFacts(ModelInfo{Template: "{{ .Suffix }}"})
	if len(facts) != 1 || facts["insert"] != fingerprint.CapProbeYes {
		t.Fatalf("nil array with a suffix template = %v, want only insert yes", facts)
	}
	if facts := OllamaListingFacts(ModelInfo{}); len(facts) != 0 {
		t.Fatalf("nothing reported = %v, want no facts", facts)
	}
	facts = OllamaListingFacts(ModelInfo{Capabilities: []string{"completion"}})
	if _, ok := facts["embed"]; ok {
		t.Fatalf("invented an embed negative: %v", facts)
	}
}

func TestTemplateUsesSuffix_SharedImplementation(t *testing.T) {
	for _, tmpl := range []string{"", "x", "{{ .Suffix }}", ".suffix", "{{ .Prompt }}"} {
		if templateUsesSuffix(tmpl) != fingerprint.TemplateUsesSuffix(tmpl) {
			t.Errorf("drift on %q", tmpl)
		}
	}
	if !templateUsesSuffix("a .Suffix b") || templateUsesSuffix(".suffix") {
		t.Error("the .Suffix rule changed")
	}
}

func TestOllamaShowRecipe_CapabilitiesAreTheCanonicalNames(t *testing.T) {
	recipe, ok := fingerprint.RecipeFor(fingerprint.MethodOllamaShow)
	if !ok {
		t.Fatal("no ollama-show recipe")
	}
	got, want := slices.Clone(recipe.Capabilities), slices.Clone(CanonicalCapabilityNames)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("recipe capabilities %v != canonical names %v", got, want)
	}
}

// A real Ollama reports the digest on /api/tags and none on /api/show; the
// listing and the single-model path must agree on it, and on a failed show.
func TestOllamaProvider_DigestComesFromTagsOnEveryPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"ok","digest":"sha256:ok"},{"name":"broken","digest":"sha256:broken"}]}`)
		case "/api/show":
			var body struct{ Name string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Name == "broken" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprint(w, `{"capabilities":["completion"]}`)
		}
	}))
	defer srv.Close()
	p := NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
	infos, err := p.Models(t.Context())
	if err != nil || len(infos) != 2 {
		t.Fatal(infos, err)
	}
	for _, info := range infos {
		if want := "sha256:" + info.Name; info.Digest != want {
			t.Errorf("Models %s digest = %q, want %q", info.Name, info.Digest, want)
		}
	}
	one, err := p.ModelInfo(t.Context(), "ok")
	if err != nil || one.Digest != "sha256:ok" {
		t.Fatalf("ModelInfo = %+v, %v, want the same digest Models reports", one, err)
	}
}

// Ollama reads a name with no tag as its :latest form, and /api/tags always lists
// the tag, so the two spellings must find one digest. Show reports none.
func TestOllamaModelInfoDigestTreatsBareNameAndLatestAsOne(t *testing.T) {
	const digest = "sha256:listed"
	for _, tc := range []struct {
		name, asked, listed, want string
	}{
		{"bare asked, latest listed", "m", "m:latest", digest},
		{"latest asked, bare listed", "m:latest", "m", digest},
		{"exact", "m:7b", "m:7b", digest},
		{"bare is not another tag", "m", "m:7b", ""},
		{"another tag is not latest", "m:7b", "m:latest", ""},
		{"another model", "m", "m2:latest", ""},
		{"namespace", "ns/m", "ns/m:latest", digest},
		{"registry port is not a tag", "host:5000/m", "host:5000/m:latest", digest},
		{"registry port, bare is not another tag", "host:5000/m", "host:5000/m:7b", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/tags":
					_, _ = fmt.Fprintf(w, `{"models":[{"name":%q,"digest":%q}]}`, tc.listed, digest)
				case "/api/show":
					_, _ = fmt.Fprint(w, `{"capabilities":["completion"]}`)
				}
			}))
			defer srv.Close()
			p := NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
			info, err := p.ModelInfo(t.Context(), tc.asked)
			if err != nil {
				t.Fatal(err)
			}
			if info.Digest != tc.want || info.Name != tc.asked {
				t.Fatalf("ModelInfo(%q) = name %q digest %q, want name %q digest %q", tc.asked, info.Name, info.Digest, tc.asked, tc.want)
			}
			// The listing still reports the server's own spelling.
			infos, err := p.Models(t.Context())
			if err != nil || len(infos) != 1 || infos[0].Name != tc.listed || infos[0].Digest != digest {
				t.Fatalf("Models = %+v, %v, want only %q under %q", infos, err, tc.listed, digest)
			}
		})
	}
}

// A known limit: with no listing there is no digest, and the fingerprint identity
// falls back for this lookup. The show data still comes back.
func TestOllamaModelInfoKeepsShowDataWhenTagsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
		case "/api/show":
			_, _ = fmt.Fprint(w, `{"capabilities":["completion"],"template":"tmpl","details":{"family":"fam","parameter_size":"7B","quantization_level":"Q4"}}`)
		}
	}))
	defer srv.Close()
	p := NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
	info, err := p.ModelInfo(t.Context(), "m")
	if err != nil {
		t.Fatalf("ModelInfo: %v", err)
	}
	want := ModelInfo{Name: "m", Family: "fam", ParameterSize: "7B", QuantLevel: "Q4", Template: "tmpl", Capabilities: []string{"completion"}}
	if !reflect.DeepEqual(*info, want) {
		t.Fatalf("ModelInfo = %+v, want %+v", *info, want)
	}
}
