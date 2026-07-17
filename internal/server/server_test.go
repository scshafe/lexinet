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
	"lexinet/internal/storage"
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

func TestTopicRoutesCreateListUpdateAssignUnassignAndListByTopic(t *testing.T) {
	topicID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	ideaID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	store := fakeStore{
		createTopic: func(ctx context.Context, input model.CreateTopicInput) (*model.Topic, error) {
			if input.Name != "Software" || input.Slug != "software" {
				t.Fatalf("unexpected create topic input: %#v", input)
			}
			return &model.Topic{ID: topicID, Slug: input.Slug, Name: input.Name, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}, nil
		},
		listTopics: func(ctx context.Context) ([]model.Topic, error) {
			return []model.Topic{{ID: topicID, Slug: "software", Name: "Software"}}, nil
		},
		updateTopic: func(ctx context.Context, ref string, input model.UpdateTopicInput) (*model.Topic, error) {
			if ref != "software" || input.Name == nil || *input.Name != "Software Systems" {
				t.Fatalf("unexpected update topic input: ref=%q input=%#v", ref, input)
			}
			return &model.Topic{ID: topicID, Slug: "software", Name: *input.Name}, nil
		},
		assignTopic: func(ctx context.Context, ideaRef, topicRef string) (*model.Topic, error) {
			if ideaRef != "rest" || topicRef != "software" {
				t.Fatalf("unexpected assign refs: idea=%q topic=%q", ideaRef, topicRef)
			}
			return &model.Topic{ID: topicID, Slug: topicRef, Name: "Software"}, nil
		},
		listTopicsByIdeaRef: func(ctx context.Context, ref string) ([]model.TopicSummary, error) {
			if ref != "rest" {
				t.Fatalf("unexpected idea topic ref: %q", ref)
			}
			return []model.TopicSummary{{ID: topicID, Slug: "software", Name: "Software"}}, nil
		},
		listIdeasByTopic: func(ctx context.Context, topicRef string, input model.IdeaListInput) (*model.IdeaListResponse, error) {
			if topicRef != "software" || input.Query != "api" || input.Limit != 5 || input.Offset != 2 {
				t.Fatalf("unexpected list by topic input: topic=%q input=%#v", topicRef, input)
			}
			return &model.IdeaListResponse{Total: 1, Limit: input.Limit, Offset: input.Offset, Ideas: []model.IdeaSummary{{ID: ideaID, CanonicalName: strPtr("REST"), Topics: []model.TopicSummary{{ID: topicID, Slug: "software", Name: "Software"}}}}}, nil
		},
		listIdeas: func(ctx context.Context, input model.IdeaListInput) (*model.IdeaListResponse, error) {
			if input.Topic != "software" {
				t.Fatalf("expected topic filter, got %#v", input)
			}
			return &model.IdeaListResponse{Total: 1, Limit: input.Limit, Offset: input.Offset, Ideas: []model.IdeaSummary{{ID: ideaID, CanonicalName: strPtr("REST")}}}, nil
		},
		unassignTopic: func(ctx context.Context, ideaRef, topicRef string) error {
			if ideaRef != "rest" || topicRef != "software" {
				t.Fatalf("unexpected unassign refs: idea=%q topic=%q", ideaRef, topicRef)
			}
			return nil
		},
	}
	router := Router(Config{}, store)

	assertStatus(t, router, http.MethodPost, "/topics", `{"slug":"software","name":"Software"}`, http.StatusCreated)
	assertStatus(t, router, http.MethodGet, "/topics", "", http.StatusOK)
	assertStatus(t, router, http.MethodPatch, "/topics/software", `{"name":"Software Systems"}`, http.StatusOK)
	assertStatus(t, router, http.MethodPost, "/ideas/rest/topics", `{"topic":"software"}`, http.StatusOK)
	assertStatus(t, router, http.MethodGet, "/ideas/rest/topics", "", http.StatusOK)
	assertStatus(t, router, http.MethodGet, "/topics/software/ideas?q=api&limit=5&offset=2", "", http.StatusOK)
	assertStatus(t, router, http.MethodGet, "/ideas?topic=software", "", http.StatusOK)
	assertStatus(t, router, http.MethodDelete, "/ideas/rest/topics/software", "", http.StatusNoContent)
}

