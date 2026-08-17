package studysession

import (
	"context"
	"encoding/binary"
	"errors"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/study"
	studydigest "lexinet/internal/study/digest"
)

func TestStudySessionServiceCreateHappyPathAllocatesLazilyAndShadowsCreatedOnly(t *testing.T) {
	t.Parallel()
	store := newServiceMaterializationStore(t, 2)
	repository := &materializingServiceRepository{store: store}
	authority := newRecordingStudyAuthoritySource()
	adapter := &countingStudyShadowAdapter{}
	runner, err := study.NewStudyShadowRunner(adapter, "service-test", "1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	service := newStudySessionServiceWithShadowForTest(t, repository, authority, runner)
	request := validCreateRequestForAuthorizationTest()
	request.Page.Limit = 2

	created, err := service.CreateOrResume(context.Background(), model.OperatorStudyPrincipalID(), request)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created == nil || !created.Created || len(created.Resource.Page.Items) != 2 || adapter.calls.Load() != 1 {
		t.Fatalf("created result = %#v, shadow calls = %d", created, adapter.calls.Load())
	}
	if got := authority.allocationCount(); got != 1 {
		t.Fatalf("item allocator calls = %d, want 1", got)
	}
	if created.Resource.Scheduler == nil || created.Resource.Scheduler.PlannedAt.Nanosecond()%1_000 != 0 ||
		created.Resource.GeneratedAt.Nanosecond()%1_000 != 0 {
		t.Fatalf("service time precision escaped: scheduler=%#v generated=%s", created.Resource.Scheduler, created.Resource.GeneratedAt)
	}

	replayed, err := service.CreateOrResume(context.Background(), model.OperatorStudyPrincipalID(), request)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed == nil || replayed.Created || replayed.Resource.SessionID != created.Resource.SessionID {
		t.Fatalf("replayed result = %#v", replayed)
	}
	if got := authority.allocationCount(); got != 1 {
		t.Fatalf("replay called item allocator: %d", got)
	}
	if adapter.calls.Load() != 1 {
		t.Fatalf("replay called shadow: %d", adapter.calls.Load())
	}
}

func TestStudySessionServiceWaitingCycleSkipsAllocatorAndShadow(t *testing.T) {
	t.Parallel()
	store := newServiceMaterializationStore(t, 0)
	repository := &materializingServiceRepository{store: store}
	authority := newRecordingStudyAuthoritySource()
	adapter := &countingStudyShadowAdapter{}
	runner, err := study.NewStudyShadowRunner(adapter, "service-test", "1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	service := newStudySessionServiceWithShadowForTest(t, repository, authority, runner)
	result, err := service.CreateOrResume(context.Background(), model.OperatorStudyPrincipalID(), validCreateRequestForAuthorizationTest())
	if err != nil {
		t.Fatalf("create waiting: %v", err)
	}
	if result.Resource.Lifecycle != model.StudySessionLifecycleWaitingForCorpus || !result.Resource.Page.Complete ||
		len(result.Resource.Page.Items) != 0 || authority.allocationCount() != 0 || adapter.calls.Load() != 0 {
		t.Fatalf("waiting result=%#v allocation=%d shadow=%d", result.Resource, authority.allocationCount(), adapter.calls.Load())
	}
}

func TestStudySessionServiceReadbackMismatchNeverRunsShadow(t *testing.T) {
	t.Parallel()
	mutations := []struct {
		name   string
		mutate func(*study.CommittedCycle)
	}{
		{name: "item content", mutate: func(committed *study.CommittedCycle) {
			committed.Items[0].FrontCanonicalName = "substituted after commit"
		}},
		{name: "session display refs", mutate: func(committed *study.CommittedCycle) {
			committed.Session.Scope.Scope.TopicRefs[0] = "substituted-ref"
		}},
		{name: "receipt display refs", mutate: func(committed *study.CommittedCycle) {
			committed.Receipt.Scope.Scope.TopicRefs[0] = "substituted-ref"
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			store := newServiceMaterializationStore(t, 1)
			repository := &materializingServiceRepository{store: store, mutateCommitted: mutation.mutate}
			authority := newRecordingStudyAuthoritySource()
			adapter := &countingStudyShadowAdapter{}
			runner, err := study.NewStudyShadowRunner(adapter, "service-test", "1", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			service := newStudySessionServiceWithShadowForTest(t, repository, authority, runner)
			_, err = service.CreateOrResume(context.Background(), model.OperatorStudyPrincipalID(), validCreateRequestForAuthorizationTest())
			if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeInternal}) {
				t.Fatalf("readback mismatch error = %v", err)
			}
			if adapter.calls.Load() != 0 {
				t.Fatalf("invalid readback ran shadow %d times", adapter.calls.Load())
			}
		})
	}
}

func TestStudySessionServiceRejectsDuplicateResolvedTopicAuthority(t *testing.T) {
	t.Parallel()
	store := newServiceMaterializationStore(t, 1)
	duplicate := cloneServiceNormalizedScope(store.captured.Scope)
	duplicate.Scope.TopicIDs = append(duplicate.Scope.TopicIDs, duplicate.Scope.TopicIDs[0])
	duplicate.Scope.TopicRefs = append(duplicate.Scope.TopicRefs, "other-ref")
	repository := &materializingServiceRepository{store: store, scopeOverride: &duplicate}
	authority := newRecordingStudyAuthoritySource()
	service := newStudySessionServiceWithShadowForTest(t, repository, authority, nil)
	_, err := service.CreateOrResume(context.Background(), model.OperatorStudyPrincipalID(), validCreateRequestForAuthorizationTest())
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeInternal}) {
		t.Fatalf("duplicate resolved topic error = %v", err)
	}
	if authority.calls() != 0 || repository.materialized != nil {
		t.Fatal("invalid resolved scope consumed authority or materialized")
	}
}

