package studydigest

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestPurposeSeparatedGoldenVectors(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		purpose  string
		digest   func(...[]byte) (string, error)
		preimage string
		expected string
	}{
		{
			name: "configuration", purpose: "configuration", digest: Configuration,
			preimage: "6c6578696e65742d73747564792d6469676573742d7631000000000d636f6e66696775726174696f6e00000005616c70686100000000000000056f6d656761",
			expected: "8a4c7f2bab7dce3fa7ec0a41736e2e89272527c64393068eb6ea65cb10accad5",
		},
		{
			name: "tie", purpose: "tie", digest: Tie,
			preimage: "6c6578696e65742d73747564792d6469676573742d7631000000000374696500000005616c70686100000000000000056f6d656761",
			expected: "aefe7e01df254a7175bf3df6b08d716c9fa53cb3f650ff5cd1e4abd7be83df06",
		},
		{
			name: "card content", purpose: "card-content", digest: CardContent,
			preimage: "6c6578696e65742d73747564792d6469676573742d7631000000000c636172642d636f6e74656e7400000005616c70686100000000000000056f6d656761",
			expected: "532f67fd60b43e46f2349a8bed79a717b84942a9bdb48e7df8a73ca7deba7959",
		},
		{
			name: "card source", purpose: "card-source", digest: CardSource,
			preimage: "6c6578696e65742d73747564792d6469676573742d7631000000000b636172642d736f7572636500000005616c70686100000000000000056f6d656761",
			expected: "f4c46339257f5373780d2fde25be63e869c844beac7f01ef9427aee181f0c86b",
		},
		{
			name: "corpus", purpose: "corpus", digest: Corpus,
			preimage: "6c6578696e65742d73747564792d6469676573742d76310000000006636f7270757300000005616c70686100000000000000056f6d656761",
			expected: "eaf656bdfb10e1938124b0050c53dc76208f67db4b4ca94a590e7df0a8347a45",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var preimage bytes.Buffer
			writeCanonicalPreimage(&preimage, testCase.purpose, [][]byte{[]byte("alpha"), nil, []byte("omega")})
			if actualPreimage := hex.EncodeToString(preimage.Bytes()); actualPreimage != testCase.preimage {
				t.Fatalf("canonical preimage mismatch: got %s want %s", actualPreimage, testCase.preimage)
			}
			actual, err := testCase.digest([]byte("alpha"), nil, []byte("omega"))
			if err != nil {
				t.Fatalf("hash golden vector: %v", err)
			}
			if actual != testCase.expected {
				t.Fatalf("golden vector mismatch: got %s want %s", actual, testCase.expected)
			}
			if actual != strings.ToLower(actual) || len(actual) != 64 {
				t.Fatalf("digest is not lowercase SHA-256: %q", actual)
			}
		})
	}
}

func TestFramingAndPurposeSeparation(t *testing.T) {
	t.Parallel()

	joined, err := Tie([]byte("ab"), []byte("c"))
	if err != nil {
		t.Fatalf("hash joined input: %v", err)
	}
	split, err := Tie([]byte("a"), []byte("bc"))
	if err != nil {
		t.Fatalf("hash split input: %v", err)
	}
	configuration, err := Configuration([]byte("ab"), []byte("c"))
	if err != nil {
		t.Fatalf("hash purpose-separated input: %v", err)
	}
	if joined == split || joined == configuration {
		t.Fatal("canonical frames or purposes converged")
	}
}

func TestDigestBoundsAreStructuredAndRedacted(t *testing.T) {
	t.Parallel()

	canary := strings.Repeat("private-card-content", MaximumPartBytes/20+1)
	_, err := CardContent([]byte(canary))
	var digestError *Error
	if !errors.As(err, &digestError) || digestError.Code != ErrorCodePartTooLarge {
		t.Fatalf("oversized part error = %#v", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatal("digest error exposed supplied content")
	}

	parts := make([][]byte, MaximumPayloadBytes/MaximumPartBytes+1)
	sharedPart := make([]byte, MaximumPartBytes)
	for index := range parts {
		parts[index] = sharedPart
	}
	_, err = Corpus(parts...)
	if !errors.Is(err, &Error{Code: ErrorCodeInputTooLarge}) {
		t.Fatalf("oversized input error = %#v", err)
	}
}
