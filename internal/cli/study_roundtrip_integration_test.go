package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/model"
	lexinetserver "lexinet/internal/server"
	"lexinet/internal/storage"
	"lexinet/internal/studysession"
	"lexinet/internal/testsupport/studypostgres"
)

const (
	studyCLIRoundTripBearer     = "study-cli-roundtrip-bearer-canary"
	studyCLIRoundTripProfileRef = "study-cli-private-profile-canary"
	studyCLIRoundTripCardName   = "Study CLI private card canary"
)

func TestPlanLexinetStep4Verify1Item20RunCliRoundTripsAgainstADisposableServer(t *testing.T) {
	setupClientConfig(t)
	harness := studypostgres.Start(t)
	migrationContext, cancelMigration := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelMigration()
	if err := storage.Migrate(migrationContext, harness.DSN(t)); err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_CLI_ROUNDTRIP.MIGRATION_FAILED", err))
	}
	pool := harness.OpenPool(t)
	repository := storage.NewRepository(pool)
	topic := seedStudyCLIRoundTripCorpus(t, repository)
	endpoint := startStudyCLIRoundTripServer(t, harness.DSN(t))

	tokenPath := filepath.Join(t.TempDir(), "study-cli-token")
	if err := os.WriteFile(tokenPath, []byte(studyCLIRoundTripBearer+"\n"), 0o600); err != nil {
		t.Fatalf("STUDY_CLI_ROUNDTRIP.TOKEN_FILE_CREATE_FAILED: cause_type=%T", err)
	}
	createRequest := studysession.StudyCreateSessionRequest{
		ProtocolVersion:   model.StudyProtocolVersion,
		SchemaVersion:     model.StudySchemaVersion,
		CreationRequestID: uuid.New(),
		ProfileRef:        studyCLIRoundTripProfileRef,
		Scope: studysession.StudyScopeRequest{
			TopicRefs: []string{topic.Slug},
			CardKind:  model.StudyCardKindIdea,
		},
		Page: studysession.StudyPageRequest{AfterPosition: -1, Limit: 1},
	}
	createRequestJSON := marshalStudyCLIRoundTripRequest(t, createRequest)
	createRequestPath := filepath.Join(t.TempDir(), "create-session.json")
	if err := os.WriteFile(createRequestPath, createRequestJSON, 0o600); err != nil {
		t.Fatalf("STUDY_CLI_ROUNDTRIP.CREATE_FILE_FAILED: cause_type=%T", err)
	}

	createdOutput, createdStderr, err := executeTestCommand(t, []string{
		"--endpoint", endpoint,
		"--token-file", tokenPath,
		"study", "session", "--request", createRequestPath,
	}, nil)
	assertStudyCLIRoundTripCommandSuccess(t, err, createdStderr)
	created := decodeStudyCLIRoundTripOutput[studysession.StudySessionResource](t, createdOutput)
	assertStudyCLIRoundTripSessionAnchors(t, created)
	if created.Lifecycle != model.StudySessionLifecycleActive || len(created.Page.Items) != 1 || created.CreationReceipt == nil {
		t.Fatal("STUDY_CLI_ROUNDTRIP.CREATE_RESOURCE_INVALID")
	}

	replayedCreateOutput, replayedCreateStderr, err := executeTestCommand(t, []string{
		"--endpoint", endpoint,
		"--token-file", tokenPath,
		"study", "session", "--request", createRequestPath,
	}, nil)
	assertStudyCLIRoundTripCommandSuccess(t, err, replayedCreateStderr)
	replayedCreate := decodeStudyCLIRoundTripOutput[studysession.StudySessionResource](t, replayedCreateOutput)
	if replayedCreate.SessionID != created.SessionID ||
		!reflect.DeepEqual(replayedCreate.CreationReceipt, created.CreationReceipt) {
		t.Fatal("STUDY_CLI_ROUNDTRIP.CREATE_REPLAY_CHANGED_AUTHORITY")
	}

	stateOutput, stateStderr, err := executeTestCommand(t, []string{
		"--endpoint", endpoint,
		"--token-file", tokenPath,
		"study", "state", created.SessionID.String(),
		"--after-position", "-1",
		"--limit", "1",
	}, nil)
	assertStudyCLIRoundTripCommandSuccess(t, err, stateStderr)
	state := decodeStudyCLIRoundTripOutput[studysession.StudySessionResource](t, stateOutput)
	assertStudyCLIRoundTripSessionAnchors(t, state)
	if state.SessionID != created.SessionID || state.CreationReceipt != nil ||
		len(state.Page.Items) != 1 || state.Page.Items[0].SessionItemID != created.Page.Items[0].SessionItemID {
		t.Fatal("STUDY_CLI_ROUNDTRIP.STATE_RESOURCE_INVALID")
	}

	item := state.Page.Items[0]
	syncRequest := studysession.StudySyncRequest{
		ProtocolVersion:      model.StudyProtocolVersion,
		SchemaVersion:        model.StudySchemaVersion,
		SyncID:               uuid.New(),
		ClientStreamID:       uuid.New(),
		ExpectedStateVersion: state.StateVersion,
		Events: []studysession.StudyEventRequest{{
			EventID:            uuid.New(),
			ClientSequence:     1,
			ProfileRef:         state.ProfileRef,
			SessionItemID:      item.SessionItemID,
			CardIdentity:       item.CardIdentity,
			ContentSHA256:      item.SourceRevision.ContentSHA256,
			Kind:               model.StudyEventKindPresented,
			ClientClockQuality: model.StudyClientClockQualityUnknown,
		}},
		Page: studysession.StudyPageRequest{AfterPosition: -1, Limit: 1},
	}
	syncRequestJSON := marshalStudyCLIRoundTripRequest(t, syncRequest)
	syncArgs := []string{
		"--endpoint", endpoint,
		"--token-file", tokenPath,
		"study", "sync", created.SessionID.String(), "--request", "-",
	}
	firstSyncOutput, firstSyncStderr, err := executeTestCommand(t, syncArgs, strings.NewReader(string(syncRequestJSON)))
	assertStudyCLIRoundTripCommandSuccess(t, err, firstSyncStderr)
	firstSync := decodeStudyCLIRoundTripOutput[studysession.StudySyncResource](t, firstSyncOutput)
	assertStudyCLIRoundTripSyncAnchors(t, firstSync)
	if len(firstSync.Receipts) != 1 || firstSync.Receipts[0].Disposition != model.StudyEventReceiptDispositionAccepted ||
		firstSync.Session.StateVersion != state.StateVersion+1 {
		t.Fatal("STUDY_CLI_ROUNDTRIP.SYNC_RESOURCE_INVALID")
	}

	replayedSyncOutput, replayedSyncStderr, err := executeTestCommand(t, syncArgs, strings.NewReader(string(syncRequestJSON)))
	assertStudyCLIRoundTripCommandSuccess(t, err, replayedSyncStderr)
	replayedSync := decodeStudyCLIRoundTripOutput[studysession.StudySyncResource](t, replayedSyncOutput)
	if !reflect.DeepEqual(replayedSync.Receipts, firstSync.Receipts) ||
		replayedSync.FlushReceipt != firstSync.FlushReceipt ||
		!reflect.DeepEqual(replayedSync.Session.Page, firstSync.Session.Page) ||
		replayedSync.Session.StateVersion != firstSync.Session.StateVersion {
		t.Fatal("STUDY_CLI_ROUNDTRIP.SYNC_REPLAY_CHANGED_AUTHORITY")
	}

	wrongToken := "study-cli-wrong-token-canary"
	wrongTokenPath := filepath.Join(t.TempDir(), "wrong-study-cli-token")
	if err := os.WriteFile(wrongTokenPath, []byte(wrongToken+"\n"), 0o600); err != nil {
		t.Fatalf("STUDY_CLI_ROUNDTRIP.WRONG_TOKEN_FILE_CREATE_FAILED: cause_type=%T", err)
	}
	_, deniedStderr, deniedErr := executeTestCommand(t, []string{
		"--endpoint", endpoint,
		"--token-file", wrongTokenPath,
		"study", "state", created.SessionID.String(),
	}, nil)
	if deniedErr == nil || exitCode(deniedErr) != ExitAuth || deniedStderr != "" {
		t.Fatal("STUDY_CLI_ROUNDTRIP.AUTH_DENIAL_INVALID")
	}
	for forbiddenIndex, forbidden := range []string{
		studyCLIRoundTripBearer,
		wrongToken,
		studyCLIRoundTripProfileRef,
		studyCLIRoundTripCardName,
		endpoint,
		"postgres://",
	} {
		if strings.Contains(deniedErr.Error(), forbidden) {
			t.Fatalf("STUDY_CLI_ROUNDTRIP.AUTH_ERROR_LEAK: category_index=%d", forbiddenIndex)
		}
	}

	assertStudyCLIRoundTripCount(t, pool, "study_sessions", 1)
	assertStudyCLIRoundTripCount(t, pool, "study_session_creation_receipts", 1)
	assertStudyCLIRoundTripCount(t, pool, "study_flush_receipts", 1)
	assertStudyCLIRoundTripCount(t, pool, "study_events", 1)
}

