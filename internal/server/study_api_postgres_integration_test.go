package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/model"
	lexinetserver "lexinet/internal/server"
	"lexinet/internal/storage"
	"lexinet/internal/study"
	"lexinet/internal/studyjson"
	"lexinet/internal/studysession"
	"lexinet/internal/testsupport/studypostgres"
)

const (
	studyAPITestBearer       = "study-api-bearer-secret-canary"
	studyAPITestProfileRef   = "study-api-private-profile-canary"
	studyAPITestCardName     = "Study API private card canary"
	studyAPITestDefinition   = "Study API private definition canary"
	studyAPITestExample      = "Study API private example canary"
	studyAPITestErrorLeakURL = "https://private.invalid/study-api-canary"
)

func TestPlanLexinetStep4Verify1Item2ReplayTheSameSessionPageSyncAndEvent(t *testing.T) {
	fixture := newStudyAPIPostgresFixture(t, 2)

	denied := fixture.postJSON("/api/v1/study/sessions", fixture.newCreateRequest(uuid.New(), nil), "")
	assertStudyAPITestError(t, denied, http.StatusUnauthorized, model.StudyErrorCodeUnauthorized)
	assertStudyAPITestErrorRedacted(t, denied)

	createRequest := fixture.newCreateRequest(uuid.New(), nil)
	createdResponse := fixture.postJSON("/api/v1/study/sessions", createRequest, studyAPITestBearer)
	created := decodeStudyAPITestJSON[studysession.StudySessionResource](t, createdResponse, http.StatusCreated)
	if created.Lifecycle != model.StudySessionLifecycleActive || len(created.Page.Items) != 1 || created.CreationReceipt == nil {
		t.Fatal("STUDY_API_POSTGRES.CREATE_INVALID")
	}

	replayedCreateResponse := fixture.postJSON("/api/v1/study/sessions", createRequest, studyAPITestBearer)
	replayedCreate := decodeStudyAPITestJSON[studysession.StudySessionResource](t, replayedCreateResponse, http.StatusOK)
	if replayedCreate.SessionID != created.SessionID || replayedCreate.CreationReceipt == nil ||
		!reflect.DeepEqual(replayedCreate.CreationReceipt, created.CreationReceipt) {
		t.Fatal("STUDY_API_POSTGRES.CREATE_REPLAY_CHANGED_AUTHORITY")
	}
	fixture.assertCount(t, "study_sessions", 1)

	firstPageResponse := fixture.get("/api/v1/study/sessions/"+created.SessionID.String()+"?page_after_position=-1&page_limit=1", studyAPITestBearer)
	firstPage := decodeStudyAPITestJSON[studysession.StudySessionResource](t, firstPageResponse, http.StatusOK)
	if len(firstPage.Page.Items) != 1 || firstPage.Page.Items[0].SessionItemID != created.Page.Items[0].SessionItemID ||
		firstPage.CreationReceipt != nil {
		t.Fatal("STUDY_API_POSTGRES.GET_FIRST_PAGE_INVALID")
	}
	secondPageResponse := fixture.get(fmt.Sprintf(
		"/api/v1/study/sessions/%s?page_after_position=%d&page_limit=1",
		created.SessionID,
		firstPage.Page.NextAfterPosition,
	), studyAPITestBearer)
	secondPage := decodeStudyAPITestJSON[studysession.StudySessionResource](t, secondPageResponse, http.StatusOK)
	if len(secondPage.Page.Items) != 1 || secondPage.Page.Items[0].SessionItemID == firstPage.Page.Items[0].SessionItemID ||
		!secondPage.Page.Complete {
		t.Fatal("STUDY_API_POSTGRES.GET_SECOND_PAGE_INVALID")
	}

	syncRequest := newStudyAPITestFullChainRequest(created, uuid.New(), uuid.New())
	firstSyncResponse := fixture.postJSON(
		"/api/v1/study/sessions/"+created.SessionID.String()+"/sync",
		syncRequest,
		studyAPITestBearer,
	)
	firstSync := decodeStudyAPITestJSON[studysession.StudySyncResource](t, firstSyncResponse, http.StatusOK)
	if len(firstSync.Receipts) != 4 || firstSync.AdjudicatedThroughSequence != 4 ||
		firstSync.ProfileIngestionHighWater != 4 || firstSync.Session.StateVersion != created.StateVersion+1 {
		t.Fatal("STUDY_API_POSTGRES.SYNC_INVALID")
	}
	for index, receipt := range firstSync.Receipts {
		if receipt.Disposition != model.StudyEventReceiptDispositionAccepted ||
			receipt.ProfileIngestionOrdinal == nil || *receipt.ProfileIngestionOrdinal != int64(index+1) {
			t.Fatalf("STUDY_API_POSTGRES.SYNC_RECEIPT_INVALID: index=%d", index)
		}
	}
	firstPageJSON := extractUniqueStudyAPITestSyncPage(t, firstSyncResponse.body)

	replayedSyncResponse := fixture.postJSON(
		"/api/v1/study/sessions/"+created.SessionID.String()+"/sync",
		syncRequest,
		studyAPITestBearer,
	)
	replayedSync := decodeStudyAPITestJSON[studysession.StudySyncResource](t, replayedSyncResponse, http.StatusOK)
	replayedPageJSON := extractUniqueStudyAPITestSyncPage(t, replayedSyncResponse.body)
	if !bytes.Equal(replayedPageJSON, firstPageJSON) ||
		!reflect.DeepEqual(replayedSync.Receipts, firstSync.Receipts) ||
		replayedSync.FlushReceipt != firstSync.FlushReceipt ||
		replayedSync.Session.StateVersion != firstSync.Session.StateVersion {
		t.Fatal("STUDY_API_POSTGRES.SYNC_REPLAY_CHANGED_IMMUTABLE_AUTHORITY")
	}
	fixture.assertCount(t, "study_flush_receipts", 1)
	fixture.assertCount(t, "study_flush_receipt_session_pages", 1)
	fixture.assertCount(t, "study_event_receipts", 4)
	fixture.assertCount(t, "study_events", 4)
	fixture.assertStoredSyncPage(t, firstSync.FlushReceipt.ReceiptID, firstPageJSON)
}

