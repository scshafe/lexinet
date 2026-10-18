package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

const (
	studyClientDialTimeout            = 5 * time.Second
	studyClientTLSHandshakeTimeout    = 5 * time.Second
	studyClientResponseHeaderTimeout  = 5 * time.Second
	studyClientTotalTimeout           = 30 * time.Second
	studyClientMaximumHeaderBytes     = 64 * 1024
	studyClientMaximumRequestIDBytes  = 128
	studyClientMaximumRetryAfterBytes = 128
)

type studyClientFailure string

const (
	studyClientFailureEndpointInvalid studyClientFailure = "endpoint_invalid"
	studyClientFailureConfiguration   studyClientFailure = "configuration_invalid"
	studyClientFailureRequestInvalid  studyClientFailure = "request_invalid"
	studyClientFailureTransport       studyClientFailure = "transport_failed"
	studyClientFailureResponseRead    studyClientFailure = "response_read_failed"
	studyClientFailureResponseLarge   studyClientFailure = "response_too_large"
	studyClientFailureResponseInvalid studyClientFailure = "response_invalid"
	studyClientFailureService         studyClientFailure = "service_error"
	studyClientFailureOutput          studyClientFailure = "output_failed"
)

var studyClientFailureMessages = map[studyClientFailure]string{
	studyClientFailureEndpointInvalid: "study endpoint is invalid",
	studyClientFailureConfiguration:   "study client configuration is unavailable",
	studyClientFailureRequestInvalid:  "study request is invalid",
	studyClientFailureTransport:       "study service is unreachable",
	studyClientFailureResponseRead:    "study service response could not be read",
	studyClientFailureResponseLarge:   "study service response exceeds the protocol bound",
	studyClientFailureResponseInvalid: "study service response is invalid",
	studyClientFailureService:         "study service returned an error",
	studyClientFailureOutput:          "study response could not be written",
}

type studyClientSuccessKind uint8

const (
	studyClientSuccessCreateSession studyClientSuccessKind = iota + 1
	studyClientSuccessSessionState
	studyClientSuccessSync
)

// StudyClientError is the complete safe failure descriptor returned by the
// dedicated study HTTP boundary. It cannot retain response bodies, raw status
// text, transport errors, endpoints, bearer credentials, or request content.
type StudyClientError struct {
	StatusCode int                  `json:"status,omitempty"`
	Code       model.StudyErrorCode `json:"code,omitempty"`
	RequestID  string               `json:"request_id,omitempty"`
	RetryAfter string               `json:"retry_after,omitempty"`
	Retryable  bool                 `json:"retryable"`
	failure    studyClientFailure
}

// Error renders only the bounded allowlisted metadata already retained by the
// safe descriptor. No underlying error is chained across this trust boundary.
func (err *StudyClientError) Error() string {
	if err == nil {
		return ""
	}
	safeError := err.sanitized()
	message, ok := studyClientFailureMessages[safeError.failure]
	if !ok {
		message = studyClientFailureMessages[studyClientFailureResponseInvalid]
	}
	metadata := make([]string, 0, 5)
	if safeError.StatusCode != 0 {
		metadata = append(metadata, "status="+strconv.Itoa(safeError.StatusCode))
	}
	if safeError.Code.Valid() {
		metadata = append(metadata, "code="+string(safeError.Code))
	}
	if safeError.RequestID != "" {
		metadata = append(metadata, "request_id="+safeError.RequestID)
	}
	if safeError.RetryAfter != "" {
		metadata = append(metadata, "retry_after="+safeError.RetryAfter)
	}
	metadata = append(metadata, "retryable="+strconv.FormatBool(safeError.Retryable))
	return message + " (" + strings.Join(metadata, ", ") + ")"
}

