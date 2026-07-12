package rag

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestPackedEmbeddingDeterministicRoundTrip(t *testing.T) {
	for _, dimension := range []int{1, 3, 768, 4096} {
		t.Run(strconv.Itoa(dimension), func(t *testing.T) {
			input := make([]float64, dimension)
			for i := range input {
				input[i] = math.Sin(float64(i)+0.25) / 3
			}

			first, err := encodeEmbedding(input)
			if err != nil {
				t.Fatalf("encodeEmbedding: %v", err)
			}
			second, err := encodeEmbedding(input)
			if err != nil {
				t.Fatalf("encodeEmbedding second call: %v", err)
			}
			if string(first) != string(second) {
				t.Fatal("encoding is not deterministic")
			}
			if got, want := len(first), packedEmbeddingHeaderSize+4*dimension; got != want {
				t.Fatalf("encoded bytes = %d, want %d", got, want)
			}

			got, format, err := decodeEmbedding(first)
			if err != nil {
				t.Fatalf("decodeEmbedding: %v", err)
			}
			if format != embeddingFormatPackedFloat32 {
				t.Fatalf("format = %q, want %q", format, embeddingFormatPackedFloat32)
			}
			if len(got) != dimension {
				t.Fatalf("decoded dimension = %d, want %d", len(got), dimension)
			}
			for i := range input {
				want := float64(float32(input[i]))
				if got[i] != want {
					t.Fatalf("component %d = %v, want float32 round-trip %v", i, got[i], want)
				}
			}
		})
	}
}

func TestPackedEmbeddingRejectsNonFiniteAndFloat32Overflow(t *testing.T) {
	tests := []struct {
		name  string
		value float64
		want  string
	}{
		{name: "nan", value: math.NaN(), want: "finite"},
		{name: "positive infinity", value: math.Inf(1), want: "finite"},
		{name: "negative infinity", value: math.Inf(-1), want: "finite"},
		{name: "float32 overflow", value: math.MaxFloat32 * 2, want: "float32"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := encodeEmbedding([]float64{tt.value})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("encodeEmbedding error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestPackedEmbeddingRejectsMalformedEnvelope(t *testing.T) {
	valid, err := encodeEmbedding([]float64{0.25, -0.5})
	if err != nil {
		t.Fatalf("encodeEmbedding: %v", err)
	}

	mutate := func(fn func([]byte)) []byte {
		b := append([]byte(nil), valid...)
		fn(b)
		return b
	}
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{name: "truncated header", data: valid[:packedEmbeddingHeaderSize-1], want: "truncated"},
		{name: "bad magic", data: mutate(func(b []byte) { b[0] ^= 0xff }), want: "encoding"},
		{name: "unknown version", data: mutate(func(b []byte) { b[4]++ }), want: "version"},
		{name: "unknown element type", data: mutate(func(b []byte) { b[5]++ }), want: "element type"},
		{name: "unknown byte order", data: mutate(func(b []byte) { b[6]++ }), want: "byte order"},
		{name: "nonzero reserved byte", data: mutate(func(b []byte) { b[7] = 1 }), want: "reserved"},
		{name: "zero dimension", data: mutate(func(b []byte) { binary.LittleEndian.PutUint32(b[8:12], 0) }), want: "dimension"},
		{name: "dimension payload mismatch", data: mutate(func(b []byte) { binary.LittleEndian.PutUint32(b[8:12], 3) }), want: "payload"},
		{name: "declared payload mismatch", data: mutate(func(b []byte) { binary.LittleEndian.PutUint32(b[12:16], 4) }), want: "payload"},
		{name: "truncated payload", data: valid[:len(valid)-1], want: "payload"},
		{name: "trailing bytes", data: append(append([]byte(nil), valid...), 0), want: "payload"},
		{name: "nonfinite payload", data: mutate(func(b []byte) { binary.LittleEndian.PutUint32(b[16:20], math.Float32bits(float32(math.Inf(1)))) }), want: "finite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := decodeEmbedding(tt.data)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("decodeEmbedding error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestPackedEmbeddingRejectsUnboundedDimension(t *testing.T) {
	header := make([]byte, packedEmbeddingHeaderSize)
	copy(header[:4], packedEmbeddingMagic)
	header[4] = packedEmbeddingVersion
	header[5] = packedEmbeddingElementFloat32
	header[6] = packedEmbeddingByteOrderLittleEndian
	binary.LittleEndian.PutUint32(header[8:12], maxEmbeddingDimension+1)
	binary.LittleEndian.PutUint32(header[12:16], 4)

	_, _, err := decodeEmbedding(header)
	if err == nil || !strings.Contains(err.Error(), "dimension") {
		t.Fatalf("decodeEmbedding error = %v, want bounded dimension error", err)
	}
}