func TestPlanLexinetStep4Verify1Item3ReuseAnIdempotencyKeyWithChangedInputAnd(t *testing.T) {
	fixture := newStudyAPIPostgresFixture(t, 1)
	creationRequestID := uuid.New()
	createRequest := fixture.newCreateRequest(creationRequestID, nil)
	created := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.postJSON("/api/v1/study/sessions", createRequest, studyAPITestBearer),
		http.StatusCreated,
	)

	changedCreate := createRequest
	changedCreate.Page.Limit = 2
	createConflict := fixture.postJSON("/api/v1/study/sessions", changedCreate, studyAPITestBearer)
	assertStudyAPITestError(t, createConflict, http.StatusConflict, model.StudyErrorCodeIdempotencyConflict)
	assertStudyAPITestErrorRedacted(t, createConflict)
	createAfterConflict := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.postJSON("/api/v1/study/sessions", createRequest, studyAPITestBearer),
		http.StatusOK,
	)
	if createAfterConflict.SessionID != created.SessionID ||
		createAfterConflict.StateVersion != created.StateVersion ||
		!reflect.DeepEqual(createAfterConflict.CreationReceipt, created.CreationReceipt) ||
		!reflect.DeepEqual(createAfterConflict.Page, created.Page) {
		t.Fatal("STUDY_API_POSTGRES.CREATE_CONFLICT_MUTATED_AUTHORITY")
	}
	fixture.assertCount(t, "study_sessions", 1)

	syncRequest := newStudyAPITestPresentedRequest(created, uuid.New(), uuid.New(), 1)
	firstSyncResponse := fixture.postJSON(
		"/api/v1/study/sessions/"+created.SessionID.String()+"/sync",
		syncRequest,
		studyAPITestBearer,
	)
	firstSync := decodeStudyAPITestJSON[studysession.StudySyncResource](
		t,
		firstSyncResponse,
		http.StatusOK,
	)
	changedSync := syncRequest
	changedSync.Page.Limit = 2
	syncConflict := fixture.postJSON(
		"/api/v1/study/sessions/"+created.SessionID.String()+"/sync",
		changedSync,
		studyAPITestBearer,
	)
	assertStudyAPITestError(t, syncConflict, http.StatusConflict, model.StudyErrorCodeIdempotencyConflict)
	assertStudyAPITestErrorRedacted(t, syncConflict)
	syncAfterConflictResponse := fixture.postJSON(
		"/api/v1/study/sessions/"+created.SessionID.String()+"/sync",
		syncRequest,
		studyAPITestBearer,
	)
	syncAfterConflict := decodeStudyAPITestJSON[studysession.StudySyncResource](t, syncAfterConflictResponse, http.StatusOK)
	if !reflect.DeepEqual(syncAfterConflict.Receipts, firstSync.Receipts) ||
		syncAfterConflict.FlushReceipt != firstSync.FlushReceipt ||
		syncAfterConflict.Session.StateVersion != firstSync.Session.StateVersion ||
		!bytes.Equal(
			extractUniqueStudyAPITestSyncPage(t, syncAfterConflictResponse.body),
			extractUniqueStudyAPITestSyncPage(t, firstSyncResponse.body),
		) {
		t.Fatal("STUDY_API_POSTGRES.SYNC_CONFLICT_MUTATED_AUTHORITY")
	}
	fixture.assertCount(t, "study_flush_receipts", 1)
	fixture.assertCount(t, "study_events", 1)
}

func TestPlanLexinetStep4Verify1Item10RaceDistinctCreationUuidsAndReceiveOneActive(t *testing.T) {
	fixture := newStudyAPIPostgresFixture(t, 1)
	fixture.ensureStudyProfile(t)
	requests := []studysession.StudyCreateSessionRequest{
		fixture.newCreateRequest(uuid.New(), nil),
		fixture.newCreateRequest(uuid.New(), nil),
	}
	responses := fixture.raceCreateRequestsWithObservedProfileContention(t, requests)
	requestedCreationIDs := studyAPITestCreationIDSet(requests)
	seenCreationIDs := make(map[uuid.UUID]struct{}, len(requests))

	var statuses []int
	var sessionID uuid.UUID
	for _, response := range responses {
		if response.status != http.StatusCreated && response.status != http.StatusOK {
			t.Fatalf("STUDY_API_POSTGRES.RACE_STATUS_INVALID: status=%d", response.status)
		}
		resource := decodeStudyAPITestJSON[studysession.StudySessionResource](t, response, response.status)
		if resource.Lifecycle != model.StudySessionLifecycleActive || resource.CreationReceipt == nil ||
			resource.CreationReceipt.CanonicalSessionID != resource.SessionID {
			t.Fatal("STUDY_API_POSTGRES.RACE_NONACTIVE_RESULT")
		}
		if _, requested := requestedCreationIDs[resource.CreationReceipt.CreationRequestID]; !requested {
			t.Fatal("STUDY_API_POSTGRES.RACE_RECEIPT_REQUEST_UNKNOWN")
		}
		if _, duplicate := seenCreationIDs[resource.CreationReceipt.CreationRequestID]; duplicate {
			t.Fatal("STUDY_API_POSTGRES.RACE_RECEIPT_REQUEST_DUPLICATED")
		}
		seenCreationIDs[resource.CreationReceipt.CreationRequestID] = struct{}{}
		wantDisposition := model.StudyCreationDispositionConvergedToExisting
		if response.status == http.StatusCreated {
			wantDisposition = model.StudyCreationDispositionCreated
		}
		if resource.CreationReceipt.Disposition != wantDisposition {
			t.Fatal("STUDY_API_POSTGRES.RACE_RECEIPT_DISPOSITION_INVALID")
		}
		if sessionID == uuid.Nil {
			sessionID = resource.SessionID
		} else if resource.SessionID != sessionID {
			t.Fatal("STUDY_API_POSTGRES.RACE_CREATED_MULTIPLE_SESSIONS")
		}
		statuses = append(statuses, response.status)
	}
	sort.Ints(statuses)
	if !reflect.DeepEqual(statuses, []int{http.StatusOK, http.StatusCreated}) {
		t.Fatalf("STUDY_API_POSTGRES.RACE_STATUSES_INVALID: %v", statuses)
	}
	if len(seenCreationIDs) != len(requestedCreationIDs) {
		t.Fatal("STUDY_API_POSTGRES.RACE_RECEIPT_COVERAGE_INVALID")
	}
	fixture.assertCount(t, "study_sessions", 1)
	fixture.assertCount(t, "study_session_creation_receipts", 2)
}