// MarshalJSON re-applies the safety boundary even when a package peer creates
// a descriptor directly instead of receiving one from the HTTP client.
func (err *StudyClientError) MarshalJSON() ([]byte, error) {
	safeError := err.sanitized()
	type safeStudyClientError struct {
		StatusCode int                  `json:"status,omitempty"`
		Code       model.StudyErrorCode `json:"code,omitempty"`
		RequestID  string               `json:"request_id,omitempty"`
		RetryAfter string               `json:"retry_after,omitempty"`
		Retryable  bool                 `json:"retryable"`
	}
	return json.Marshal(safeStudyClientError{
		StatusCode: safeError.StatusCode,
		Code:       safeError.Code,
		RequestID:  safeError.RequestID,
		RetryAfter: safeError.RetryAfter,
		Retryable:  safeError.Retryable,
	})
}

func (err *StudyClientError) sanitized() StudyClientError {
	if err == nil {
		return StudyClientError{failure: studyClientFailureResponseInvalid}
	}
	safeError := StudyClientError{
		StatusCode: boundedStudyStatusCode(err.StatusCode),
		RequestID:  boundedStudyRequestID(err.RequestID),
		RetryAfter: boundedStudyRetryAfter(err.RetryAfter),
		failure:    err.failure,
	}
	if err.Code.Valid() {
		safeError.Code = err.Code
	}
	safeError.Retryable = deriveStudyRetryable(
		safeError.StatusCode,
		safeError.Code,
		safeError.failure == studyClientFailureTransport || safeError.failure == studyClientFailureResponseRead,
	)
	return safeError
}

// studyHTTPClient is the @canonical study_http_client. All study CLI traffic
// must cross this boundary so URL, TLS, redirect, deadline, size, and redaction
// policy cannot drift between commands.
type studyHTTPClient struct {
	endpoint    *url.URL
	bearerToken string
	httpClient  *http.Client
}

