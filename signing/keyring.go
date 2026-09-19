package signing

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
)

// Keyring verifies signatures by dispatching on their key id, so records
// signed by a retired key keep verifying after a new key starts signing.
// Rotation is composition: hold one current Signer and a Keyring of every
// trusted verifier for this purpose. Cryptographic validity is not domain
// authorization: callers use separate rings for separate trust policies.
// The zero value is ready to use. Populate before use; Add is not safe to
// call concurrently with Verify.
type Keyring struct {
	byID map[string]Verifier
}

// NewKeyring builds a keyring from vs, failing on nil or duplicate entries.
func NewKeyring(vs ...Verifier) (*Keyring, error) {
	k := &Keyring{}
	for _, v := range vs {
		if err := k.Add(v); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// Add registers v. A nil verifier, an empty key id, or a key id already
// present is an error.
func (k *Keyring) Add(v Verifier) error {
	if k == nil {
		return errors.New("signing: keyring: nil keyring")
	}
	if isNilVerifier(v) {
		return errors.New("signing: keyring: nil verifier")
	}
	id := v.KeyID()
	if id == "" {
		return errors.New("signing: keyring: verifier has an empty key id")
	}
	if v.Algorithm() == "" {
		return errors.New("signing: keyring: verifier has an empty algorithm")
	}
	if k.byID == nil {
		k.byID = map[string]Verifier{}
	}
	if _, dup := k.byID[id]; dup {
		return fmt.Errorf("signing: keyring: duplicate key id %s", id)
	}
	k.byID[id] = v
	return nil
}

// Verify dispatches to the verifier named by sig.KeyID. An unknown id is
// ErrUnknownKey; otherwise the member's Verify result is returned as-is.
func (k *Keyring) Verify(ctx context.Context, domain string, payload []byte, sig Signature) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if k == nil {
		return ErrUninitializedKey
	}
	v, ok := k.byID[sig.KeyID]
	if !ok {
		return ErrUnknownKey
	}
	return v.Verify(ctx, domain, payload, sig)
}

// KeyIDs returns the registered key ids in sorted order.
func (k *Keyring) KeyIDs() []string {
	if k == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(k.byID))
}

func isNilVerifier(v Verifier) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
