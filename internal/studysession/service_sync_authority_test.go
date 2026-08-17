package studysession

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/study"
)

func TestValidateStudyEventSyncAuthorityAcceptsExactFreshAuthority(t *testing.T) {
	t.Parallel()
	fixture := newValidStudySyncAuthorityFixture(t)
	if err := fixture.validate(); err != nil {
		t.Fatalf("validate fresh sync authority: %v", err)
	}
}

func TestValidateStudyEventSyncAuthorityBindsNoSuffixStateWithoutAdvance(t *testing.T) {
	t.Parallel()
	fixture := newValidStudySyncAuthorityFixture(t)
	fixture.request.Events = []StudyEventRequest{}
	var err error
	fixture.hash, fixture.events, err = normalizeSyncStudyRequest(fixture.sessionID, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.authority.Receipts = []model.StudyEventReceipt{}
	fixture.authority.Session = cloneServiceSession(fixture.previous.Session)
	fixture.authority.FlushReceipt.NormalizedSyncRequestSHA256 = fixture.hash
	fixture.authority.FlushReceipt.RequestFirstClientSequence = nil
	fixture.authority.FlushReceipt.RequestLastClientSequence = nil
	fixture.authority.FlushReceipt.FirstClientSequence = nil
	fixture.authority.FlushReceipt.LastClientSequence = nil
	fixture.authority.FlushReceipt.AcknowledgedThroughSequence = 0
	fixture.authority.FlushReceipt.ProfileIngestionHighWater = fixture.previous.Profile.ProfileIngestionHighWater
	fixture.authority.FlushReceipt.StudySessionStateVersion = fixture.request.ExpectedStateVersion
	if err := fixture.validate(); err != nil {
		t.Fatalf("validate no-suffix sync authority: %v", err)
	}
	fixture.authority.FlushReceipt.StudySessionStateVersion++
	fixture.authority.Session.StateVersion++
	if err := fixture.validate(); err == nil {
		t.Fatal("no-suffix state advance was accepted")
	}
}

func TestValidateStudyEventSyncAuthorityRejectsMutationMatrix(t *testing.T) {
	t.Parallel()
	fixture := newValidStudySyncAuthorityFixture(t)
	otherPrincipal, err := model.NewStudyPrincipalID(model.StudyPrincipalNamespaceFixture, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*studySyncAuthorityFixture)
	}{
		{name: "principal", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.FlushReceipt.StudyPrincipalID = otherPrincipal
		}},
		{name: "profile", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.FlushReceipt.LearnerProfileID = uuid.New()
		}},
		{name: "session", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.FlushReceipt.StudySessionID = uuid.New()
		}},
		{name: "stream", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.FlushReceipt.ClientStreamID = uuid.New()
		}},
		{name: "normalized hash", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.FlushReceipt.NormalizedSyncRequestSHA256 = repeatedSHA('9')
		}},
		{name: "required page marker", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.FlushReceipt.SessionPageArtifactRequired = false
		}},
		{name: "request window", mutate: func(candidate *studySyncAuthorityFixture) {
			changed := int64(2)
			candidate.authority.FlushReceipt.RequestFirstClientSequence = &changed
		}},
		{name: "acknowledged suffix", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.FlushReceipt.AcknowledgedThroughSequence++
		}},
		{name: "state delta", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.FlushReceipt.StudySessionStateVersion++
			candidate.authority.Session.StateVersion++
		}},
		{name: "fresh high water equality", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.FlushReceipt.ProfileIngestionHighWater--
		}},
		{name: "fresh page bytes", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.PageArtifact.CanonicalJSON = append([]byte(nil), candidate.authority.PageArtifact.CanonicalJSON...)
			candidate.authority.PageArtifact.CanonicalJSON[len(candidate.authority.PageArtifact.CanonicalJSON)-1] ^= 1
			candidate.authority.PageArtifact.CanonicalSHA256 = model.SHA256Hex(candidate.authority.PageArtifact.CanonicalJSON)
		}},
		{name: "receipt event", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.Receipts[0].EventID = uuid.New()
		}},
		{name: "bound payload", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.Receipts[0].BoundPayloadSHA256 = repeatedSHA('8')
		}},
		{name: "binding truth", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.Receipts[0].EventIDBindingMatchesInput = false
		}},
		{name: "created after flush", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.Receipts[0].CreatedAt = candidate.authority.FlushReceipt.IssuedAt.Add(time.Microsecond)
		}},
		{name: "accepted canonical id", mutate: func(candidate *studySyncAuthorityFixture) {
			changed := uuid.New()
			candidate.authority.Receipts[0].CanonicalStudyEventID = &changed
		}},
		{name: "canonical ordinal above high water", mutate: func(candidate *studySyncAuthorityFixture) {
			changed := candidate.authority.FlushReceipt.ProfileIngestionHighWater + 1
			candidate.authority.Receipts[0].CanonicalProfileIngestionOrdinal = &changed
		}},
		{name: "receipt origin", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.Receipts[0].OriginatingStudyFlushReceiptID = uuid.New()
		}},
		{name: "accepted terminal cause", mutate: func(candidate *studySyncAuthorityFixture) {
			cause := candidate.authority.Receipts[0].ClientSequence
			candidate.authority.Receipts[0].TerminalFailureClientSequence = &cause
		}},
		{name: "internally valid scope and corpus substitution", mutate: func(candidate *studySyncAuthorityFixture) {
			scope, normalizeErr := model.NormalizeStudyScope(model.StudyScope{
				TopicIDs:  []uuid.UUID{uuid.New()},
				TopicRefs: []string{"substituted-topic"},
				CardKind:  model.StudyCardKindIdea,
			})
			if normalizeErr != nil {
				panic(normalizeErr)
			}
			candidate.authority.Session.Scope = scope
			candidate.authority.Session.CorpusSHA256 = serviceCorpusFromItems(t, scope, candidate.previous.Items)
		}},
		{name: "corpus", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.Session.CorpusSHA256 = repeatedSHA('7')
		}},
		{name: "scheduler", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.Session.Scheduler.Name = "substituted"
		}},
		{name: "lifecycle", mutate: func(candidate *studySyncAuthorityFixture) {
			candidate.authority.Session.Lifecycle = model.StudySessionLifecycleWaitingForCorpus
			candidate.authority.Session.Scheduler = nil
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			candidate := fixture.clone()
			mutation.mutate(&candidate)
			if err := candidate.validate(); !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeInternal}) {
				t.Fatalf("mutation error = %v", err)
			}
		})
	}
}

