package study

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

const (
	StudyMaximumShadowAdapterNameRunes       = 128
	StudyMaximumShadowAdapterVersionRunes    = 64
	StudyMaximumShadowEvaluationPayloadBytes = 64 << 10
	StudyMaximumShadowTimeout                = 30 * time.Second
)

// StudyShadowStatus is the exact non-authoritative adapter outcome vocabulary.
type StudyShadowStatus string

const (
	StudyShadowStatusNotConfigured StudyShadowStatus = "not_configured"
	StudyShadowStatusCompleted     StudyShadowStatus = "completed"
	StudyShadowStatusFailed        StudyShadowStatus = "failed"
	StudyShadowStatusTimedOut      StudyShadowStatus = "timed_out"
	StudyShadowStatusPanic         StudyShadowStatus = "panic"
)

// Valid reports whether status belongs to the frozen shadow vocabulary.
func (status StudyShadowStatus) Valid() bool {
	switch status {
	case StudyShadowStatusNotConfigured,
		StudyShadowStatusCompleted,
		StudyShadowStatusFailed,
		StudyShadowStatusTimedOut,
		StudyShadowStatusPanic:
		return true
	default:
		return false
	}
}

// StudyShadowAuthority makes the report's lack of selection authority explicit.
type StudyShadowAuthority string

const StudyShadowAuthorityNonAuthoritative StudyShadowAuthority = "non_authoritative"

// StudyShadowFailureCode is a stable machine-readable shadow-only failure.
type StudyShadowFailureCode string

const (
	StudyShadowFailureCodeInputInvalid      StudyShadowFailureCode = "STUDY_SHADOW_INPUT_INVALID"
	StudyShadowFailureCodeAdapterFailed     StudyShadowFailureCode = "STUDY_SHADOW_ADAPTER_FAILED"
	StudyShadowFailureCodeAdapterTimedOut   StudyShadowFailureCode = "STUDY_SHADOW_ADAPTER_TIMED_OUT"
	StudyShadowFailureCodeAdapterPanic      StudyShadowFailureCode = "STUDY_SHADOW_ADAPTER_PANIC"
	StudyShadowFailureCodeAdapterBusy       StudyShadowFailureCode = "STUDY_SHADOW_ADAPTER_BUSY"
	StudyShadowFailureCodeEvaluationInvalid StudyShadowFailureCode = "STUDY_SHADOW_EVALUATION_INVALID"
	StudyShadowFailureCodeCancelled         StudyShadowFailureCode = "STUDY_SHADOW_CANCELLED"
)

// StudyShadowEvaluation is bounded, opaque comparison evidence. Ordered card
// identities must be an exact permutation of the authoritative plan.
type StudyShadowEvaluation struct {
	OrderedCardIdentities []uuid.UUID     `json:"ordered_card_identities"`
	Payload               json.RawMessage `json:"payload,omitempty"`
}

// StudyShadowAdapter is the experimental evaluation seam. It receives only
// value-free cancellation plus private snapshot and plan copies—never a store,
// transaction, persistence callback, or authoritative output handle.
type StudyShadowAdapter interface {
	Evaluate(context.Context, StudySchedulingSnapshot, StudyPlan) (StudyShadowEvaluation, error)
}

// StudyShadowFailure deliberately excludes adapter errors and panic values;
// those values can contain source content, learner identities, or seeds.
type StudyShadowFailure struct {
	Code    StudyShadowFailureCode `json:"code"`
	Message string                 `json:"message"`
	Context map[string]string      `json:"context"`
}

// StudyShadowReport is observation-only and can never authorize persistence.
type StudyShadowReport struct {
	Authority      StudyShadowAuthority   `json:"authority"`
	Status         StudyShadowStatus      `json:"status"`
	AdapterName    string                 `json:"adapter_name,omitempty"`
	AdapterVersion string                 `json:"adapter_version,omitempty"`
	Evaluation     *StudyShadowEvaluation `json:"evaluation,omitempty"`
	Failure        *StudyShadowFailure    `json:"failure,omitempty"`
}

