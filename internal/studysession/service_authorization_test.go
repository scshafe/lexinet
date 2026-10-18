package studysession

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/study"
)

func TestStudySessionServiceAuthenticationAndCapabilityDenialShortCircuitRepository(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name      string
		principal model.StudyPrincipalID
		operation StudyOperation
	}{
		{name: "create unauthenticated", operation: StudyOperationCreateOrResume},
		{name: "get unauthenticated", operation: StudyOperationGet},
		{name: "sync unauthenticated", operation: StudyOperationSync},
		{name: "create capability denied", principal: model.OperatorStudyPrincipalID(), operation: StudyOperationCreateOrResume},
		{name: "get capability denied", principal: model.OperatorStudyPrincipalID(), operation: StudyOperationGet},
		{name: "sync capability denied", principal: model.OperatorStudyPrincipalID(), operation: StudyOperationSync},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			repository := &recordingStudySessionRepository{}
			authorizer := &recordingStudyAuthorizer{denyStage: StudyAuthorizationStageCapability}
			authority := newRecordingStudyAuthoritySource()
			service := newStudySessionServiceForTest(t, repository, authorizer, authority)
			if !testCase.principal.Valid() {
				authorizer.denyStage = ""
			}
			var err error
			switch testCase.operation {
			case StudyOperationCreateOrResume:
				_, err = service.CreateOrResume(context.Background(), testCase.principal, validCreateRequestForAuthorizationTest())
			case StudyOperationGet:
				_, err = service.Get(context.Background(), testCase.principal, uuid.New(), StudyPageRequest{AfterPosition: -1, Limit: 1})
			case StudyOperationSync:
				_, err = service.Sync(context.Background(), testCase.principal, uuid.New(), validEmptySyncRequestForAuthorizationTest())
			}
			expectedCode := model.StudyErrorCodeAuthorizationDenied
			if !testCase.principal.Valid() {
				expectedCode = model.StudyErrorCodeUnauthorized
			}
			if !errors.Is(err, &model.StudyError{Code: expectedCode}) {
				t.Fatalf("error = %v", err)
			}
			if repository.calls() != 0 {
				t.Fatalf("denied operation made %d repository calls", repository.calls())
			}
			if authority.calls() != 0 {
				t.Fatalf("denied operation consumed %d authority values", authority.calls())
			}
			wantAuthorizationCalls := 1
			if !testCase.principal.Valid() {
				wantAuthorizationCalls = 0
			}
			if got := authorizer.calls(); got != wantAuthorizationCalls {
				t.Fatalf("authorization calls = %d, want %d", got, wantAuthorizationCalls)
			}
		})
	}
}

func TestStudySessionServiceResolvedTargetDenialReadsOnlyNarrowProfile(t *testing.T) {
	t.Parallel()
	for _, operation := range []StudyOperation{StudyOperationGet, StudyOperationSync} {
		t.Run(string(operation), func(t *testing.T) {
			profile := &model.StudyLearnerProfile{ID: uuid.New(), Ref: "operator"}
			repository := &recordingStudySessionRepository{profile: profile}
			authorizer := &recordingStudyAuthorizer{denyStage: StudyAuthorizationStageResolvedTarget}
			service := newStudySessionServiceForTest(t, repository, authorizer, newRecordingStudyAuthoritySource())
			sessionID := uuid.New()
			var err error
			if operation == StudyOperationGet {
				_, err = service.Get(context.Background(), model.OperatorStudyPrincipalID(), sessionID, StudyPageRequest{AfterPosition: -1, Limit: 1})
			} else {
				_, err = service.Sync(context.Background(), model.OperatorStudyPrincipalID(), sessionID, validEmptySyncRequestForAuthorizationTest())
			}
			if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeAuthorizationDenied}) {
				t.Fatalf("error = %v", err)
			}
			if repository.profileCalls != 1 || repository.cycleCalls != 0 || repository.adjudicateCalls != 0 {
				t.Fatalf("repository calls profile=%d cycle=%d adjudicate=%d", repository.profileCalls, repository.cycleCalls, repository.adjudicateCalls)
			}
			if authorizer.calls() != 2 {
				t.Fatalf("authorization calls = %d", authorizer.calls())
			}
		})
	}
}

