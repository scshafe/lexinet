package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

const studyMaximumJSONNestingDepth = 64

type studyHTTPErrorEnvelope struct {
	Error studyHTTPError `json:"error"`
}

type studyHTTPError struct {
	ProtocolVersion string               `json:"protocol_version"`
	SchemaVersion   int                  `json:"schema_version"`
	Code            model.StudyErrorCode `json:"code"`
	Message         string               `json:"message"`
}

// @canonical study_http_transport_validation -- raw study requests cross this
// exact-member, duplicate-aware decoder before any service invocation.
func decodeStrictStudyJSON(r *http.Request, destination any) error {
	if r == nil || destination == nil || !validStudyJSONMediaType(r.Header.Values("Content-Type")) ||
		!validStudyContentEncoding(r.Header.Values("Content-Encoding")) {
		return studyTransportError(model.StudyErrorCodeSchemaInvalid)
	}
	if r.ContentLength > studysession.StudyMaximumRequestBodyBytes {
		return studyTransportError(model.StudyErrorCodeRequestTooLarge)
	}

	rawJSON, err := io.ReadAll(io.LimitReader(r.Body, studysession.StudyMaximumRequestBodyBytes+1))
	if err != nil {
		return studyTransportError(model.StudyErrorCodeSchemaInvalid)
	}
	if len(rawJSON) > studysession.StudyMaximumRequestBodyBytes {
		return studyTransportError(model.StudyErrorCodeRequestTooLarge)
	}
	trimmedJSON := bytes.TrimSpace(rawJSON)
	if len(trimmedJSON) == 0 || trimmedJSON[0] != '{' || !utf8.Valid(rawJSON) {
		return studyTransportError(model.StudyErrorCodeSchemaInvalid)
	}
	if err := validateStrictStudyJSONStructure(rawJSON, reflect.TypeOf(destination)); err != nil {
		return studyTransportError(model.StudyErrorCodeSchemaInvalid)
	}

	decoder := json.NewDecoder(bytes.NewReader(rawJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return studyTransportError(model.StudyErrorCodeSchemaInvalid)
	}
	if err := requireStudyJSONEOF(decoder); err != nil {
		return studyTransportError(model.StudyErrorCodeSchemaInvalid)
	}
	return nil
}

func validStudyJSONMediaType(values []string) bool {
	if len(values) != 1 {
		return false
	}
	mediaType, parameters, err := mime.ParseMediaType(values[0])
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return false
	}
	for name, value := range parameters {
		if !strings.EqualFold(name, "charset") || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

func validStudyContentEncoding(values []string) bool {
	if len(values) == 0 {
		return true
	}
	return len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "identity")
}

func validateStrictStudyJSONStructure(rawJSON []byte, destinationType reflect.Type) error {
	for destinationType.Kind() == reflect.Pointer {
		destinationType = destinationType.Elem()
	}
	decoder := json.NewDecoder(bytes.NewReader(rawJSON))
	decoder.UseNumber()
	if err := validateStudyJSONValue(decoder, destinationType, 0); err != nil {
		return err
	}
	return requireStudyJSONEOF(decoder)
}

func validateStudyJSONValue(decoder *json.Decoder, expectedType reflect.Type, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return io.ErrUnexpectedEOF
	}
	if depth >= studyMaximumJSONNestingDepth {
		return &json.SyntaxError{}
	}

	expectedType = dereferenceStudyJSONType(expectedType)
	switch delimiter {
	case '{':
		fieldTypes := studyJSONFieldTypes(expectedType)
		seen := make(map[string]struct{}, len(fieldTypes))
		for decoder.More() {
			memberToken, memberErr := decoder.Token()
			if memberErr != nil {
				return memberErr
			}
			memberName, ok := memberToken.(string)
			if !ok {
				return &json.SyntaxError{}
			}
			if _, duplicate := seen[memberName]; duplicate {
				return &json.SyntaxError{}
			}
			seen[memberName] = struct{}{}

			memberType, known := fieldTypes[memberName]
			if expectedType != nil && expectedType.Kind() == reflect.Struct && !known {
				return &json.UnmarshalFieldError{}
			}
			if err := validateStudyJSONValue(decoder, memberType, depth+1); err != nil {
				return err
			}
		}
		closing, closingErr := decoder.Token()
		if closingErr != nil || closing != json.Delim('}') {
			if closingErr != nil {
				return closingErr
			}
			return io.ErrUnexpectedEOF
		}
	case '[':
		var elementType reflect.Type
		if expectedType != nil && (expectedType.Kind() == reflect.Slice || expectedType.Kind() == reflect.Array) {
			elementType = expectedType.Elem()
		}
		for decoder.More() {
			if err := validateStudyJSONValue(decoder, elementType, depth+1); err != nil {
				return err
			}
		}
		closing, closingErr := decoder.Token()
		if closingErr != nil || closing != json.Delim(']') {
			if closingErr != nil {
				return closingErr
			}
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

func dereferenceStudyJSONType(valueType reflect.Type) reflect.Type {
	for valueType != nil && valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	return valueType
}

func studyJSONFieldTypes(valueType reflect.Type) map[string]reflect.Type {
	valueType = dereferenceStudyJSONType(valueType)
	if valueType == nil || valueType.Kind() != reflect.Struct {
		return nil
	}
	fields := make(map[string]reflect.Type, valueType.NumField())
	for index := 0; index < valueType.NumField(); index++ {
		field := valueType.Field(index)
		if !field.IsExported() {
			continue
		}
		jsonTag := field.Tag.Get("json")
		memberName := strings.Split(jsonTag, ",")[0]
		if memberName == "-" {
			continue
		}
		if memberName == "" {
			memberName = field.Name
		}
		fields[memberName] = field.Type
	}
	return fields
}

func requireStudyJSONEOF(decoder *json.Decoder) error {
	if decoder == nil {
		return io.ErrUnexpectedEOF
	}
	_, err := decoder.Token()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	return &json.SyntaxError{}
}

func decodeStudyPageQuery(rawQuery string) (studysession.StudyPageRequest, error) {
	page := studysession.StudyPageRequest{AfterPosition: -1, Limit: 16}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return studysession.StudyPageRequest{}, studyTransportError(model.StudyErrorCodeSchemaInvalid)
	}
	for name, entries := range values {
		if (name != "page_after_position" && name != "page_limit") || len(entries) != 1 || entries[0] == "" {
			return studysession.StudyPageRequest{}, studyTransportError(model.StudyErrorCodeSchemaInvalid)
		}
		switch name {
		case "page_after_position":
			parsed, parseErr := strconv.ParseInt(entries[0], 10, 32)
			if parseErr != nil || parsed < -1 {
				return studysession.StudyPageRequest{}, studyTransportError(model.StudyErrorCodeSchemaInvalid)
			}
			page.AfterPosition = int(parsed)
		case "page_limit":
			parsed, parseErr := strconv.ParseInt(entries[0], 10, 32)
			if parseErr != nil || parsed < 1 || parsed > studysession.StudyMaximumPageItems {
				return studysession.StudyPageRequest{}, studyTransportError(model.StudyErrorCodeSchemaInvalid)
			}
			page.Limit = int(parsed)
		}
	}
	return page, nil
}

func parseStudySessionID(value string) (uuid.UUID, error) {
	sessionID, err := uuid.Parse(value)
	if err != nil || sessionID == uuid.Nil {
		return uuid.Nil, studyTransportError(model.StudyErrorCodeSchemaInvalid)
	}
	return sessionID, nil
}

func writeStudyResponseJSON(w http.ResponseWriter, status int, responseJSON json.RawMessage) bool {
	if len(responseJSON) == 0 || len(responseJSON) > studysession.StudyMaximumResponseBodyBytes ||
		!utf8.Valid(responseJSON) || !json.Valid(responseJSON) {
		writeStudyError(w, studyTransportError(model.StudyErrorCodeInternal))
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(responseJSON)
	return true
}

func writeStudyError(w http.ResponseWriter, err error) {
	publicError := studysession.PublicStudyError(err)
	status := studyHTTPStatus(publicError.Code)
	envelope := studyHTTPErrorEnvelope{Error: studyHTTPError{
		ProtocolVersion: model.StudyProtocolVersion,
		SchemaVersion:   model.StudySchemaVersion,
		Code:            publicError.Code,
		Message:         publicError.Message,
	}}
	encoded, marshalErr := json.Marshal(envelope)
	if marshalErr != nil || len(encoded) > studysession.StudyMaximumResponseBodyBytes {
		status = http.StatusInternalServerError
		encoded = []byte(`{"error":{"protocol_version":"study.v1","schema_version":1,"code":"INTERNAL","message":"study service failed"}}`)
	}
	if publicError.Retryable && (publicError.Code == model.StudyErrorCodeRateLimited || publicError.Code == model.StudyErrorCodeServiceUnavailable) {
		w.Header().Set("Retry-After", "1")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func studyHTTPStatus(code model.StudyErrorCode) int {
	switch code {
	case model.StudyErrorCodeSchemaInvalid:
		return http.StatusBadRequest
	case model.StudyErrorCodeUnauthorized:
		return http.StatusUnauthorized
	case model.StudyErrorCodeAuthorizationDenied:
		return http.StatusForbidden
	case model.StudyErrorCodeProfileNotFound, model.StudyErrorCodeSessionNotFound:
		return http.StatusNotFound
	case model.StudyErrorCodeIdempotencyConflict,
		model.StudyErrorCodeCycleNotExhausted,
		model.StudyErrorCodeFlushReceiptRequired,
		model.StudyErrorCodeFlushReceiptStale,
		model.StudyErrorCodeStateStale,
		model.StudyErrorCodeStreamSequenceGap:
		return http.StatusConflict
	case model.StudyErrorCodeRequestTooLarge:
		return http.StatusRequestEntityTooLarge
	case model.StudyErrorCodeRateLimited:
		return http.StatusTooManyRequests
	case model.StudyErrorCodeServiceUnavailable:
		return http.StatusServiceUnavailable
	case model.StudyErrorCodeInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

func studyTransportError(code model.StudyErrorCode) *model.StudyError {
	return &model.StudyError{Code: code}
}