func newStudyHTTPClient(endpoint, bearerToken string) (*studyHTTPClient, error) {
	parsedEndpoint, err := parseStudyEndpoint(endpoint)
	if err != nil {
		return nil, &ExitError{Code: ExitUsage, Err: err}
	}
	transport := &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            (&net.Dialer{Timeout: studyClientDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           16,
		MaxIdleConnsPerHost:    4,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    studyClientTLSHandshakeTimeout,
		ResponseHeaderTimeout:  studyClientResponseHeaderTimeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: studyClientMaximumHeaderBytes,
		DisableCompression:     true,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &studyHTTPClient{
		endpoint:    parsedEndpoint,
		bearerToken: bearerToken,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   studyClientTotalTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func parseStudyEndpoint(endpoint string) (*url.URL, error) {
	if endpoint == "" || strings.TrimSpace(endpoint) != endpoint || !utf8.ValidString(endpoint) || strings.Contains(endpoint, "#") {
		return nil, newStudyClientError(studyClientFailureEndpointInvalid, 0, "", "")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return nil, newStudyClientError(studyClientFailureEndpointInvalid, 0, "", "")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	switch parsed.Scheme {
	case "https":
	case "http":
		if !isExplicitStudyLoopbackHost(parsed.Hostname()) {
			return nil, newStudyClientError(studyClientFailureEndpointInvalid, 0, "", "")
		}
	default:
		return nil, newStudyClientError(studyClientFailureEndpointInvalid, 0, "", "")
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed, nil
}

func isExplicitStudyLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (client *studyHTTPClient) post(ctx context.Context, path string, rawJSON []byte, successKind studyClientSuccessKind) ([]byte, error) {
	if len(rawJSON) == 0 || len(rawJSON) > studysession.StudyMaximumRequestBodyBytes {
		return nil, &ExitError{Code: ExitValidation, Err: newStudyClientError(studyClientFailureRequestInvalid, 0, "", "")}
	}
	return client.do(ctx, http.MethodPost, path, nil, rawJSON, successKind)
}

func (client *studyHTTPClient) get(ctx context.Context, path string, query url.Values, successKind studyClientSuccessKind) ([]byte, error) {
	return client.do(ctx, http.MethodGet, path, query, nil, successKind)
}

func (client *studyHTTPClient) do(ctx context.Context, method, requestPath string, query url.Values, rawJSON []byte, successKind studyClientSuccessKind) ([]byte, error) {
	if client == nil || client.endpoint == nil || client.httpClient == nil || ctx == nil ||
		(method != http.MethodGet && method != http.MethodPost) || !strings.HasPrefix(requestPath, "/") || !successKind.valid() {
		return nil, &ExitError{Code: ExitUnexpected, Err: newStudyClientError(studyClientFailureRequestInvalid, 0, "", "")}
	}
	requestURL := client.endpoint.JoinPath(strings.TrimPrefix(requestPath, "/"))
	requestURL.RawQuery = query.Encode()

	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader(rawJSON)
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return nil, &ExitError{Code: ExitUnexpected, Err: newStudyClientError(studyClientFailureRequestInvalid, 0, "", "")}
	}
	request.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
		// net/http can replay requests carrying GetBody on some connection
		// failures. Study mutations are application-idempotent but retries remain
		// an explicit caller decision, so the transport receives no replay body.
		request.GetBody = nil
	}
	if client.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+client.bearerToken)
	}

	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, &ExitError{Code: ExitUnreachable, Err: newStudyClientError(studyClientFailureTransport, 0, "", "")}
	}
	defer response.Body.Close()

	statusCode := boundedStudyStatusCode(response.StatusCode)
	requestID := boundedStudyRequestIDHeader(response.Header.Values("X-Request-ID"))
	if client.bearerToken != "" && strings.Contains(requestID, client.bearerToken) {
		requestID = ""
	}
	retryAfter := boundedStudyRetryAfterHeader(response.Header.Values("Retry-After"))
	responseJSON, readErr := readStudyResponseBody(response.Body)
	if readErr != nil {
		failure := studyClientFailureResponseRead
		if errors.Is(readErr, errStudyClientResponseTooLarge) {
			failure = studyClientFailureResponseLarge
		}
		clientErr := newStudyClientError(failure, statusCode, requestID, retryAfter)
		return nil, &ExitError{Code: studyClientExitCode(clientErr), Err: clientErr}
	}

	if !successKind.acceptsStatus(response.StatusCode) {
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			clientErr := newStudyClientError(studyClientFailureResponseInvalid, statusCode, requestID, retryAfter)
			return nil, &ExitError{Code: ExitUnexpected, Err: clientErr}
		}
		code := model.StudyErrorCode("")
		if validStudyResponseMedia(response.Header) {
			code = decodeStudyErrorCode(responseJSON)
		}
		clientErr := newStudyClientError(studyClientFailureService, statusCode, requestID, retryAfter)
		clientErr.Code = code
		clientErr.Retryable = deriveStudyRetryable(statusCode, code, false)
		return nil, &ExitError{Code: studyClientExitCode(clientErr), Err: clientErr}
	}
	if !validStudyResponseMedia(response.Header) || !validateStudySuccessResource(responseJSON, successKind) {
		clientErr := newStudyClientError(studyClientFailureResponseInvalid, statusCode, requestID, retryAfter)
		return nil, &ExitError{Code: ExitUnexpected, Err: clientErr}
	}
	return responseJSON, nil
}

func (kind studyClientSuccessKind) valid() bool {
	return kind == studyClientSuccessCreateSession || kind == studyClientSuccessSessionState || kind == studyClientSuccessSync
}

func (kind studyClientSuccessKind) acceptsStatus(statusCode int) bool {
	switch kind {
	case studyClientSuccessCreateSession:
		return statusCode == http.StatusOK || statusCode == http.StatusCreated
	case studyClientSuccessSessionState, studyClientSuccessSync:
		return statusCode == http.StatusOK
	default:
		return false
	}
}