func TestTopicRoutesReturnTypedErrorsForUnknownRefs(t *testing.T) {
	store := fakeStore{
		assignTopic: func(ctx context.Context, ideaRef, topicRef string) (*model.Topic, error) {
			return nil, storage.ErrIdeaNotFound
		},
		listIdeasByTopic: func(ctx context.Context, topicRef string, input model.IdeaListInput) (*model.IdeaListResponse, error) {
			return nil, storage.ErrTopicNotFound
		},
	}
	router := Router(Config{}, store)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ideas/missing/topics", bytes.NewBufferString(`{"topic":"software"}`)))
	assertError(t, rec, http.StatusNotFound, "IDEA_NOT_FOUND")

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/topics/missing/ideas", nil))
	assertError(t, rec, http.StatusNotFound, "TOPIC_NOT_FOUND")
}

func TestAcronymLinkRoutesCreateListDeleteAndBothSideVisibility(t *testing.T) {
	linkID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	acronymID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	expansionID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	note := "common terminal UI abbreviation"
	store := fakeStore{
		upsertAcronymLink: func(ctx context.Context, input model.UpsertAcronymLinkInput) (*model.AcronymLink, error) {
			if input.Acronym != "TUI" || input.Expansion != "Terminal User Interface" || input.Note == nil || *input.Note != note {
				t.Fatalf("unexpected acronym link input: %#v", input)
			}
			return &model.AcronymLink{ID: linkID, AcronymIdeaID: acronymID, AcronymName: strPtr("TUI"), ExpansionIdeaID: expansionID, ExpansionName: strPtr("Terminal User Interface"), Note: &note}, nil
		},
		listAcronymLinksByIdeaRef: func(ctx context.Context, ref string) ([]model.AcronymLink, error) {
			direction := model.AcronymLinkDirectionAcronym
			if ref == "Terminal User Interface" {
				direction = model.AcronymLinkDirectionExpansion
			} else if ref != "TUI" {
				t.Fatalf("unexpected acronym list ref: %q", ref)
			}
			return []model.AcronymLink{{ID: linkID, AcronymIdeaID: acronymID, AcronymName: strPtr("TUI"), ExpansionIdeaID: expansionID, ExpansionName: strPtr("Terminal User Interface"), Direction: &direction}}, nil
		},
		deleteAcronymLink: func(ctx context.Context, id uuid.UUID) error {
			if id != linkID {
				t.Fatalf("unexpected acronym link delete id: %s", id)
			}
			return nil
		},
	}
	router := Router(Config{}, store)

	assertStatus(t, router, http.MethodPost, "/acronym-links", `{"acronym":"TUI","expansion":"Terminal User Interface","note":"common terminal UI abbreviation"}`, http.StatusCreated)
	assertAcronymDirection(t, router, "/ideas/TUI/acronym-links", model.AcronymLinkDirectionAcronym)
	assertAcronymDirection(t, router, "/ideas/Terminal%20User%20Interface/acronym-links", model.AcronymLinkDirectionExpansion)
	assertStatus(t, router, http.MethodDelete, "/acronym-links/33333333-3333-3333-3333-333333333333", "", http.StatusNoContent)
}

func TestAcronymLinkRoutesReturnTypedErrorsForUnknownRefs(t *testing.T) {
	store := fakeStore{
		upsertAcronymLink: func(ctx context.Context, input model.UpsertAcronymLinkInput) (*model.AcronymLink, error) {
			return nil, storage.ErrIdeaNotFound
		},
		deleteAcronymLink: func(ctx context.Context, id uuid.UUID) error {
			return storage.ErrAcronymLinkNotFound
		},
	}
	router := Router(Config{}, store)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/acronym-links", bytes.NewBufferString(`{"acronym":"missing","expansion":"Terminal User Interface"}`)))
	assertError(t, rec, http.StatusNotFound, "IDEA_NOT_FOUND")

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/acronym-links/33333333-3333-3333-3333-333333333333", nil))
	assertError(t, rec, http.StatusNotFound, "ACRONYM_LINK_NOT_FOUND")
}

