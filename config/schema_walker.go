package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
)

// schemaNode is one level of the config schema shared by the merge
// renderer and the duplicate/alias detector (spec §5: ONE walker).
// Reflected schema shape stays shared; render and collision behavior
// remain pinned by their separate golden and detector tests.
//
// Invariant: exactly one of known/mapNode is set on a non-leaf; a leaf is
// a nil *schemaNode.
type schemaNode struct {
	known   map[string]*schemaNode // exact struct tag -> child; nil = leaf
	mapNode bool
	elem    *schemaNode // nil under mapNode means leaf entries (defaults)
}

func (n *schemaNode) isStruct() bool { return n != nil && n.known != nil }
func (n *schemaNode) isMap() bool    { return n != nil && n.mapNode }

var configSchemaOnce = sync.OnceValue(func() *schemaNode {
	return schemaNodeFor(reflect.TypeOf(Config{}))
})

// configSchema returns the memoized schema tree reflected from Config.
func configSchema() *schemaNode { return configSchemaOnce() }

var jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()

// schemaNodeFor reflects one type into its schema node: string-keyed maps
// become map nodes, plain structs become known-key nodes, and everything
// else — including custom-marshal types like Duration, whose JSON shape is
// not their field set — is a leaf (nil).
func schemaNodeFor(t reflect.Type) *schemaNode {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch {
	case t.Kind() == reflect.Map && t.Key().Kind() == reflect.String:
		return &schemaNode{mapNode: true, elem: schemaNodeFor(t.Elem())}
	case t.Kind() == reflect.Struct && !reflect.PointerTo(t).Implements(jsonMarshalerType):
		n := &schemaNode{known: map[string]*schemaNode{}}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			n.known[tag] = schemaNodeFor(f.Type)
		}
		return n
	default:
		return nil // leaf
	}
}