// StudyShadowRunner owns one bounded in-process shadow adapter configuration.
// The zero value is a valid not-configured runner.
//
// The authoritative scheduler is reviewed, bounded, and invoked in-process by
// the materializer before this runner is called. Shadow adapters are expected
// to honor context cancellation. A genuinely hostile adapter that may never
// terminate requires process isolation; a Go timeout cannot kill a goroutine.
// The single execution slot bounds an ignoring adapter to one leaked invocation
// per runner. That late goroutine retains only private copies, so ignored
// cancellation cannot mutate authoritative inputs or issued state.
//
// @canonical study_shadow_runner -- all experimental scheduling evaluation
// must cross this non-authoritative boundary.
type StudyShadowRunner struct {
	adapter        StudyShadowAdapter
	adapterName    string
	adapterVersion string
	timeout        time.Duration
	executionSlot  chan struct{}
}

// NewStudyShadowRunner binds one bounded adapter. A nil adapter requires empty
// identity and zero timeout and produces not_configured reports. Invalid
// configuration returns STUDY_SCHEMA_INVALID without retaining supplied text.
func NewStudyShadowRunner(
	adapter StudyShadowAdapter,
	adapterName string,
	adapterVersion string,
	timeout time.Duration,
) (*StudyShadowRunner, error) {
	if adapter == nil {
		if adapterName != "" || adapterVersion != "" || timeout != 0 {
			return nil, newSchedulerError(
				model.StudyErrorCodeSchemaInvalid,
				"unconfigured study shadow runner has unexpected configuration",
				"study_shadow_runner_construct",
				"adapter",
				"invalid_configuration",
				nil,
			)
		}
		return &StudyShadowRunner{}, nil
	}
	if !boundedShadowText(adapterName, StudyMaximumShadowAdapterNameRunes) {
		return nil, newSchedulerError(
			model.StudyErrorCodeSchemaInvalid,
			"study shadow adapter name is invalid",
			"study_shadow_runner_construct",
			"adapter_name",
			"invalid_configuration",
			nil,
		)
	}
	if !boundedShadowText(adapterVersion, StudyMaximumShadowAdapterVersionRunes) {
		return nil, newSchedulerError(
			model.StudyErrorCodeSchemaInvalid,
			"study shadow adapter version is invalid",
			"study_shadow_runner_construct",
			"adapter_version",
			"invalid_configuration",
			nil,
		)
	}
	if timeout <= 0 || timeout > StudyMaximumShadowTimeout {
		return nil, newSchedulerError(
			model.StudyErrorCodeSchemaInvalid,
			"study shadow adapter timeout is invalid",
			"study_shadow_runner_construct",
			"timeout",
			"invalid_configuration",
			nil,
		)
	}
	executionSlot := make(chan struct{}, 1)
	executionSlot <- struct{}{}
	return &StudyShadowRunner{
		adapter:        adapter,
		adapterName:    adapterName,
		adapterVersion: adapterVersion,
		timeout:        timeout,
		executionSlot:  executionSlot,
	}, nil
}