func TestPlanLexinetStep4Verify1Item12RaceConcurrentSuccessorRequestsAndReceiveOneLinked(t *testing.T) {
	fixture := newStudyAPIPostgresFixture(t, 1)
	predecessor := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.postJSON(
			"/api/v1/study/sessions",
			fixture.newCreateRequest(uuid.New(), nil),
			studyAPITestBearer,
		),
		http.StatusCreated,
	)
	flush := decodeStudyAPITestJSON[studysession.StudySyncResource](
		t,
		fixture.postJSON(
			"/api/v1/study/sessions/"+predecessor.SessionID.String()+"/sync",
			newStudyAPITestPresentedRequest(predecessor, uuid.New(), uuid.New(), 1),
			studyAPITestBearer,
		),
		http.StatusOK,
	)
	barrier := &studysession.StudyPredecessorBarrierRequest{
		SessionID:      predecessor.SessionID,
		FlushReceiptID: flush.FlushReceipt.ReceiptID,
	}
	requests := []studysession.StudyCreateSessionRequest{
		fixture.newCreateRequest(uuid.New(), barrier),
		fixture.newCreateRequest(uuid.New(), barrier),
	}
	responses := fixture.raceCreateRequestsWithObservedProfileContention(t, requests)
	requestedCreationIDs := studyAPITestCreationIDSet(requests)
	seenCreationIDs := make(map[uuid.UUID]struct{}, len(requests))

	var statuses []int
	var successorSessionID uuid.UUID
	for _, response := range responses {
		if response.status != http.StatusCreated && response.status != http.StatusOK {
			t.Fatalf("STUDY_API_POSTGRES.SUCCESSOR_RACE_STATUS_INVALID: status=%d", response.status)
		}
		resource := decodeStudyAPITestJSON[studysession.StudySessionResource](t, response, response.status)
		if resource.Lifecycle != model.StudySessionLifecycleActive ||
			resource.PredecessorSessionID == nil || *resource.PredecessorSessionID != predecessor.SessionID ||
			resource.CreationReceipt == nil ||
			resource.CreationReceipt.CanonicalSessionID != resource.SessionID ||
			resource.CreationReceipt.Disposition != model.StudyCreationDispositionSuccessor {
			t.Fatal("STUDY_API_POSTGRES.SUCCESSOR_RACE_RESOURCE_INVALID")
		}
		if _, requested := requestedCreationIDs[resource.CreationReceipt.CreationRequestID]; !requested {
			t.Fatal("STUDY_API_POSTGRES.SUCCESSOR_RACE_RECEIPT_REQUEST_UNKNOWN")
		}
		if _, duplicate := seenCreationIDs[resource.CreationReceipt.CreationRequestID]; duplicate {
			t.Fatal("STUDY_API_POSTGRES.SUCCESSOR_RACE_RECEIPT_REQUEST_DUPLICATED")
		}
		seenCreationIDs[resource.CreationReceipt.CreationRequestID] = struct{}{}
		if successorSessionID == uuid.Nil {
			successorSessionID = resource.SessionID
		} else if resource.SessionID != successorSessionID {
			t.Fatal("STUDY_API_POSTGRES.SUCCESSOR_RACE_CREATED_MULTIPLE_SUCCESSORS")
		}
		statuses = append(statuses, response.status)
	}
	sort.Ints(statuses)
	if !reflect.DeepEqual(statuses, []int{http.StatusOK, http.StatusCreated}) {
		t.Fatalf("STUDY_API_POSTGRES.SUCCESSOR_RACE_STATUSES_INVALID: %v", statuses)
	}
	if len(seenCreationIDs) != len(requestedCreationIDs) {
		t.Fatal("STUDY_API_POSTGRES.SUCCESSOR_RACE_RECEIPT_COVERAGE_INVALID")
	}
	completedPredecessor := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.get("/api/v1/study/sessions/"+predecessor.SessionID.String(), studyAPITestBearer),
		http.StatusOK,
	)
	if completedPredecessor.Lifecycle != model.StudySessionLifecycleCompleted ||
		completedPredecessor.SuccessorSessionID == nil ||
		*completedPredecessor.SuccessorSessionID != successorSessionID {
		t.Fatal("STUDY_API_POSTGRES.SUCCESSOR_RACE_LINK_INVALID")
	}
	fixture.assertCount(t, "study_sessions", 2)
	fixture.assertCount(t, "study_session_creation_receipts", 3)
}

func TestPlanLexinetStep4Verify1Item13ReplayIt(t *testing.T) {
	fixture := newStudyAPIPostgresFixture(t, 1)
	predecessor := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.postJSON(
			"/api/v1/study/sessions",
			fixture.newCreateRequest(uuid.New(), nil),
			studyAPITestBearer,
		),
		http.StatusCreated,
	)
	flush := decodeStudyAPITestJSON[studysession.StudySyncResource](
		t,
		fixture.postJSON(
			"/api/v1/study/sessions/"+predecessor.SessionID.String()+"/sync",
			newStudyAPITestPresentedRequest(predecessor, uuid.New(), uuid.New(), 1),
			studyAPITestBearer,
		),
		http.StatusOK,
	)
	barrier := &studysession.StudyPredecessorBarrierRequest{
		SessionID:      predecessor.SessionID,
		FlushReceiptID: flush.FlushReceipt.ReceiptID,
	}
	successorRequest := fixture.newCreateRequest(uuid.New(), barrier)
	createdSuccessor := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.postJSON("/api/v1/study/sessions", successorRequest, studyAPITestBearer),
		http.StatusCreated,
	)
	if createdSuccessor.PredecessorSessionID == nil || *createdSuccessor.PredecessorSessionID != predecessor.SessionID ||
		createdSuccessor.CreationReceipt == nil ||
		createdSuccessor.CreationReceipt.Disposition != model.StudyCreationDispositionSuccessor {
		t.Fatal("STUDY_API_POSTGRES.SUCCESSOR_CREATE_INVALID")
	}
	replayedSuccessor := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.postJSON("/api/v1/study/sessions", successorRequest, studyAPITestBearer),
		http.StatusOK,
	)
	if replayedSuccessor.SessionID != createdSuccessor.SessionID ||
		!reflect.DeepEqual(replayedSuccessor.CreationReceipt, createdSuccessor.CreationReceipt) {
		t.Fatal("STUDY_API_POSTGRES.SUCCESSOR_REPLAY_CHANGED_AUTHORITY")
	}
	completedPredecessor := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.get("/api/v1/study/sessions/"+predecessor.SessionID.String(), studyAPITestBearer),
		http.StatusOK,
	)
	if completedPredecessor.Lifecycle != model.StudySessionLifecycleCompleted ||
		completedPredecessor.SuccessorSessionID == nil ||
		*completedPredecessor.SuccessorSessionID != createdSuccessor.SessionID {
		t.Fatal("STUDY_API_POSTGRES.PREDECESSOR_LINK_INVALID")
	}
	fixture.assertCount(t, "study_sessions", 2)
	fixture.assertCount(t, "study_session_creation_receipts", 2)
}