func validStudyResponseMedia(header http.Header) bool {
	if header == nil {
		return false
	}
	contentTypes := header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return false
	}
	mediaType, parameters, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return false
	}
	for name, value := range parameters {
		if !strings.EqualFold(name, "charset") || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	contentEncodings := header.Values("Content-Encoding")
	return len(contentEncodings) == 0 || (len(contentEncodings) == 1 && strings.EqualFold(strings.TrimSpace(contentEncodings[0]), "identity"))
}

func validateStudySuccessResource(responseJSON []byte, successKind studyClientSuccessKind) bool {
	if len(responseJSON) == 0 || !utf8.Valid(responseJSON) || !json.Valid(responseJSON) {
		return false
	}
	members, ok := decodeUniqueStudyTopLevelObject(responseJSON)
	if !ok {
		return false
	}
	if !validateStudyProtocolEnvelope(members) {
		return false
	}
	switch successKind {
	case studyClientSuccessCreateSession:
		return validateStudySessionResource(responseJSON, members, true)
	case studyClientSuccessSessionState:
		return validateStudySessionResource(responseJSON, members, false)
	case studyClientSuccessSync:
		if !hasStudyResponseMembers(members, studySyncRequiredMembers[:]) ||
			!validNonzeroStudyUUIDMember(members["sync_id"]) {
			return false
		}
		var resource studysession.StudySyncResource
		if err := json.Unmarshal(responseJSON, &resource); err != nil || !validateStudySyncResource(resource) {
			return false
		}
		sessionJSON := members["session"]
		sessionMembers, sessionOK := decodeUniqueStudyTopLevelObject(sessionJSON)
		return sessionOK && validateStudyProtocolEnvelope(sessionMembers) &&
			validateStudySessionResource(sessionJSON, sessionMembers, false) &&
			resource.Session.SessionID == resource.FlushReceipt.SessionID
	default:
		return false
	}
}

var studySessionRequiredMembers = [...]string{
	"protocol_version",
	"schema_version",
	"required_semantics",
	"session_id",
	"profile_ref",
	"scope",
	"lifecycle",
	"creation_receipt",
	"predecessor_session_id",
	"successor_session_id",
	"corpus_sha256",
	"profile_ingestion_high_water",
	"state_version",
	"scheduler",
	"page",
	"generated_at",
}

var studySyncRequiredMembers = [...]string{
	"protocol_version",
	"schema_version",
	"required_semantics",
	"sync_id",
	"normalized_request_sha256",
	"client_stream_id",
	"adjudicated_through_sequence",
	"profile_ingestion_high_water",
	"receipts",
	"flush_receipt",
	"session",
	"generated_at",
}

func validateStudyProtocolEnvelope(members map[string]json.RawMessage) bool {
	var protocolVersion string
	var schemaVersion int
	var requiredSemantics []string
	if err := json.Unmarshal(members["protocol_version"], &protocolVersion); err != nil || protocolVersion != model.StudyProtocolVersion {
		return false
	}
	if err := json.Unmarshal(members["schema_version"], &schemaVersion); err != nil || schemaVersion != model.StudySchemaVersion {
		return false
	}
	if err := json.Unmarshal(members["required_semantics"], &requiredSemantics); err != nil {
		return false
	}
	canonicalSemantics := studysession.RequiredStudySemantics()
	if len(requiredSemantics) != len(canonicalSemantics) {
		return false
	}
	for index := range canonicalSemantics {
		if requiredSemantics[index] != canonicalSemantics[index] {
			return false
		}
	}
	return true
}

