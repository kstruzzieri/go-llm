package openaicompat

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// embedGuardTransport is guardTransport answering with a well-formed
// /v1/embeddings response for one input.
type embedGuardTransport struct{ guardTransport }

func (e *embedGuardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := e.guardTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(strings.NewReader(`{"model":"embed-m","data":[{"index":0,"embedding":[0.5]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
	return resp, nil
}

// TestEmbedDeniedBeforeAdmission pins the early denial: a credential in the
// model or in any input is refused before the flight key is computed and before
// admission, so no permit is ever taken. Zero acquisitions is the proof; a clean
// call cannot stand in for it, because the flight key includes the input.
func TestEmbedDeniedBeforeAdmission(t *testing.T) {
	rows := []struct {
		name string
		req  provider.EmbedRequest
	}{
		{"model_selector", provider.EmbedRequest{Model: guardFixture, Input: []string{"clean"}}},
		{"first_input", provider.EmbedRequest{Model: "embed-m", Input: []string{guardFixture, "clean"}}},
		{"later_input", provider.EmbedRequest{Model: "embed-m", Input: []string{"clean", "also clean", guardFixture}}},
		{"embedded_in_text", provider.EmbedRequest{Model: "embed-m", Input: []string{"see https://docs.example.invalid/hook?token=" + guardFixture + "&x=1"}}},
	}
	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			tr := &guardTransport{}
			p := NewProvider(newGuardClient(tr, guardContains(guardFixture)))
			admit := &countingAdmit{invoked: make(chan struct{}, 1)}

			_, err := p.EmbedAdmitted(context.Background(), tt.req, admit.fn())

			assertOutcome(t, outcomeDenied, err, tr, guardFixture)
			if n := admit.acquires.Load(); n != 0 {
				t.Errorf("admission acquires = %d, want 0 (denied before admission)", n)
			}
			select {
			case <-admit.invoked:
				t.Error("admit was invoked, want the call refused before the flight started")
			default:
			}
		})
	}
}

// TestEmbedFreezesInputs pins that the denial check and the request body see the
// same inputs. The window is inside admission: the leader reads its inputs after
// admit returns, so the caller overwrites its slice while the leader holds a
// permit. The check ran on a frozen copy, so the original input is what is sent.
func TestEmbedFreezesInputs(t *testing.T) {
	tr := &embedGuardTransport{}
	p := NewProvider(NewClient(guardBaseURL,
		WithHTTPClient(&http.Client{Transport: tr}),
		WithContentGuard(guardContains(guardFixture))))

	admit := &countingAdmit{invoked: make(chan struct{}, 1)}
	held := make(chan struct{}, 1)
	proceed := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
	})
	// hold takes the permit, then keeps the leader inside admission until the
	// test lets it go.
	hold := func(ctx context.Context) (func(), error) {
		release, err := admit.fn()(ctx)
		if err != nil {
			return nil, err
		}
		select {
		case held <- struct{}{}:
		default:
		}
		select {
		case <-proceed:
		case <-ctx.Done():
		}
		return release, nil
	}

	req := provider.EmbedRequest{Model: "embed-m", Input: []string{"clean-input"}}
	errC := make(chan error, 1)
	go func() {
		_, err := p.EmbedAdmitted(context.Background(), req, hold)
		errC <- err
	}()

	select {
	case <-held:
	case <-time.After(admWait):
		t.Fatal("leader never acquired a permit")
	}
	if n := admit.acquires.Load(); n != 1 {
		t.Fatalf("acquires = %d, want 1 while the leader holds the permit", n)
	}
	if n := tr.calls.Load(); n != 0 {
		t.Fatalf("transport calls = %d before the leader left admission, want 0", n)
	}

	req.Input[0] = guardFixture // the caller reuses its slice mid-flight
	close(proceed)

	select {
	case err := <-errC:
		if err != nil {
			t.Fatalf("EmbedAdmitted: %v, want the frozen clean input sent", err)
		}
	case <-time.After(admWait):
		t.Fatal("call never completed")
	}
	if n := tr.calls.Load(); n != 1 {
		t.Errorf("transport calls = %d, want 1", n)
	}
	if _, body := tr.last(); body != `{"model":"embed-m","input":["clean-input"]}` {
		t.Errorf("body = %s, want the original input", body)
	}
}
