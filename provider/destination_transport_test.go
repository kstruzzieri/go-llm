package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
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

// I12: a "localhost" destination must not dial an address that is not
// loopback — a poisoned resolver (hosts file) must fail the dial entirely,
// not be filtered around.
func TestGuardedLocalhostDialValidatesResolution(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	baseURL := fmt.Sprintf("http://localhost:%d", port)
	newClient := func(t *testing.T, ips []net.IP, lookupErr error) (*http.Client, context.Context) {
		t.Helper()
		dest := mustDest(t, "llamacpp", baseURL)
		gate := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: dest})
		client, err := guardHTTPClient(gate, dest, nil, func(_ context.Context, host string) ([]net.IP, error) {
			if !strings.EqualFold(host, "localhost") {
				t.Errorf("lookup called for %q, want localhost", host)
			}
			return ips, lookupErr
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := gate.Bind(context.Background(), "agent", "llamacpp")
		if err != nil {
			t.Fatal(err)
		}
		return client, ctx
	}

	t.Run("loopback resolution connects", func(t *testing.T) {
		client, ctx := newClient(t, []net.IP{net.ParseIP("127.0.0.1")}, nil)
		resp, err := client.Do(mustReq(t, ctx, baseURL+"/v1/models"))
		if err != nil {
			t.Fatalf("loopback-resolved localhost dial failed: %v", err)
		}
		_ = resp.Body.Close()
	})

	t.Run("non-loopback resolution refused", func(t *testing.T) {
		client, ctx := newClient(t, []net.IP{net.ParseIP("192.168.1.5")}, nil)
		if resp, err := client.Do(mustReq(t, ctx, baseURL+"/v1/models")); err == nil {
			_ = resp.Body.Close()
			t.Fatal("localhost resolving off-host was dialed")
		}
	})

	t.Run("mixed resolution refused entirely", func(t *testing.T) {
		// One loopback plus one non-loopback signals tampering; filtering to
		// the loopback half would dial through a poisoned name anyway.
		client, ctx := newClient(t, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.168.1.5")}, nil)
		if resp, err := client.Do(mustReq(t, ctx, baseURL+"/v1/models")); err == nil {
			_ = resp.Body.Close()
			t.Fatal("localhost with a mixed resolution was dialed")
		}
	})

	t.Run("resolution failure refused", func(t *testing.T) {
		client, ctx := newClient(t, nil, fmt.Errorf("resolver down"))
		if resp, err := client.Do(mustReq(t, ctx, baseURL+"/v1/models")); err == nil {
			_ = resp.Body.Close()
			t.Fatal("unresolvable localhost was dialed")
		}
	})
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