func TestPlanLexinetStep4Verify1Item14PollOneStableEmptySentinel(t *testing.T) {
	fixture := newStudyAPIPostgresFixture(t, 0)
	waiting := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.postJSON(
			"/api/v1/study/sessions",
			fixture.newCreateRequest(uuid.New(), nil),
			studyAPITestBearer,
		),
		http.StatusCreated,
	)
	if waiting.Lifecycle != model.StudySessionLifecycleWaitingForCorpus || waiting.Scheduler != nil ||
		!waiting.Page.Complete || len(waiting.Page.Items) != 0 {
		t.Fatal("STUDY_API_POSTGRES.WAITING_SENTINEL_INVALID")
	}

	firstPoll := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.get("/api/v1/study/sessions/"+waiting.SessionID.String(), studyAPITestBearer),
		http.StatusOK,
	)
	secondPoll := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.get("/api/v1/study/sessions/"+waiting.SessionID.String(), studyAPITestBearer),
		http.StatusOK,
	)
	firstPageJSON, err := json.Marshal(firstPoll.Page)
	if err != nil {
		t.Fatalf("STUDY_API_POSTGRES.WAITING_PAGE_MARSHAL_FAILED: cause_type=%T", err)
	}
	secondPageJSON, err := json.Marshal(secondPoll.Page)
	if err != nil {
		t.Fatalf("STUDY_API_POSTGRES.WAITING_REPLAY_PAGE_MARSHAL_FAILED: cause_type=%T", err)
	}
	if firstPoll.SessionID != waiting.SessionID || secondPoll.SessionID != waiting.SessionID ||
		firstPoll.Lifecycle != model.StudySessionLifecycleWaitingForCorpus ||
		secondPoll.Lifecycle != model.StudySessionLifecycleWaitingForCorpus ||
		firstPoll.StateVersion != waiting.StateVersion || secondPoll.StateVersion != waiting.StateVersion ||
		!bytes.Equal(firstPageJSON, secondPageJSON) {
		t.Fatal("STUDY_API_POSTGRES.WAITING_SENTINEL_POLL_CHANGED")
	}
	fixture.assertCount(t, "study_sessions", 1)
}

func TestPlanLexinetStep4Verify1Item15AtomicallyActivateItAfterEligibleContentArrives(t *testing.T) {
	fixture := newStudyAPIPostgresFixture(t, 0)
	waiting := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.postJSON(
			"/api/v1/study/sessions",
			fixture.newCreateRequest(uuid.New(), nil),
			studyAPITestBearer,
		),
		http.StatusCreated,
	)
	if waiting.Lifecycle != model.StudySessionLifecycleWaitingForCorpus {
		t.Fatal("STUDY_API_POSTGRES.ACTIVATION_MISSING_WAITING_SENTINEL")
	}
	fixtureContext, cancelFixture := context.WithTimeout(context.Background(), 10*time.Second)
	fixture.createEligibleIdea(fixtureContext, "activation")
	cancelFixture()

	activationRequests := []studysession.StudyCreateSessionRequest{
		fixture.newCreateRequest(uuid.New(), nil),
		fixture.newCreateRequest(uuid.New(), nil),
	}
	responses := fixture.raceCreateRequestsWithObservedProfileContention(t, activationRequests)
	requestedCreationIDs := studyAPITestCreationIDSet(activationRequests)
	seenCreationIDs := make(map[uuid.UUID]struct{}, len(activationRequests))

	var activeSessionID uuid.UUID
	var statuses []int
	for _, response := range responses {
		if response.status != http.StatusCreated && response.status != http.StatusOK {
			t.Fatalf("STUDY_API_POSTGRES.ACTIVATION_STATUS_INVALID: status=%d", response.status)
		}
		resource := decodeStudyAPITestJSON[studysession.StudySessionResource](t, response, response.status)
		if resource.Lifecycle != model.StudySessionLifecycleActive || len(resource.Page.Items) != 1 ||
			resource.PredecessorSessionID == nil || *resource.PredecessorSessionID != waiting.SessionID {
			t.Fatal("STUDY_API_POSTGRES.ACTIVATION_RESOURCE_SHAPE_INVALID")
		}
		if resource.CreationReceipt == nil {
			t.Fatal("STUDY_API_POSTGRES.ACTIVATION_RECEIPT_MISSING")
		}
		if resource.CreationReceipt.CanonicalSessionID != resource.SessionID {
			t.Fatal("STUDY_API_POSTGRES.ACTIVATION_RECEIPT_CANONICAL_INVALID")
		}
		wantDisposition := model.StudyCreationDispositionConvergedToExisting
		if response.status == http.StatusCreated {
			wantDisposition = model.StudyCreationDispositionSuccessor
		}
		if resource.CreationReceipt.Disposition != wantDisposition {
			t.Fatal("STUDY_API_POSTGRES.ACTIVATION_RECEIPT_DISPOSITION_INVALID")
		}
		if _, requested := requestedCreationIDs[resource.CreationReceipt.CreationRequestID]; !requested {
			t.Fatal("STUDY_API_POSTGRES.ACTIVATION_RECEIPT_REQUEST_UNKNOWN")
		}
		if _, duplicate := seenCreationIDs[resource.CreationReceipt.CreationRequestID]; duplicate {
			t.Fatal("STUDY_API_POSTGRES.ACTIVATION_RECEIPT_REQUEST_DUPLICATED")
		}
		seenCreationIDs[resource.CreationReceipt.CreationRequestID] = struct{}{}
		if activeSessionID == uuid.Nil {
			activeSessionID = resource.SessionID
		} else if resource.SessionID != activeSessionID {
			t.Fatal("STUDY_API_POSTGRES.ACTIVATION_CREATED_MULTIPLE_SUCCESSORS")
		}
		statuses = append(statuses, response.status)
	}
	sort.Ints(statuses)
	if !reflect.DeepEqual(statuses, []int{http.StatusOK, http.StatusCreated}) {
		t.Fatalf("STUDY_API_POSTGRES.ACTIVATION_STATUSES_INVALID: %v", statuses)
	}
	if len(seenCreationIDs) != len(requestedCreationIDs) {
		t.Fatal("STUDY_API_POSTGRES.ACTIVATION_RECEIPT_COVERAGE_INVALID")
	}

	superseded := decodeStudyAPITestJSON[studysession.StudySessionResource](
		t,
		fixture.get("/api/v1/study/sessions/"+waiting.SessionID.String(), studyAPITestBearer),
		http.StatusOK,
	)
	if superseded.Lifecycle != model.StudySessionLifecycleSupersededEmpty ||
		superseded.SuccessorSessionID == nil || *superseded.SuccessorSessionID != activeSessionID {
		t.Fatal("STUDY_API_POSTGRES.ACTIVATION_WAITING_LINK_INVALID")
	}
	fixture.assertCount(t, "study_sessions", 2)
	fixture.assertCount(t, "study_session_creation_receipts", 3)
}

