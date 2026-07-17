package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

func TestOpenRoutesBypassAuth(t *testing.T) {
	router := Router(Config{Version: "test", AuthToken: "secret"}, fakeStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestAuthGroupRequiresBearer(t *testing.T) {
	router := Router(Config{AuthToken: "secret"}, fakeStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ideas", nil)

	router.ServeHTTP(rec, req)

	assertError(t, rec, http.StatusUnauthorized, "UNAUTHORIZED")
}

func TestCreateIdeaRejectsUnknownFields(t *testing.T) {
	router := Router(Config{AuthToken: "secret"}, fakeStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ideas", bytes.NewBufferString(`{"names":[{"text":"REST","kind":"canonical"}],"extra":true}`))
	req.Header.Set("Authorization", "Bearer secret")

	router.ServeHTTP(rec, req)

	assertError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
}

func TestCreateIdea(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	store := fakeStore{createIdea: func(ctx context.Context, input model.CreateIdeaInput) (*model.Idea, error) {
		if len(input.Names) != 1 || input.Names[0].Text != "REST" {
			t.Fatalf("unexpected create input: %#v", input)
		}
		return &model.Idea{ID: id, Status: model.IdeaStatusActive, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}, nil
	}}
	router := Router(Config{}, store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ideas", bytes.NewBufferString(`{"names":[{"text":"REST","kind":"canonical"}]}`))

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
}

func TestRelationshipSelfEdgeError(t *testing.T) {
	store := fakeStore{upsertRelationship: func(ctx context.Context, from, to string, kind model.RelationKind, note *string) (*model.Relationship, error) {
		return nil, model.ErrSelfRelationship
	}}
	router := Router(Config{}, store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/relationships", bytes.NewBufferString(`{"from":"a","to":"a","kind":"is_a"}`))

	router.ServeHTTP(rec, req)

	assertError(t, rec, http.StatusBadRequest, "RELATIONSHIP_SELF_EDGE")
}

func TestSearchRejectsEmptyQuery(t *testing.T) {
	store := fakeStore{search: func(ctx context.Context, input model.SearchInput) (*model.SearchResponse, error) {
		_, err := model.NormalizeSearchInput(input)
		return nil, err
	}}
	router := Router(Config{}, store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=", nil)

	router.ServeHTTP(rec, req)

	assertError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	var envelope ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if envelope.Error.Code != code {
		t.Fatalf("code = %q, want %q", envelope.Error.Code, code)
	}
}

var errUnsupported = errors.New("unsupported fake store method")

type fakeStore struct {
	createIdea         func(context.Context, model.CreateIdeaInput) (*model.Idea, error)
	upsertRelationship func(context.Context, string, string, model.RelationKind, *string) (*model.Relationship, error)
	search             func(context.Context, model.SearchInput) (*model.SearchResponse, error)
}

func (f fakeStore) CreateIdea(ctx context.Context, input model.CreateIdeaInput) (*model.Idea, error) {
	if f.createIdea != nil {
		return f.createIdea(ctx, input)
	}
	return nil, errUnsupported
}

func (f fakeStore) UpsertRelationship(ctx context.Context, from, to string, kind model.RelationKind, note *string) (*model.Relationship, error) {
	if f.upsertRelationship != nil {
		return f.upsertRelationship(ctx, from, to, kind, note)
	}
	return nil, errUnsupported
}

func (f fakeStore) Search(ctx context.Context, input model.SearchInput) (*model.SearchResponse, error) {
	if f.search != nil {
		return f.search(ctx, input)
	}
	return nil, errUnsupported
}

func (fakeStore) ListIdeas(context.Context, model.IdeaListInput) (*model.IdeaListResponse, error) {
	return &model.IdeaListResponse{}, nil
}
func (fakeStore) GetIdeaByRef(context.Context, string) (*model.Idea, error) {
	return nil, errUnsupported
}
func (fakeStore) UpdateIdea(context.Context, string, model.UpdateIdeaInput) (*model.Idea, error) {
	return nil, errUnsupported
}
func (fakeStore) DeleteIdea(context.Context, string) error { return errUnsupported }
func (fakeStore) MergeIdea(context.Context, string, string) (*model.Idea, error) {
	return nil, errUnsupported
}
func (fakeStore) ResolveIdeaID(context.Context, string) (uuid.UUID, error) {
	return uuid.Nil, errUnsupported
}
func (fakeStore) AddNameByRef(context.Context, string, model.CreateNameInput) (*model.Name, error) {
	return nil, errUnsupported
}
func (fakeStore) ListNamesByRef(context.Context, string) ([]model.Name, error) {
	return nil, errUnsupported
}
func (fakeStore) UpdateName(context.Context, uuid.UUID, model.UpdateNameInput) (*model.Name, error) {
	return nil, errUnsupported
}
func (fakeStore) DeleteName(context.Context, uuid.UUID) error { return errUnsupported }
func (fakeStore) PromoteName(context.Context, uuid.UUID, string) (*model.Name, error) {
	return nil, errUnsupported
}
func (fakeStore) CreateDefinition(context.Context, string, model.CreateDefinitionInput) (*model.Definition, error) {
	return nil, errUnsupported
}
func (fakeStore) ListDefinitions(context.Context, string) ([]model.Definition, error) {
	return nil, errUnsupported
}
func (fakeStore) UpdateDefinition(context.Context, uuid.UUID, model.UpdateDefinitionInput) (*model.Definition, error) {
	return nil, errUnsupported
}
func (fakeStore) DeleteDefinition(context.Context, uuid.UUID) error { return errUnsupported }
func (fakeStore) CreateExample(context.Context, string, model.CreateExampleInput) (*model.Example, error) {
	return nil, errUnsupported
}
func (fakeStore) ListExamples(context.Context, string) ([]model.Example, error) {
	return nil, errUnsupported
}
func (fakeStore) UpdateExample(context.Context, uuid.UUID, model.UpdateExampleInput) (*model.Example, error) {
	return nil, errUnsupported
}
func (fakeStore) DeleteExample(context.Context, uuid.UUID) error { return errUnsupported }
func (fakeStore) CreateNote(context.Context, string, model.CreateNoteInput) (*model.Note, error) {
	return nil, errUnsupported
}
func (fakeStore) ListNotes(context.Context, string) ([]model.Note, error) { return nil, errUnsupported }
func (fakeStore) UpdateNote(context.Context, uuid.UUID, model.UpdateNoteInput) (*model.Note, error) {
	return nil, errUnsupported
}
func (fakeStore) DeleteNote(context.Context, uuid.UUID) error { return errUnsupported }
func (fakeStore) ListRelationships(context.Context, string, *model.RelationKind, model.RelationDirection) ([]model.Relationship, error) {
	return nil, errUnsupported
}
func (fakeStore) DeleteRelationship(context.Context, uuid.UUID) error { return errUnsupported }
func (fakeStore) TraverseRelationships(context.Context, string, model.TraverseRelationshipsInput) (*model.RelationshipTraversal, error) {
	return nil, errUnsupported
}
func (fakeStore) CreateTopic(context.Context, model.CreateTopicInput) (*model.Topic, error) {
	return nil, errUnsupported
}
func (fakeStore) ListTopics(context.Context) ([]model.Topic, error) { return nil, errUnsupported }
func (fakeStore) UpdateTopic(context.Context, string, model.UpdateTopicInput) (*model.Topic, error) {
	return nil, errUnsupported
}
func (fakeStore) DeleteTopic(context.Context, string) error { return errUnsupported }
func (fakeStore) AssignTopic(context.Context, string, string) (*model.Topic, error) {
	return nil, errUnsupported
}
func (fakeStore) UnassignTopic(context.Context, string, string) error { return errUnsupported }
func (fakeStore) ListTopicsByIdeaRef(context.Context, string) ([]model.TopicSummary, error) {
	return nil, errUnsupported
}
func (fakeStore) ListIdeasByTopic(context.Context, string, model.IdeaListInput) (*model.IdeaListResponse, error) {
	return nil, errUnsupported
}
func (fakeStore) UpsertAcronymLink(context.Context, model.UpsertAcronymLinkInput) (*model.AcronymLink, error) {
	return nil, errUnsupported
}
func (fakeStore) ListAcronymLinksByIdeaRef(context.Context, string) ([]model.AcronymLink, error) {
	return nil, errUnsupported
}
func (fakeStore) DeleteAcronymLink(context.Context, uuid.UUID) error { return errUnsupported }
