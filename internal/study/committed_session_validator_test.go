package study

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

func TestValidateCommittedSessionAuthorityAcceptsCanonicalDetachedCycle(t *testing.T) {
	t.Parallel()
	materializer := newCycleMaterializerForTest(t)
	store := newCycleMaterializationStoreForTest(t, 2)
	materialized, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(2))
	if err != nil {
		t.Fatalf("materialize validator fixture: %v", err)
	}
	validated, err := ValidateCommittedSessionAuthority(
		materialized.Persistence.Session,
		materialized.Persistence.Items,
	)
	if err != nil {
		t.Fatalf("validate committed session: %v", err)
	}
	materialized.Persistence.Items[0].FrontCanonicalName = "mutated caller bytes"
	if validated.Items[0].FrontCanonicalName == materialized.Persistence.Items[0].FrontCanonicalName {
		t.Fatal("validated authority aliases caller-owned item bytes")
	}
}

func TestValidateCommittedSessionAdvanceAcceptsMonotonicTerminalAdvance(t *testing.T) {
	t.Parallel()
	materializer := newCycleMaterializerForTest(t)
	store := newCycleMaterializationStoreForTest(t, 2)
	materialized, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(2))
	if err != nil {
		t.Fatalf("materialize advance fixture: %v", err)
	}
	previous := materialized.Persistence.Session
	current := cloneCycleSession(previous)
	current.Lifecycle = model.StudySessionLifecycleCompleted
	current.StateVersion++
	current.ProfileIngestionHighWater++
	current.UpdatedAt = previous.UpdatedAt.Add(time.Microsecond)
	terminalizedAt := current.UpdatedAt
	terminalHighWater := current.ProfileIngestionHighWater
	current.TerminalizedAt = &terminalizedAt
	current.TerminalProfileIngestionHighWater = &terminalHighWater
	successorID := uuid.New()
	current.SuccessorStudySessionID = &successorID
	if _, err := ValidateCommittedSessionAdvance(previous, current, materialized.Persistence.Items); err != nil {
		t.Fatalf("validate terminal advance: %v", err)
	}
}

func TestValidateCommittedSessionAdvanceRejectsExactStoredScopeMutation(t *testing.T) {
	t.Parallel()
	materializer := newCycleMaterializerForTest(t)
	store := newCycleMaterializationStoreForTest(t, 2)
	materialized, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(2))
	if err != nil {
		t.Fatalf("materialize scope fixture: %v", err)
	}
	testCases := []struct {
		name   string
		mutate func(*model.StudySession)
	}{
		{name: "topic refs", mutate: func(session *model.StudySession) {
			session.Scope.Scope.TopicRefs[0] = "renamed-display-ref"
		}},
		{name: "raw topic ids", mutate: func(session *model.StudySession) {
			session.Scope.Scope.TopicIDs[0] = uuid.New()
		}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			current := cloneCycleSession(materialized.Persistence.Session)
			testCase.mutate(&current)
			if _, err := ValidateCommittedSessionAdvance(
				materialized.Persistence.Session,
				current,
				materialized.Persistence.Items,
			); err == nil {
				t.Fatal("stored scope mutation was accepted")
			}
		})
	}
}

func TestValidateCommittedSessionAdvanceFreezesTerminalAuthorityAcrossLateCorrection(t *testing.T) {
	t.Parallel()
	materializer := newCycleMaterializerForTest(t)
	store := newCycleMaterializationStoreForTest(t, 1)
	materialized, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(1))
	if err != nil {
		t.Fatalf("materialize terminal fixture: %v", err)
	}
	previous := cloneCycleSession(materialized.Persistence.Session)
	previous.Lifecycle = model.StudySessionLifecycleCompleted
	previous.StateVersion++
	previous.ProfileIngestionHighWater++
	previous.UpdatedAt = previous.UpdatedAt.Add(time.Microsecond)
	terminalizedAt := previous.UpdatedAt
	terminalHighWater := previous.ProfileIngestionHighWater
	previous.TerminalizedAt = &terminalizedAt
	previous.TerminalProfileIngestionHighWater = &terminalHighWater
	successorID := uuid.New()
	previous.SuccessorStudySessionID = &successorID

	lateCorrection := cloneCycleSession(previous)
	lateCorrection.StateVersion++
	lateCorrection.ProfileIngestionHighWater++
	lateCorrection.UpdatedAt = lateCorrection.UpdatedAt.Add(time.Microsecond)
	if _, err := ValidateCommittedSessionAdvance(previous, lateCorrection, materialized.Persistence.Items); err != nil {
		t.Fatalf("valid late correction rejected: %v", err)
	}

	mutatedTerminal := cloneCycleSession(lateCorrection)
	changedTerminalizedAt := terminalizedAt.Add(time.Microsecond)
	mutatedTerminal.TerminalizedAt = &changedTerminalizedAt
	if _, err := ValidateCommittedSessionAdvance(previous, mutatedTerminal, materialized.Persistence.Items); err == nil {
		t.Fatal("changed terminalized_at was accepted")
	}
	mutatedTerminal = cloneCycleSession(lateCorrection)
	changedTerminalHighWater := terminalHighWater + 1
	mutatedTerminal.TerminalProfileIngestionHighWater = &changedTerminalHighWater
	if _, err := ValidateCommittedSessionAdvance(previous, mutatedTerminal, materialized.Persistence.Items); err == nil {
		t.Fatal("changed terminal profile high-water was accepted")
	}
}

func TestValidateCommittedSessionAuthorityRejectsContentCorpusAndLifecycleSubstitution(t *testing.T) {
	t.Parallel()
	materializer := newCycleMaterializerForTest(t)
	store := newCycleMaterializationStoreForTest(t, 2)
	materialized, err := materializer.Materialize(context.Background(), store, cycleMaterializationInputForTest(2))
	if err != nil {
		t.Fatalf("materialize substitution fixture: %v", err)
	}
	testCases := []struct {
		name   string
		mutate func(*CyclePersistenceResult)
	}{
		{name: "content", mutate: func(result *CyclePersistenceResult) {
			result.Items[0].BackDefinition = "substituted definition"
		}},
		{name: "corpus", mutate: func(result *CyclePersistenceResult) {
			result.Session.CorpusSHA256 = model.SHA256Hex([]byte("substituted-corpus"))
		}},
		{name: "lifecycle", mutate: func(result *CyclePersistenceResult) {
			result.Session.Lifecycle = "impossible"
		}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := cloneCyclePersistenceResult(materialized.Persistence)
			testCase.mutate(&candidate)
			if _, err := ValidateCommittedSessionAuthority(candidate.Session, candidate.Items); err == nil {
				t.Fatal("substituted authority was accepted")
			}
		})
	}
}