func TestValidateStudyFlushSuffixReceiptGroupBindsTerminalCause(t *testing.T) {
	t.Parallel()
	first, last, cause := int64(1), int64(2), int64(2)
	flush := model.StudyFlushReceipt{FirstClientSequence: &first, LastClientSequence: &last}
	receipts := []model.StudyEventReceipt{
		{ClientSequence: 1, Disposition: model.StudyEventReceiptDispositionTerminalRejected, Code: model.StudyEventReceiptCodeTransitionInvalid, TerminalFailureClientSequence: &cause, EventIDBindingMatchesInput: true},
		{ClientSequence: 2, Disposition: model.StudyEventReceiptDispositionTerminalRejected, Code: model.StudyEventReceiptCodeTransitionInvalid, TerminalFailureClientSequence: &cause, EventIDBindingMatchesInput: true},
	}
	if err := validateStudyFlushSuffixReceiptGroup(flush, receipts); err != nil {
		t.Fatalf("valid terminal group: %v", err)
	}
	changedCause := int64(1)
	receipts[0].TerminalFailureClientSequence = &changedCause
	if err := validateStudyFlushSuffixReceiptGroup(flush, receipts); err == nil {
		t.Fatal("mixed terminal cause was accepted")
	}
	receipts[0].TerminalFailureClientSequence = &cause
	receipts[1].Code = model.StudyEventReceiptCodeIDConflict
	if err := validateStudyFlushSuffixReceiptGroup(flush, receipts); err == nil {
		t.Fatal("mixed terminal code was accepted")
	}
	receipts[0].Code = model.StudyEventReceiptCodeIDConflict
	receipts[1].EventIDBindingMatchesInput = false
	if err := validateStudyFlushSuffixReceiptGroup(flush, receipts); err != nil {
		t.Fatalf("valid ID conflict cause rejected: %v", err)
	}
	receipts[1].EventIDBindingMatchesInput = true
	if err := validateStudyFlushSuffixReceiptGroup(flush, receipts); err == nil {
		t.Fatal("ID conflict self-cause with matching binding was accepted")
	}
}