type studyAPIPostgresFixture struct {
	t                     *testing.T
	harness               *studypostgres.Harness
	pool                  *pgxpool.Pool
	repository            *storage.Repository
	server                *httptest.Server
	workerApplicationName string
	topic                 model.Topic
	ideas                 []model.Idea
}

func newStudyAPIPostgresFixture(t *testing.T, eligibleIdeaCount int) *studyAPIPostgresFixture {
	t.Helper()
	harness := studypostgres.Start(t)
	migrationContext, cancelMigration := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelMigration()
	if err := storage.Migrate(migrationContext, harness.DSN(t)); err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.MIGRATION_FAILED", err))
	}
	workerApplicationName := "lexinet-step4-http-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	poolConfig, err := pgxpool.ParseConfig(harness.DSN(t))
	if err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.POOL_CONFIG_FAILED", err))
	}
	poolConfig.MaxConns = 12
	poolConfig.ConnConfig.RuntimeParams["application_name"] = workerApplicationName
	connectContext, cancelConnect := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelConnect()
	pool, err := pgxpool.NewWithConfig(connectContext, poolConfig)
	if err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.POOL_CREATE_FAILED", err))
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(connectContext); err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.POOL_PING_FAILED", err))
	}
	repository := storage.NewRepository(pool)
	fixture := &studyAPIPostgresFixture{
		t:                     t,
		harness:               harness,
		pool:                  pool,
		repository:            repository,
		workerApplicationName: workerApplicationName,
	}

	fixtureContext, cancelFixture := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelFixture()
	topic, err := repository.CreateTopic(fixtureContext, model.CreateTopicInput{
		Slug: "study-api-integration",
		Name: "Study API integration",
	})
	if err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.TOPIC_CREATE_FAILED", err))
	}
	fixture.topic = *topic
	for index := 0; index < eligibleIdeaCount; index++ {
		fixture.ideas = append(fixture.ideas, fixture.createEligibleIdea(fixtureContext, fmt.Sprintf("%d", index)))
	}

	adapter, err := studysession.NewStorageRepositoryAdapter(repository)
	if err != nil {
		t.Fatalf("STUDY_API_POSTGRES.ADAPTER_CREATE_FAILED: cause_type=%T", err)
	}
	scheduler, err := study.NewVarietyFirstV1()
	if err != nil {
		t.Fatalf("STUDY_API_POSTGRES.SCHEDULER_CREATE_FAILED: cause_type=%T", err)
	}
	service, err := studysession.NewStudySessionService(
		adapter,
		studysession.OperatorStudyAuthorizer{},
		scheduler,
		newDeterministicStudyAPIAuthority(),
		nil,
	)
	if err != nil {
		t.Fatalf("STUDY_API_POSTGRES.SERVICE_CREATE_FAILED: cause_type=%T", err)
	}
	handler := lexinetserver.Router(
		lexinetserver.Config{Version: "study-api-integration", AuthToken: studyAPITestBearer},
		repository,
		lexinetserver.WithStudySessionService(service),
	)
	fixture.server = httptest.NewServer(handler)
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *studyAPIPostgresFixture) createEligibleIdea(ctx context.Context, suffix string) model.Idea {
	fixture.t.Helper()
	idea, err := fixture.repository.CreateIdea(ctx, model.CreateIdeaInput{
		Names:       []model.CreateNameInput{{Text: studyAPITestCardName + " " + suffix, Kind: model.NameKindCanonical}},
		Definitions: []model.CreateDefinitionInput{{Body: studyAPITestDefinition + " " + suffix, IsPrimary: true}},
		Examples:    []model.CreateExampleInput{{Body: studyAPITestExample + " " + suffix}},
	})
	if err != nil {
		fixture.t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.IDEA_CREATE_FAILED", err))
	}
	if _, err := fixture.repository.AssignTopic(ctx, idea.ID.String(), fixture.topic.Slug); err != nil {
		fixture.t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.TOPIC_ASSIGN_FAILED", err))
	}
	return *idea
}

func (fixture *studyAPIPostgresFixture) newCreateRequest(
	creationRequestID uuid.UUID,
	predecessor *studysession.StudyPredecessorBarrierRequest,
) studysession.StudyCreateSessionRequest {
	fixture.t.Helper()
	return studysession.StudyCreateSessionRequest{
		ProtocolVersion:   model.StudyProtocolVersion,
		SchemaVersion:     model.StudySchemaVersion,
		CreationRequestID: creationRequestID,
		ProfileRef:        studyAPITestProfileRef,
		Scope: studysession.StudyScopeRequest{
			TopicRefs: []string{fixture.topic.Slug},
			CardKind:  model.StudyCardKindIdea,
		},
		Predecessor: predecessor,
		Page:        studysession.StudyPageRequest{AfterPosition: -1, Limit: 1},
	}
}

func studyAPITestCreationIDSet(
	requests []studysession.StudyCreateSessionRequest,
) map[uuid.UUID]struct{} {
	identities := make(map[uuid.UUID]struct{}, len(requests))
	for _, request := range requests {
		identities[request.CreationRequestID] = struct{}{}
	}
	return identities
}