func TestStudySessionServiceCreateRunsCapabilityThenResolvedTargetAuthorization(t *testing.T) {
	t.Parallel()
	repository := &recordingStudySessionRepository{}
	authorizer := &recordingStudyAuthorizer{denyStage: StudyAuthorizationStageResolvedTarget}
	service := newStudySessionServiceForTest(t, repository, authorizer, newRecordingStudyAuthoritySource())
	_, err := service.CreateOrResume(context.Background(), model.OperatorStudyPrincipalID(), validCreateRequestForAuthorizationTest())
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeAuthorizationDenied}) {
		t.Fatalf("error = %v", err)
	}
	requests := authorizer.requestsCopy()
	if len(requests) != 2 || requests[0].Stage != StudyAuthorizationStageCapability ||
		requests[1].Stage != StudyAuthorizationStageResolvedTarget || requests[1].ProfileRef != "operator" {
		t.Fatalf("authorization sequence = %#v", requests)
	}
	if repository.calls() != 0 {
		t.Fatalf("resolved-target denial made %d repository calls", repository.calls())
	}
}

func TestStudySessionServiceTruncatesAuthorityTimeToPostgresPrecision(t *testing.T) {
	t.Parallel()
	authority := newRecordingStudyAuthoritySource()
	authority.now = time.Date(2026, 8, 17, 1, 2, 3, 456789987, time.FixedZone("offset", -7*60*60))
	service := newStudySessionServiceForTest(t, &recordingStudySessionRepository{}, &recordingStudyAuthorizer{}, authority)
	got, err := service.authoritativeTime()
	if err != nil {
		t.Fatal(err)
	}
	want := authority.now.UTC().Truncate(time.Microsecond)
	if !got.Equal(want) || got.Location() != time.UTC || got.Nanosecond()%1_000 != 0 {
		t.Fatalf("authoritative time = %s, want %s", got, want)
	}
}

func TestStudySessionServiceBudgetsAuthorizationInsideOperationDeadline(t *testing.T) {
	t.Parallel()
	authorizer := &deadlineRecordingStudyAuthorizer{}
	service := newStudySessionServiceForTest(t, &recordingStudySessionRepository{}, authorizer, newRecordingStudyAuthoritySource())
	started := time.Now()
	_, err := service.CreateOrResume(context.Background(), model.OperatorStudyPrincipalID(), validCreateRequestForAuthorizationTest())
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeAuthorizationDenied}) {
		t.Fatalf("error = %v", err)
	}
	if authorizer.deadline.IsZero() || authorizer.deadline.Before(started.Add(StudyAuthoritativeOperationTimeout-time.Second)) ||
		authorizer.deadline.After(started.Add(StudyAuthoritativeOperationTimeout+time.Second)) {
		t.Fatalf("authorization deadline = %s", authorizer.deadline)
	}
}

type deadlineRecordingStudyAuthorizer struct{ deadline time.Time }

func (authorizer *deadlineRecordingStudyAuthorizer) AuthorizeStudy(ctx context.Context, _ StudyAuthorizationRequest) error {
	authorizer.deadline, _ = ctx.Deadline()
	return newStudySessionError(model.StudyErrorCodeAuthorizationDenied)
}

type recordingStudyAuthorizer struct {
	mu        sync.Mutex
	denyStage StudyAuthorizationStage
	requests  []StudyAuthorizationRequest
}

func (authorizer *recordingStudyAuthorizer) AuthorizeStudy(_ context.Context, request StudyAuthorizationRequest) error {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	authorizer.requests = append(authorizer.requests, request)
	if authorizer.denyStage != "" && authorizer.denyStage == request.Stage {
		return newStudySessionError(model.StudyErrorCodeAuthorizationDenied)
	}
	return nil
}

func (authorizer *recordingStudyAuthorizer) calls() int {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return len(authorizer.requests)
}

func (authorizer *recordingStudyAuthorizer) requestsCopy() []StudyAuthorizationRequest {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return append([]StudyAuthorizationRequest(nil), authorizer.requests...)
}

type recordingStudyAuthoritySource struct {
	mu              sync.Mutex
	now             time.Time
	sessionCalls    int
	seedCalls       int
	allocationCalls int
}

func newRecordingStudyAuthoritySource() *recordingStudyAuthoritySource {
	return &recordingStudyAuthoritySource{now: time.Date(2026, 8, 17, 12, 0, 0, 123456789, time.UTC)}
}

func (source *recordingStudyAuthoritySource) Now() time.Time {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.now
}

func (source *recordingStudyAuthoritySource) NewStudySessionID() (uuid.UUID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.sessionCalls++
	return uuid.MustParse("10000000-0000-4000-8000-000000000001"), nil
}

func (source *recordingStudyAuthoritySource) NewStudySeed() (string, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.seedCalls++
	return "deterministic-service-seed", nil
}