func TestStudySessionServiceSyncLateReplayUsesPersistedPageWithLiveEnvelope(t *testing.T) {
	t.Parallel()
	fixture := newValidStudySyncAuthorityFixture(t)
	originalLiveSource := *fixture.previous.Items[0].LiveSourceIdeaID
	repointed := uuid.New()
	fixture.previous.Items[0].LiveSourceIdeaID = &repointed
	fixture.previous.Profile.ProfileIngestionHighWater = 3
	fixture.previous.Session.ProfileIngestionHighWater = 3
	fixture.previous.Session.StateVersion = 3
	fixture.previous.Session.UpdatedAt = fixture.previous.Session.UpdatedAt.Add(2 * time.Microsecond)
	fixture.authority.Session = cloneServiceSession(fixture.previous.Session)
	fixture.authority.Replayed = true
	repository := &syncAuthorityServiceRepository{fixture: fixture}
	service := newStudySessionServiceForTest(t, repository, OperatorStudyAuthorizer{}, newRecordingStudyAuthoritySource())
	result, err := service.Sync(context.Background(), fixture.principal, fixture.sessionID, fixture.request)
	if err != nil {
		t.Fatalf("late replay sync: %v", err)
	}
	if result.Resource.Session.StateVersion != 3 || result.Resource.FlushReceipt.StateVersion != 2 ||
		len(result.Resource.Session.Page.Items) != 1 || result.Resource.Session.Page.Items[0].LiveSourceIdeaID == nil ||
		*result.Resource.Session.Page.Items[0].LiveSourceIdeaID != originalLiveSource {
		t.Fatalf("late replay response = %#v", result.Resource)
	}
	if repository.adjudicateCalls != 1 {
		t.Fatalf("adjudicate calls = %d", repository.adjudicateCalls)
	}
}

type studySyncAuthorityFixture struct {
	principal model.StudyPrincipalID
	previous  StudySessionCycleAuthority
	sessionID uuid.UUID
	request   StudySyncRequest
	hash      string
	events    []model.StudyEvent
	page      model.StudySessionPageArtifact
	authority StudyEventSyncAuthority
}