func (fixture *studyAPIPostgresFixture) ensureStudyProfile(t *testing.T) {
	t.Helper()
	scope, err := model.NormalizeStudyScope(model.StudyScope{
		TopicIDs:  []uuid.UUID{fixture.topic.ID},
		TopicRefs: []string{fixture.topic.Slug},
		CardKind:  model.StudyCardKindIdea,
	})
	if err != nil {
		t.Fatal("STUDY_API_POSTGRES.PROFILE_SCOPE_INVALID")
	}
	profileContext, cancelProfile := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelProfile()
	err = fixture.repository.WithStudyScopeTransaction(
		profileContext,
		storage.StudyScopeTransactionInput{ProfileRef: studyAPITestProfileRef, Scope: scope},
		func(storage.StudyStoreTransaction) error { return nil },
	)
	if err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.PROFILE_CREATE_FAILED", err))
	}
}

func (fixture *studyAPIPostgresFixture) raceCreateRequestsWithObservedProfileContention(
	t *testing.T,
	requests []studysession.StudyCreateSessionRequest,
) []studyAPITestHTTPResponse {
	t.Helper()
	if len(requests) != 2 {
		t.Fatal("STUDY_API_POSTGRES.RACE_REQUEST_COUNT_INVALID")
	}
	profileLock := fixture.holdStudyProfileLock(t)
	start := make(chan struct{})
	responses := make(chan studyAPITestHTTPResponse, len(requests))
	var workers sync.WaitGroup
	for _, request := range requests {
		request := request
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			responses <- fixture.postJSON("/api/v1/study/sessions", request, studyAPITestBearer)
		}()
	}
	close(start)
	blockedWorkerPIDs, overlapErr := fixture.waitForBlockedStudyWorkers(profileLock.backendPID, len(requests))
	releaseErr := profileLock.release()
	workers.Wait()
	close(responses)
	if overlapErr != nil {
		t.Fatalf("STUDY_API_POSTGRES.RACE_OVERLAP_NOT_OBSERVED: cause_type=%T", overlapErr)
	}
	if releaseErr != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.RACE_LOCK_RELEASE_FAILED", releaseErr))
	}
	if len(blockedWorkerPIDs) != len(requests) || blockedWorkerPIDs[0] == blockedWorkerPIDs[1] {
		t.Fatal("STUDY_API_POSTGRES.RACE_BLOCKED_WORKER_PIDS_INVALID")
	}
	collected := make([]studyAPITestHTTPResponse, 0, len(requests))
	for response := range responses {
		collected = append(collected, response)
	}
	if len(collected) != len(requests) {
		t.Fatal("STUDY_API_POSTGRES.RACE_RESPONSE_COUNT_INVALID")
	}
	return collected
}

func (fixture *studyAPIPostgresFixture) holdStudyProfileLock(t *testing.T) *studyAPITestProfileLock {
	t.Helper()
	lockContext, cancelLock := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelLock()
	connection, err := fixture.pool.Acquire(lockContext)
	if err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.LOCK_CONNECTION_FAILED", err))
	}
	transaction, err := connection.Begin(lockContext)
	if err != nil {
		connection.Release()
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.LOCK_TRANSACTION_FAILED", err))
	}
	profileLock := &studyAPITestProfileLock{transaction: transaction, connection: connection}
	t.Cleanup(profileLock.cleanup)
	if _, err := transaction.Exec(
		lockContext,
		`SELECT set_config('application_name', 'lexinet-step4-http-lock-holder', true)`,
	); err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.LOCK_APPLICATION_NAME_FAILED", err))
	}
	if err := transaction.QueryRow(lockContext, `SELECT pg_backend_pid()`).Scan(&profileLock.backendPID); err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.LOCK_PID_FAILED", err))
	}
	var profileID uuid.UUID
	if err := transaction.QueryRow(lockContext, `
		SELECT learner_profile_id
		FROM study_learner_profiles
		WHERE learner_profile_ref = $1
		FOR UPDATE`, studyAPITestProfileRef).Scan(&profileID); err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.PROFILE_LOCK_FAILED", err))
	}
	if profileID == uuid.Nil || profileLock.backendPID <= 0 {
		t.Fatal("STUDY_API_POSTGRES.PROFILE_LOCK_AUTHORITY_INVALID")
	}
	return profileLock
}