func validateStudySessionResource(rawJSON []byte, members map[string]json.RawMessage, requireCreationReceipt bool) bool {
	if !hasStudyResponseMembers(members, studySessionRequiredMembers[:]) ||
		!validNonzeroStudyUUIDMember(members["session_id"]) {
		return false
	}
	var resource studysession.StudySessionResource
	if err := json.Unmarshal(rawJSON, &resource); err != nil {
		return false
	}
	if resource.SessionID == uuid.Nil || resource.ProtocolVersion != model.StudyProtocolVersion ||
		resource.SchemaVersion != model.StudySchemaVersion || !resource.Lifecycle.Valid() ||
		!resource.Scope.CardKind.Valid() || len(resource.Scope.TopicRefs) < 1 ||
		len(resource.Scope.TopicRefs) > model.StudyMaximumTopics ||
		strings.TrimSpace(resource.ProfileRef) == "" || !model.IsStudySHA256(resource.CorpusSHA256) ||
		resource.ProfileIngestionHighWater < 0 || resource.StateVersion < 1 ||
		resource.GeneratedAt.IsZero() || !validateStudySessionPageResource(members["page"], resource.Page) {
		return false
	}
	if requireCreationReceipt && resource.CreationReceipt == nil {
		return false
	}
	if !requireCreationReceipt && resource.CreationReceipt != nil {
		return false
	}
	if resource.CreationReceipt != nil && !validateStudyCreationReceiptResource(*resource.CreationReceipt, resource.SessionID) {
		return false
	}
	if (resource.PredecessorSessionID != nil && (*resource.PredecessorSessionID == uuid.Nil || *resource.PredecessorSessionID == resource.SessionID)) ||
		(resource.SuccessorSessionID != nil && (*resource.SuccessorSessionID == uuid.Nil || *resource.SuccessorSessionID == resource.SessionID)) ||
		(resource.PredecessorSessionID != nil && resource.SuccessorSessionID != nil && *resource.PredecessorSessionID == *resource.SuccessorSessionID) {
		return false
	}
	if resource.Scheduler != nil && !validateStudySchedulerResource(*resource.Scheduler, resource.ProfileIngestionHighWater) {
		return false
	}
	switch resource.Lifecycle {
	case model.StudySessionLifecycleActive:
		return resource.Scheduler != nil && resource.SuccessorSessionID == nil
	case model.StudySessionLifecycleCompleted:
		return resource.Scheduler != nil && resource.SuccessorSessionID != nil
	case model.StudySessionLifecycleWaitingForCorpus:
		return resource.Scheduler == nil && resource.SuccessorSessionID == nil && len(resource.Page.Items) == 0
	case model.StudySessionLifecycleSupersededEmpty:
		return resource.Scheduler == nil && resource.SuccessorSessionID != nil && len(resource.Page.Items) == 0
	case model.StudySessionLifecycleAbandoned:
		return resource.SuccessorSessionID == nil
	default:
		return false
	}
}

func validateStudyCreationReceiptResource(receipt studysession.StudyCreationReceiptResource, sessionID uuid.UUID) bool {
	return receipt.CreationRequestID != uuid.Nil && model.IsStudySHA256(receipt.NormalizedRequestSHA256) &&
		receipt.Disposition.Valid() && receipt.CanonicalSessionID == sessionID
}

func validateStudySchedulerResource(scheduler studysession.StudySchedulerResource, liveHighWater int64) bool {
	return strings.TrimSpace(scheduler.Name) != "" && strings.TrimSpace(scheduler.Version) != "" &&
		model.IsStudySHA256(scheduler.ConfigurationSHA256) && scheduler.InputProfileIngestionHighWater >= 0 &&
		scheduler.InputProfileIngestionHighWater <= liveHighWater && !scheduler.PlannedAt.IsZero() &&
		strings.TrimSpace(scheduler.Seed) != ""
}

func validateStudySessionPageResource(rawJSON json.RawMessage, page studysession.StudySessionPageResource) bool {
	members, ok := decodeUniqueStudyTopLevelObject(rawJSON)
	if !ok || !hasStudyResponseMembers(members, []string{"after_position", "next_after_position", "complete", "items"}) {
		return false
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		return false
	}
	_, err = model.DecodeExactStudySessionPageDocument(encoded)
	return err == nil
}

