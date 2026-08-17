// Package studydigest owns the canonical, purpose-separated hashes used by
// study scheduling and persistence. It is intentionally a standard-library
// leaf package so every layer can share the same bytes without dependency
// inversion.
package studydigest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
)

const (
	// Domain is the frozen study digest domain, including its terminating NUL.
	Domain = "lexinet-study-digest-v1\x00"

	// These bounds reject accidental unbounded hashing before any content is
	// processed. They comfortably exceed every frozen study.v1 wire field.
	MaximumParts        = 1 << 20
	MaximumPartBytes    = 64 << 10
	MaximumPayloadBytes = 64 << 20
)

// ErrorCode is a stable machine-readable digest failure code.
type ErrorCode string

const (
	ErrorCodeTooManyParts  ErrorCode = "STUDY_DIGEST_TOO_MANY_PARTS"
	ErrorCodePartTooLarge  ErrorCode = "STUDY_DIGEST_PART_TOO_LARGE"
	ErrorCodeInputTooLarge ErrorCode = "STUDY_DIGEST_INPUT_TOO_LARGE"
)

// Error reports only allowlisted size metadata. It never retains or renders a
// supplied digest part.
type Error struct {
	Code    ErrorCode         `json:"code"`
	Message string            `json:"message"`
	Context map[string]string `json:"context,omitempty"`
}

func (err *Error) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

func (err *Error) Is(target error) bool {
	digestTarget, ok := target.(*Error)
	return ok && err != nil && digestTarget != nil && digestTarget.Code == err.Code
}

// Configuration hashes an explicitly ordered scheduler configuration.
func Configuration(parts ...[]byte) (string, error) {
	return sum("configuration", parts)
}

// Tie hashes the explicitly ordered inputs to a deterministic scheduler tie.
func Tie(parts ...[]byte) (string, error) {
	return sum("tie", parts)
}

// CardContent hashes an immutable, explicitly framed card presentation.
func CardContent(parts ...[]byte) (string, error) {
	return sum("card-content", parts)
}

// CardSource hashes immutable source identities and UTC revisions.
func CardSource(parts ...[]byte) (string, error) {
	return sum("card-source", parts)
}

// Corpus hashes the sorted topic union and identity-sorted immutable cards.
func Corpus(parts ...[]byte) (string, error) {
	return sum("corpus", parts)
}

func sum(purpose string, parts [][]byte) (string, error) {
	if len(parts) > MaximumParts {
		return "", newError(
			ErrorCodeTooManyParts,
			"study digest has too many framed parts",
			"part_count",
			MaximumParts,
		)
	}

	payloadBytes := len(Domain) + 4 + len(purpose)
	for _, part := range parts {
		if len(part) > MaximumPartBytes {
			return "", newError(
				ErrorCodePartTooLarge,
				"study digest part exceeds the frozen bound",
				"part_bytes",
				MaximumPartBytes,
			)
		}
		if payloadBytes > MaximumPayloadBytes-4-len(part) {
			return "", newError(
				ErrorCodeInputTooLarge,
				"study digest input exceeds the frozen bound",
				"payload_bytes",
				MaximumPayloadBytes,
			)
		}
		payloadBytes += 4 + len(part)
	}

	hash := sha256.New()
	writeCanonicalPreimage(hash, purpose, parts)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type writer interface {
	Write([]byte) (int, error)
}

func writeFrame(destination writer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = destination.Write(length[:])
	_, _ = destination.Write(value)
}

func writeCanonicalPreimage(destination writer, purpose string, parts [][]byte) {
	_, _ = destination.Write([]byte(Domain))
	writeFrame(destination, []byte(purpose))
	for _, part := range parts {
		writeFrame(destination, part)
	}
}

func newError(code ErrorCode, message, field string, bound int) *Error {
	return &Error{
		Code:    code,
		Message: message,
		Context: map[string]string{
			"operation": "study_digest",
			"field":     field,
			"bound":     strconv.Itoa(bound),
		},
	}
}
