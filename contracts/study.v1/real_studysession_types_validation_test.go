package studyv1contract_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

// TestStudyV1RealStudysessionTypesValidateAgainstFrozenOpenAPI proves that the
// concrete service types, rather than parallel map fixtures, emit the frozen
// request envelopes, every tagged event variant, and representative success
// resources accepted by the canonical schemas.
func TestStudyV1RealStudysessionTypesValidateAgainstFrozenOpenAPI(t *testing.T) {
	directory := contractDirectory(t)
	manifest := loadManifest(t, directory)
	compiler := loadCompiler(t, filepath.Join(directory, manifest.OpenAPIPath))

	createRequest := studysession.StudyCreateSessionRequest{
		ProtocolVersion:   model.StudyProtocolVersion,
		SchemaVersion:     model.StudySchemaVersion,
		CreationRequestID: uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
		ProfileRef:        "real-type-profile",
		Scope: studysession.StudyScopeRequest{
			TopicRefs: []string{"real-type-topic"},
			CardKind:  model.StudyCardKindIdea,
		},
		Page: studysession.StudyPageRequest{AfterPosition: -1, Limit: 16},
	}
	validateRealStudyType(t, compiler, "CreateSessionRequest", createRequest)

	rating := model.StudyRatingGood
	ratingEventID := uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	syncRequest := studysession.StudySyncRequest{
		ProtocolVersion:      model.StudyProtocolVersion,
		SchemaVersion:        model.StudySchemaVersion,
		SyncID:               uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
		ClientStreamID:       uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"),
		ExpectedStateVersion: 7,
		Events: []studysession.StudyEventRequest{
			realStudyTypeEvent(uuid.MustParse("11111111-1111-4111-8111-111111111111"), 1, model.StudyEventKindPresented),
			realStudyTypeEvent(uuid.MustParse("22222222-2222-4222-8222-222222222222"), 2, model.StudyEventKindRevealed),
			func() studysession.StudyEventRequest {
				event := realStudyTypeEvent(ratingEventID, 3, model.StudyEventKindRated)
				event.Rating = &rating
				return event
			}(),
			func() studysession.StudyEventRequest {
				event := realStudyTypeEvent(uuid.MustParse("33333333-3333-4333-8333-333333333333"), 4, model.StudyEventKindRatingVoided)
				event.RatingEventID = &ratingEventID
				return event
			}(),
		},
		Page: studysession.StudyPageRequest{AfterPosition: -1, Limit: 16},
	}
	validateRealStudyType(t, compiler, "SyncRequest", syncRequest)

	for _, example := range []struct {
		path       string
		schema     string
		newSubject func() any
	}{
		{
			path:       "examples/session-created.json",
			schema:     "CreateSessionResource",
			newSubject: func() any { return &studysession.StudySessionResource{} },
		},
		{
			path:       "examples/session-active-page-exhausted.json",
			schema:     "SessionResource",
			newSubject: func() any { return &studysession.StudySessionResource{} },
		},
		{
			path:       "examples/session-waiting-for-corpus.json",
			schema:     "CreateSessionResource",
			newSubject: func() any { return &studysession.StudySessionResource{} },
		},
		{
			path:       "examples/sync-new-suffix-mixed-success.json",
			schema:     "SyncResource",
			newSubject: func() any { return &studysession.StudySyncResource{} },
		},
	} {
		exampleBytes, err := os.ReadFile(filepath.Join(directory, example.path))
		if err != nil {
			t.Fatalf("read real-type source example %s: %v", example.path, err)
		}
		subject := example.newSubject()
		decoder := json.NewDecoder(bytes.NewReader(exampleBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(subject); err != nil {
			t.Fatalf("decode %s into concrete studysession type: %v", example.path, err)
		}
		if err := requireRealStudyTypeJSONEOF(decoder); err != nil {
			t.Fatalf("decode %s into concrete studysession type: %v", example.path, err)
		}
		validateRealStudyType(t, compiler, example.schema, subject)
	}
}

func realStudyTypeEvent(
	eventID uuid.UUID,
	sequence int64,
	kind model.StudyEventKind,
) studysession.StudyEventRequest {
	return studysession.StudyEventRequest{
		EventID:            eventID,
		ClientSequence:     sequence,
		ProfileRef:         "real-type-profile",
		SessionItemID:      uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"),
		CardIdentity:       uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff"),
		ContentSHA256:      "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Kind:               kind,
		ClientClockQuality: model.StudyClientClockQualityUnknown,
	}
}

func validateRealStudyType(t *testing.T, compiler interface {
	Compile(string) (*jsonschema.Schema, error)
}, schemaName string, subject any) {
	t.Helper()
	schema, err := compiler.Compile(openAPIResource + "#/components/schemas/" + schemaName)
	if err != nil {
		t.Fatalf("STUDY_REAL_TYPES.SCHEMA_COMPILE_FAILED: schema=%s cause_type=%T", schemaName, err)
	}
	emitted, err := json.Marshal(subject)
	if err != nil {
		t.Fatalf("STUDY_REAL_TYPES.MARSHAL_FAILED: schema=%s cause_type=%T", schemaName, err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(emitted))
	if err != nil {
		t.Fatalf("STUDY_REAL_TYPES.EMITTED_DECODE_FAILED: schema=%s cause_type=%T", schemaName, err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatalf("STUDY_REAL_TYPES.SCHEMA_VALIDATION_FAILED: schema=%s cause_type=%T", schemaName, err)
	}
}

func requireRealStudyTypeJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}
