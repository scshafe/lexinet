package studysession

import (
	"time"

	"lexinet/internal/model"
)

const (
	StudyMaximumRequestBodyBytes  = 262_144
	StudyMaximumResponseBodyBytes = 1_048_576
	StudyMaximumPageJSONBytes     = model.StudyMaximumSessionPageBytes
	StudyMaximumPageItems         = model.StudyMaximumPageItems

	StudyGlobalAdmissionLimit       = 4
	StudyPerPrincipalAdmissionLimit = 2

	StudyAuthoritativeOperationTimeout = 30 * time.Second
)

const (
	studyLifecycleReasonCreated             = "study_session_created"
	studyPredecessorTerminalReasonCompleted = "study_predecessor_cycle_completed"
)

var studyRequiredSemantics = [...]string{
	"active_scope_create_or_resume",
	"immutable_complete_card_projection",
	"scheduler_provenance",
	"singular_presented_revealed",
	"append_only_rated_rating_voided",
	"durable_client_stream_terminal_receipts",
	"server_authoritative_cycle_exhaustion",
	"atomic_predecessor_successor_linking",
	"predecessor_flush_receipt_barrier",
	"waiting_for_corpus_convergence",
	"normalized_hash_idempotency",
	"late_completed_predecessor_correction",
}
