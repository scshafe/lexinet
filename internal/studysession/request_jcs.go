package studysession

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/studyjson"
)

var studyReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// CanonicalizeStudyRequestJSON transforms one unique-member I-JSON value into
// RFC 8785 bytes. All normalized request hashes must cross this service entry
// point, which delegates the shared scalar/member/work-bound rules to
// internal/studyjson.
//
// @canonical study_request_jcs -- request identity never hashes raw request
// bytes or ordinary encoding/json output.
func CanonicalizeStudyRequestJSON(input []byte) ([]byte, error) {
	canonical, err := studyjson.CanonicalizeUnique(input)
	if err != nil {
		return nil, newStudySessionError(model.StudyErrorCodeSchemaInvalid)
	}
	return canonical, nil
}

func canonicalizeNormalizedStudyRequest(value any) ([]byte, string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, "", newStudySessionError(model.StudyErrorCodeInternal)
	}
	canonical, err := CanonicalizeStudyRequestJSON(encoded)
	if err != nil {
		return nil, "", newStudySessionError(model.StudyErrorCodeInternal)
	}
	return canonical, model.SHA256Hex(canonical), nil
}

type normalizedCreateStudyRequest struct {
	Operation       string                      `json:"operation"`
	ProtocolVersion string                      `json:"protocol_version"`
	SchemaVersion   int                         `json:"schema_version"`
	ProfileRef      string                      `json:"profile_ref"`
	Scope           normalizedCreateStudyScope  `json:"scope"`
	Predecessor     *normalizedStudyPredecessor `json:"predecessor"`
	Page            StudyPageRequest            `json:"page"`
}

type normalizedCreateStudyScope struct {
	TopicIDs []string            `json:"topic_ids"`
	CardKind model.StudyCardKind `json:"card_kind"`
}

type normalizedStudyPredecessor struct {
	SessionID      string `json:"session_id"`
	FlushReceiptID string `json:"flush_receipt_id"`
}

func normalizeCreateStudyRequest(
	request StudyCreateSessionRequest,
	resolvedScope model.StudyNormalizedScope,
) (string, error) {
	if err := validateCreateStudyRequestBeforeResolution(request); err != nil {
		return "", err
	}
	if err := validateResolvedStudyScope(request.Scope.CardKind, resolvedScope); err != nil {
		return "", err
	}
	topicIDs := make([]string, len(resolvedScope.Scope.TopicIDs))
	for index, topicID := range resolvedScope.Scope.TopicIDs {
		topicIDs[index] = topicID.String()
	}
	sort.Strings(topicIDs)

	var predecessor *normalizedStudyPredecessor
	if request.Predecessor != nil {
		predecessor = &normalizedStudyPredecessor{
			SessionID:      request.Predecessor.SessionID.String(),
			FlushReceiptID: request.Predecessor.FlushReceiptID.String(),
		}
	}
	_, digest, err := canonicalizeNormalizedStudyRequest(normalizedCreateStudyRequest{
		Operation:       "study_session_create_or_resume",
		ProtocolVersion: model.StudyProtocolVersion,
		SchemaVersion:   model.StudySchemaVersion,
		ProfileRef:      request.ProfileRef,
		Scope: normalizedCreateStudyScope{
			TopicIDs: topicIDs,
			CardKind: resolvedScope.Scope.CardKind,
		},
		Predecessor: predecessor,
		Page:        request.Page,
	})
	return digest, err
}

type normalizedSyncStudyRequest struct {
	Operation            string                 `json:"operation"`
	ProtocolVersion      string                 `json:"protocol_version"`
	SchemaVersion        int                    `json:"schema_version"`
	SessionID            string                 `json:"session_id"`
	ClientStreamID       string                 `json:"client_stream_id"`
	ExpectedStateVersion string                 `json:"expected_state_version"`
	Events               []normalizedStudyEvent `json:"events"`
	Page                 StudyPageRequest       `json:"page"`
}

type normalizedStudyEvent struct {
	EventID            string                        `json:"event_id"`
	ClientSequence     string                        `json:"client_sequence"`
	ProfileRef         string                        `json:"profile_ref"`
	SessionItemID      string                        `json:"session_item_id"`
	CardIdentity       string                        `json:"card_identity"`
	ContentSHA256      string                        `json:"content_sha256"`
	Kind               model.StudyEventKind          `json:"kind"`
	Rating             *model.StudyRating            `json:"rating"`
	RatingEventID      *string                       `json:"rating_event_id"`
	ClientObservedAt   *string                       `json:"client_observed_at"`
	ClientClockQuality model.StudyClientClockQuality `json:"client_clock_quality"`
}

