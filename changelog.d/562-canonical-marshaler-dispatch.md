### Changed — signing refuses toolchain-dependent marshaler dispatch (#562)

`signing.MarshalCanonical` now returns `ErrAmbiguousMarshaler` for one input
shape: a field, slice element, or map value whose static type is an interface
embedding only `encoding.TextMarshaler` while its dynamic value also
implements `json.Marshaler`. `encoding/json` before Go 1.27 marshaled that
shape through the static route (`MarshalText`); the json-v2-backed
implementation in Go 1.27 marshals it through the dynamic value
(`MarshalJSON`), so its canonical bytes depended on the toolchain that built
the binary. Every other input canonicalizes exactly as before, including
`any` fields, interface types embedding `json.Marshaler`, and
`TextMarshaler`-only values behind `TextMarshaler`-only interfaces. No
shipped record type uses the refused shape.