// Run evaluates private copies and always returns a non-authoritative report.
// It is a post-commit-only hook: composition must call it only after the
// authority-owning transaction has returned nil. This pure core cannot prove a
// commit occurred. Adapter error, timeout, invalid output, or panic cannot
// change the supplied snapshot or authoritative plan and is never promoted to
// a production error.
func (runner *StudyShadowRunner) Run(
	callerContext context.Context,
	snapshot StudySchedulingSnapshot,
	authoritativePlan StudyPlan,
) StudyShadowReport {
	report := StudyShadowReport{
		Authority: StudyShadowAuthorityNonAuthoritative,
		Status:    StudyShadowStatusNotConfigured,
	}
	if runner == nil || runner.adapter == nil {
		return report
	}
	report.AdapterName = runner.adapterName
	report.AdapterVersion = runner.adapterVersion
	if callerContext == nil {
		return report.withFailure(
			StudyShadowStatusFailed,
			newStudyShadowFailure(StudyShadowFailureCodeInputInvalid, "study shadow input is invalid", "caller_context_missing"),
		)
	}
	if failure := validateStudyShadowInputs(snapshot, authoritativePlan); failure != nil {
		return report.withFailure(StudyShadowStatusFailed, failure)
	}
	if callerErr := callerContext.Err(); callerErr != nil {
		return reportForStudyShadowContextError(report, callerErr)
	}
	select {
	case <-runner.executionSlot:
		// The adapter goroutine releases this slot only as it exits.
	default:
		return report.withFailure(
			StudyShadowStatusFailed,
			newStudyShadowFailure(StudyShadowFailureCodeAdapterBusy, "study shadow adapter already has an in-flight evaluation", "adapter_execution_in_progress"),
		)
	}

	privateSnapshot := cloneStudyShadowSnapshot(snapshot)
	frozenAuthoritativePlan := cloneStudyShadowPlan(authoritativePlan)
	privateAdapterPlan := cloneStudyShadowPlan(frozenAuthoritativePlan)
	// Deliberately discard caller context values at the adapter boundary. Run
	// observes caller cancellation separately below.
	adapterContext, cancelAdapter := context.WithTimeout(context.Background(), runner.timeout)
	defer cancelAdapter()
	resultChannel := make(chan studyShadowAdapterResult, 1)
	go invokeStudyShadowAdapter(
		adapterContext,
		runner.adapter,
		privateSnapshot,
		privateAdapterPlan,
		resultChannel,
		runner.executionSlot,
	)

	select {
	case <-callerContext.Done():
		cancelAdapter()
		return reportForStudyShadowContextError(report, callerContext.Err())
	case <-adapterContext.Done():
		return report.withFailure(
			StudyShadowStatusTimedOut,
			newStudyShadowFailure(StudyShadowFailureCodeAdapterTimedOut, "study shadow adapter timed out", "adapter_timeout"),
		)
	case result := <-resultChannel:
		if callerErr := callerContext.Err(); callerErr != nil {
			cancelAdapter()
			return reportForStudyShadowContextError(report, callerErr)
		}
		if adapterErr := adapterContext.Err(); adapterErr != nil {
			return report.withFailure(
				StudyShadowStatusTimedOut,
				newStudyShadowFailure(StudyShadowFailureCodeAdapterTimedOut, "study shadow adapter timed out", "adapter_timeout"),
			)
		}
		cancelAdapter()
		if result.panicked {
			return report.withFailure(
				StudyShadowStatusPanic,
				newStudyShadowFailure(StudyShadowFailureCodeAdapterPanic, "study shadow adapter panicked", "adapter_panic"),
			)
		}
		if result.err != nil {
			return report.withFailure(
				StudyShadowStatusFailed,
				newStudyShadowFailure(StudyShadowFailureCodeAdapterFailed, "study shadow adapter failed", "adapter_error"),
			)
		}
		evaluation, failure := validateAndCloneStudyShadowEvaluation(result.evaluation, frozenAuthoritativePlan)
		if failure != nil {
			return report.withFailure(StudyShadowStatusFailed, failure)
		}
		report.Status = StudyShadowStatusCompleted
		report.Evaluation = &evaluation
		return report
	}
}

type studyShadowAdapterResult struct {
	evaluation StudyShadowEvaluation
	err        error
	panicked   bool
}

func invokeStudyShadowAdapter(
	adapterContext context.Context,
	adapter StudyShadowAdapter,
	snapshot StudySchedulingSnapshot,
	authoritativePlan StudyPlan,
	resultChannel chan<- studyShadowAdapterResult,
	executionSlot chan<- struct{},
) {
	result := studyShadowAdapterResult{}
	completed := false
	defer func() {
		if recovered := recover(); recovered != nil || !completed {
			result = studyShadowAdapterResult{panicked: true}
		}
		// The adapter has returned or unwound. Make the runner reusable before
		// publishing the result so a completed Run cannot report false busy.
		executionSlot <- struct{}{}
		resultChannel <- result
	}()
	result.evaluation, result.err = adapter.Evaluate(adapterContext, snapshot, authoritativePlan)
	completed = true
}

