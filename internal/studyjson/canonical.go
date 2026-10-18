// Package studyjson owns the bounded JSON Canonicalization Scheme boundary for
// normalized study request identity documents.
package studyjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

var errInvalidStudyJSON = errors.New("study JSON is invalid or has duplicate members")

const (
	// MaximumDocumentBytes bounds allocation and work before the pinned
	// canonicalizer receives a document.
	MaximumDocumentBytes = 1 << 20
	// MaximumNestingDepth bounds recursive unique-member inspection and the
	// downstream recursive transform.
	MaximumNestingDepth = 64
)

// CanonicalizeUnique rejects non-I-JSON inputs with duplicate object members,
// then returns their exact RFC 8785 representation.
//
// @canonical study_json_jcs -- normalized study request identities cross this
// unique-member canonicalization leaf.
func CanonicalizeUnique(input []byte) ([]byte, error) {
	if len(input) == 0 || len(input) > MaximumDocumentBytes || !utf8.Valid(input) {
		return nil, errInvalidStudyJSON
	}
	if err := validateRawStringScalars(input); err != nil {
		return nil, errInvalidStudyJSON
	}
	if err := validateUniqueMembers(input); err != nil {
		return nil, errInvalidStudyJSON
	}
	canonical, err := jsoncanonicalizer.Transform(input)
	if err != nil {
		return nil, errInvalidStudyJSON
	}
	return canonical, nil
}

func validateUniqueMembers(input []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if err := consumeUniqueValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing study JSON value")
		}
		return err
	}
	return nil
}

func consumeUniqueValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		if number, ok := token.(json.Number); ok {
			return validateIJSONNumber(number)
		}
		return nil
	}
	depth++
	if depth > MaximumNestingDepth {
		return errors.New("study JSON exceeds the nesting bound")
	}
	switch delimiter {
	case '{':
		members := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("study JSON object member name is invalid")
			}
			if _, duplicate := members[name]; duplicate {
				return errors.New("duplicate study JSON object member")
			}
			members[name] = struct{}{}
			if err := consumeUniqueValue(decoder, depth); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("study JSON object is not terminated")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := consumeUniqueValue(decoder, depth); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("study JSON array is not terminated")
		}
		return nil
	default:
		return errors.New("unexpected study JSON delimiter")
	}
}

func validateRawStringScalars(input []byte) error {
	for index := 0; index < len(input); {
		if input[index] != '"' {
			index++
			continue
		}
		next, err := consumeRawJSONString(input, index)
		if err != nil {
			return err
		}
		index = next
	}
	return nil
}

func consumeRawJSONString(input []byte, openingQuote int) (int, error) {
	for index := openingQuote + 1; index < len(input); {
		character := input[index]
		switch {
		case character == '"':
			return index + 1, nil
		case character < 0x20:
			return 0, errors.New("study JSON string contains an unescaped control")
		case character == '\\':
			if index+1 >= len(input) {
				return 0, errors.New("study JSON string has a truncated escape")
			}
			escape := input[index+1]
			if escape != 'u' {
				switch escape {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				default:
					return 0, errors.New("study JSON string escape is invalid")
				}
				index += 2
				continue
			}
			first, next, err := decodeRawUTF16Escape(input, index)
			if err != nil {
				return 0, err
			}
			if first >= 0xd800 && first <= 0xdbff {
				if next+6 > len(input) || input[next] != '\\' || input[next+1] != 'u' {
					return 0, errors.New("study JSON string has an unpaired high surrogate")
				}
				second, afterSecond, err := decodeRawUTF16Escape(input, next)
				if err != nil || second < 0xdc00 || second > 0xdfff {
					return 0, errors.New("study JSON string has an invalid surrogate pair")
				}
				if isUnicodeNoncharacter(utf16.DecodeRune(rune(first), rune(second))) {
					return 0, errors.New("study JSON string contains a Unicode noncharacter")
				}
				index = afterSecond
				continue
			}
			if first >= 0xdc00 && first <= 0xdfff {
				return 0, errors.New("study JSON string has an unpaired low surrogate")
			}
			if isUnicodeNoncharacter(rune(first)) {
				return 0, errors.New("study JSON string contains a Unicode noncharacter")
			}
			index = next
		default:
			value, size := utf8.DecodeRune(input[index:])
			if size == 0 {
				return 0, errors.New("study JSON string UTF-8 is invalid")
			}
			if isUnicodeNoncharacter(value) {
				return 0, errors.New("study JSON string contains a Unicode noncharacter")
			}
			index += size
		}
	}
	return 0, errors.New("study JSON string is not terminated")
}

func isUnicodeNoncharacter(value rune) bool {
	return (value >= 0xfdd0 && value <= 0xfdef) ||
		(value >= 0 && value <= 0x10ffff && value&0xffff >= 0xfffe)
}

func decodeRawUTF16Escape(input []byte, slash int) (uint16, int, error) {
	if slash+6 > len(input) || input[slash] != '\\' || input[slash+1] != 'u' {
		return 0, 0, errors.New("study JSON Unicode escape is truncated")
	}
	value, err := strconv.ParseUint(string(input[slash+2:slash+6]), 16, 16)
	if err != nil {
		return 0, 0, errors.New("study JSON Unicode escape is invalid")
	}
	return uint16(value), slash + 6, nil
}

func validateIJSONNumber(number json.Number) error {
	literal := number.String()
	value, err := strconv.ParseFloat(literal, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return errors.New("study JSON number is outside finite IEEE 754")
	}
	// Normalized study request documents represent lossless int64 counters as
	// strings. Reject plain integer spellings outside the interoperable IEEE-754
	// range before transformation. Exponential/fractional JSON numbers remain
	// ordinary doubles, including the RFC 8785 conformance vectors.
	if !strings.ContainsAny(literal, ".eE") {
		integer, ok := new(big.Int).SetString(literal, 10)
		if !ok {
			return errors.New("study JSON integer syntax is invalid")
		}
		maximumExactInteger := big.NewInt(9007199254740991)
		if new(big.Int).Abs(integer).Cmp(maximumExactInteger) > 0 {
			return errors.New("study JSON integer exceeds interoperable IEEE 754 precision")
		}
	}
	return nil
}