func validateStudySyncResource(resource studysession.StudySyncResource) bool {
	if resource.SyncID == uuid.Nil || !model.IsStudySHA256(resource.NormalizedRequestSHA256) ||
		resource.ClientStreamID == uuid.Nil || resource.AdjudicatedThroughSequence < 0 ||
		resource.ProfileIngestionHighWater < 0 || resource.Receipts == nil ||
		len(resource.Receipts) > model.StudyMaximumEvents || resource.GeneratedAt.IsZero() {
		return false
	}
	flush := resource.FlushReceipt
	if flush.ReceiptID == uuid.Nil || flush.SessionID == uuid.Nil || flush.SyncID != resource.SyncID ||
		flush.ClientStreamID != resource.ClientStreamID ||
		flush.AcknowledgedThroughSequence != resource.AdjudicatedThroughSequence ||
		flush.ProfileIngestionHighWater != resource.ProfileIngestionHighWater ||
		flush.StateVersion < 1 || flush.IssuedAt.IsZero() {
		return false
	}
	previousSequence := int64(0)
	seenEventIDs := make(map[uuid.UUID]struct{}, len(resource.Receipts))
	for _, receipt := range resource.Receipts {
		if receipt.ClientSequence < 1 || receipt.ClientSequence <= previousSequence ||
			receipt.ClientSequence > resource.AdjudicatedThroughSequence || receipt.EventID == uuid.Nil ||
			!model.IsStudySHA256(receipt.PayloadSHA256) || !receipt.Disposition.Valid() ||
			!receipt.Code.ValidFor(receipt.Disposition) {
			return false
		}
		if _, duplicate := seenEventIDs[receipt.EventID]; duplicate {
			return false
		}
		seenEventIDs[receipt.EventID] = struct{}{}
		previousSequence = receipt.ClientSequence
		switch receipt.Disposition {
		case model.StudyEventReceiptDispositionAccepted, model.StudyEventReceiptDispositionSemanticDuplicate:
			if receipt.CanonicalEventID == nil || *receipt.CanonicalEventID == uuid.Nil ||
				receipt.ProfileIngestionOrdinal == nil || *receipt.ProfileIngestionOrdinal < 1 ||
				*receipt.ProfileIngestionOrdinal > resource.ProfileIngestionHighWater {
				return false
			}
		case model.StudyEventReceiptDispositionTerminalRejected:
			if receipt.CanonicalEventID != nil || receipt.ProfileIngestionOrdinal != nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func hasStudyResponseMembers(members map[string]json.RawMessage, required []string) bool {
	for _, name := range required {
		if _, ok := members[name]; !ok {
			return false
		}
	}
	return true
}

func decodeUniqueStudyTopLevelObject(rawJSON []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(rawJSON))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, false
	}
	members := make(map[string]json.RawMessage)
	for decoder.More() {
		memberToken, tokenErr := decoder.Token()
		memberName, nameOK := memberToken.(string)
		if tokenErr != nil || !nameOK {
			return nil, false
		}
		if _, duplicate := members[memberName]; duplicate {
			return nil, false
		}
		var member json.RawMessage
		if err := decoder.Decode(&member); err != nil {
			return nil, false
		}
		members[memberName] = member
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return members, true
}

func validNonzeroStudyUUIDMember(rawJSON json.RawMessage) bool {
	var value string
	if err := json.Unmarshal(rawJSON, &value); err != nil {
		return false
	}
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil
}

var errStudyClientResponseTooLarge = errors.New("study response exceeds bound")

func readStudyResponseBody(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, io.ErrUnexpectedEOF
	}
	responseJSON, err := io.ReadAll(io.LimitReader(body, studysession.StudyMaximumResponseBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(responseJSON) > studysession.StudyMaximumResponseBodyBytes {
		return nil, errStudyClientResponseTooLarge
	}
	return responseJSON, nil
}

func decodeStudyErrorCode(responseJSON []byte) model.StudyErrorCode {
	if len(responseJSON) == 0 || !utf8.Valid(responseJSON) || !json.Valid(responseJSON) {
		return ""
	}
	topLevel, ok := decodeUniqueStudyTopLevelObject(responseJSON)
	if !ok {
		return ""
	}
	errorObject, ok := decodeUniqueStudyTopLevelObject(topLevel["error"])
	if !ok {
		return ""
	}
	var protocolVersion string
	var schemaVersion int
	var code model.StudyErrorCode
	if err := json.Unmarshal(errorObject["protocol_version"], &protocolVersion); err != nil || protocolVersion != model.StudyProtocolVersion {
		return ""
	}
	if err := json.Unmarshal(errorObject["schema_version"], &schemaVersion); err != nil || schemaVersion != model.StudySchemaVersion {
		return ""
	}
	if err := json.Unmarshal(errorObject["code"], &code); err != nil || !code.Valid() {
		return ""
	}
	return code
}

func newStudyClientError(failure studyClientFailure, statusCode int, requestID, retryAfter string) *StudyClientError {
	return &StudyClientError{
		StatusCode: boundedStudyStatusCode(statusCode),
		RequestID:  boundedStudyRequestID(requestID),
		RetryAfter: boundedStudyRetryAfter(retryAfter),
		Retryable:  deriveStudyRetryable(statusCode, "", failure == studyClientFailureTransport || failure == studyClientFailureResponseRead),
		failure:    failure,
	}
}

func boundedStudyStatusCode(statusCode int) int {
	if statusCode < 100 || statusCode > 599 {
		return 0
	}
	return statusCode
}

func boundedStudyRequestID(value string) string {
	if value == "" || len(value) > studyClientMaximumRequestIDBytes || !utf8.ValidString(value) {
		return ""
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' || character == ':' || character == '-') {
			return ""
		}
	}
	return value
}

func boundedStudyRequestIDHeader(values []string) string {
	if len(values) != 1 {
		return ""
	}
	return boundedStudyRequestID(values[0])
}

func boundedStudyRetryAfter(value string) string {
	if value == "" || len(value) > studyClientMaximumRetryAfterBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return ""
	}
	allDigits := true
	for _, character := range value {
		if character < '0' || character > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return value
	}
	if _, err := http.ParseTime(value); err == nil {
		return value
	}
	return ""
}

func boundedStudyRetryAfterHeader(values []string) string {
	if len(values) != 1 {
		return ""
	}
	return boundedStudyRetryAfter(values[0])
}

func deriveStudyRetryable(statusCode int, code model.StudyErrorCode, transportFailure bool) bool {
	if transportFailure {
		return true
	}
	return statusCode == http.StatusTooManyRequests || statusCode == http.StatusServiceUnavailable ||
		code == model.StudyErrorCodeRateLimited || code == model.StudyErrorCodeServiceUnavailable
}

func studyClientExitCode(err *StudyClientError) int {
	if err == nil {
		return ExitUnexpected
	}
	if err.Retryable {
		return ExitUnreachable
	}
	switch err.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ExitAuth
	case http.StatusNotFound:
		return ExitNotFound
	case http.StatusBadRequest, http.StatusConflict, http.StatusRequestEntityTooLarge:
		return ExitValidation
	default:
		return ExitUnexpected
	}
}

func writeStudyRawResponse(writer io.Writer, responseJSON []byte) error {
	if writer == nil || len(responseJSON) == 0 || len(responseJSON) > studysession.StudyMaximumResponseBodyBytes || !json.Valid(responseJSON) {
		return newStudyClientError(studyClientFailureResponseInvalid, 0, "", "")
	}
	if _, err := writer.Write(responseJSON); err != nil {
		return newStudyClientError(studyClientFailureOutput, 0, "", "")
	}
	if responseJSON[len(responseJSON)-1] != '\n' {
		if _, err := writer.Write([]byte{'\n'}); err != nil {
			return newStudyClientError(studyClientFailureOutput, 0, "", "")
		}
	}
	return nil
}