func normalizeSyncStudyRequest(
	sessionID uuid.UUID,
	request StudySyncRequest,
) (string, []model.StudyEvent, error) {
	if err := validateSyncStudyRequest(sessionID, request); err != nil {
		return "", nil, err
	}
	normalizedEvents := make([]normalizedStudyEvent, len(request.Events))
	storageEvents := make([]model.StudyEvent, len(request.Events))
	for index, wireEvent := range request.Events {
		storageEvent := studyEventRequestToModel(wireEvent)
		if err := model.ValidateStudyEvent(storageEvent); err != nil {
			return "", nil, newStudySessionError(model.StudyErrorCodeSchemaInvalid)
		}
		storageEvents[index] = storageEvent

		var ratingEventID *string
		if wireEvent.RatingEventID != nil {
			value := wireEvent.RatingEventID.String()
			ratingEventID = &value
		}
		var observedAt *string
		if wireEvent.ClientObservedAt != nil {
			if wireEvent.ClientObservedAt.IsZero() {
				return "", nil, newStudySessionError(model.StudyErrorCodeSchemaInvalid)
			}
			value := wireEvent.ClientObservedAt.UTC().Format(time.RFC3339Nano)
			observedAt = &value
		}
		normalizedEvents[index] = normalizedStudyEvent{
			EventID:            wireEvent.EventID.String(),
			ClientSequence:     strconv.FormatInt(wireEvent.ClientSequence, 10),
			ProfileRef:         wireEvent.ProfileRef,
			SessionItemID:      wireEvent.SessionItemID.String(),
			CardIdentity:       wireEvent.CardIdentity.String(),
			ContentSHA256:      wireEvent.ContentSHA256,
			Kind:               wireEvent.Kind,
			Rating:             cloneStudyRating(wireEvent.Rating),
			RatingEventID:      ratingEventID,
			ClientObservedAt:   observedAt,
			ClientClockQuality: wireEvent.ClientClockQuality,
		}
	}

	_, digest, err := canonicalizeNormalizedStudyRequest(normalizedSyncStudyRequest{
		Operation:            "study_session_sync",
		ProtocolVersion:      model.StudyProtocolVersion,
		SchemaVersion:        model.StudySchemaVersion,
		SessionID:            sessionID.String(),
		ClientStreamID:       request.ClientStreamID.String(),
		ExpectedStateVersion: strconv.FormatInt(request.ExpectedStateVersion, 10),
		Events:               normalizedEvents,
		Page:                 request.Page,
	})
	if err != nil {
		return "", nil, err
	}
	return digest, storageEvents, nil
}

func validateCreateStudyRequestBeforeResolution(request StudyCreateSessionRequest) error {
	if request.ProtocolVersion != model.StudyProtocolVersion || request.SchemaVersion != model.StudySchemaVersion ||
		request.CreationRequestID == uuid.Nil || !validStudyReference(request.ProfileRef) ||
		!request.Scope.CardKind.Valid() || len(request.Scope.TopicRefs) < 1 || len(request.Scope.TopicRefs) > model.StudyMaximumTopics {
		return newStudySessionError(model.StudyErrorCodeSchemaInvalid)
	}
	if err := validateStudyPageRequest(request.Page); err != nil {
		return err
	}
	seenRefs := make(map[string]struct{}, len(request.Scope.TopicRefs))
	for _, topicRef := range request.Scope.TopicRefs {
		if !validStudyReference(topicRef) {
			return newStudySessionError(model.StudyErrorCodeSchemaInvalid)
		}
		if _, duplicate := seenRefs[topicRef]; duplicate {
			return newStudySessionError(model.StudyErrorCodeSchemaInvalid)
		}
		seenRefs[topicRef] = struct{}{}
	}
	if request.Predecessor != nil {
		if request.Predecessor.SessionID == uuid.Nil || request.Predecessor.FlushReceiptID == uuid.Nil {
			return newStudySessionError(model.StudyErrorCodeSchemaInvalid)
		}
	}
	return nil
}

func validateResolvedStudyScope(cardKind model.StudyCardKind, scope model.StudyNormalizedScope) error {
	if scope.Scope.CardKind != cardKind {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	normalized, err := model.NormalizeStudyScope(scope.Scope)
	if err != nil || normalized.CanonicalSelectorHash != scope.CanonicalSelectorHash ||
		!bytes.Equal(normalized.CanonicalSelector, scope.CanonicalSelector) ||
		!equalUUIDSlices(normalized.Scope.TopicIDs, scope.Scope.TopicIDs) ||
		!equalStringSlices(normalized.Scope.TopicRefs, scope.Scope.TopicRefs) {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	return nil
}

func validateSyncStudyRequest(sessionID uuid.UUID, request StudySyncRequest) error {
	if sessionID == uuid.Nil || request.ProtocolVersion != model.StudyProtocolVersion ||
		request.SchemaVersion != model.StudySchemaVersion || request.SyncID == uuid.Nil ||
		request.ClientStreamID == uuid.Nil || request.ExpectedStateVersion < 1 ||
		len(request.Events) > model.StudyMaximumEvents {
		return newStudySessionError(model.StudyErrorCodeSchemaInvalid)
	}
	if err := validateStudyPageRequest(request.Page); err != nil {
		return err
	}
	for index := range request.Events {
		if index > 0 && request.Events[index].ClientSequence != request.Events[index-1].ClientSequence+1 {
			return newStudySessionError(model.StudyErrorCodeStreamSequenceGap)
		}
	}
	return nil
}

func validateStudyPageRequest(request StudyPageRequest) error {
	if request.AfterPosition < -1 || int64(request.AfterPosition) > int64(2_147_483_647) ||
		request.Limit < 1 || request.Limit > StudyMaximumPageItems {
		return newStudySessionError(model.StudyErrorCodeSchemaInvalid)
	}
	return nil
}

func studyEventRequestToModel(event StudyEventRequest) model.StudyEvent {
	return model.StudyEvent{
		ID:                 event.EventID,
		ClientSequence:     event.ClientSequence,
		ProfileRef:         event.ProfileRef,
		StudySessionItemID: event.SessionItemID,
		CardIdentity:       event.CardIdentity,
		ContentSHA256:      event.ContentSHA256,
		Kind:               event.Kind,
		Rating:             cloneStudyRating(event.Rating),
		RatingEventID:      cloneUUIDPointer(event.RatingEventID),
		ClientObservedAt:   cloneTimePointer(event.ClientObservedAt),
		ClientClockQuality: event.ClientClockQuality,
	}
}

func validStudyReference(value string) bool {
	return len(value) >= 1 && len(value) <= 128 && studyReferencePattern.MatchString(value)
}

func cloneStudyRating(value *model.StudyRating) *model.StudyRating {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneUUIDPointer(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := value.UTC()
	return &clone
}

func equalUUIDSlices(left, right []uuid.UUID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