func (source *recordingStudyAuthoritySource) AllocateStudyItemIDs(_ context.Context, count int) ([]uuid.UUID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.allocationCalls++
	identities := make([]uuid.UUID, count)
	for index := range identities {
		identities[index] = uuid.NewSHA1(uuid.NameSpaceOID, []byte("service-item-"+string(rune(index))))
	}
	return identities, nil
}

func (source *recordingStudyAuthoritySource) calls() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.sessionCalls + source.seedCalls + source.allocationCalls
}

type recordingStudySessionRepository struct {
	mu sync.Mutex

	profile *model.StudyLearnerProfile

	resolveCalls     int
	profileCalls     int
	materializeCalls int
	committedCalls   int
	cycleCalls       int
	adjudicateCalls  int
}

func (repository *recordingStudySessionRepository) ResolveStudyScope(context.Context, []string, model.StudyCardKind) (model.StudyNormalizedScope, error) {
	repository.mu.Lock()
	repository.resolveCalls++
	repository.mu.Unlock()
	return model.StudyNormalizedScope{}, newStudySessionError(model.StudyErrorCodeInternal)
}

func (repository *recordingStudySessionRepository) GetStudyLearnerProfileBySessionID(context.Context, uuid.UUID) (*model.StudyLearnerProfile, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	repository.profileCalls++
	if repository.profile == nil {
		return nil, newStudySessionError(model.StudyErrorCodeSessionNotFound)
	}
	clone := *repository.profile
	return &clone, nil
}

func (repository *recordingStudySessionRepository) MaterializeStudyCycle(context.Context, *study.CycleMaterializer, StudyCycleMaterializationCommand) (*study.CycleMaterializationResult, error) {
	repository.mu.Lock()
	repository.materializeCalls++
	repository.mu.Unlock()
	return nil, newStudySessionError(model.StudyErrorCodeInternal)
}

func (repository *recordingStudySessionRepository) ReadCommittedStudyCycle(context.Context, StudyCommittedCycleReadCommand) (*study.CommittedCycle, error) {
	repository.mu.Lock()
	repository.committedCalls++
	repository.mu.Unlock()
	return nil, newStudySessionError(model.StudyErrorCodeInternal)
}

func (repository *recordingStudySessionRepository) ReadStudySessionCycle(context.Context, uuid.UUID) (*StudySessionCycleAuthority, error) {
	repository.mu.Lock()
	repository.cycleCalls++
	repository.mu.Unlock()
	return nil, newStudySessionError(model.StudyErrorCodeInternal)
}

func (repository *recordingStudySessionRepository) AdjudicateStudyEvents(context.Context, StudyEventSyncCommand) (*StudyEventSyncAuthority, error) {
	repository.mu.Lock()
	repository.adjudicateCalls++
	repository.mu.Unlock()
	return nil, newStudySessionError(model.StudyErrorCodeInternal)
}

func (repository *recordingStudySessionRepository) calls() int {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.resolveCalls + repository.profileCalls + repository.materializeCalls +
		repository.committedCalls + repository.cycleCalls + repository.adjudicateCalls
}

func newStudySessionServiceForTest(
	t *testing.T,
	repository StudySessionRepository,
	authorizer StudyAuthorizer,
	authority StudyAuthoritySource,
) *StudySessionService {
	t.Helper()
	scheduler, err := study.NewVarietyFirstV1()
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}
	service, err := NewStudySessionService(repository, authorizer, scheduler, authority, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}

func validCreateRequestForAuthorizationTest() StudyCreateSessionRequest {
	return StudyCreateSessionRequest{
		ProtocolVersion:   model.StudyProtocolVersion,
		SchemaVersion:     model.StudySchemaVersion,
		CreationRequestID: uuid.New(),
		ProfileRef:        "operator",
		Scope: StudyScopeRequest{
			TopicRefs: []string{"topic"},
			CardKind:  model.StudyCardKindIdea,
		},
		Page: StudyPageRequest{AfterPosition: -1, Limit: 1},
	}
}

func validEmptySyncRequestForAuthorizationTest() StudySyncRequest {
	return StudySyncRequest{
		ProtocolVersion:      model.StudyProtocolVersion,
		SchemaVersion:        model.StudySchemaVersion,
		SyncID:               uuid.New(),
		ClientStreamID:       uuid.New(),
		ExpectedStateVersion: 1,
		Events:               []StudyEventRequest{},
		Page:                 StudyPageRequest{AfterPosition: -1, Limit: 1},
	}
}
