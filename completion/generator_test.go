package completion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

type fakeGenerator struct {
	generate func(context.Context, GenerateRequest) (GenerateResult, error)
	stream   func(context.Context, GenerateRequest, func(GenerateChunk) error) (GenerateResult, error)
}

func (g *fakeGenerator) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	if g.generate == nil {
		return GenerateResult{}, errors.New("unexpected Generate call")
	}
	return g.generate(ctx, req)
}

func (g *fakeGenerator) GenerateStream(ctx context.Context, req GenerateRequest, fn func(GenerateChunk) error) (GenerateResult, error) {
	if g.stream == nil {
		return GenerateResult{}, errors.New("unexpected GenerateStream call")
	}
	return g.stream(ctx, req, fn)
}

func TestNewProviderWithGeneratorValidation(t *testing.T) {
	if _, err := NewProviderWithGenerator(nil, "test-model", testProviderConfig()); err == nil {
		t.Fatal("expected nil generator to be rejected")
	}

	p, err := NewProviderWithGenerator(&fakeGenerator{}, "", testProviderConfig())
	if err != nil {
		t.Fatalf("empty model should be rejected at call time, not construction: %v", err)
	}
	if _, err := p.Complete(context.Background(), FIMRequest{Prefix: "code"}); err == nil {
		t.Fatal("expected empty model to be rejected at call time")
	}
}