func TestIdeaDetailIncludesTopicsAndAcronymLinks(t *testing.T) {
	ideaID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	topicID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	linkID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	store := fakeStore{
		getIdeaByRef: func(ctx context.Context, ref string) (*model.Idea, error) {
			return &model.Idea{ID: ideaID, Status: model.IdeaStatusActive}, nil
		},
		listNamesByRef: func(ctx context.Context, ref string) ([]model.Name, error) {
			return []model.Name{{ID: uuid.MustParse("66666666-6666-6666-6666-666666666666"), IdeaID: ideaID, Text: "TUI", Kind: model.NameKindCanonical}}, nil
		},
		listTopicsByIdeaRef: func(ctx context.Context, ref string) ([]model.TopicSummary, error) {
			return []model.TopicSummary{{ID: topicID, Slug: "software", Name: "Software"}}, nil
		},
		listAcronymLinksByIdeaRef: func(ctx context.Context, ref string) ([]model.AcronymLink, error) {
			return []model.AcronymLink{{ID: linkID, AcronymIdeaID: ideaID, AcronymName: strPtr("TUI"), ExpansionIdeaID: uuid.MustParse("55555555-5555-5555-5555-555555555555"), ExpansionName: strPtr("Terminal User Interface")}}, nil
		},
	}
	router := Router(Config{}, store)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ideas/TUI", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Topics       []model.TopicSummary `json:"topics"`
		AcronymLinks []model.AcronymLink  `json:"acronym_links"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if len(body.Topics) != 1 || body.Topics[0].Slug != "software" {
		t.Fatalf("unexpected topics: %#v", body.Topics)
	}
	if len(body.AcronymLinks) != 1 || *body.AcronymLinks[0].ExpansionName != "Terminal User Interface" {
		t.Fatalf("unexpected acronym links: %#v", body.AcronymLinks)
	}
}

func TestSearchResponseIncludesEnrichment(t *testing.T) {
	ideaID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	store := fakeStore{search: func(ctx context.Context, input model.SearchInput) (*model.SearchResponse, error) {
		return &model.SearchResponse{Query: input.Query, Scope: model.SearchScopeAll, Limit: input.Limit, Results: []model.SearchResult{{Idea: model.SearchIdea{ID: ideaID, CanonicalName: strPtr("TUI"), Topics: []model.TopicSummary{{Slug: "software", Name: "Software"}}, AcronymLinks: []model.AcronymLinkSummary{{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), AcronymIdeaID: ideaID, ExpansionName: strPtr("Terminal User Interface")}}}, MatchType: model.SearchMatchExact}}}, nil
	}}
	router := Router(Config{}, store)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=TUI", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body model.SearchResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if len(body.Results) != 1 || len(body.Results[0].Idea.Topics) != 1 || len(body.Results[0].Idea.AcronymLinks) != 1 {
		t.Fatalf("expected enriched search result, got %#v", body.Results)
	}
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

func assertStatus(t *testing.T, router http.Handler, method, target, body string, status int) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, bytes.NewBufferString(body))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s status = %d, want %d: %s", method, target, rec.Code, status, rec.Body.String())
	}
}

func strPtr(value string) *string {
	return &value
}

func assertAcronymDirection(t *testing.T, router http.Handler, target string, direction model.AcronymLinkDirection) {
	t.Helper()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d: %s", target, rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		AcronymLinks []model.AcronymLink `json:"acronym_links"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode acronym links: %v", err)
	}
	if len(body.AcronymLinks) != 1 || body.AcronymLinks[0].Direction == nil || *body.AcronymLinks[0].Direction != direction {
		t.Fatalf("expected direction %q, got %#v", direction, body.AcronymLinks)
	}
}

var errUnsupported = errors.New("unsupported fake store method")