func (fixture *studyAPIPostgresFixture) waitForBlockedStudyWorkers(holderPID, want int) ([]int32, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		queryContext, cancelQuery := context.WithTimeout(context.Background(), time.Second)
		var workerPIDs []int32
		var holderDirectlyObserved bool
		err := fixture.pool.QueryRow(queryContext, `
			SELECT COALESCE(array_agg(DISTINCT activity.pid ORDER BY activity.pid), '{}'::integer[]),
			       COALESCE(bool_or($2 = ANY(pg_blocking_pids(activity.pid))), false)
			FROM pg_stat_activity AS activity
			WHERE activity.datname = current_database()
			  AND activity.application_name = $1
			  AND activity.pid <> $2
			  AND activity.state = 'active'
			  AND activity.wait_event_type = 'Lock'`,
			fixture.workerApplicationName,
			holderPID,
		).Scan(&workerPIDs, &holderDirectlyObserved)
		cancelQuery()
		if err != nil {
			return nil, studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.BLOCKED_WORKER_QUERY_FAILED", err)
		}
		if len(workerPIDs) == want && holderDirectlyObserved {
			return workerPIDs, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("STUDY_API_POSTGRES.BLOCKED_WORKER_COUNT_NOT_REACHED")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type studyAPITestProfileLock struct {
	mu          sync.Mutex
	transaction pgx.Tx
	connection  *pgxpool.Conn
	backendPID  int
	released    bool
}

func (profileLock *studyAPITestProfileLock) release() error {
	profileLock.mu.Lock()
	defer profileLock.mu.Unlock()
	if profileLock.released {
		return nil
	}
	releaseContext, cancelRelease := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRelease()
	err := profileLock.transaction.Commit(releaseContext)
	profileLock.connection.Release()
	profileLock.released = true
	return err
}

func (profileLock *studyAPITestProfileLock) cleanup() {
	profileLock.mu.Lock()
	defer profileLock.mu.Unlock()
	if profileLock.released {
		return
	}
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCleanup()
	_ = profileLock.transaction.Rollback(cleanupContext)
	profileLock.connection.Release()
	profileLock.released = true
}

func (fixture *studyAPIPostgresFixture) postJSON(path string, payload any, bearer string) studyAPITestHTTPResponse {
	fixture.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		fixture.t.Fatalf("STUDY_API_POSTGRES.REQUEST_MARSHAL_FAILED: cause_type=%T", err)
	}
	return fixture.request(http.MethodPost, path, body, bearer)
}

func (fixture *studyAPIPostgresFixture) get(path, bearer string) studyAPITestHTTPResponse {
	fixture.t.Helper()
	return fixture.request(http.MethodGet, path, nil, bearer)
}

func (fixture *studyAPIPostgresFixture) request(method, path string, body []byte, bearer string) studyAPITestHTTPResponse {
	fixture.t.Helper()
	requestContext, cancelRequest := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestContext, method, fixture.server.URL+path, bytes.NewReader(body))
	if err != nil {
		fixture.t.Fatalf("STUDY_API_POSTGRES.REQUEST_CREATE_FAILED: cause_type=%T", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := fixture.server.Client().Do(request)
	if err != nil {
		fixture.t.Fatalf("STUDY_API_POSTGRES.REQUEST_FAILED: cause_type=%T", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, studysession.StudyMaximumResponseBodyBytes+1))
	if err != nil {
		fixture.t.Fatalf("STUDY_API_POSTGRES.RESPONSE_READ_FAILED: cause_type=%T", err)
	}
	if len(responseBody) > studysession.StudyMaximumResponseBodyBytes {
		fixture.t.Fatal("STUDY_API_POSTGRES.RESPONSE_TOO_LARGE")
	}
	return studyAPITestHTTPResponse{status: response.StatusCode, header: response.Header.Clone(), body: responseBody}
}

func (fixture *studyAPIPostgresFixture) assertCount(t *testing.T, table string, want int) {
	t.Helper()
	allowed := map[string]bool{
		"study_sessions":                    true,
		"study_session_creation_receipts":   true,
		"study_flush_receipts":              true,
		"study_flush_receipt_session_pages": true,
		"study_event_receipts":              true,
		"study_events":                      true,
	}
	if !allowed[table] {
		t.Fatal("STUDY_API_POSTGRES.COUNT_TABLE_NOT_ALLOWLISTED")
	}
	queryContext, cancelQuery := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelQuery()
	var got int
	if err := fixture.pool.QueryRow(queryContext, "SELECT count(*) FROM "+table).Scan(&got); err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.COUNT_FAILED", err))
	}
	if got != want {
		t.Fatalf("STUDY_API_POSTGRES.COUNT_INVALID: table=%s got=%d want=%d", table, got, want)
	}
}

func (fixture *studyAPIPostgresFixture) assertStoredSyncPage(
	t *testing.T,
	flushReceiptID uuid.UUID,
	wantPageJSON []byte,
) {
	t.Helper()
	queryContext, cancelQuery := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelQuery()
	var storedJSON string
	var storedSHA256 string
	err := fixture.pool.QueryRow(queryContext, `
		SELECT canonical_session_page, canonical_session_page_sha256
		FROM study_flush_receipt_session_pages
		WHERE study_flush_receipt_id = $1`, flushReceiptID).Scan(&storedJSON, &storedSHA256)
	if err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_API_POSTGRES.STORED_PAGE_READ_FAILED", err))
	}
	if !bytes.Equal([]byte(storedJSON), wantPageJSON) || storedSHA256 != model.SHA256Hex(wantPageJSON) {
		t.Fatal("STUDY_API_POSTGRES.STORED_PAGE_BYTES_OR_DIGEST_CHANGED")
	}
}

type studyAPITestHTTPResponse struct {
	status int
	header http.Header
	body   []byte
}

type studyAPITestErrorEnvelope struct {
	Error struct {
		ProtocolVersion string               `json:"protocol_version"`
		SchemaVersion   int                  `json:"schema_version"`
		Code            model.StudyErrorCode `json:"code"`
		Message         string               `json:"message"`
	} `json:"error"`
}