type countingStudyShadowAdapter struct{ calls atomic.Int64 }

func (adapter *countingStudyShadowAdapter) Evaluate(
	_ context.Context,
	_ study.StudySchedulingSnapshot,
	plan study.StudyPlan,
) (study.StudyShadowEvaluation, error) {
	adapter.calls.Add(1)
	identities := make([]uuid.UUID, len(plan.Cards))
	for index := range plan.Cards {
		identities[index] = plan.Cards[index].CardIdentity
	}
	return study.StudyShadowEvaluation{OrderedCardIdentities: identities}, nil
}

type materializingServiceRepository struct {
	store           *serviceMaterializationStore
	scopeOverride   *model.StudyNormalizedScope
	materialized    *study.CycleMaterializationResult
	mutateCommitted func(*study.CommittedCycle)
}

func (repository *materializingServiceRepository) ResolveStudyScope(
	_ context.Context,
	_ []string,
	_ model.StudyCardKind,
) (model.StudyNormalizedScope, error) {
	if repository.scopeOverride != nil {
		return cloneServiceNormalizedScope(*repository.scopeOverride), nil
	}
	return cloneServiceNormalizedScope(repository.store.captured.Scope), nil
}

func (repository *materializingServiceRepository) GetStudyLearnerProfileBySessionID(context.Context, uuid.UUID) (*model.StudyLearnerProfile, error) {
	profile := repository.store.captured.LearnerProfile
	return &profile, nil
}

func (repository *materializingServiceRepository) MaterializeStudyCycle(
	ctx context.Context,
	materializer *study.CycleMaterializer,
	command StudyCycleMaterializationCommand,
) (*study.CycleMaterializationResult, error) {
	result, err := materializer.Materialize(ctx, repository.store, command.Materialization)
	if err != nil {
		return nil, err
	}
	repository.materialized = result
	return result, nil
}

func (repository *materializingServiceRepository) ReadCommittedStudyCycle(
	context.Context,
	StudyCommittedCycleReadCommand,
) (*study.CommittedCycle, error) {
	if repository.materialized == nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	persistence := cloneServicePersistence(repository.materialized.Persistence)
	committed := &study.CommittedCycle{Session: persistence.Session, Items: persistence.Items, Receipt: persistence.Receipt}
	if repository.mutateCommitted != nil {
		repository.mutateCommitted(committed)
	}
	return committed, nil
}

func (*materializingServiceRepository) ReadStudySessionCycle(context.Context, uuid.UUID) (*StudySessionCycleAuthority, error) {
	return nil, newStudySessionError(model.StudyErrorCodeInternal)
}

func (*materializingServiceRepository) AdjudicateStudyEvents(context.Context, StudyEventSyncCommand) (*StudyEventSyncAuthority, error) {
	return nil, newStudySessionError(model.StudyErrorCodeInternal)
}

type serviceMaterializationStore struct {
	captured    study.CapturedEligibleCorpus
	projections map[uuid.UUID]model.StudySchedulerCardProjection
	existing    *study.CyclePersistenceResult
	createdAt   time.Time
}