func seedStudyCLIRoundTripCorpus(t *testing.T, repository *storage.Repository) model.Topic {
	t.Helper()
	seedContext, cancelSeed := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSeed()
	topic, err := repository.CreateTopic(seedContext, model.CreateTopicInput{
		Slug: "study-cli-roundtrip",
		Name: "Study CLI roundtrip",
	})
	if err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_CLI_ROUNDTRIP.TOPIC_CREATE_FAILED", err))
	}
	idea, err := repository.CreateIdea(seedContext, model.CreateIdeaInput{
		Names: []model.CreateNameInput{{Text: studyCLIRoundTripCardName, Kind: model.NameKindCanonical}},
		Definitions: []model.CreateDefinitionInput{{
			Body:      "Study CLI private definition canary",
			IsPrimary: true,
		}},
		Examples: []model.CreateExampleInput{{Body: "Study CLI private example canary"}},
	})
	if err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_CLI_ROUNDTRIP.IDEA_CREATE_FAILED", err))
	}
	if _, err := repository.AssignTopic(seedContext, idea.ID.String(), topic.Slug); err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_CLI_ROUNDTRIP.TOPIC_ASSIGN_FAILED", err))
	}
	return *topic
}

func startStudyCLIRoundTripServer(t *testing.T, databaseDSN string) string {
	t.Helper()
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("STUDY_CLI_ROUNDTRIP.PORT_RESERVATION_FAILED: cause_type=%T", err)
	}
	address := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatalf("STUDY_CLI_ROUNDTRIP.PORT_RELEASE_FAILED: cause_type=%T", err)
	}
	serveContext, cancelServe := context.WithCancel(context.Background())
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- lexinetserver.Serve(serveContext, lexinetserver.Config{
			Version:         "study-cli-roundtrip",
			ListenLoopback:  address,
			AuthToken:       studyCLIRoundTripBearer,
			DatabaseDSN:     databaseDSN,
			TailnetEnabled:  false,
			TailnetHostname: "unused-study-cli-roundtrip",
		})
	}()
	t.Cleanup(func() {
		cancelServe()
		select {
		case serveErr := <-serveResult:
			if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
				t.Errorf("STUDY_CLI_ROUNDTRIP.SERVER_SHUTDOWN_FAILED: cause_type=%T", serveErr)
			}
		case <-time.After(10 * time.Second):
			t.Error("STUDY_CLI_ROUNDTRIP.SERVER_SHUTDOWN_TIMEOUT")
		}
	})

	endpoint := "http://" + address
	readinessClient := &http.Client{Timeout: 500 * time.Millisecond}
	readinessDeadline := time.Now().Add(10 * time.Second)
	for {
		request, requestErr := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint+"/healthz", nil)
		if requestErr != nil {
			t.Fatalf("STUDY_CLI_ROUNDTRIP.READINESS_REQUEST_FAILED: cause_type=%T", requestErr)
		}
		response, requestErr := readinessClient.Do(request)
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return endpoint
			}
		}
		select {
		case serveErr := <-serveResult:
			// Preserve the terminal result for the registered cleanup so an
			// early startup failure cannot turn into a second shutdown timeout.
			serveResult <- serveErr
			cancelServe()
			t.Fatalf("STUDY_CLI_ROUNDTRIP.SERVER_START_FAILED: cause_type=%T", serveErr)
		default:
		}
		if time.Now().After(readinessDeadline) {
			cancelServe()
			t.Fatal("STUDY_CLI_ROUNDTRIP.SERVER_READINESS_TIMEOUT")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func marshalStudyCLIRoundTripRequest(t *testing.T, request any) []byte {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("STUDY_CLI_ROUNDTRIP.REQUEST_MARSHAL_FAILED: cause_type=%T", err)
	}
	return encoded
}