func newValidStudySyncAuthorityFixture(t *testing.T) studySyncAuthorityFixture {
	t.Helper()
	store := newServiceMaterializationStore(t, 1)
	scheduler, err := study.NewVarietyFirstV1()
	if err != nil {
		t.Fatal(err)
	}
	authoritySource := newRecordingStudyAuthoritySource()
	materializer, err := study.NewCanonicalCycleMaterializer(scheduler, authoritySource)
	if err != nil {
		t.Fatal(err)
	}
	principal := model.OperatorStudyPrincipalID()
	sessionID := uuid.MustParse("40000000-0000-4000-8000-000000000001")
	materialized, err := materializer.Materialize(context.Background(), store, study.CycleMaterializationInput{
		SessionID:                     sessionID,
		Principal:                     principal,
		CreationRequestID:             uuid.New(),
		NormalizedRequestSHA256:       repeatedSHA('4'),
		PlannedAt:                     store.createdAt,
		Seed:                          "sync-fixture-seed",
		LifecycleReasonCode:           "created",
		PredecessorTerminalReasonCode: "completed",
	})
	if err != nil {
		t.Fatalf("materialize sync fixture: %v", err)
	}
	previous := StudySessionCycleAuthority{
		Profile: store.captured.LearnerProfile,
		Session: cloneServiceSession(materialized.Persistence.Session),
		Items:   cloneStudySessionItems(materialized.Persistence.Items),
	}
	item := previous.Items[0]
	request := StudySyncRequest{
		ProtocolVersion:      model.StudyProtocolVersion,
		SchemaVersion:        model.StudySchemaVersion,
		SyncID:               uuid.New(),
		ClientStreamID:       uuid.New(),
		ExpectedStateVersion: previous.Session.StateVersion,
		Events: []StudyEventRequest{
			{
				EventID:            uuid.New(),
				ClientSequence:     1,
				ProfileRef:         previous.Profile.Ref,
				SessionItemID:      item.ID,
				CardIdentity:       item.CardIdentity,
				ContentSHA256:      item.SourceContentSHA256,
				Kind:               model.StudyEventKindPresented,
				ClientClockQuality: model.StudyClientClockQualityUnknown,
			},
			{
				EventID:            uuid.New(),
				ClientSequence:     2,
				ProfileRef:         previous.Profile.Ref,
				SessionItemID:      item.ID,
				CardIdentity:       item.CardIdentity,
				ContentSHA256:      item.SourceContentSHA256,
				Kind:               model.StudyEventKindRevealed,
				ClientClockQuality: model.StudyClientClockQualityUnknown,
			},
		},
		Page: StudyPageRequest{AfterPosition: -1, Limit: 1},
	}
	hash, events, err := normalizeSyncStudyRequest(sessionID, request)
	if err != nil {
		t.Fatal(err)
	}
	_, page, err := BuildStudySessionPage(previous.Items, request.Page)
	if err != nil {
		t.Fatal(err)
	}
	current := cloneServiceSession(previous.Session)
	current.StateVersion++
	current.ProfileIngestionHighWater = 2
	current.UpdatedAt = current.UpdatedAt.Add(time.Microsecond)
	first, last := int64(1), int64(2)
	issuedAt := store.createdAt.Add(10 * time.Microsecond)
	flushID := uuid.New()
	flush := model.StudyFlushReceipt{
		ID:                          flushID,
		StudyPrincipalID:            principal,
		LearnerProfileID:            previous.Profile.ID,
		SyncRequestID:               request.SyncID,
		NormalizedSyncRequestSHA256: hash,
		StudySessionID:              sessionID,
		ClientStreamID:              request.ClientStreamID,
		RequestFirstClientSequence:  &first,
		RequestLastClientSequence:   &last,
		FirstClientSequence:         &first,
		LastClientSequence:          &last,
		AcknowledgedThroughSequence: last,
		ProfileIngestionHighWater:   2,
		StudySessionStateVersion:    current.StateVersion,
		SessionPageArtifactRequired: true,
		IssuedAt:                    issuedAt,
	}
	receipts := make([]model.StudyEventReceipt, len(events))
	for index, event := range events {
		_, payloadSHA256, normalizeErr := model.NormalizeStudyEventPayload(event)
		if normalizeErr != nil {
			t.Fatal(normalizeErr)
		}
		ordinal := int64(index + 1)
		canonicalID := event.ID
		receipts[index] = model.StudyEventReceipt{
			ClientStreamID:                   request.ClientStreamID,
			ClientSequence:                   event.ClientSequence,
			OriginatingStudyFlushReceiptID:   flushID,
			LearnerProfileID:                 previous.Profile.ID,
			StudySessionID:                   sessionID,
			StudySessionItemID:               event.StudySessionItemID,
			CardIdentity:                     event.CardIdentity,
			ContentSHA256:                    event.ContentSHA256,
			EventID:                          event.ID,
			BoundPayloadSHA256:               payloadSHA256,
			PayloadSHA256:                    payloadSHA256,
			EventIDBindingMatchesInput:       true,
			Disposition:                      model.StudyEventReceiptDispositionAccepted,
			Code:                             model.StudyEventReceiptCodeAccepted,
			CanonicalStudyEventID:            &canonicalID,
			CanonicalProfileIngestionOrdinal: &ordinal,
			CreatedAt:                        issuedAt.Add(-time.Microsecond),
		}
	}
	return studySyncAuthorityFixture{
		principal: principal,
		previous:  previous,
		sessionID: sessionID,
		request:   request,
		hash:      hash,
		events:    events,
		page:      page,
		authority: StudyEventSyncAuthority{
			Receipts:     receipts,
			FlushReceipt: flush,
			Session:      current,
			PageArtifact: page,
		},
	}
}

func (fixture studySyncAuthorityFixture) validate() error {
	return validateStudyEventSyncAuthority(
		&fixture.authority,
		fixture.principal,
		fixture.previous,
		fixture.sessionID,
		fixture.request,
		fixture.hash,
		fixture.events,
		fixture.page,
	)
}

func (fixture studySyncAuthorityFixture) clone() studySyncAuthorityFixture {
	clone := fixture
	clone.previous = cloneStudySessionCycleAuthority(fixture.previous)
	clone.events = append([]model.StudyEvent(nil), fixture.events...)
	clone.page = cloneServicePageArtifact(fixture.page)
	clone.authority = cloneStudyEventSyncAuthority(fixture.authority)
	return clone
}

type syncAuthorityServiceRepository struct {
	fixture         studySyncAuthorityFixture
	adjudicateCalls int
}

func (repository *syncAuthorityServiceRepository) ResolveStudyScope(context.Context, []string, model.StudyCardKind) (model.StudyNormalizedScope, error) {
	return model.StudyNormalizedScope{}, newStudySessionError(model.StudyErrorCodeInternal)
}

func (repository *syncAuthorityServiceRepository) GetStudyLearnerProfileBySessionID(context.Context, uuid.UUID) (*model.StudyLearnerProfile, error) {
	profile := repository.fixture.previous.Profile
	return &profile, nil
}