type fakeStore struct {
	createIdea                func(context.Context, model.CreateIdeaInput) (*model.Idea, error)
	listIdeas                 func(context.Context, model.IdeaListInput) (*model.IdeaListResponse, error)
	getIdeaByRef              func(context.Context, string) (*model.Idea, error)
	listNamesByRef            func(context.Context, string) ([]model.Name, error)
	upsertRelationship        func(context.Context, string, string, model.RelationKind, *string) (*model.Relationship, error)
	createTopic               func(context.Context, model.CreateTopicInput) (*model.Topic, error)
	listTopics                func(context.Context) ([]model.Topic, error)
	updateTopic               func(context.Context, string, model.UpdateTopicInput) (*model.Topic, error)
	assignTopic               func(context.Context, string, string) (*model.Topic, error)
	unassignTopic             func(context.Context, string, string) error
	listTopicsByIdeaRef       func(context.Context, string) ([]model.TopicSummary, error)
	listIdeasByTopic          func(context.Context, string, model.IdeaListInput) (*model.IdeaListResponse, error)
	upsertAcronymLink         func(context.Context, model.UpsertAcronymLinkInput) (*model.AcronymLink, error)
	listAcronymLinksByIdeaRef func(context.Context, string) ([]model.AcronymLink, error)
	deleteAcronymLink         func(context.Context, uuid.UUID) error
	search                    func(context.Context, model.SearchInput) (*model.SearchResponse, error)
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

func (f fakeStore) ListIdeas(ctx context.Context, input model.IdeaListInput) (*model.IdeaListResponse, error) {
	if f.listIdeas != nil {
		return f.listIdeas(ctx, input)
	}
	return &model.IdeaListResponse{}, nil
}
func (f fakeStore) GetIdeaByRef(ctx context.Context, ref string) (*model.Idea, error) {
	if f.getIdeaByRef != nil {
		return f.getIdeaByRef(ctx, ref)
	}
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
func (f fakeStore) ListNamesByRef(ctx context.Context, ref string) ([]model.Name, error) {
	if f.listNamesByRef != nil {
		return f.listNamesByRef(ctx, ref)
	}
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
func (f fakeStore) CreateTopic(ctx context.Context, input model.CreateTopicInput) (*model.Topic, error) {
	if f.createTopic != nil {
		return f.createTopic(ctx, input)
	}
	return nil, errUnsupported
}
func (f fakeStore) ListTopics(ctx context.Context) ([]model.Topic, error) {
	if f.listTopics != nil {
		return f.listTopics(ctx)
	}
	return nil, errUnsupported
}
func (f fakeStore) UpdateTopic(ctx context.Context, ref string, input model.UpdateTopicInput) (*model.Topic, error) {
	if f.updateTopic != nil {
		return f.updateTopic(ctx, ref, input)
	}
	return nil, errUnsupported
}
func (fakeStore) DeleteTopic(context.Context, string) error { return errUnsupported }
func (f fakeStore) AssignTopic(ctx context.Context, ideaRef, topicRef string) (*model.Topic, error) {
	if f.assignTopic != nil {
		return f.assignTopic(ctx, ideaRef, topicRef)
	}
	return nil, errUnsupported
}
func (f fakeStore) UnassignTopic(ctx context.Context, ideaRef, topicRef string) error {
	if f.unassignTopic != nil {
		return f.unassignTopic(ctx, ideaRef, topicRef)
	}
	return errUnsupported
}
func (f fakeStore) ListTopicsByIdeaRef(ctx context.Context, ref string) ([]model.TopicSummary, error) {
	if f.listTopicsByIdeaRef != nil {
		return f.listTopicsByIdeaRef(ctx, ref)
	}
	return nil, errUnsupported
}
func (f fakeStore) ListIdeasByTopic(ctx context.Context, topicRef string, input model.IdeaListInput) (*model.IdeaListResponse, error) {
	if f.listIdeasByTopic != nil {
		return f.listIdeasByTopic(ctx, topicRef, input)
	}
	return nil, errUnsupported
}
func (f fakeStore) UpsertAcronymLink(ctx context.Context, input model.UpsertAcronymLinkInput) (*model.AcronymLink, error) {
	if f.upsertAcronymLink != nil {
		return f.upsertAcronymLink(ctx, input)
	}
	return nil, errUnsupported
}
func (f fakeStore) ListAcronymLinksByIdeaRef(ctx context.Context, ref string) ([]model.AcronymLink, error) {
	if f.listAcronymLinksByIdeaRef != nil {
		return f.listAcronymLinksByIdeaRef(ctx, ref)
	}
	return nil, errUnsupported
}
func (f fakeStore) DeleteAcronymLink(ctx context.Context, id uuid.UUID) error {
	if f.deleteAcronymLink != nil {
		return f.deleteAcronymLink(ctx, id)
	}
	return errUnsupported
}
