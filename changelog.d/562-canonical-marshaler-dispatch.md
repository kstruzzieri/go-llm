### Changed — signing refuses toolchain-dependent marshaler dispatch (#562)

`signing.MarshalCanonical` now returns `ErrAmbiguousMarshaler` for two input
shapes, in a field, slice element, or map value. First, a static type that is
an interface embedding only `encoding.TextMarshaler` while the dynamic value
also implements `json.Marshaler`: `encoding/json` before Go 1.27 marshaled it
through the static route (`MarshalText`) and the json-v2-backed
implementation in Go 1.27 marshals it through the dynamic value
(`MarshalJSON`). Second, a static `json.Marshaler` or `encoding.TextMarshaler`
interface type holding a typed nil pointer: the older encoder calls the
method on the nil pointer and the newer one panics inside the encoder. In
both cases the canonical bytes depended on the toolchain that built the
binary. Every other input canonicalizes exactly as before, including `any`
fields, nil concrete pointers, interface types embedding `json.Marshaler`,
and `TextMarshaler`-only values behind `TextMarshaler`-only interfaces. No
shipped record type uses either refused shape.