func validateStudyShadowInputs(snapshot StudySchedulingSnapshot, authoritativePlan StudyPlan) *StudyShadowFailure {
	if len(snapshot.Cards) > StudyMaximumSchedulingCards || len(authoritativePlan.Cards) > StudyMaximumSchedulingCards {
		return newStudyShadowFailure(StudyShadowFailureCodeInputInvalid, "study shadow input exceeds the current work bound", "input_work_bound_exceeded")
	}
	if len(snapshot.Cards) != len(authoritativePlan.Cards) {
		return newStudyShadowFailure(StudyShadowFailureCodeInputInvalid, "study shadow authoritative order does not match its snapshot", "authoritative_identity_count_mismatch")
	}
	if authoritativePlan.CorpusSHA256 != snapshot.CorpusSHA256 ||
		authoritativePlan.Scheduler.InputProfileIngestionHighWater != snapshot.InputProfileIngestionHighWater ||
		!authoritativePlan.Scheduler.PlannedAt.Equal(snapshot.PlannedAt) ||
		authoritativePlan.Scheduler.Seed != snapshot.Seed ||
		!boundedShadowText(authoritativePlan.Scheduler.Name, 128) ||
		!boundedShadowText(authoritativePlan.Scheduler.Version, 64) ||
		!model.IsStudySHA256(authoritativePlan.Scheduler.ConfigurationSHA256) {
		return newStudyShadowFailure(StudyShadowFailureCodeInputInvalid, "study shadow authoritative provenance does not match its snapshot", "authoritative_provenance_mismatch")
	}
	snapshotIdentities := make(map[uuid.UUID]struct{}, len(snapshot.Cards))
	for _, card := range snapshot.Cards {
		if card.CardIdentity == uuid.Nil {
			return newStudyShadowFailure(StudyShadowFailureCodeInputInvalid, "study shadow snapshot order is invalid", "snapshot_identity_invalid")
		}
		if _, duplicate := snapshotIdentities[card.CardIdentity]; duplicate {
			return newStudyShadowFailure(StudyShadowFailureCodeInputInvalid, "study shadow snapshot order is invalid", "snapshot_identity_duplicate")
		}
		snapshotIdentities[card.CardIdentity] = struct{}{}
	}
	authoritativeIdentities := make(map[uuid.UUID]struct{}, len(authoritativePlan.Cards))
	for _, card := range authoritativePlan.Cards {
		if card.CardIdentity == uuid.Nil {
			return newStudyShadowFailure(StudyShadowFailureCodeInputInvalid, "study shadow authoritative order is invalid", "authoritative_order_invalid")
		}
		if _, duplicate := authoritativeIdentities[card.CardIdentity]; duplicate {
			return newStudyShadowFailure(StudyShadowFailureCodeInputInvalid, "study shadow authoritative order is invalid", "authoritative_order_duplicate")
		}
		authoritativeIdentities[card.CardIdentity] = struct{}{}
		if _, expected := snapshotIdentities[card.CardIdentity]; !expected {
			return newStudyShadowFailure(StudyShadowFailureCodeInputInvalid, "study shadow authoritative order does not match its snapshot", "authoritative_identity_mismatch")
		}
	}
	return nil
}

