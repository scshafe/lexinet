package studyjson

import (
	"bytes"
	"strings"
	"testing"
)

func TestCanonicalizeUniqueReturnsRFC8785Bytes(t *testing.T) {
	canonical, err := CanonicalizeUnique([]byte(` { "z" : 1.0, "a" : [true, null] } `))
	if err != nil {
		t.Fatalf("canonicalize unique JSON: %v", err)
	}
	want := []byte(`{"a":[true,null],"z":1}`)
	if !bytes.Equal(canonical, want) {
		t.Fatalf("canonical JSON mismatch: got=%q want=%q", canonical, want)
	}
}

func TestCanonicalizeUniqueRejectsDuplicateNestedMember(t *testing.T) {
	if _, err := CanonicalizeUnique([]byte(`{"outer":{"same":1,"same":2}}`)); err == nil {
		t.Fatal("duplicate nested member was accepted")
	}
}

func TestCanonicalizeUniqueRejectsTrailingValue(t *testing.T) {
	if _, err := CanonicalizeUnique([]byte(`{} {}`)); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
}

func TestCanonicalizeUniqueRejectsInvalidUTF8(t *testing.T) {
	if _, err := CanonicalizeUnique([]byte{0xff}); err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
}

func TestCanonicalizeUniqueRejectsUnpairedUTF16Surrogates(t *testing.T) {
	testCases := [][]byte{
		[]byte(`{"value":"\ud800"}`),
		[]byte(`{"value":"\udc00"}`),
		[]byte(`{"value":"\ud800\u0041"}`),
		[]byte(`{"value":"\udc00\ud800"}`),
	}
	for _, input := range testCases {
		if _, err := CanonicalizeUnique(input); err == nil {
			t.Fatalf("unpaired UTF-16 surrogate was accepted: %q", input)
		}
	}
}

func TestCanonicalizeUniqueAcceptsPairedUTF16Surrogates(t *testing.T) {
	canonical, err := CanonicalizeUnique([]byte(`{"value":"\ud83d\ude00"}`))
	if err != nil {
		t.Fatalf("canonicalize paired surrogate: %v", err)
	}
	if want := []byte("{\"value\":\"😀\"}"); !bytes.Equal(canonical, want) {
		t.Fatalf("paired surrogate canonical JSON mismatch: got=%q want=%q", canonical, want)
	}
}

func TestCanonicalizeUniqueRejectsUnicodeNoncharacters(t *testing.T) {
	testCases := [][]byte{
		[]byte(`{"value":"\ufdd0"}`),
		[]byte(`{"value":"\uffff"}`),
		[]byte(`{"value":"\ud83f\udffe"}`),
		[]byte("{\"value\":\"\ufdd0\"}"),
		[]byte("{\"\uffff\":true}"),
	}
	for _, input := range testCases {
		if _, err := CanonicalizeUnique(input); err == nil {
			t.Fatalf("Unicode noncharacter was accepted: %q", input)
		}
	}
}

func TestCanonicalizeUniqueRejectsInexactOrNonFiniteIJSONNumbers(t *testing.T) {
	testCases := [][]byte{
		[]byte(`{"number":9007199254740992}`),
		[]byte(`{"number":9007199254740993}`),
		[]byte(`{"number":-9007199254740992}`),
		[]byte(`{"number":-9007199254740993}`),
		[]byte(`{"number":1e400}`),
	}
	for _, input := range testCases {
		if _, err := CanonicalizeUnique(input); err == nil {
			t.Fatalf("non-I-JSON number was accepted: %q", input)
		}
	}
}

func TestCanonicalizeUniqueAcceptsExactIntegerAndOrdinaryFraction(t *testing.T) {
	for _, input := range [][]byte{
		[]byte(`{"number":9007199254740991}`),
		[]byte(`{"number":0.1}`),
		[]byte(`{"number":1E30}`),
	} {
		if _, err := CanonicalizeUnique(input); err != nil {
			t.Fatalf("valid I-JSON number %q was rejected: %v", input, err)
		}
	}
}

func TestCanonicalizeUniqueErrorsNeverExposeInputBytes(t *testing.T) {
	const canary = "private-study-json-canary"
	_, err := CanonicalizeUnique([]byte(`{"` + canary + `":}`))
	if err == nil {
		t.Fatal("malformed canary JSON was accepted")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatal("canonicalization error exposed rejected JSON bytes")
	}
}

func TestCanonicalizeUniqueAcceptsMaximumBoundedDocument(t *testing.T) {
	prefix := []byte(`{"value":"`)
	suffix := []byte(`"}`)
	input := make([]byte, 0, MaximumDocumentBytes)
	input = append(input, prefix...)
	input = append(input, bytes.Repeat([]byte{'a'}, MaximumDocumentBytes-len(prefix)-len(suffix))...)
	input = append(input, suffix...)
	if len(input) != MaximumDocumentBytes {
		t.Fatalf("maximum document fixture size = %d", len(input))
	}
	if _, err := CanonicalizeUnique(input); err != nil {
		t.Fatalf("maximum bounded document was rejected: %v", err)
	}
}

func TestCanonicalizeUniqueRejectsDocumentOverByteBound(t *testing.T) {
	input := bytes.Repeat([]byte{' '}, MaximumDocumentBytes+1)
	if _, err := CanonicalizeUnique(input); err == nil {
		t.Fatal("document over byte bound was accepted")
	}
}

func TestCanonicalizeUniqueEnforcesNestingBound(t *testing.T) {
	maximum := []byte(strings.Repeat("[", MaximumNestingDepth) + "0" + strings.Repeat("]", MaximumNestingDepth))
	if _, err := CanonicalizeUnique(maximum); err != nil {
		t.Fatalf("maximum nesting depth was rejected: %v", err)
	}
	over := []byte(strings.Repeat("[", MaximumNestingDepth+1) + "0" + strings.Repeat("]", MaximumNestingDepth+1))
	if _, err := CanonicalizeUnique(over); err == nil {
		t.Fatal("nesting depth over bound was accepted")
	}
}
