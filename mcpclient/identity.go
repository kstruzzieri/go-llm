package mcpclient

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"regexp"

	"github.com/kstruzzieri/go-llm/signing"
)

// connectionIdentity is a server's frozen launch or destination identity
// (spec §5.6). Values can be secret: argv and endpoint are never rendered and
// no value is persisted; only keyed digests leave the process. Values must be
// valid UTF-8: digestConnection refuses anything else (signing.ErrInvalidUTF8).
type connectionIdentity struct {
	workspace, alias, kind string
	// stdio
	envBaseline           string
	env                   []string // source:NAME, sorted
	launcher, target, dir string
	argv                  []string
	// http
	origin   string // canonical scheme://host[:port]
	endpoint string // canonical path plus optional query
}

const (
	connectionFormat      = 1
	connectionDomain      = "go-llm/mcp-connection/v1"
	connectionFieldDomain = "go-llm/mcp-connection-field/v1/"
	connectionKeyFile     = "connection-hmac.pem"
)

var (
	fingerprintRE = regexp.MustCompile(`^hmac-sha256:[0-9a-f]{64}$`)
	hexTagRE      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// connectionFieldNames lists each kind's digest-bearing fields in label order.
var connectionFieldNames = map[string][]string{
	"stdio": {"launcher", "target", "dir", "env", "env_baseline", "argv"},
	"http":  {"origin", "endpoint"},
}

func (c connectionIdentity) fieldValues() map[string]any {
	if c.kind == "http" {
		return map[string]any{"origin": c.origin, "endpoint": c.endpoint}
	}
	return map[string]any{"launcher": c.launcher, "target": c.target, "dir": c.dir,
		"env": nonNilStrings(c.env), "env_baseline": c.envBaseline, "argv": nonNilStrings(c.argv)}
}

// nonNilStrings makes nil and empty lists encode identically, as [].
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// connectionPin is the keyed fingerprint material for one identity: all a pin
// record stores about a connection.
type connectionPin struct {
	fingerprint string            // "hmac-sha256:" + 64 hex, over the whole identity
	keyID       string            // 64 hex
	kind        string            // "stdio" or "http"
	fields      map[string]string // field name -> 64 hex
}

// digestConnection fingerprints c, overall and per field, with the store's
// per-user key (spec §5.6).
func (s *PinStore) digestConnection(ctx context.Context, c connectionIdentity) (*connectionPin, error) {
	values := c.fieldValues()
	whole := map[string]any{"format": connectionFormat, "workspace": c.workspace, "alias": c.alias, "kind": c.kind}
	for name, v := range values {
		whole[name] = v
	}
	sign := func(domain string, v any) (signing.Signature, error) {
		payload, err := signing.MarshalCanonical(v)
		if err != nil {
			return signing.Signature{}, err
		}
		return s.connKey.Sign(ctx, domain, payload)
	}
	sig, err := sign(connectionDomain, whole)
	if err != nil {
		return nil, err
	}
	pin := &connectionPin{fingerprint: "hmac-sha256:" + hex.EncodeToString(sig.Bytes), keyID: sig.KeyID, kind: c.kind, fields: make(map[string]string, len(values))}
	for name, v := range values {
		fieldSig, err := sign(connectionFieldDomain+name, v)
		if err != nil {
			return nil, err
		}
		pin.fields[name] = hex.EncodeToString(fieldSig.Bytes)
	}
	return pin, nil
}

// compareConnectionPins returns fixed labels for what differs; nil means the
// connections are identical. Tags compare in constant time.
func compareConnectionPins(pinned, candidate *connectionPin) []string {
	// A kind or key change makes every field tag differ, so those labels
	// alone describe it (spec §5.7: kind, then key).
	var labels []string
	if pinned.kind != candidate.kind {
		labels = append(labels, "kind")
	}
	if pinned.keyID != candidate.keyID {
		labels = append(labels, "key")
	}
	if len(labels) > 0 {
		return labels
	}
	if equalTag(pinned.fingerprint, candidate.fingerprint) {
		return nil
	}
	for _, name := range connectionFieldNames[candidate.kind] {
		if !equalTag(pinned.fields[name], candidate.fields[name]) {
			labels = append(labels, name)
		}
	}
	if len(labels) == 0 {
		// Unexplained by any field: corruption or a non-field change.
		return []string{"identity"}
	}
	return labels
}

func equalTag(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
