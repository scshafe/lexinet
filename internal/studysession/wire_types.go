package studysession

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

// StudyScopeRequest is the strict study.v1 wire selector. Topic references are
// resolved to stable IDs before request hashing and materialization.
type StudyScopeRequest struct {
	TopicRefs []string            `json:"topic_refs"`
	CardKind  model.StudyCardKind `json:"card_kind"`
}

// StudyPageRequest selects one deterministic immutable-item prefix.
type StudyPageRequest struct {
	AfterPosition int `json:"after_position"`
	Limit         int `json:"limit"`
}

// StudyPredecessorBarrierRequest presents the exact predecessor and durable
// flush receipt needed for successor materialization.
type StudyPredecessorBarrierRequest struct {
	SessionID      uuid.UUID `json:"session_id"`
	FlushReceiptID uuid.UUID `json:"flush_receipt_id"`
}

// StudyCreateSessionRequest is the strict POST /study/sessions body.
type StudyCreateSessionRequest struct {
	ProtocolVersion   string                          `json:"protocol_version"`
	SchemaVersion     int                             `json:"schema_version"`
	CreationRequestID uuid.UUID                       `json:"creation_request_id"`
	ProfileRef        string                          `json:"profile_ref"`
	Scope             StudyScopeRequest               `json:"scope"`
	Predecessor       *StudyPredecessorBarrierRequest `json:"predecessor,omitempty"`
	Page              StudyPageRequest                `json:"page"`
}

// StudyEventRequest is the strict tagged union payload for one submitted
// presented, revealed, rated, or rating_voided fact.
type StudyEventRequest struct {
	EventID            uuid.UUID                     `json:"event_id"`
	ClientSequence     int64                         `json:"client_sequence"`
	ProfileRef         string                        `json:"profile_ref"`
	SessionItemID      uuid.UUID                     `json:"session_item_id"`
	CardIdentity       uuid.UUID                     `json:"card_identity"`
	ContentSHA256      string                        `json:"content_sha256"`
	Kind               model.StudyEventKind          `json:"kind"`
	Rating             *model.StudyRating            `json:"rating,omitempty"`
	RatingEventID      *uuid.UUID                    `json:"rating_event_id,omitempty"`
	ClientObservedAt   *time.Time                    `json:"client_observed_at,omitempty"`
	ClientClockQuality model.StudyClientClockQuality `json:"client_clock_quality"`
}

// StudySyncRequest is the strict POST /study/sessions/{id}/sync body.
type StudySyncRequest struct {
	ProtocolVersion      string              `json:"protocol_version"`
	SchemaVersion        int                 `json:"schema_version"`
	SyncID               uuid.UUID           `json:"sync_id"`
	ClientStreamID       uuid.UUID           `json:"client_stream_id"`
	ExpectedStateVersion int64               `json:"expected_state_version"`
	Events               []StudyEventRequest `json:"events"`
	Page                 StudyPageRequest    `json:"page"`
}

// StudyScopeResource is the canonical display selector returned on the wire.
type StudyScopeResource struct {
	TopicRefs []string            `json:"topic_refs"`
	CardKind  model.StudyCardKind `json:"card_kind"`
}

// StudyCreationReceiptResource is one immutable creation-key adjudication.
type StudyCreationReceiptResource struct {
	CreationRequestID       uuid.UUID                      `json:"creation_request_id"`
	NormalizedRequestSHA256 string                         `json:"normalized_request_sha256"`
	Disposition             model.StudyCreationDisposition `json:"disposition"`
	CanonicalSessionID      uuid.UUID                      `json:"canonical_session_id"`
}

// StudySchedulerResource freezes the scheduler identity and inputs.
type StudySchedulerResource struct {
	Name                           string    `json:"name"`
	Version                        string    `json:"version"`
	ConfigurationSHA256            string    `json:"configuration_sha256"`
	InputProfileIngestionHighWater int64     `json:"input_profile_ingestion_high_water"`
	PlannedAt                      time.Time `json:"planned_at"`
	Seed                           string    `json:"seed"`
}

// StudySourceRevisionResource identifies immutable projected source bytes.
type StudySourceRevisionResource struct {
	SourceKind      string    `json:"source_kind"`
	SourceIdentity  uuid.UUID `json:"source_identity"`
	ContentRevision string    `json:"content_revision"`
	ContentSHA256   string    `json:"content_sha256"`
}

// StudyCardFrontResource is the complete immutable front projection.
type StudyCardFrontResource struct {
	CanonicalName string `json:"canonical_name"`
}

// StudyCardBackResource is the complete immutable back projection.
type StudyCardBackResource struct {
	Definition string  `json:"definition"`
	Example    *string `json:"example"`
}

// StudySelectionReasonResource explains one scheduler position.
type StudySelectionReasonResource struct {
	RatingRank                      model.StudyRatingRank         `json:"rating_rank"`
	LatestPresentedIngestionOrdinal *int64                        `json:"latest_presented_ingestion_ordinal"`
	TieBreakerSHA256                string                        `json:"tie_breaker_sha256"`
	BoundaryAdjustment              model.StudyBoundaryAdjustment `json:"boundary_adjustment"`
}