func decodeStudyCLIRoundTripOutput[T any](t *testing.T, output string) T {
	t.Helper()
	var resource T
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&resource); err != nil {
		t.Fatalf("STUDY_CLI_ROUNDTRIP.OUTPUT_DECODE_FAILED: cause_type=%T", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		t.Fatal("STUDY_CLI_ROUNDTRIP.OUTPUT_TRAILING_JSON")
	}
	return resource
}

func assertStudyCLIRoundTripCommandSuccess(t *testing.T, err error, stderr string) {
	t.Helper()
	if err != nil {
		t.Fatalf("STUDY_CLI_ROUNDTRIP.COMMAND_FAILED: exit_code=%d cause_type=%T", exitCode(err), err)
	}
	if stderr != "" {
		t.Fatal("STUDY_CLI_ROUNDTRIP.COMMAND_STDERR_NOT_EMPTY")
	}
}

func assertStudyCLIRoundTripSessionAnchors(t *testing.T, resource studysession.StudySessionResource) {
	t.Helper()
	if resource.ProtocolVersion != model.StudyProtocolVersion ||
		resource.SchemaVersion != model.StudySchemaVersion ||
		!reflect.DeepEqual(resource.RequiredSemantics, studysession.RequiredStudySemantics()) {
		t.Fatal("STUDY_CLI_ROUNDTRIP.SESSION_CONTRACT_ANCHORS_INVALID")
	}
}

