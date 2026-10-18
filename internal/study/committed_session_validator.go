package study

import (
	"bytes"
	"time"

	"lexinet/internal/model"
)

// CommittedSessionAuthority is a detached, receipt-free session/items
// snapshot validated against the canonical persisted-cycle invariants.
type CommittedSessionAuthority struct {
	Session model.StudySession
	Items   []model.StudySessionItem
}

// ValidateCommittedSessionAuthority validates one GET/sync repository result
// with the same lifecycle, scope, scheduler, content, corpus, order, and
// provenance rules used for post-materialization readback.
//
// @canonical study_committed_session_authority -- every receipt-free service
// read must use this validator rather than reconstructing persistence rules.
func ValidateCommittedSessionAuthority(
	session model.StudySession,
	items []model.StudySessionItem,
) (*CommittedSessionAuthority, error) {
	canonicalScheduler, err := NewVarietyFirstV1()
	if err != nil {
		return nil, newCycleMaterializationError(
			model.StudyErrorCodeInternal,
			"canonical study scheduler identity is unavailable",
			"configuration",
			"canonical_scheduler_unavailable",
			err,
		)
	}
	configuration := cycleMaterializerConfiguration{
		SchedulerName:                VarietyFirstV1Name,
		SchedulerVersion:             VarietyFirstV1Version,
		SchedulerConfigurationSHA256: canonicalScheduler.identity.configurationSHA256,
		ProjectionSchemaVersion:      StudyProjectionSchemaVersionV1,
	}
	_, validatedItems, err := reconstructPersistedCycleAuthority(CyclePersistenceResult{
		Session: session,
		Items:   items,
		// The receipt-free validator supplies only the scope field consulted by
		// the shared reconstruction path. Receipt identity is deliberately not
		// inferred and remains the create readback validator's responsibility.
		Receipt: model.StudyCreationReceipt{Scope: session.Scope},
	}, configuration)
	if err != nil {
		return nil, err
	}
	validated := &CommittedSessionAuthority{
		Session: cloneCycleSession(session),
		Items:   make([]model.StudySessionItem, len(validatedItems)),
	}
	for index := range validatedItems {
		validated.Items[index] = cloneCycleSessionItem(validatedItems[index])
	}
	return validated, nil
}

// ValidateCommittedSessionAdvance binds a later live envelope to one earlier
// immutable issuance authority while permitting only monotonic lifecycle,
// successor, state-version, profile-high-water, and live-link advances.
func ValidateCommittedSessionAdvance(
	previous model.StudySession,
	current model.StudySession,
	items []model.StudySessionItem,
) (*CommittedSessionAuthority, error) {
	if _, err := ValidateCommittedSessionAuthority(previous, items); err != nil {
		return nil, err
	}
	validated, err := ValidateCommittedSessionAuthority(current, items)
	if err != nil {
		return nil, err
	}
	if !equalCycleImmutableSessionAuthority(previous, current) || !sameCommittedSessionExactScope(previous.Scope, current.Scope) {
		return nil, newCycleMaterializationError(model.StudyErrorCodeInternal, "committed study session issuance authority changed", "committed_session", "immutable_mismatch", nil)
	}
	if err := validateCommittedSessionMutableAdvance(previous, current); err != nil {
		return nil, err
	}
	return validated, nil
}

func sameCommittedSessionExactScope(left, right model.StudyNormalizedScope) bool {
	return left.CanonicalSelectorHash == right.CanonicalSelectorHash &&
		bytes.Equal(left.CanonicalSelector, right.CanonicalSelector) &&
		equalCycleUUIDs(left.Scope.TopicIDs, right.Scope.TopicIDs) &&
		equalCycleStrings(left.Scope.TopicRefs, right.Scope.TopicRefs) &&
		left.Scope.CardKind == right.Scope.CardKind
}

func validateCommittedSessionMutableAdvance(previous, current model.StudySession) error {
	if current.StateVersion < previous.StateVersion ||
		current.ProfileIngestionHighWater < previous.ProfileIngestionHighWater ||
		current.UpdatedAt.Before(previous.UpdatedAt) ||
		!validCommittedSessionLifecycleAdvance(previous.Lifecycle, current.Lifecycle) {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "committed study session live authority regressed", "committed_session", "invalid_advance", nil)
	}
	if previous.SuccessorStudySessionID != nil &&
		!sameCycleUUIDPointer(previous.SuccessorStudySessionID, current.SuccessorStudySessionID) {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "committed study successor authority changed", "committed_session", "invalid_advance", nil)
	}
	if previous.SuccessorStudySessionID == nil && current.SuccessorStudySessionID != nil &&
		current.Lifecycle != model.StudySessionLifecycleCompleted && current.Lifecycle != model.StudySessionLifecycleSupersededEmpty {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "committed study successor appeared without terminal linkage", "committed_session", "invalid_advance", nil)
	}
	if previous.Lifecycle.Terminal() &&
		(!sameCycleTimePointer(previous.TerminalizedAt, current.TerminalizedAt) ||
			!sameCycleInt64Pointer(previous.TerminalProfileIngestionHighWater, current.TerminalProfileIngestionHighWater)) {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "committed study terminal authority changed", "committed_session", "invalid_advance", nil)
	}
	if current.StateVersion == previous.StateVersion &&
		(previous.Lifecycle != current.Lifecycle ||
			!sameCycleUUIDPointer(previous.SuccessorStudySessionID, current.SuccessorStudySessionID) ||
			!sameCycleTimePointer(previous.TerminalizedAt, current.TerminalizedAt) ||
			!sameCycleInt64Pointer(previous.TerminalProfileIngestionHighWater, current.TerminalProfileIngestionHighWater)) {
		return newCycleMaterializationError(model.StudyErrorCodeInternal, "committed study session changed without a state version advance", "committed_session", "invalid_advance", nil)
	}
	return nil
}

func validCommittedSessionLifecycleAdvance(previous, current model.StudySessionLifecycle) bool {
	if previous == current {
		return true
	}
	switch previous {
	case model.StudySessionLifecycleActive:
		return current == model.StudySessionLifecycleCompleted || current == model.StudySessionLifecycleAbandoned
	case model.StudySessionLifecycleWaitingForCorpus:
		return current == model.StudySessionLifecycleSupersededEmpty || current == model.StudySessionLifecycleAbandoned
	default:
		return false
	}
}

func sameCycleTimePointer(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}