func TestCompleteUsesGeneratorRequestPlan(t *testing.T) {
	cfg := testProviderConfig()
	prefix := "func main() {\n\t"
	suffix := "\n}"
	var got GenerateRequest

	gen := &fakeGenerator{
		generate: func(_ context.Context, req GenerateRequest) (GenerateResult, error) {
			got = req
			return GenerateResult{
				Response: "fmt.Println()<|endoftext|>",
				Tokens:   5,
				Model:    req.Model,
				Provider: "fake",
			}, nil
		},
	}

	p, err := NewProviderWithGenerator(gen, "test-model", cfg)
	if err != nil {
		t.Fatalf("NewProviderWithGenerator: %v", err)
	}

	resp, err := p.Complete(context.Background(), FIMRequest{
		Prefix:    prefix,
		Suffix:    suffix,
		FilePath:  "main.go",
		MaxTokens: 17,
		Trace:     true,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got.Model != "test-model" {
		t.Errorf("Model = %q, want test-model", got.Model)
	}
	if got.Prompt != prefix {
		t.Errorf("Prompt = %q, want %q", got.Prompt, prefix)
	}
	if got.Suffix != suffix {
		t.Errorf("Suffix = %q, want %q", got.Suffix, suffix)
	}
	if got.NumPredict != 17 {
		t.Errorf("NumPredict = %d, want 17", got.NumPredict)
	}
	if got.NumCtx != cfg.ContextWindow {
		t.Errorf("NumCtx = %d, want %d", got.NumCtx, cfg.ContextWindow)
	}
	if resp.BudgetTrace == nil {
		t.Fatal("BudgetTrace is nil")
	}
	if got.Temperature != resp.BudgetTrace.Temperature {
		t.Errorf("Temperature = %v, want trace temperature %v", got.Temperature, resp.BudgetTrace.Temperature)
	}
	if len(got.Stop) == 0 || got.Stop[0] != "<|endoftext|>" {
		t.Fatalf("Stop = %v, want first stop token <|endoftext|>", got.Stop)
	}
	if resp.Completion != "fmt.Println()" {
		t.Errorf("Completion = %q, want stop-stripped response", resp.Completion)
	}
	if resp.Tokens != 5 {
		t.Errorf("Tokens = %d, want 5", resp.Tokens)
	}
}

func TestCompleteStreamUsesGeneratorRequestPlan(t *testing.T) {
	var got GenerateRequest
	gen := &fakeGenerator{
		stream: func(_ context.Context, req GenerateRequest, fn func(GenerateChunk) error) (GenerateResult, error) {
			got = req
			chunks := []GenerateChunk{
				{Response: "fmt.", Done: false},
				{Response: "Println()", Done: false},
				{Response: "<|endoftext|>", Done: true},
			}
			for _, chunk := range chunks {
				if err := fn(chunk); err != nil {
					return GenerateResult{}, err
				}
			}
			return GenerateResult{
				Response: "fmt.Println()<|endoftext|>",
				Tokens:   3,
				Model:    req.Model,
				Provider: "fake",
			}, nil
		},
	}

	p, err := NewProviderWithGenerator(gen, "test-model", testProviderConfig())
	if err != nil {
		t.Fatalf("NewProviderWithGenerator: %v", err)
	}

	var tokens []string
	err = p.CompleteStream(context.Background(), FIMRequest{
		Prefix:    "func main() {\n\t",
		Suffix:    "\n}",
		MaxTokens: 11,
	}, func(token string) error {
		tokens = append(tokens, token)
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}

	if got.Model != "test-model" {
		t.Errorf("Model = %q, want test-model", got.Model)
	}
	if got.NumPredict != 11 {
		t.Errorf("NumPredict = %d, want 11", got.NumPredict)
	}
	joined := strings.Join(tokens, "")
	if joined != "fmt.Println()" {
		t.Errorf("stream output = %q, want stop-stripped response", joined)
	}
	if strings.Contains(joined, "<|endoftext|>") {
		t.Errorf("stop token leaked to stream callback: %q", joined)
	}
}

func TestGeneratorFromOllamaClientNilPassthrough(t *testing.T) {
	if got := generatorFromOllamaClient(nil); got != nil {
		t.Fatalf("generatorFromOllamaClient(nil) = %v, want nil", got)
	}
}

func TestNewProviderNilClientCompatibility(t *testing.T) {
	p, err := NewProvider(nil, "test-model", testProviderConfig())
	if err != nil {
		t.Fatalf("NewProvider(nil, ...) construction must succeed for legacy compat, got: %v", err)
	}
	_, err = p.Complete(context.Background(), FIMRequest{Prefix: "x", Suffix: "y"})
	if err == nil {
		t.Fatal("Complete with nil client must error")
	}
	if !strings.Contains(err.Error(), "generator is required") {
		t.Fatalf("Complete error = %v, want substring %q", err, "generator is required")
	}
	err = p.CompleteStream(context.Background(), FIMRequest{Prefix: "x", Suffix: "y"}, func(string) error { return nil })
	if err == nil {
		t.Fatal("CompleteStream with nil client must error")
	}
	if !strings.Contains(err.Error(), "generator is required") {
		t.Fatalf("CompleteStream error = %v, want substring %q", err, "generator is required")
	}
}

func TestOllamaGeneratorDelegation(t *testing.T) {
	t.Run("Generate", func(t *testing.T) {
		var captured ollama.GenerateRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/generate" {
				t.Errorf("path = %q, want /api/generate", r.URL.Path)
			}
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("decode: %v", err)
			}
			_ = json.NewEncoder(w).Encode(ollama.GenerateResponse{
				Model:     "qwen3:8b",
				Response:  "delegated",
				Done:      true,
				EvalCount: 7,
			})
		}))
		defer srv.Close()

		gen := generatorFromOllamaClient(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
		if gen == nil {
			t.Fatal("generatorFromOllamaClient returned nil for non-nil client")
		}

		got, err := gen.Generate(context.Background(), GenerateRequest{
			Model:      "qwen3:8b",
			Prompt:     "func main() {",
			Suffix:     "}",
			NumPredict: 32,
			NumCtx:     2048,
			Stop:       []string{"<|endoftext|>"},
		})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if got.Response != "delegated" {
			t.Errorf("Response = %q, want delegated", got.Response)
		}
		if got.Tokens != 7 {
			t.Errorf("Tokens = %d, want 7 (from EvalCount)", got.Tokens)
		}
		if got.Model != "qwen3:8b" {
			t.Errorf("Model = %q, want qwen3:8b (from request)", got.Model)
		}
		if got.Provider != "ollama" {
			t.Errorf("Provider = %q, want ollama", got.Provider)
		}
		if got.Outcome != nil {
			t.Errorf("Outcome = %v, want nil for ollama adapter", got.Outcome)
		}
		if captured.Model != "qwen3:8b" {
			t.Errorf("forwarded Model = %q, want qwen3:8b", captured.Model)
		}
		if captured.Suffix != "}" {
			t.Errorf("forwarded Suffix = %q, want }", captured.Suffix)
		}
	})

	t.Run("GenerateStream", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/generate" {
				t.Errorf("path = %q, want /api/generate", r.URL.Path)
			}
			enc := json.NewEncoder(w)
			_ = enc.Encode(ollama.GenerateResponse{Response: "hel", Done: false})
			_ = enc.Encode(ollama.GenerateResponse{Response: "lo", Done: false})
			_ = enc.Encode(ollama.GenerateResponse{Response: "", Done: true, EvalCount: 3})
		}))
		defer srv.Close()

		gen := generatorFromOllamaClient(ollama.NewClient(ollama.WithBaseURL(srv.URL)))

		var chunks []GenerateChunk
		got, err := gen.GenerateStream(
			context.Background(),
			GenerateRequest{Model: "qwen3:8b", Prompt: "say hi"},
			func(c GenerateChunk) error {
				chunks = append(chunks, c)
				return nil
			},
		)
		if err != nil {
			t.Fatalf("GenerateStream: %v", err)
		}
		if len(chunks) != 3 {
			t.Fatalf("chunks = %d, want 3", len(chunks))
		}
		if !chunks[len(chunks)-1].Done {
			t.Error("last chunk Done = false, want true")
		}
		if got.Response != "hello" {
			t.Errorf("aggregated Response = %q, want hello", got.Response)
		}
		if got.Tokens != 3 {
			t.Errorf("Tokens = %d, want 3 (from final EvalCount)", got.Tokens)
		}
		if got.Provider != "ollama" {
			t.Errorf("Provider = %q, want ollama", got.Provider)
		}
	})
}