func assertStudyCLIRoundTripSyncAnchors(t *testing.T, resource studysession.StudySyncResource) {
	t.Helper()
	if resource.ProtocolVersion != model.StudyProtocolVersion ||
		resource.SchemaVersion != model.StudySchemaVersion ||
		!reflect.DeepEqual(resource.RequiredSemantics, studysession.RequiredStudySemantics()) {
		t.Fatal("STUDY_CLI_ROUNDTRIP.SYNC_CONTRACT_ANCHORS_INVALID")
	}
	assertStudyCLIRoundTripSessionAnchors(t, resource.Session)
}

func assertStudyCLIRoundTripCount(t *testing.T, pool *pgxpool.Pool, table string, want int) {
	t.Helper()
	allowed := map[string]bool{
		"study_sessions":                  true,
		"study_session_creation_receipts": true,
		"study_flush_receipts":            true,
		"study_events":                    true,
	}
	if !allowed[table] {
		t.Fatal("STUDY_CLI_ROUNDTRIP.COUNT_TABLE_NOT_ALLOWLISTED")
	}
	queryContext, cancelQuery := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelQuery()
	var got int
	if err := pool.QueryRow(queryContext, "SELECT count(*) FROM "+table).Scan(&got); err != nil {
		t.Fatal(studypostgres.RedactedDatabaseError("STUDY_CLI_ROUNDTRIP.COUNT_FAILED", err))
	}
	if got != want {
		t.Fatalf("STUDY_CLI_ROUNDTRIP.COUNT_INVALID: table=%s got=%d want=%d", table, got, want)
	}
}
