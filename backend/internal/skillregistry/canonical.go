package skillregistry

import (
	"bytes"
	"encoding/binary"
	"time"
)

// canonical.go -- the bytes a signature actually covers.
//
// # Why not "sign the JSON"
//
// A signature is only as good as the reader's ability to reconstruct, exactly,
// the bytes the signer signed. JSON cannot do that: object member order is
// unspecified, whitespace is free, numbers have several spellings, duplicate
// keys are legal in most parsers, and a field the reader's struct does not
// know about vanishes silently. Every one of those is a way for two parties to
// disagree about what was signed while both believing they agree -- and a
// signature over an ambiguous encoding is a signature over whatever the
// attacker can make the verifier reconstruct.
//
// So AO signs a purpose-built encoding with four properties:
//
//   - LENGTH-PREFIXED. Every string is preceded by its byte length, so no
//     separator exists for a value to contain. "a|b" and "a" + "b" cannot
//     collide, which is the classic canonicalization break.
//   - FIXED FIELD ORDER. Fields are written in the order this file declares,
//     not the order they arrived in. Reordering a payload cannot change its
//     encoding because the payload's order is never read.
//   - DOMAIN-SEPARATED. Every encoding opens with a purpose string. A release
//     signature can never verify as a key certificate, whatever the bytes look
//     like, because the two start with different domains.
//   - VERSIONED. The magic carries a version. A v2 encoding cannot be verified
//     by a v1 reader as though it were v1, and a v1 signature cannot be
//     replayed against a v2 payload.
//
// # Everything is explicit, including emptiness
//
// An absent optional field is encoded as a zero-length value, and its name is
// still written. That is what stops "aoMaxVersion missing" and
// "aoMaxVersion empty" from producing the same bytes as some other pair of
// fields shifted along by one.

// canonicalMagic identifies this encoding and its version. It is the first
// thing in every payload, so a reader that does not know this exact string
// cannot be tricked into treating the rest as a format it does know.
const canonicalMagic = "ao.trust.canonical/v1"

// Encoding tags. They distinguish a scalar from a list so that a one-element
// list and the scalar of the same value do not encode identically.
const (
	tagScalar byte = 0x01
	tagList   byte = 0x02
	tagNested byte = 0x03
)

// canonicalField is one named value in a payload.
type canonicalField struct {
	name string
	tag  byte
	// scalar holds the value for tagScalar.
	scalar string
	// list holds the elements for tagList and the pre-encoded sub-payloads for
	// tagNested. Element order is the CALLER's responsibility: a set that must
	// not depend on ordering is sorted before it gets here, because sorting
	// inside the encoder would silently repair a payload the signer meant to
	// order differently.
	list []string
}

func scalarField(name, value string) canonicalField {
	return canonicalField{name: name, tag: tagScalar, scalar: value}
}

func listField(name string, values []string) canonicalField {
	return canonicalField{name: name, tag: tagList, list: values}
}

func nestedField(name string, blobs []string) canonicalField {
	return canonicalField{name: name, tag: tagNested, list: blobs}
}

// canonicalEncode writes the payload for one purpose.
//
// The field count is written before the fields, so a payload cannot be
// extended by appending: a verifier reading three fields where four were
// signed reconstructs different bytes and the signature fails, rather than
// reconstructing a valid prefix.
func canonicalEncode(domain string, fields []canonicalField) []byte {
	var buf bytes.Buffer
	writeChunk(&buf, canonicalMagic)
	writeChunk(&buf, domain)
	writeCount(&buf, len(fields))
	for _, f := range fields {
		writeChunk(&buf, f.name)
		buf.WriteByte(f.tag)
		switch f.tag {
		case tagScalar:
			writeChunk(&buf, f.scalar)
		case tagList, tagNested:
			writeCount(&buf, len(f.list))
			for _, e := range f.list {
				writeChunk(&buf, e)
			}
		}
	}
	return buf.Bytes()
}

// oversized is the length written for anything that will not fit the 32-bit
// prefix, and it is deliberately a value no real chunk can carry.
//
// A silent wrap here would be a CANONICALIZATION BREAK -- two different inputs
// encoding to the same bytes is exactly what a length-prefixed format exists to
// prevent, and it is how a signature gets forged by choosing field values. So
// an over-long chunk writes this sentinel and its content is dropped, which
// makes the payload deterministic and impossible for any legitimate signature
// to match. Both sides fail closed: a verifier refuses, and the fixture signer
// produces a signature nothing accepts.
//
// It is unreachable in practice -- Release.Validate bounds every field this
// encoder sees, and a 4 GiB description is a memory problem long before it is a
// signing one -- and it is here because "unreachable" is not the same as
// "impossible".
const oversized uint32 = 0xFFFFFFFF

// maxChunkBytes is the largest chunk this encoding represents faithfully.
const maxChunkBytes = 1 << 30 // 1 GiB, far above anything a release carries.

func writeU32(buf *bytes.Buffer, n uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], n)
	buf.Write(b[:])
}

// writeCount writes a collection size. The bound is checked before the
// conversion, so the conversion cannot wrap.
func writeCount(buf *bytes.Buffer, n int) {
	if n < 0 || n > maxChunkBytes {
		writeU32(buf, oversized)
		return
	}
	writeU32(buf, uint32(n))
}

func writeChunk(buf *bytes.Buffer, s string) {
	n := len(s)
	if n < 0 || n > maxChunkBytes {
		writeU32(buf, oversized)
		return
	}
	writeU32(buf, uint32(n))
	buf.WriteString(s)
}

// canonicalTime is the one spelling of an instant this encoding accepts.
//
// UTC and whole seconds, because a signer and a verifier that disagree about a
// timezone offset or a trailing nanosecond disagree about the payload. The
// truncation is lossy on purpose: sub-second precision on a publication date
// buys nothing and costs a class of "it verifies on my machine".
func canonicalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}