func decodeStudyAPITestJSON[T any](t *testing.T, response studyAPITestHTTPResponse, wantStatus int) T {
	t.Helper()
	if response.status != wantStatus {
		t.Fatalf("STUDY_API_POSTGRES.STATUS_INVALID: got=%d want=%d", response.status, wantStatus)
	}
	if response.header.Get("Content-Type") != "application/json" || strings.TrimSpace(response.header.Get("X-Request-ID")) == "" {
		t.Fatal("STUDY_API_POSTGRES.RESPONSE_HEADERS_INVALID")
	}
	var decoded T
	decoder := json.NewDecoder(bytes.NewReader(response.body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("STUDY_API_POSTGRES.RESPONSE_DECODE_FAILED: cause_type=%T", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		t.Fatal("STUDY_API_POSTGRES.RESPONSE_TRAILING_JSON")
	}
	assertStudyAPITestSuccessAnchors(t, any(decoded))
	return decoded
}

func assertStudyAPITestSuccessAnchors(t *testing.T, decoded any) {
	t.Helper()
	assertSession := func(resource studysession.StudySessionResource) {
		if resource.ProtocolVersion != model.StudyProtocolVersion ||
			resource.SchemaVersion != model.StudySchemaVersion ||
			!reflect.DeepEqual(resource.RequiredSemantics, studysession.RequiredStudySemantics()) {
			t.Fatal("STUDY_API_POSTGRES.SESSION_CONTRACT_ANCHORS_INVALID")
		}
	}
	switch resource := decoded.(type) {
	case studysession.StudySessionResource:
		assertSession(resource)
	case studysession.StudySyncResource:
		if resource.ProtocolVersion != model.StudyProtocolVersion ||
			resource.SchemaVersion != model.StudySchemaVersion ||
			!reflect.DeepEqual(resource.RequiredSemantics, studysession.RequiredStudySemantics()) {
			t.Fatal("STUDY_API_POSTGRES.SYNC_CONTRACT_ANCHORS_INVALID")
		}
		assertSession(resource.Session)
	}
}

func extractUniqueStudyAPITestSyncPage(t *testing.T, responseJSON []byte) []byte {
	t.Helper()
	if _, err := studyjson.CanonicalizeUnique(responseJSON); err != nil {
		t.Fatalf("STUDY_API_POSTGRES.RESPONSE_DUPLICATE_OR_INVALID_JSON: cause_type=%T", err)
	}
	var syncEnvelope struct {
		Session json.RawMessage `json:"session"`
	}
	if err := json.Unmarshal(responseJSON, &syncEnvelope); err != nil || len(syncEnvelope.Session) == 0 {
		t.Fatalf("STUDY_API_POSTGRES.RAW_SYNC_SESSION_INVALID: cause_type=%T", err)
	}
	var sessionEnvelope struct {
		Page json.RawMessage `json:"page"`
	}
	if err := json.Unmarshal(syncEnvelope.Session, &sessionEnvelope); err != nil || len(sessionEnvelope.Page) == 0 {
		t.Fatalf("STUDY_API_POSTGRES.RAW_SYNC_PAGE_INVALID: cause_type=%T", err)
	}
	return append([]byte(nil), sessionEnvelope.Page...)
}

func assertStudyAPITestError(
	t *testing.T,
	response studyAPITestHTTPResponse,
	wantStatus int,
	wantCode model.StudyErrorCode,
) {
	t.Helper()
	envelope := decodeStudyAPITestJSON[studyAPITestErrorEnvelope](t, response, wantStatus)
	if envelope.Error.ProtocolVersion != model.StudyProtocolVersion ||
		envelope.Error.SchemaVersion != model.StudySchemaVersion ||
		envelope.Error.Code != wantCode || strings.TrimSpace(envelope.Error.Message) == "" {
		t.Fatal("STUDY_API_POSTGRES.ERROR_INVALID")
	}
}

func assertStudyAPITestErrorRedacted(t *testing.T, response studyAPITestHTTPResponse) {
	t.Helper()
	for forbiddenIndex, forbidden := range []string{
		studyAPITestBearer,
		studyAPITestProfileRef,
		studyAPITestCardName,
		studyAPITestDefinition,
		studyAPITestExample,
		studyAPITestErrorLeakURL,
		"postgres://",
	} {
		if bytes.Contains(response.body, []byte(forbidden)) {
			t.Fatalf("STUDY_API_POSTGRES.ERROR_LEAKED_FORBIDDEN_DATA: category_index=%d", forbiddenIndex)
		}
	}
}

func newStudyAPITestPresentedRequest(
	session studysession.StudySessionResource,
	syncID uuid.UUID,
	streamID uuid.UUID,
	sequence int64,
) studysession.StudySyncRequest {
	item := session.Page.Items[0]
	return studysession.StudySyncRequest{
		ProtocolVersion:      model.StudyProtocolVersion,
		SchemaVersion:        model.StudySchemaVersion,
		SyncID:               syncID,
		ClientStreamID:       streamID,
		ExpectedStateVersion: session.StateVersion,
		Events: []studysession.StudyEventRequest{{
			EventID:            uuid.New(),
			ClientSequence:     sequence,
			ProfileRef:         session.ProfileRef,
			SessionItemID:      item.SessionItemID,
			CardIdentity:       item.CardIdentity,
			ContentSHA256:      item.SourceRevision.ContentSHA256,
			Kind:               model.StudyEventKindPresented,
			ClientClockQuality: model.StudyClientClockQualityUnknown,
		}},
		Page: studysession.StudyPageRequest{AfterPosition: -1, Limit: 1},
	}
}

func newStudyAPITestFullChainRequest(
	session studysession.StudySessionResource,
	syncID uuid.UUID,
	streamID uuid.UUID,
) studysession.StudySyncRequest {
	request := newStudyAPITestPresentedRequest(session, syncID, streamID, 1)
	item := session.Page.Items[0]
	rating := model.StudyRatingGood
	ratingEventID := uuid.New()
	request.Events = append(request.Events,
		studysession.StudyEventRequest{
			EventID:            uuid.New(),
			ClientSequence:     2,
			ProfileRef:         session.ProfileRef,
			SessionItemID:      item.SessionItemID,
			CardIdentity:       item.CardIdentity,
			ContentSHA256:      item.SourceRevision.ContentSHA256,
			Kind:               model.StudyEventKindRevealed,
			ClientClockQuality: model.StudyClientClockQualityUnknown,
		},
		studysession.StudyEventRequest{
			EventID:            ratingEventID,
			ClientSequence:     3,
			ProfileRef:         session.ProfileRef,
			SessionItemID:      item.SessionItemID,
			CardIdentity:       item.CardIdentity,
			ContentSHA256:      item.SourceRevision.ContentSHA256,
			Kind:               model.StudyEventKindRated,
			Rating:             &rating,
			ClientClockQuality: model.StudyClientClockQualityUnknown,
		},
		studysession.StudyEventRequest{
			EventID:            uuid.New(),
			ClientSequence:     4,
			ProfileRef:         session.ProfileRef,
			SessionItemID:      item.SessionItemID,
			CardIdentity:       item.CardIdentity,
			ContentSHA256:      item.SourceRevision.ContentSHA256,
			Kind:               model.StudyEventKindRatingVoided,
			RatingEventID:      &ratingEventID,
			ClientClockQuality: model.StudyClientClockQualityUnknown,
		},
	)
	return request
}

type deterministicStudyAPIAuthority struct {
	mu          sync.Mutex
	identitySeq uint64
	timeSeq     uint64
}

func newDeterministicStudyAPIAuthority() *deterministicStudyAPIAuthority {
	return &deterministicStudyAPIAuthority{}
}

func (authority *deterministicStudyAPIAuthority) Now() time.Time {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.timeSeq++
	return time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC).Add(time.Duration(authority.timeSeq) * time.Microsecond)
}

func (authority *deterministicStudyAPIAuthority) NewStudySessionID() (uuid.UUID, error) {
	return authority.nextUUID("session"), nil
}

func (authority *deterministicStudyAPIAuthority) NewStudySeed() (string, error) {
	return "study-api-deterministic-seed-" + authority.nextUUID("seed").String(), nil
}

func (authority *deterministicStudyAPIAuthority) AllocateStudyItemIDs(
	ctx context.Context,
	requestedCount int,
) ([]uuid.UUID, error) {
	if requestedCount < 0 || requestedCount > study.StudyMaximumSchedulingCards {
		return nil, fmt.Errorf("STUDY_API_POSTGRES.AUTHORITY_COUNT_INVALID")
	}
	identities := make([]uuid.UUID, requestedCount)
	for index := range identities {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		identities[index] = authority.nextUUID("item")
	}
	return identities, nil
}

func (authority *deterministicStudyAPIAuthority) nextUUID(kind string) uuid.UUID {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	authority.identitySeq++
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("lexinet-study-api:%s:%d", kind, authority.identitySeq)))
}