func TestOllamaGeneratorValidation(t *testing.T) {
	// Use a real client wired to a server that should never be hit — validation
	// must reject before any HTTP call. If a request escapes validation the
	// handler fails the test loudly.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("HTTP handler called; validation should have rejected the request")
	}))
	defer srv.Close()

	gen := generatorFromOllamaClient(ollama.NewClient(ollama.WithBaseURL(srv.URL)))

	cases := []struct {
		name    string
		req     GenerateRequest
		wantErr string
	}{
		{
			name:    "empty model",
			req:     GenerateRequest{Prompt: "x", Suffix: "y"},
			wantErr: "model is required",
		},
		{
			name:    "empty prompt and suffix",
			req:     GenerateRequest{Model: "qwen3:8b"},
			wantErr: "prompt or suffix is required",
		},
	}
	for _, tc := range cases {
		t.Run("Generate/"+tc.name, func(t *testing.T) {
			_, err := gen.Generate(context.Background(), tc.req)
			if err == nil {
				t.Fatalf("Generate did not reject %+v", tc.req)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Generate err = %v, want substring %q", err, tc.wantErr)
			}
		})
		t.Run("GenerateStream/"+tc.name, func(t *testing.T) {
			_, err := gen.GenerateStream(context.Background(), tc.req, func(GenerateChunk) error { return nil })
			if err == nil {
				t.Fatalf("GenerateStream did not reject %+v", tc.req)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("GenerateStream err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}

	t.Run("GenerateStream/nil callback", func(t *testing.T) {
		_, err := gen.GenerateStream(context.Background(), GenerateRequest{Model: "qwen3:8b", Prompt: "x"}, nil)
		if err == nil {
			t.Fatal("GenerateStream with nil callback must error")
		}
		if !strings.Contains(err.Error(), "callback is required") {
			t.Fatalf("err = %v, want substring %q", err, "callback is required")
		}
	})
}
