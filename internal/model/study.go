package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	StudyProtocolVersion = "study.v1"
	StudySchemaVersion   = 1
	StudyMaximumTopics   = 32
	StudyMaximumEvents   = 64
)

var (
	studyProfileRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	studySHA256Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// StudyCardKind identifies one scheduler-visible card family.
type StudyCardKind string

const (
	StudyCardKindIdea StudyCardKind = "idea"
)

func (kind StudyCardKind) Valid() bool {
	return kind == StudyCardKindIdea
}

// StudySessionLifecycle is the durable state of one finite coverage cycle.
type StudySessionLifecycle string

const (
	StudySessionLifecycleActive           StudySessionLifecycle = "active"
	StudySessionLifecycleCompleted        StudySessionLifecycle = "completed"
	StudySessionLifecycleWaitingForCorpus StudySessionLifecycle = "waiting_for_corpus"
	StudySessionLifecycleSupersededEmpty  StudySessionLifecycle = "superseded_empty"
	StudySessionLifecycleAbandoned        StudySessionLifecycle = "abandoned"
)

func (lifecycle StudySessionLifecycle) Valid() bool {
	switch lifecycle {
	case StudySessionLifecycleActive,
		StudySessionLifecycleCompleted,
		StudySessionLifecycleWaitingForCorpus,
		StudySessionLifecycleSupersededEmpty,
		StudySessionLifecycleAbandoned:
		return true
	default:
		return false
	}
}

func (lifecycle StudySessionLifecycle) Terminal() bool {
	switch lifecycle {
	case StudySessionLifecycleCompleted, StudySessionLifecycleSupersededEmpty, StudySessionLifecycleAbandoned:
		return true
	default:
		return false
	}
}

// StudyEventKind identifies one append-only learning fact.
type StudyEventKind string

const (
	StudyEventKindPresented    StudyEventKind = "presented"
	StudyEventKindRevealed     StudyEventKind = "revealed"
	StudyEventKindRated        StudyEventKind = "rated"
	StudyEventKindRatingVoided StudyEventKind = "rating_voided"
)

func (kind StudyEventKind) Valid() bool {
	switch kind {
	case StudyEventKindPresented, StudyEventKindRevealed, StudyEventKindRated, StudyEventKindRatingVoided:
		return true
	default:
		return false
	}
}

// StudyRating is the exact rating vocabulary frozen by study.v1.
type StudyRating string

const (
	StudyRatingAgain StudyRating = "again"
	StudyRatingHard  StudyRating = "hard"
	StudyRatingGood  StudyRating = "good"
	StudyRatingEasy  StudyRating = "easy"
)

func (rating StudyRating) Valid() bool {
	switch rating {
	case StudyRatingAgain, StudyRatingHard, StudyRatingGood, StudyRatingEasy:
		return true
	default:
		return false
	}
}

// StudyClientClockQuality describes optional client-clock evidence without
// granting that clock ordering authority.
type StudyClientClockQuality string

const (
	StudyClientClockQualityTrusted          StudyClientClockQuality = "trusted"
	StudyClientClockQualityBoundedUncertain StudyClientClockQuality = "bounded_uncertain"
	StudyClientClockQualityUnknown          StudyClientClockQuality = "unknown"
)

func (quality StudyClientClockQuality) Valid() bool {
	switch quality {
	case StudyClientClockQualityTrusted, StudyClientClockQualityBoundedUncertain, StudyClientClockQualityUnknown:
		return true
	default:
		return false
	}
}

// StudyCreationDisposition records how a creation UUID reached its canonical
// server-generated session identity.
type StudyCreationDisposition string

const (
	StudyCreationDispositionCreated             StudyCreationDisposition = "created"
	StudyCreationDispositionResumed             StudyCreationDisposition = "resumed"
	StudyCreationDispositionConvergedToExisting StudyCreationDisposition = "converged_to_existing"
	StudyCreationDispositionReplayed            StudyCreationDisposition = "replayed"
	StudyCreationDispositionSuccessor           StudyCreationDisposition = "successor"
)

func (disposition StudyCreationDisposition) Valid() bool {
	switch disposition {
	case StudyCreationDispositionCreated,
		StudyCreationDispositionResumed,
		StudyCreationDispositionConvergedToExisting,
		StudyCreationDispositionReplayed,
		StudyCreationDispositionSuccessor:
		return true
	default:
		return false
	}
}

// StudyEventReceiptDisposition distinguishes canonical scheduler facts from
// terminally consumed non-facts.
type StudyEventReceiptDisposition string

const (
	StudyEventReceiptDispositionAccepted          StudyEventReceiptDisposition = "accepted"
	StudyEventReceiptDispositionSemanticDuplicate StudyEventReceiptDisposition = "semantic_duplicate"
	StudyEventReceiptDispositionTerminalRejected  StudyEventReceiptDisposition = "terminal_rejected"
)

func (disposition StudyEventReceiptDisposition) Valid() bool {
	switch disposition {
	case StudyEventReceiptDispositionAccepted,
		StudyEventReceiptDispositionSemanticDuplicate,
		StudyEventReceiptDispositionTerminalRejected:
		return true
	default:
		return false
	}
}

// StudyErrorCode is a stable machine-readable study.v1 failure code.
type StudyErrorCode string

const (
	StudyErrorCodeSchemaInvalid        StudyErrorCode = "STUDY_SCHEMA_INVALID"
	StudyErrorCodeUnauthorized         StudyErrorCode = "UNAUTHORIZED"
	StudyErrorCodeAuthorizationDenied  StudyErrorCode = "STUDY_AUTHORIZATION_DENIED"
	StudyErrorCodeProfileNotFound      StudyErrorCode = "STUDY_PROFILE_NOT_FOUND"
	StudyErrorCodeSessionNotFound      StudyErrorCode = "STUDY_SESSION_NOT_FOUND"
	StudyErrorCodeIdempotencyConflict  StudyErrorCode = "IDEMPOTENCY_CONFLICT"
	StudyErrorCodeCycleNotExhausted    StudyErrorCode = "STUDY_CYCLE_NOT_EXHAUSTED"
	StudyErrorCodeFlushReceiptRequired StudyErrorCode = "STUDY_FLUSH_RECEIPT_REQUIRED"
	StudyErrorCodeFlushReceiptStale    StudyErrorCode = "STUDY_FLUSH_RECEIPT_STALE"
	StudyErrorCodeStateStale           StudyErrorCode = "STUDY_STATE_STALE"
	StudyErrorCodeStreamSequenceGap    StudyErrorCode = "STUDY_STREAM_SEQUENCE_GAP"
	StudyErrorCodeRequestTooLarge      StudyErrorCode = "STUDY_REQUEST_TOO_LARGE"
	StudyErrorCodeRateLimited          StudyErrorCode = "STUDY_RATE_LIMITED"
	StudyErrorCodeServiceUnavailable   StudyErrorCode = "STUDY_SERVICE_UNAVAILABLE"
	StudyErrorCodeInternal             StudyErrorCode = "INTERNAL"
)

func (code StudyErrorCode) Valid() bool {
	switch code {
	case StudyErrorCodeSchemaInvalid,
		StudyErrorCodeUnauthorized,
		StudyErrorCodeAuthorizationDenied,
		StudyErrorCodeProfileNotFound,
		StudyErrorCodeSessionNotFound,
		StudyErrorCodeIdempotencyConflict,
		StudyErrorCodeCycleNotExhausted,
		StudyErrorCodeFlushReceiptRequired,
		StudyErrorCodeFlushReceiptStale,
		StudyErrorCodeStateStale,
		StudyErrorCodeStreamSequenceGap,
		StudyErrorCodeRequestTooLarge,
		StudyErrorCodeRateLimited,
		StudyErrorCodeServiceUnavailable,
		StudyErrorCodeInternal:
		return true
	default:
		return false
	}
}

// StudyEventReceiptCode is one terminal event adjudication code.
type StudyEventReceiptCode string

const (
	StudyEventReceiptCodeAccepted                StudyEventReceiptCode = "STUDY_EVENT_ACCEPTED"
	StudyEventReceiptCodeSemanticDuplicate       StudyEventReceiptCode = "STUDY_EVENT_SEMANTIC_DUPLICATE"
	StudyEventReceiptCodeTransitionInvalid       StudyEventReceiptCode = "STUDY_EVENT_TRANSITION_INVALID"
	StudyEventReceiptCodeIDConflict              StudyEventReceiptCode = "STUDY_EVENT_ID_CONFLICT"
	StudyEventReceiptCodeRatingAlreadyEffective  StudyEventReceiptCode = "STUDY_RATING_ALREADY_EFFECTIVE"
	StudyEventReceiptCodeRatingVoidTargetInvalid StudyEventReceiptCode = "STUDY_RATING_VOID_TARGET_INVALID"
	StudyEventReceiptCodeContentRevisionMismatch StudyEventReceiptCode = "STUDY_CONTENT_REVISION_MISMATCH"
)

func (code StudyEventReceiptCode) Valid() bool {
	switch code {
	case StudyEventReceiptCodeAccepted,
		StudyEventReceiptCodeSemanticDuplicate,
		StudyEventReceiptCodeTransitionInvalid,
		StudyEventReceiptCodeIDConflict,
		StudyEventReceiptCodeRatingAlreadyEffective,
		StudyEventReceiptCodeRatingVoidTargetInvalid,
		StudyEventReceiptCodeContentRevisionMismatch:
		return true
	default:
		return false
	}
}

func (code StudyEventReceiptCode) ValidFor(disposition StudyEventReceiptDisposition) bool {
	switch disposition {
	case StudyEventReceiptDispositionAccepted:
		return code == StudyEventReceiptCodeAccepted
	case StudyEventReceiptDispositionSemanticDuplicate:
		return code == StudyEventReceiptCodeSemanticDuplicate
	case StudyEventReceiptDispositionTerminalRejected:
		return code.Valid() && code != StudyEventReceiptCodeAccepted && code != StudyEventReceiptCodeSemanticDuplicate
	default:
		return false
	}
}

// StudyPrincipalNamespace identifies the trusted derivation path for a
// non-secret principal record. It never contains credential material.
type StudyPrincipalNamespace string

const (
	StudyPrincipalNamespaceTailnetNode StudyPrincipalNamespace = "tailnet-node"
	StudyPrincipalNamespaceScopedToken StudyPrincipalNamespace = "scoped-token"
	StudyPrincipalNamespaceFixture     StudyPrincipalNamespace = "fixture"
)

func (namespace StudyPrincipalNamespace) Valid() bool {
	switch namespace {
	case StudyPrincipalNamespaceTailnetNode,
		StudyPrincipalNamespaceScopedToken,
		StudyPrincipalNamespaceFixture:
		return true
	default:
		return false
	}
}

// StudyPrincipalID is an opaque, non-secret authorization namespace key. Its
// unexported representation prevents request parsers from casting bearer bytes
// directly into a persistable principal.
type StudyPrincipalID struct {
	canonical string
}

func OperatorStudyPrincipalID() StudyPrincipalID {
	return StudyPrincipalID{canonical: "operator"}
}

func NewStudyPrincipalID(namespace StudyPrincipalNamespace, subjectID uuid.UUID) (StudyPrincipalID, error) {
	if !namespace.Valid() || subjectID == uuid.Nil {
		return StudyPrincipalID{}, newStudySchemaError("study principal derivation is invalid", "study_principal_id")
	}
	return StudyPrincipalID{canonical: string(namespace) + ":" + subjectID.String()}, nil
}

// RestoreStudyPrincipalID is only for already-persisted non-secret principal
// identifiers. Authentication adapters derive new values through the typed
// constructor above; they never pass Authorization content here.
func RestoreStudyPrincipalID(value string) (StudyPrincipalID, error) {
	if value == "operator" {
		return OperatorStudyPrincipalID(), nil
	}
	separator := strings.IndexByte(value, ':')
	if separator < 1 {
		return StudyPrincipalID{}, newStudySchemaError("stored study principal is invalid", "study_principal_id")
	}
	namespace := StudyPrincipalNamespace(value[:separator])
	subjectID, err := uuid.Parse(value[separator+1:])
	if err != nil || !namespace.Valid() || subjectID == uuid.Nil || subjectID.String() != value[separator+1:] {
		return StudyPrincipalID{}, newStudySchemaError("stored study principal is invalid", "study_principal_id")
	}
	return StudyPrincipalID{canonical: value}, nil
}

func (principalID StudyPrincipalID) String() string {
	return principalID.canonical
}

func (principalID StudyPrincipalID) Valid() bool {
	restored, err := RestoreStudyPrincipalID(principalID.canonical)
	return err == nil && restored.canonical == principalID.canonical
}

// StudyRatingRank is scheduler selection provenance, not a due-date claim.
type StudyRatingRank string

const (
	StudyRatingRankAgain         StudyRatingRank = "again"
	StudyRatingRankHard          StudyRatingRank = "hard"
	StudyRatingRankUnseenUnrated StudyRatingRank = "unseen_or_unrated"
	StudyRatingRankGood          StudyRatingRank = "good"
	StudyRatingRankEasy          StudyRatingRank = "easy"
)

func (rank StudyRatingRank) Valid() bool {
	switch rank {
	case StudyRatingRankAgain, StudyRatingRankHard, StudyRatingRankUnseenUnrated, StudyRatingRankGood, StudyRatingRankEasy:
		return true
	default:
		return false
	}
}

// StudyBoundaryAdjustment describes an explicit cycle-boundary decision.
type StudyBoundaryAdjustment string

const (
	StudyBoundaryAdjustmentNone          StudyBoundaryAdjustment = "none"
	StudyBoundaryAdjustmentRotated       StudyBoundaryAdjustment = "boundary_repeat_rotated"
	StudyBoundaryAdjustmentPriorityBound StudyBoundaryAdjustment = "boundary_repeat_due_to_priority"
)

func (adjustment StudyBoundaryAdjustment) Valid() bool {
	switch adjustment {
	case StudyBoundaryAdjustmentNone, StudyBoundaryAdjustmentRotated, StudyBoundaryAdjustmentPriorityBound:
		return true
	default:
		return false
	}
}

// StudyError preserves a stable code, safe machine context, and the original
// cause without exposing raw database details to protocol callers.
type StudyError struct {
	Code      StudyErrorCode    `json:"code"`
	Message   string            `json:"message"`
	Context   map[string]string `json:"context,omitempty"`
	Retryable bool              `json:"retryable"`
	Cause     error             `json:"-"`
}

func (err *StudyError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

func (err *StudyError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func (err *StudyError) Is(target error) bool {
	studyTarget, ok := target.(*StudyError)
	return ok && err != nil && studyTarget != nil && studyTarget.Code == err.Code
}

// StudyScope is the normalized active-cycle selector. TopicIDs are the stable
// persistence identity; TopicRefs are the canonical references returned to a
// client and may be renamed without changing an already issued selector.
type StudyScope struct {
	TopicIDs  []uuid.UUID   `json:"topic_ids"`
	TopicRefs []string      `json:"topic_refs"`
	CardKind  StudyCardKind `json:"card_kind"`
}

// StudyNormalizedScope contains the canonical selector bytes and digest used
// by transactional nonterminal-scope uniqueness.
type StudyNormalizedScope struct {
	Scope                 StudyScope      `json:"scope"`
	CanonicalSelector     json.RawMessage `json:"canonical_selector"`
	CanonicalSelectorHash string          `json:"canonical_selector_sha256"`
}

// StudyLearnerProfile is the server-side learner and accepted-prefix lock
// anchor. It is deliberately independent from a Tailnet appliance identity.
type StudyLearnerProfile struct {
	ID                        uuid.UUID `json:"learner_profile_id"`
	Ref                       string    `json:"learner_profile_ref"`
	ProfileIngestionHighWater int64     `json:"profile_ingestion_high_water"`
	CreatedAt                 time.Time `json:"learner_profile_created_at"`
	UpdatedAt                 time.Time `json:"learner_profile_updated_at"`
}

// StudySchedulerProvenance freezes every input needed to reproduce one issued
// cycle without treating receipt time as a selection key.
type StudySchedulerProvenance struct {
	Name                           string    `json:"name"`
	Version                        string    `json:"version"`
	ConfigurationSHA256            string    `json:"configuration_sha256"`
	InputProfileIngestionHighWater int64     `json:"input_profile_ingestion_high_water"`
	PlannedAt                      time.Time `json:"planned_at"`
	Seed                           string    `json:"seed"`
}

// StudyCreationReceipt binds one client request identity to one authoritative
// server session identity.
type StudyCreationReceipt struct {
	StudyPrincipalID             StudyPrincipalID         `json:"-"`
	CreationRequestID            uuid.UUID                `json:"creation_request_id"`
	NormalizedRequestSHA256      string                   `json:"normalized_request_sha256"`
	Disposition                  StudyCreationDisposition `json:"disposition"`
	CanonicalStudySessionID      uuid.UUID                `json:"canonical_session_id"`
	LearnerProfileID             uuid.UUID                `json:"-"`
	Scope                        StudyNormalizedScope     `json:"-"`
	CreationReceiptAdjudicatedAt time.Time                `json:"-"`
}

// StudySession is one immutable finite cycle plus its optimistic lifecycle
// state. Successor identity is derived by the unique predecessor link.
type StudySession struct {
	ID                                uuid.UUID                 `json:"session_id"`
	LearnerProfileID                  uuid.UUID                 `json:"learner_profile_id"`
	Scope                             StudyNormalizedScope      `json:"scope"`
	Lifecycle                         StudySessionLifecycle     `json:"lifecycle"`
	CorpusSHA256                      string                    `json:"corpus_sha256"`
	Scheduler                         *StudySchedulerProvenance `json:"scheduler"`
	PredecessorStudySessionID         *uuid.UUID                `json:"predecessor_session_id"`
	SuccessorStudySessionID           *uuid.UUID                `json:"successor_session_id"`
	ProfileIngestionHighWater         int64                     `json:"profile_ingestion_high_water"`
	StateVersion                      int64                     `json:"state_version"`
	CreatedAt                         time.Time                 `json:"created_at"`
	UpdatedAt                         time.Time                 `json:"updated_at"`
	TerminalizedAt                    *time.Time                `json:"terminalized_at"`
	TerminalProfileIngestionHighWater *int64                    `json:"terminal_profile_ingestion_high_water"`
}

// StudySessionLifecycleEvent is one append-only audited state transition.
type StudySessionLifecycleEvent struct {
	ID             uuid.UUID              `json:"lifecycle_event_id"`
	StudySessionID uuid.UUID              `json:"session_id"`
	FromState      *StudySessionLifecycle `json:"from_state"`
	ToState        StudySessionLifecycle  `json:"to_state"`
	ReasonCode     string                 `json:"reason_code"`
	ActorRef       StudyPrincipalID       `json:"-"`
	StateVersion   int64                  `json:"state_version"`
	RecordedAt     time.Time              `json:"recorded_at"`
}

// StudySelectionReason is durable per-item named/versioned scheduler provenance.
type StudySelectionReason struct {
	RatingRank                      StudyRatingRank         `json:"rating_rank"`
	LatestPresentedIngestionOrdinal *int64                  `json:"latest_presented_ingestion_ordinal"`
	TieBreakerSHA256                string                  `json:"tie_breaker_sha256"`
	BoundaryAdjustment              StudyBoundaryAdjustment `json:"boundary_adjustment"`
}

// StudySessionItem is immutable issued historical evidence. LiveSourceIdeaID
// is the sole mutable link: merge may repoint it and hard deletion may clear it.
type StudySessionItem struct {
	ID                       uuid.UUID            `json:"session_item_id"`
	StudySessionID           uuid.UUID            `json:"session_id"`
	Position                 int                  `json:"position"`
	CardKind                 StudyCardKind        `json:"card_kind"`
	CardIdentity             uuid.UUID            `json:"card_identity"`
	HistoricalSourceKind     string               `json:"source_kind"`
	HistoricalSourceIdentity uuid.UUID            `json:"source_identity"`
	LiveSourceIdeaID         *uuid.UUID           `json:"live_source_idea_id"`
	SourceContentRevision    string               `json:"content_revision"`
	SourceContentSHA256      string               `json:"content_sha256"`
	FrontCanonicalName       string               `json:"canonical_name"`
	BackDefinition           string               `json:"definition"`
	BackExample              *string              `json:"example"`
	SelectionReason          StudySelectionReason `json:"selection_reason"`
	CreatedAt                time.Time            `json:"created_at"`
}

// StudyEvent is either a submitted event awaiting adjudication or an accepted
// canonical event when ProfileIngestionOrdinal is non-nil.
type StudyEvent struct {
	ID                      uuid.UUID               `json:"event_id"`
	ClientSequence          int64                   `json:"client_sequence"`
	ProfileRef              string                  `json:"profile_ref"`
	LearnerProfileID        uuid.UUID               `json:"-"`
	StudySessionID          uuid.UUID               `json:"-"`
	StudySessionItemID      uuid.UUID               `json:"session_item_id"`
	CardIdentity            uuid.UUID               `json:"card_identity"`
	ContentSHA256           string                  `json:"content_sha256"`
	Kind                    StudyEventKind          `json:"kind"`
	Rating                  *StudyRating            `json:"rating,omitempty"`
	RatingEventID           *uuid.UUID              `json:"rating_event_id,omitempty"`
	ClientObservedAt        *time.Time              `json:"client_observed_at"`
	ClientClockQuality      StudyClientClockQuality `json:"client_clock_quality"`
	ClientStreamID          uuid.UUID               `json:"-"`
	NormalizedPayloadSHA256 string                  `json:"-"`
	ProfileIngestionOrdinal *int64                  `json:"-"`
	ReceivedAt              *time.Time              `json:"-"`
}

// StudyClientStream is the durable random source-order cursor for one learner.
type StudyClientStream struct {
	ID                         uuid.UUID `json:"client_stream_id"`
	LearnerProfileID           uuid.UUID `json:"learner_profile_id"`
	AdjudicatedThroughSequence int64     `json:"adjudicated_through_sequence"`
	CreatedAt                  time.Time `json:"created_at"`
	UpdatedAt                  time.Time `json:"updated_at"`
}

// StudyEventIDBinding permanently binds every first-seen submitted UUID,
// including a rejected one, to one payload and study scope.
type StudyEventIDBinding struct {
	EventID             uuid.UUID `json:"event_id"`
	BoundPayloadSHA256  string    `json:"bound_payload_sha256"`
	LearnerProfileID    uuid.UUID `json:"learner_profile_id"`
	StudySessionID      uuid.UUID `json:"session_id"`
	StudySessionItemID  uuid.UUID `json:"session_item_id"`
	CardIdentity        uuid.UUID `json:"card_identity"`
	ContentSHA256       string    `json:"content_sha256"`
	FirstClientStreamID uuid.UUID `json:"first_client_stream_id"`
	FirstClientSequence int64     `json:"first_client_sequence"`
	BoundAt             time.Time `json:"bound_at"`
}

// StudyEventReceipt is one immutable terminal sequence adjudication.
type StudyEventReceipt struct {
	ClientStreamID                   uuid.UUID                    `json:"-"`
	ClientSequence                   int64                        `json:"client_sequence"`
	OriginatingStudyFlushReceiptID   uuid.UUID                    `json:"-"`
	LearnerProfileID                 uuid.UUID                    `json:"-"`
	StudySessionID                   uuid.UUID                    `json:"-"`
	StudySessionItemID               uuid.UUID                    `json:"-"`
	CardIdentity                     uuid.UUID                    `json:"-"`
	ContentSHA256                    string                       `json:"-"`
	EventID                          uuid.UUID                    `json:"event_id"`
	BoundPayloadSHA256               string                       `json:"-"`
	PayloadSHA256                    string                       `json:"payload_sha256"`
	EventIDBindingMatchesInput       bool                         `json:"-"`
	Disposition                      StudyEventReceiptDisposition `json:"disposition"`
	Code                             StudyEventReceiptCode        `json:"code"`
	TerminalFailureClientSequence    *int64                       `json:"-"`
	CanonicalStudyEventID            *uuid.UUID                   `json:"canonical_event_id"`
	CanonicalProfileIngestionOrdinal *int64                       `json:"profile_ingestion_ordinal"`
	CreatedAt                        time.Time                    `json:"-"`
}

// StudyFlushReceipt is the immutable sync replay and predecessor barrier fact.
type StudyFlushReceipt struct {
	ID                          uuid.UUID        `json:"receipt_id"`
	StudyPrincipalID            StudyPrincipalID `json:"-"`
	LearnerProfileID            uuid.UUID        `json:"-"`
	SyncRequestID               uuid.UUID        `json:"sync_id"`
	NormalizedSyncRequestSHA256 string           `json:"-"`
	StudySessionID              uuid.UUID        `json:"session_id"`
	ClientStreamID              uuid.UUID        `json:"client_stream_id"`
	FirstClientSequence         *int64           `json:"-"`
	LastClientSequence          *int64           `json:"-"`
	AcknowledgedThroughSequence int64            `json:"acknowledged_through_sequence"`
	ProfileIngestionHighWater   int64            `json:"profile_ingestion_high_water"`
	StudySessionStateVersion    int64            `json:"state_version"`
	IssuedAt                    time.Time        `json:"issued_at"`
}

// StudySchedulerCardProjection is deterministic derived state. UpdatedAt is
// operational metadata and is explicitly excluded from CanonicalPayload bytes.
type StudySchedulerCardProjection struct {
	LearnerProfileID                          uuid.UUID       `json:"learner_profile_id"`
	ProjectedCardIdentity                     uuid.UUID       `json:"card_identity"`
	LiveSourceIdeaID                          *uuid.UUID      `json:"live_source_idea_id"`
	SchedulerName                             string          `json:"scheduler_name"`
	SchedulerVersion                          string          `json:"scheduler_version"`
	ProjectionSchemaVersion                   int             `json:"projection_schema_version"`
	LatestEffectiveRatingEventID              *uuid.UUID      `json:"latest_effective_rating_event_id"`
	LatestEffectiveRatingEvidenceCardIdentity *uuid.UUID      `json:"latest_effective_rating_evidence_card_identity"`
	LatestEffectiveRating                     *StudyRating    `json:"latest_effective_rating"`
	LatestEffectiveRatingIngestionOrdinal     *int64          `json:"latest_effective_rating_ingestion_ordinal"`
	LatestPresentedEventID                    *uuid.UUID      `json:"latest_presented_event_id"`
	LatestPresentedEvidenceCardIdentity       *uuid.UUID      `json:"latest_presented_evidence_card_identity"`
	LatestPresentedIngestionOrdinal           *int64          `json:"latest_presented_ingestion_ordinal"`
	RebuiltThroughProfileIngestionOrdinal     int64           `json:"rebuilt_through_profile_ingestion_ordinal"`
	CanonicalPayload                          json.RawMessage `json:"canonical_projection_payload"`
	CanonicalSHA256                           string          `json:"canonical_projection_sha256"`
	ProjectionRevision                        int64           `json:"projection_revision"`
	UpdatedAt                                 time.Time       `json:"projection_updated_at"`
}

// StudyCardIdentityRedirect maps historical evidence to a current merge
// survivor without mutating accepted events or issued snapshots.
type StudyCardIdentityRedirect struct {
	HistoricalCardIdentity uuid.UUID `json:"historical_card_identity"`
	SurvivorCardIdentity   uuid.UUID `json:"survivor_card_identity"`
	Reason                 string    `json:"reason"`
	RedirectedAt           time.Time `json:"redirected_at"`
}

// NormalizeStudyScope validates, sorts, and content-identifies one active
// scope. Duplicate references or stable topic identities are rejected instead
// of being silently collapsed.
func NormalizeStudyScope(scope StudyScope) (StudyNormalizedScope, error) {
	if !scope.CardKind.Valid() {
		return StudyNormalizedScope{}, newStudySchemaError("study scope card_kind is invalid", "card_kind")
	}
	if len(scope.TopicIDs) < 1 || len(scope.TopicIDs) > StudyMaximumTopics {
		return StudyNormalizedScope{}, newStudySchemaError("study scope topic count is outside the frozen bound", "topic_count")
	}
	if len(scope.TopicRefs) != len(scope.TopicIDs) {
		return StudyNormalizedScope{}, newStudySchemaError("study scope topic identities and references have different counts", "topic_ref_count")
	}

	topicIDs := append([]uuid.UUID(nil), scope.TopicIDs...)
	for _, topicID := range topicIDs {
		if topicID == uuid.Nil {
			return StudyNormalizedScope{}, newStudySchemaError("study scope contains a zero topic identity", "topic_id")
		}
	}
	sort.Slice(topicIDs, func(left, right int) bool {
		return topicIDs[left].String() < topicIDs[right].String()
	})
	for index := 1; index < len(topicIDs); index++ {
		if topicIDs[index] == topicIDs[index-1] {
			return StudyNormalizedScope{}, newStudySchemaError("study scope contains a duplicate topic identity", "topic_id")
		}
	}

	topicRefs := append([]string(nil), scope.TopicRefs...)
	for index := range topicRefs {
		topicRefs[index] = strings.TrimSpace(topicRefs[index])
		if !validStudyReference(topicRefs[index]) {
			return StudyNormalizedScope{}, newStudySchemaError("study scope contains an invalid topic reference", "topic_ref")
		}
	}
	sort.Strings(topicRefs)
	for index := 1; index < len(topicRefs); index++ {
		if topicRefs[index] == topicRefs[index-1] {
			return StudyNormalizedScope{}, newStudySchemaError("study scope contains a duplicate topic reference", "topic_ref")
		}
	}

	canonicalTopicIDs := make([]string, len(topicIDs))
	for index, topicID := range topicIDs {
		canonicalTopicIDs[index] = topicID.String()
	}
	canonicalSelector, err := json.Marshal(struct {
		CardKind StudyCardKind `json:"card_kind"`
		TopicIDs []string      `json:"topic_ids"`
	}{
		CardKind: scope.CardKind,
		TopicIDs: canonicalTopicIDs,
	})
	if err != nil {
		return StudyNormalizedScope{}, &StudyError{
			Code:      StudyErrorCodeInternal,
			Message:   "failed to encode normalized study scope",
			Retryable: false,
			Cause:     err,
		}
	}

	return StudyNormalizedScope{
		Scope: StudyScope{
			TopicIDs:  topicIDs,
			TopicRefs: topicRefs,
			CardKind:  scope.CardKind,
		},
		CanonicalSelector:     canonicalSelector,
		CanonicalSelectorHash: SHA256Hex(canonicalSelector),
	}, nil
}

// ValidateStudyEvent enforces the frozen event shape before storage performs
// causal transition checks.
func ValidateStudyEvent(event StudyEvent) error {
	if event.ID == uuid.Nil {
		return newStudySchemaError("study event_id is required", "event_id")
	}
	if event.ClientSequence < 1 {
		return newStudySchemaError("study client_sequence must be positive", "client_sequence")
	}
	if !validStudyReference(event.ProfileRef) {
		return newStudySchemaError("study profile_ref is invalid", "profile_ref")
	}
	if event.StudySessionItemID == uuid.Nil {
		return newStudySchemaError("study session_item_id is required", "session_item_id")
	}
	if event.CardIdentity == uuid.Nil {
		return newStudySchemaError("study card_identity is required", "card_identity")
	}
	if !IsStudySHA256(event.ContentSHA256) {
		return newStudySchemaError("study content_sha256 is invalid", "content_sha256")
	}
	if !event.Kind.Valid() {
		return newStudySchemaError("study event kind is invalid", "kind")
	}
	if !event.ClientClockQuality.Valid() {
		return newStudySchemaError("study client_clock_quality is invalid", "client_clock_quality")
	}

	switch event.Kind {
	case StudyEventKindPresented, StudyEventKindRevealed:
		if event.Rating != nil || event.RatingEventID != nil {
			return newStudySchemaError("study observation event contains rating-only fields", "kind")
		}
	case StudyEventKindRated:
		if event.Rating == nil || !event.Rating.Valid() || event.RatingEventID != nil {
			return newStudySchemaError("study rated event has an invalid rating payload", "kind")
		}
	case StudyEventKindRatingVoided:
		if event.Rating != nil || event.RatingEventID == nil || *event.RatingEventID == uuid.Nil {
			return newStudySchemaError("study rating_voided event requires an exact nonzero rating_event_id", "kind")
		}
	}
	return nil
}

// NormalizeStudyEventPayload validates and hashes the canonical wire-semantic
// event fields. Server-enriched identities and receipt timestamps are excluded.
func NormalizeStudyEventPayload(event StudyEvent) (json.RawMessage, string, error) {
	if err := ValidateStudyEvent(event); err != nil {
		return nil, "", err
	}
	var observedAt *string
	if event.ClientObservedAt != nil {
		value := event.ClientObservedAt.UTC().Format(time.RFC3339Nano)
		observedAt = &value
	}
	payload, err := json.Marshal(struct {
		EventID            uuid.UUID               `json:"event_id"`
		ClientSequence     int64                   `json:"client_sequence"`
		ProfileRef         string                  `json:"profile_ref"`
		SessionItemID      uuid.UUID               `json:"session_item_id"`
		CardIdentity       uuid.UUID               `json:"card_identity"`
		ContentSHA256      string                  `json:"content_sha256"`
		Kind               StudyEventKind          `json:"kind"`
		Rating             *StudyRating            `json:"rating"`
		RatingEventID      *uuid.UUID              `json:"rating_event_id"`
		ClientObservedAt   *string                 `json:"client_observed_at"`
		ClientClockQuality StudyClientClockQuality `json:"client_clock_quality"`
	}{
		EventID:            event.ID,
		ClientSequence:     event.ClientSequence,
		ProfileRef:         event.ProfileRef,
		SessionItemID:      event.StudySessionItemID,
		CardIdentity:       event.CardIdentity,
		ContentSHA256:      event.ContentSHA256,
		Kind:               event.Kind,
		Rating:             event.Rating,
		RatingEventID:      event.RatingEventID,
		ClientObservedAt:   observedAt,
		ClientClockQuality: event.ClientClockQuality,
	})
	if err != nil {
		return nil, "", &StudyError{
			Code:      StudyErrorCodeInternal,
			Message:   "failed to encode normalized study event",
			Retryable: false,
			Cause:     err,
		}
	}
	return payload, SHA256Hex(payload), nil
}

// ValidateStudySessionItem enforces every persisted study.v1 projection bound.
func ValidateStudySessionItem(item StudySessionItem) error {
	if item.ID == uuid.Nil || item.StudySessionID == uuid.Nil || item.CardIdentity == uuid.Nil || item.HistoricalSourceIdentity == uuid.Nil {
		return newStudySchemaError("study session item identities are required", "session_item_id")
	}
	if item.Position < 0 {
		return newStudySchemaError("study session item position must be nonnegative", "position")
	}
	if !item.CardKind.Valid() || item.HistoricalSourceKind != string(StudyCardKindIdea) {
		return newStudySchemaError("study session item source kind is invalid", "card_kind")
	}
	if !boundedNonblankText(item.SourceContentRevision, 128) || !IsStudySHA256(item.SourceContentSHA256) {
		return newStudySchemaError("study session item source revision is invalid", "content_revision")
	}
	if !boundedNonblankText(item.FrontCanonicalName, 512) {
		return newStudySchemaError("study session item front exceeds the frozen bound", "canonical_name")
	}
	if !boundedNonblankText(item.BackDefinition, 16384) {
		return newStudySchemaError("study session item back exceeds the frozen bound", "definition")
	}
	if item.BackExample != nil && !boundedNonblankText(*item.BackExample, 16384) {
		return newStudySchemaError("study session item example exceeds the frozen bound", "example")
	}
	if !item.SelectionReason.RatingRank.Valid() || !item.SelectionReason.BoundaryAdjustment.Valid() || !IsStudySHA256(item.SelectionReason.TieBreakerSHA256) {
		return newStudySchemaError("study session item selection provenance is invalid", "rating_rank")
	}
	if item.SelectionReason.LatestPresentedIngestionOrdinal != nil && *item.SelectionReason.LatestPresentedIngestionOrdinal < 1 {
		return newStudySchemaError("study session item latest presented ordinal must be positive", "latest_presented_ingestion_ordinal")
	}
	return nil
}

// IsStudySHA256 reports whether a digest matches the lowercase frozen wire form.
func IsStudySHA256(value string) bool {
	return studySHA256Pattern.MatchString(value)
}

// SHA256Hex returns the lowercase content identity for canonical bytes.
func SHA256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validStudyReference(value string) bool {
	return len(value) >= 1 && len(value) <= 128 && studyProfileRefPattern.MatchString(value)
}

func boundedNonblankText(value string, maximumRunes int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= maximumRunes
}

func newStudySchemaError(message, field string) *StudyError {
	return &StudyError{
		Code:    StudyErrorCodeSchemaInvalid,
		Message: message,
		Context: map[string]string{
			"operation": "study_schema_validation",
			"field":     field,
		},
		Retryable: false,
	}
}