func newServiceMaterializationStore(t *testing.T, sourceCount int) *serviceMaterializationStore {
	t.Helper()
	scope, err := model.NormalizeStudyScope(model.StudyScope{
		TopicIDs:  []uuid.UUID{uuid.MustParse("20000000-0000-4000-8000-000000000001")},
		TopicRefs: []string{"topic"},
		CardKind:  model.StudyCardKindIdea,
	})
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	store := &serviceMaterializationStore{
		captured: study.CapturedEligibleCorpus{
			LearnerProfile: model.StudyLearnerProfile{
				ID:        uuid.MustParse("30000000-0000-4000-8000-000000000001"),
				Ref:       "operator",
				CreatedAt: createdAt,
				UpdatedAt: createdAt,
			},
			Scope:   scope,
			Sources: make([]study.MaterializationSource, sourceCount),
		},
		projections: make(map[uuid.UUID]model.StudySchedulerCardProjection),
		createdAt:   createdAt,
	}
	for index := range store.captured.Sources {
		identity := uuid.NewSHA1(uuid.NameSpaceOID, []byte("service-card-"+string(rune(index))))
		source := study.MaterializationSource{
			CardIdentity:             identity,
			LiveSourceIdeaID:         identity,
			HistoricalSourceKind:     string(model.StudyCardKindIdea),
			HistoricalSourceIdentity: identity,
			SourceContentRevision:    "source-v1:" + identity.String(),
			FrontCanonicalName:       "Card " + identity.String(),
			BackDefinition:           "Definition " + identity.String(),
		}
		source.SourceContentSHA256, err = studydigest.CardContent(
			[]byte(model.StudyCardKindIdea),
			[]byte(identity.String()),
			[]byte(identity.String()),
			[]byte(source.FrontCanonicalName),
			[]byte(source.BackDefinition),
			[]byte{0},
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		store.captured.Sources[index] = source
	}
	store.captured.CorpusSHA256, err = serviceCorpusSHA256(scope, store.captured.Sources)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func (store *serviceMaterializationStore) ResolveExistingCycle(
	context.Context,
	study.ExistingCycleResolutionInput,
) (*study.ExistingCycleResolution, error) {
	if store.existing == nil {
		return nil, nil
	}
	persistence := cloneServicePersistence(*store.existing)
	persistence.Created = false
	return &study.ExistingCycleResolution{
		LearnerProfile: store.captured.LearnerProfile,
		Scope:          cloneServiceNormalizedScope(store.captured.Scope),
		Persistence:    persistence,
	}, nil
}

func (store *serviceMaterializationStore) CaptureEligibleCorpus(context.Context) (study.CapturedEligibleCorpus, error) {
	return store.captured, nil
}

func (*serviceMaterializationStore) ReadAcceptedPrefix(context.Context, int64) ([]model.StudyEvent, error) {
	return []model.StudyEvent{}, nil
}

func (*serviceMaterializationStore) ResolveCardIdentity(_ context.Context, identity uuid.UUID) (uuid.UUID, error) {
	return identity, nil
}

func (store *serviceMaterializationStore) GetProjection(
	_ context.Context,
	key study.MaterializationProjectionKey,
) (*model.StudySchedulerCardProjection, error) {
	projection, ok := store.projections[key.CardIdentity]
	if !ok {
		return nil, nil
	}
	projection.CanonicalPayload = append([]byte(nil), projection.CanonicalPayload...)
	return &projection, nil
}

func (store *serviceMaterializationStore) ReplaceProjection(
	_ context.Context,
	projection model.StudySchedulerCardProjection,
	expectedRevision int64,
) (*model.StudySchedulerCardProjection, error) {
	projection.CanonicalPayload = append([]byte(nil), projection.CanonicalPayload...)
	projection.ProjectionRevision = expectedRevision + 1
	projection.UpdatedAt = store.createdAt
	store.projections[projection.ProjectedCardIdentity] = projection
	clone := projection
	clone.CanonicalPayload = append([]byte(nil), projection.CanonicalPayload...)
	return &clone, nil
}

func (*serviceMaterializationStore) ListSessionItems(context.Context, uuid.UUID) ([]model.StudySessionItem, error) {
	return nil, nil
}

func (store *serviceMaterializationStore) InsertCycleOnce(
	_ context.Context,
	input study.CyclePersistenceInput,
) (*study.CyclePersistenceResult, error) {
	items := make([]model.StudySessionItem, len(input.Items))
	copy(items, input.Items)
	for index := range items {
		items[index].CreatedAt = store.createdAt
	}
	result := study.CyclePersistenceResult{
		Session: model.StudySession{
			ID:                        input.SessionID,
			LearnerProfileID:          store.captured.LearnerProfile.ID,
			Scope:                     cloneServiceNormalizedScope(store.captured.Scope),
			Lifecycle:                 input.Lifecycle,
			CorpusSHA256:              input.CorpusSHA256,
			Scheduler:                 cloneServiceScheduler(input.Scheduler),
			PredecessorStudySessionID: cloneUUIDPointer(input.PredecessorSessionID),
			ProfileIngestionHighWater: store.captured.LearnerProfile.ProfileIngestionHighWater,
			StateVersion:              1,
			CreatedAt:                 store.createdAt,
			UpdatedAt:                 store.createdAt,
		},
		Items: items,
		Receipt: model.StudyCreationReceipt{
			StudyPrincipalID:             input.Principal,
			CreationRequestID:            input.CreationRequestID,
			NormalizedRequestSHA256:      input.NormalizedRequestSHA256,
			Disposition:                  model.StudyCreationDispositionCreated,
			CanonicalStudySessionID:      input.SessionID,
			LearnerProfileID:             store.captured.LearnerProfile.ID,
			Scope:                        cloneServiceNormalizedScope(store.captured.Scope),
			CreationReceiptAdjudicatedAt: store.createdAt,
		},
		Created: true,
	}
	store.existing = pointerToServicePersistence(cloneServicePersistence(result))
	return pointerToServicePersistence(cloneServicePersistence(result)), nil
}

func newStudySessionServiceWithShadowForTest(
	t *testing.T,
	repository StudySessionRepository,
	authority StudyAuthoritySource,
	shadow *study.StudyShadowRunner,
) *StudySessionService {
	t.Helper()
	scheduler, err := study.NewVarietyFirstV1()
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewStudySessionService(repository, OperatorStudyAuthorizer{}, scheduler, authority, shadow)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func (source *recordingStudyAuthoritySource) allocationCount() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.allocationCalls
}

func cloneServicePersistence(source study.CyclePersistenceResult) study.CyclePersistenceResult {
	clone := source
	clone.Session.Scope = cloneServiceNormalizedScope(source.Session.Scope)
	clone.Session.Scheduler = cloneServiceScheduler(source.Session.Scheduler)
	clone.Session.PredecessorStudySessionID = cloneUUIDPointer(source.Session.PredecessorStudySessionID)
	clone.Session.SuccessorStudySessionID = cloneUUIDPointer(source.Session.SuccessorStudySessionID)
	clone.Session.TerminalizedAt = cloneTimePointer(source.Session.TerminalizedAt)
	clone.Session.TerminalProfileIngestionHighWater = cloneInt64Pointer(source.Session.TerminalProfileIngestionHighWater)
	clone.Items = make([]model.StudySessionItem, len(source.Items))
	for index := range source.Items {
		clone.Items[index] = cloneStudySessionItem(source.Items[index])
	}
	clone.Receipt.Scope = cloneServiceNormalizedScope(source.Receipt.Scope)
	return clone
}

func pointerToServicePersistence(value study.CyclePersistenceResult) *study.CyclePersistenceResult {
	return &value
}

func cloneServiceNormalizedScope(source model.StudyNormalizedScope) model.StudyNormalizedScope {
	clone := source
	clone.CanonicalSelector = append([]byte(nil), source.CanonicalSelector...)
	clone.Scope.TopicIDs = append([]uuid.UUID(nil), source.Scope.TopicIDs...)
	clone.Scope.TopicRefs = append([]string(nil), source.Scope.TopicRefs...)
	return clone
}

func cloneServiceScheduler(source *model.StudySchedulerProvenance) *model.StudySchedulerProvenance {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}

func serviceCorpusSHA256(scope model.StudyNormalizedScope, sources []study.MaterializationSource) (string, error) {
	ordered := append([]study.MaterializationSource(nil), sources...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].CardIdentity.String() < ordered[right].CardIdentity.String()
	})
	parts := make([][]byte, 0, 3+len(scope.Scope.TopicIDs)+4*len(ordered))
	parts = append(parts, serviceDigestCount(len(scope.Scope.TopicIDs)))
	for _, topicID := range scope.Scope.TopicIDs {
		parts = append(parts, []byte(topicID.String()))
	}
	parts = append(parts, []byte(scope.Scope.CardKind), serviceDigestCount(len(ordered)))
	for _, source := range ordered {
		parts = append(parts,
			[]byte(source.CardIdentity.String()),
			[]byte(source.CardIdentity.String()),
			[]byte(source.SourceContentRevision),
			[]byte(source.SourceContentSHA256),
		)
	}
	return studydigest.Corpus(parts...)
}

func serviceDigestCount(count int) []byte {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], uint32(count))
	return encoded[:]
}