func validateAndCloneStudyShadowEvaluation(
	evaluation StudyShadowEvaluation,
	authoritativePlan StudyPlan,
) (StudyShadowEvaluation, *StudyShadowFailure) {
	if len(evaluation.OrderedCardIdentities) > StudyMaximumSchedulingCards {
		return StudyShadowEvaluation{}, newStudyShadowFailure(StudyShadowFailureCodeEvaluationInvalid, "study shadow evaluation order exceeds the current work bound", "evaluation_order_work_bound_exceeded")
	}
	if len(evaluation.OrderedCardIdentities) != len(authoritativePlan.Cards) {
		return StudyShadowEvaluation{}, newStudyShadowFailure(StudyShadowFailureCodeEvaluationInvalid, "study shadow evaluation order is incomplete", "evaluation_order_count_mismatch")
	}
	if len(evaluation.Payload) > StudyMaximumShadowEvaluationPayloadBytes {
		return StudyShadowEvaluation{}, newStudyShadowFailure(StudyShadowFailureCodeEvaluationInvalid, "study shadow evaluation payload exceeds the current work bound", "evaluation_payload_work_bound_exceeded")
	}
	if len(evaluation.Payload) > 0 && !json.Valid(evaluation.Payload) {
		return StudyShadowEvaluation{}, newStudyShadowFailure(StudyShadowFailureCodeEvaluationInvalid, "study shadow evaluation payload is invalid", "evaluation_payload_invalid")
	}

	authoritativeIdentities := make(map[uuid.UUID]struct{}, len(authoritativePlan.Cards))
	for _, card := range authoritativePlan.Cards {
		authoritativeIdentities[card.CardIdentity] = struct{}{}
	}
	seen := make(map[uuid.UUID]struct{}, len(evaluation.OrderedCardIdentities))
	for _, identity := range evaluation.OrderedCardIdentities {
		if _, expected := authoritativeIdentities[identity]; !expected {
			return StudyShadowEvaluation{}, newStudyShadowFailure(StudyShadowFailureCodeEvaluationInvalid, "study shadow evaluation contains an unknown identity", "evaluation_order_identity_mismatch")
		}
		if _, duplicate := seen[identity]; duplicate {
			return StudyShadowEvaluation{}, newStudyShadowFailure(StudyShadowFailureCodeEvaluationInvalid, "study shadow evaluation repeats an identity", "evaluation_order_duplicate")
		}
		seen[identity] = struct{}{}
	}
	return StudyShadowEvaluation{
		OrderedCardIdentities: append([]uuid.UUID(nil), evaluation.OrderedCardIdentities...),
		Payload:               json.RawMessage(append([]byte(nil), evaluation.Payload...)),
	}, nil
}

func cloneStudyShadowSnapshot(snapshot StudySchedulingSnapshot) StudySchedulingSnapshot {
	clone := snapshot
	clone.Cards = append([]StudySchedulingCard(nil), snapshot.Cards...)
	if snapshot.PredecessorTailCardIdentity != nil {
		predecessorTail := *snapshot.PredecessorTailCardIdentity
		clone.PredecessorTailCardIdentity = &predecessorTail
	}
	for index := range clone.Cards {
		if snapshot.Cards[index].Projection.LatestEffectiveRating != nil {
			rating := *snapshot.Cards[index].Projection.LatestEffectiveRating
			clone.Cards[index].Projection.LatestEffectiveRating = &rating
		}
		if snapshot.Cards[index].Projection.LatestPresentedIngestionOrdinal != nil {
			ordinal := *snapshot.Cards[index].Projection.LatestPresentedIngestionOrdinal
			clone.Cards[index].Projection.LatestPresentedIngestionOrdinal = &ordinal
		}
	}
	return clone
}

func cloneStudyShadowPlan(plan StudyPlan) StudyPlan {
	clone := plan
	clone.Cards = append([]StudyPlannedCard(nil), plan.Cards...)
	for index := range clone.Cards {
		ordinal := plan.Cards[index].SelectionReason.LatestPresentedIngestionOrdinal
		if ordinal != nil {
			copyOfOrdinal := *ordinal
			clone.Cards[index].SelectionReason.LatestPresentedIngestionOrdinal = &copyOfOrdinal
		}
	}
	return clone
}

func reportForStudyShadowContextError(report StudyShadowReport, contextErr error) StudyShadowReport {
	if contextErr == context.DeadlineExceeded {
		return report.withFailure(
			StudyShadowStatusTimedOut,
			newStudyShadowFailure(StudyShadowFailureCodeAdapterTimedOut, "study shadow evaluation timed out", "caller_deadline"),
		)
	}
	return report.withFailure(
		StudyShadowStatusFailed,
		newStudyShadowFailure(StudyShadowFailureCodeCancelled, "study shadow evaluation was cancelled", "caller_cancelled"),
	)
}

func (report StudyShadowReport) withFailure(status StudyShadowStatus, failure *StudyShadowFailure) StudyShadowReport {
	report.Status = status
	report.Evaluation = nil
	report.Failure = failure
	return report
}

func newStudyShadowFailure(code StudyShadowFailureCode, message, reason string) *StudyShadowFailure {
	return &StudyShadowFailure{
		Code:    code,
		Message: message,
		Context: map[string]string{
			"operation": "study_shadow_evaluate",
			"reason":    reason,
		},
	}
}

func boundedShadowText(value string, maximumRunes int) bool {
	return utf8.ValidString(value) && boundedSchedulerText(value, maximumRunes)
}