func (*syncAuthorityServiceRepository) MaterializeStudyCycle(context.Context, *study.CycleMaterializer, StudyCycleMaterializationCommand) (*study.CycleMaterializationResult, error) {
	return nil, newStudySessionError(model.StudyErrorCodeInternal)
}

func (*syncAuthorityServiceRepository) ReadCommittedStudyCycle(context.Context, StudyCommittedCycleReadCommand) (*study.CommittedCycle, error) {
	return nil, newStudySessionError(model.StudyErrorCodeInternal)
}

func (repository *syncAuthorityServiceRepository) ReadStudySessionCycle(context.Context, uuid.UUID) (*StudySessionCycleAuthority, error) {
	cycle := cloneStudySessionCycleAuthority(repository.fixture.previous)
	return &cycle, nil
}

func (repository *syncAuthorityServiceRepository) AdjudicateStudyEvents(_ context.Context, command StudyEventSyncCommand) (*StudyEventSyncAuthority, error) {
	repository.adjudicateCalls++
	if command.NormalizedRequestSHA256 != repository.fixture.hash || command.SyncRequestID != repository.fixture.request.SyncID {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	authority := cloneStudyEventSyncAuthority(repository.fixture.authority)
	return &authority, nil
}

func cloneStudySessionCycleAuthority(source StudySessionCycleAuthority) StudySessionCycleAuthority {
	clone := source
	clone.Session = cloneServiceSession(source.Session)
	clone.Items = cloneStudySessionItems(source.Items)
	return clone
}

func cloneStudyEventSyncAuthority(source StudyEventSyncAuthority) StudyEventSyncAuthority {
	clone := source
	clone.Session = cloneServiceSession(source.Session)
	clone.PageArtifact = cloneServicePageArtifact(source.PageArtifact)
	clone.Receipts = make([]model.StudyEventReceipt, len(source.Receipts))
	for index, receipt := range source.Receipts {
		clone.Receipts[index] = receipt
		clone.Receipts[index].TerminalFailureClientSequence = cloneInt64Pointer(receipt.TerminalFailureClientSequence)
		clone.Receipts[index].CanonicalStudyEventID = cloneUUIDPointer(receipt.CanonicalStudyEventID)
		clone.Receipts[index].CanonicalProfileIngestionOrdinal = cloneInt64Pointer(receipt.CanonicalProfileIngestionOrdinal)
	}
	clone.FlushReceipt.RequestFirstClientSequence = cloneInt64Pointer(source.FlushReceipt.RequestFirstClientSequence)
	clone.FlushReceipt.RequestLastClientSequence = cloneInt64Pointer(source.FlushReceipt.RequestLastClientSequence)
	clone.FlushReceipt.FirstClientSequence = cloneInt64Pointer(source.FlushReceipt.FirstClientSequence)
	clone.FlushReceipt.LastClientSequence = cloneInt64Pointer(source.FlushReceipt.LastClientSequence)
	return clone
}

func cloneServiceSession(source model.StudySession) model.StudySession {
	clone := source
	clone.Scope = cloneServiceNormalizedScope(source.Scope)
	clone.Scheduler = cloneServiceScheduler(source.Scheduler)
	clone.PredecessorStudySessionID = cloneUUIDPointer(source.PredecessorStudySessionID)
	clone.SuccessorStudySessionID = cloneUUIDPointer(source.SuccessorStudySessionID)
	clone.TerminalizedAt = cloneTimePointer(source.TerminalizedAt)
	clone.TerminalProfileIngestionHighWater = cloneInt64Pointer(source.TerminalProfileIngestionHighWater)
	return clone
}

func cloneServicePageArtifact(source model.StudySessionPageArtifact) model.StudySessionPageArtifact {
	clone := source
	clone.CanonicalJSON = append([]byte(nil), source.CanonicalJSON...)
	return clone
}

func serviceCorpusFromItems(t *testing.T, scope model.StudyNormalizedScope, items []model.StudySessionItem) string {
	t.Helper()
	sources := make([]study.MaterializationSource, len(items))
	for index, item := range items {
		sources[index] = study.MaterializationSource{
			CardIdentity:          item.CardIdentity,
			SourceContentRevision: item.SourceContentRevision,
			SourceContentSHA256:   item.SourceContentSHA256,
		}
	}
	digest, err := serviceCorpusSHA256(scope, sources)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