// StudyCardProjectionResource is one immutable issued card.
type StudyCardProjectionResource struct {
	SessionItemID    uuid.UUID                    `json:"session_item_id"`
	Position         int                          `json:"position"`
	CardKind         model.StudyCardKind          `json:"card_kind"`
	CardIdentity     uuid.UUID                    `json:"card_identity"`
	LiveSourceIdeaID *uuid.UUID                   `json:"live_source_idea_id"`
	SourceRevision   StudySourceRevisionResource  `json:"source_revision"`
	Front            StudyCardFrontResource       `json:"front"`
	Back             StudyCardBackResource        `json:"back"`
	SelectionReason  StudySelectionReasonResource `json:"selection_reason"`
}

// StudySessionPageResource is one count- and byte-bounded issued prefix.
type StudySessionPageResource struct {
	AfterPosition     int                           `json:"after_position"`
	NextAfterPosition int                           `json:"next_after_position"`
	Complete          bool                          `json:"complete"`
	Items             []StudyCardProjectionResource `json:"items"`
}

// MarshalJSON delegates every nested response page to the same deterministic
// typed encoder used for the durable replay artifact.
func (page StudySessionPageResource) MarshalJSON() ([]byte, error) {
	return encodeStudySessionPageResource(page)
}

// StudySessionResource is the exact raw success resource frozen by study.v1.
// CreationReceipt is nonnil only for create-or-resume responses.
type StudySessionResource struct {
	ProtocolVersion           string                        `json:"protocol_version"`
	SchemaVersion             int                           `json:"schema_version"`
	RequiredSemantics         []string                      `json:"required_semantics"`
	SessionID                 uuid.UUID                     `json:"session_id"`
	ProfileRef                string                        `json:"profile_ref"`
	Scope                     StudyScopeResource            `json:"scope"`
	Lifecycle                 model.StudySessionLifecycle   `json:"lifecycle"`
	CreationReceipt           *StudyCreationReceiptResource `json:"creation_receipt"`
	PredecessorSessionID      *uuid.UUID                    `json:"predecessor_session_id"`
	SuccessorSessionID        *uuid.UUID                    `json:"successor_session_id"`
	CorpusSHA256              string                        `json:"corpus_sha256"`
	ProfileIngestionHighWater int64                         `json:"profile_ingestion_high_water"`
	StateVersion              int64                         `json:"state_version"`
	Scheduler                 *StudySchedulerResource       `json:"scheduler"`
	Page                      StudySessionPageResource      `json:"page"`
	GeneratedAt               time.Time                     `json:"generated_at"`
}

// StudyEventReceiptResource is one immutable terminal sequence receipt.
type StudyEventReceiptResource struct {
	ClientSequence          int64                              `json:"client_sequence"`
	EventID                 uuid.UUID                          `json:"event_id"`
	PayloadSHA256           string                             `json:"payload_sha256"`
	Disposition             model.StudyEventReceiptDisposition `json:"disposition"`
	Code                    model.StudyEventReceiptCode        `json:"code"`
	CanonicalEventID        *uuid.UUID                         `json:"canonical_event_id"`
	ProfileIngestionOrdinal *int64                             `json:"profile_ingestion_ordinal"`
}

// StudyFlushReceiptResource is the immutable sync replay and successor barrier.
type StudyFlushReceiptResource struct {
	ReceiptID                   uuid.UUID `json:"receipt_id"`
	SessionID                   uuid.UUID `json:"session_id"`
	SyncID                      uuid.UUID `json:"sync_id"`
	ClientStreamID              uuid.UUID `json:"client_stream_id"`
	AcknowledgedThroughSequence int64     `json:"acknowledged_through_sequence"`
	ProfileIngestionHighWater   int64     `json:"profile_ingestion_high_water"`
	StateVersion                int64     `json:"state_version"`
	IssuedAt                    time.Time `json:"issued_at"`
}

// StudySyncResource returns immutable sync artifacts beside a live session.
type StudySyncResource struct {
	ProtocolVersion            string                      `json:"protocol_version"`
	SchemaVersion              int                         `json:"schema_version"`
	RequiredSemantics          []string                    `json:"required_semantics"`
	SyncID                     uuid.UUID                   `json:"sync_id"`
	NormalizedRequestSHA256    string                      `json:"normalized_request_sha256"`
	ClientStreamID             uuid.UUID                   `json:"client_stream_id"`
	AdjudicatedThroughSequence int64                       `json:"adjudicated_through_sequence"`
	ProfileIngestionHighWater  int64                       `json:"profile_ingestion_high_water"`
	Receipts                   []StudyEventReceiptResource `json:"receipts"`
	FlushReceipt               StudyFlushReceiptResource   `json:"flush_receipt"`
	Session                    StudySessionResource        `json:"session"`
	GeneratedAt                time.Time                   `json:"generated_at"`
}

// StudyCreateSessionResult carries the typed resource, exact bounded response
// bytes, and the status discriminator used for HTTP 201 versus 200.
type StudyCreateSessionResult struct {
	Resource     StudySessionResource
	ResponseJSON json.RawMessage
	Created      bool
}

// StudyGetSessionResult carries the typed resource and exact bounded response
// bytes prepared before any HTTP headers are written.
type StudyGetSessionResult struct {
	Resource     StudySessionResource
	ResponseJSON json.RawMessage
}

// StudySyncResult carries the typed sync resource and exact bounded response
// bytes prepared before any HTTP headers are written.
type StudySyncResult struct {
	Resource     StudySyncResource
	ResponseJSON json.RawMessage
}
