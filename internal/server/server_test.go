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
	searchcfg "lexinet/internal/search"
	"lexinet/internal/storage"
)

func TestValidateConfigurationRejectsTailnetWithoutBearer(t *testing.T) {
	for _, token := range []string{"", " \t\n "} {
		err := ValidateConfiguration(Config{AuthToken: token, TailnetEnabled: true})

		var configurationError *ConfigurationError
		if !errors.As(err, &configurationError) {
			t.Fatalf("error = %T %v, want *ConfigurationError", err, err)
		}
		if configurationError.Code != ErrorCodeTailnetAuthTokenRequired {
			t.Fatalf("error code = %q, want %q", configurationError.Code, ErrorCodeTailnetAuthTokenRequired)
		}
		if configurationError.Context["configuration_field"] != "service.auth_token" {
			t.Fatalf("configuration field context = %#v, want service.auth_token", configurationError.Context["configuration_field"])
		}
		if configurationError.Context["tailnet_enabled"] != true {
			t.Fatalf("tailnet context = %#v, want true", configurationError.Context["tailnet_enabled"])
		}
		if got, want := err.Error(), "CONFIG.TAILNET_AUTH_TOKEN_REQUIRED: service.auth_token must be non-empty when service.tailnet_enabled is true"; got != want {
			t.Fatalf("error = %q, want %q", got, want)
		}
	}
}

func TestValidateConfigurationAllowsTailnetDisabledWithoutBearer(t *testing.T) {
	if err := ValidateConfiguration(Config{AuthToken: " \t\n ", TailnetEnabled: false}); err != nil {
		t.Fatalf("validate Tailnet-disabled configuration: %v", err)
	}
}

func TestServeRejectsUnsafeTailnetConfigurationBeforeDatabaseOrListenerWork(t *testing.T) {
	err := Serve(context.Background(), Config{
		AuthToken:       " \t\n ",
		TailnetEnabled:  true,
		DatabaseDSN:     ":// invalid database DSN",
		ListenLoopback:  "invalid listen address",
		TailnetHostname: "unsafe-test",
	})

	var configurationError *ConfigurationError
	if !errors.As(err, &configurationError) {
		t.Fatalf("error = %T %v, want *ConfigurationError", err, err)
	}
	if configurationError.Code != ErrorCodeTailnetAuthTokenRequired {
		t.Fatalf("error code = %q, want %q", configurationError.Code, ErrorCodeTailnetAuthTokenRequired)
	}
}

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

func TestSubmissionRoutesCreateListCompleteFail(t *testing.T) {
	submissionID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	ideaID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	now := time.Unix(1, 0)
	store := fakeStore{
		createSubmission: func(ctx context.Context, input model.CreateSubmissionInput) (*model.Submission, error) {
			if input.Term != "TUI" || input.Context == nil || *input.Context != "menu bar capture" {
				t.Fatalf("unexpected create submission input: %#v", input)
			}
			return &model.Submission{ID: submissionID, Term: input.Term, Context: input.Context, Status: model.SubmissionStatusPending, CreatedAt: now, UpdatedAt: now}, nil
		},
		listSubmissions: func(ctx context.Context, input model.ListSubmissionsInput) (*model.ListSubmissionsResponse, error) {
			if input.Status == nil || *input.Status != model.SubmissionStatusPending || input.Limit != 5 || input.Offset != 2 {
				t.Fatalf("unexpected list submissions input: %#v", input)
			}
			return &model.ListSubmissionsResponse{Total: 1, Limit: input.Limit, Offset: input.Offset, Submissions: []model.Submission{{ID: submissionID, Term: "TUI", Status: model.SubmissionStatusPending}}}, nil
		},
		completeSubmission: func(ctx context.Context, id uuid.UUID, input model.CompleteSubmissionInput) (*model.Submission, error) {
			if id != submissionID || input.Idea != "Terminal User Interface" || input.Note == nil || *input.Note != "created idea" {
				t.Fatalf("unexpected complete input: id=%s input=%#v", id, input)
			}
			return &model.Submission{ID: id, Term: "TUI", Status: model.SubmissionStatusEnriched, IdeaID: &ideaID, Note: input.Note, CompletedAt: &now, CreatedAt: now, UpdatedAt: now}, nil
		},
		failSubmission: func(ctx context.Context, id uuid.UUID, input model.FailSubmissionInput) (*model.Submission, error) {
			if id != submissionID || input.Note == nil || *input.Note != "duplicate" {
				t.Fatalf("unexpected fail input: id=%s input=%#v", id, input)
			}
			return &model.Submission{ID: id, Term: "TUI", Status: model.SubmissionStatusFailed, Note: input.Note, CompletedAt: &now, CreatedAt: now, UpdatedAt: now}, nil
		},
	}
	router := Router(Config{}, store)

	assertStatus(t, router, http.MethodPost, "/submissions", `{"term":"  TUI  ","context":" menu bar capture "}`, http.StatusCreated)
	assertStatus(t, router, http.MethodGet, "/submissions?status=pending&limit=5&offset=2", "", http.StatusOK)
	assertStatus(t, router, http.MethodPost, "/submissions/77777777-7777-7777-7777-777777777777/complete", `{"idea":" Terminal User Interface ","note":" created idea "}`, http.StatusOK)
	assertStatus(t, router, http.MethodPost, "/submissions/77777777-7777-7777-7777-777777777777/fail", `{"note":" duplicate "}`, http.StatusOK)
}

func TestSubmissionRoutesRejectUnknownFieldsAndInvalidStatus(t *testing.T) {
	router := Router(Config{}, fakeStore{})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/submissions", bytes.NewBufferString(`{"term":"TUI","extra":true}`)))
	assertError(t, rec, http.StatusBadRequest, "BAD_REQUEST")

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/submissions?status=done", nil))
	assertError(t, rec, http.StatusBadRequest, "INVALID_SUBMISSION_STATUS")
}

func TestSubmissionRoutesMapMissingIdeaSubmissionAndInvalidTransition(t *testing.T) {
	submissionID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	store := fakeStore{
		completeSubmission: func(ctx context.Context, id uuid.UUID, input model.CompleteSubmissionInput) (*model.Submission, error) {
			switch input.Idea {
			case "missing-idea":
				return nil, storage.ErrIdeaNotFound
			case "terminal-row":
				return nil, storage.ErrInvalidSubmissionTransition
			default:
				return nil, storage.ErrSubmissionNotFound
			}
		},
		failSubmission: func(ctx context.Context, id uuid.UUID, input model.FailSubmissionInput) (*model.Submission, error) {
			return nil, storage.ErrInvalidSubmissionTransition
		},
	}
	router := Router(Config{}, store)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/submissions/"+submissionID.String()+"/complete", bytes.NewBufferString(`{"idea":"missing-idea"}`)))
	assertError(t, rec, http.StatusNotFound, "IDEA_NOT_FOUND")

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/submissions/"+submissionID.String()+"/complete", bytes.NewBufferString(`{"idea":"missing-submission"}`)))
	assertError(t, rec, http.StatusNotFound, "SUBMISSION_NOT_FOUND")

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/submissions/"+submissionID.String()+"/complete", bytes.NewBufferString(`{"idea":"terminal-row"}`)))
	assertError(t, rec, http.StatusConflict, "SUBMISSION_INVALID_TRANSITION")

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/submissions/"+submissionID.String()+"/fail", bytes.NewBufferString(`{"note":"already done"}`)))
	assertError(t, rec, http.StatusConflict, "SUBMISSION_INVALID_TRANSITION")
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

func TestSearchHandlerDefaultRequestUsesDailyDriverAndExistingParams(t *testing.T) {
	store := fakeStore{search: func(ctx context.Context, input model.SearchInput) (*model.SearchResponse, error) {
		if input.Query != "redos" || input.Scope != model.SearchScopeNames || input.Limit != 5 || input.Offset != 2 {
			t.Fatalf("unexpected raw search input: %#v", input)
		}
		if input.Strategy != "" {
			t.Fatalf("strategy query should be omitted for server default, got %q", input.Strategy)
		}
		if input.FuzzyOverride != nil || input.ThresholdOverride != nil {
			t.Fatalf("default request should not set overrides: fuzzy=%#v threshold=%#v", input.FuzzyOverride, input.ThresholdOverride)
		}
		wantKinds := []model.NameKind{model.NameKindCanonical, model.NameKindAlias, model.NameKindAcronym}
		assertNameKinds(t, input.Kinds, wantKinds)

		input, err := model.NormalizeSearchInput(input)
		if err != nil {
			return nil, err
		}
		if input.Strategy != searchcfg.StrategyDailyDriver || !input.Fuzzy || input.Threshold != 0.3 {
			t.Fatalf("default strategy semantics = strategy %q fuzzy %v threshold %v", input.Strategy, input.Fuzzy, input.Threshold)
		}
		return &model.SearchResponse{Query: input.Query, Scope: input.Scope, Fuzzy: input.Fuzzy, Strategy: input.Strategy, Limit: input.Limit, Offset: input.Offset, Provenance: model.SearchResponseProvenance(input)}, nil
	}}
	router := Router(Config{}, store)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=redos&scope=names&kind=canonical,alias&kind=acronym&limit=5&offset=2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body model.SearchResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if body.Strategy != searchcfg.StrategyDailyDriver || !body.Fuzzy || body.Limit != 5 || body.Offset != 2 {
		t.Fatalf("unexpected default response: %#v", body)
	}
}

func TestSearchHandlerPassesStrategyAndExplicitOverrides(t *testing.T) {
	falseValue := false
	trueValue := true
	thresholdValue := 0.72
	tests := []struct {
		name                  string
		target                string
		wantRawStrategy       model.SearchStrategyName
		wantStrategy          model.SearchStrategyName
		wantFuzzy             bool
		wantThreshold         float64
		wantFuzzyOverride     *bool
		wantThresholdOverride *float64
	}{
		{
			name:            "legacy defaults fuzzy off",
			target:          "/search?q=redos&strategy=legacy",
			wantRawStrategy: searchcfg.StrategyLegacy,
			wantStrategy:    searchcfg.StrategyLegacy,
			wantFuzzy:       false,
			wantThreshold:   0.3,
		},
		{
			name:              "explicit fuzzy false overrides daily driver",
			target:            "/search?q=redos&fuzzy=false",
			wantStrategy:      searchcfg.StrategyDailyDriver,
			wantFuzzy:         false,
			wantThreshold:     0.3,
			wantFuzzyOverride: &falseValue,
		},
		{
			name:              "explicit fuzzy true overrides legacy",
			target:            "/search?q=redos&strategy=legacy&fuzzy=true",
			wantRawStrategy:   searchcfg.StrategyLegacy,
			wantStrategy:      searchcfg.StrategyLegacy,
			wantFuzzy:         true,
			wantThreshold:     0.3,
			wantFuzzyOverride: &trueValue,
		},
		{
			name:                  "explicit threshold overrides legacy",
			target:                "/search?q=redos&strategy=legacy&threshold=0.72",
			wantRawStrategy:       searchcfg.StrategyLegacy,
			wantStrategy:          searchcfg.StrategyLegacy,
			wantFuzzy:             false,
			wantThreshold:         thresholdValue,
			wantThresholdOverride: &thresholdValue,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := fakeStore{search: func(ctx context.Context, input model.SearchInput) (*model.SearchResponse, error) {
				if input.Strategy != tc.wantRawStrategy {
					t.Fatalf("raw strategy = %q, want %q", input.Strategy, tc.wantRawStrategy)
				}
				assertBoolPointer(t, "fuzzy override", input.FuzzyOverride, tc.wantFuzzyOverride)
				assertFloatPointer(t, "threshold override", input.ThresholdOverride, tc.wantThresholdOverride)

				input, err := model.NormalizeSearchInput(input)
				if err != nil {
					return nil, err
				}
				if input.Strategy != tc.wantStrategy || input.Fuzzy != tc.wantFuzzy || input.Threshold != tc.wantThreshold {
					t.Fatalf("normalized search = strategy %q fuzzy %v threshold %v", input.Strategy, input.Fuzzy, input.Threshold)
				}
				return &model.SearchResponse{Query: input.Query, Scope: input.Scope, Fuzzy: input.Fuzzy, Strategy: input.Strategy, Limit: input.Limit, Provenance: model.SearchResponseProvenance(input)}, nil
			}}
			router := Router(Config{}, store)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			var body model.SearchResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode search: %v", err)
			}
			if body.Strategy != tc.wantStrategy || body.Fuzzy != tc.wantFuzzy {
				t.Fatalf("unexpected search response strategy/fuzzy: %#v", body)
			}
		})
	}
}

func TestSearchHandlerRejectsUnknownStrategy(t *testing.T) {
	store := fakeStore{search: func(ctx context.Context, input model.SearchInput) (*model.SearchResponse, error) {
		input, err := model.NormalizeSearchInput(input)
		if err != nil {
			return nil, err
		}
		return &model.SearchResponse{Query: input.Query, Scope: input.Scope, Fuzzy: input.Fuzzy, Strategy: input.Strategy}, nil
	}}
	router := Router(Config{}, store)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=redos&strategy=unknown", nil))
	assertError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
}

func TestSearchHandlerRejectsInvalidOverrides(t *testing.T) {
	tests := []string{
		"/search?q=redos&fuzzy=maybe",
		"/search?q=redos&threshold=close",
	}
	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			store := fakeStore{search: func(ctx context.Context, input model.SearchInput) (*model.SearchResponse, error) {
				t.Fatal("store search should not be called for invalid overrides")
				return nil, nil
			}}
			router := Router(Config{}, store)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
			assertError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
		})
	}
}

func TestSearchHandlerAllowsProviderDefaultStrategyAndLimit(t *testing.T) {
	provider := searchcfg.NewStaticProvider(searchcfg.StrategyLegacy, nil, 7)
	store := fakeStore{search: func(ctx context.Context, input model.SearchInput) (*model.SearchResponse, error) {
		if input.Strategy != "" {
			t.Fatalf("strategy query should be omitted, got %q", input.Strategy)
		}
		if input.Limit != 0 {
			t.Fatalf("limit query should be omitted for provider default, got %d", input.Limit)
		}
		input, err := model.NormalizeSearchInputWithProvider(input, provider)
		if err != nil {
			return nil, err
		}
		return &model.SearchResponse{Query: input.Query, Scope: input.Scope, Fuzzy: input.Fuzzy, Strategy: input.Strategy, Limit: input.Limit, Provenance: model.SearchResponseProvenance(input)}, nil
	}}
	router := Router(Config{}, store)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search?q=redos", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body model.SearchResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if body.Strategy != searchcfg.StrategyLegacy || body.Fuzzy || body.Limit != 7 {
		t.Fatalf("unexpected provider-normalized response: %#v", body)
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

func assertNameKinds(t *testing.T, got, want []model.NameKind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("name kinds = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("name kinds = %#v, want %#v", got, want)
		}
	}
}

func assertBoolPointer(t *testing.T, label string, got, want *bool) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("%s = %#v, want %#v", label, got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("%s = %v, want %v", label, *got, *want)
	}
}

func assertFloatPointer(t *testing.T, label string, got, want *float64) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("%s = %#v, want %#v", label, got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("%s = %v, want %v", label, *got, *want)
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
	createSubmission          func(context.Context, model.CreateSubmissionInput) (*model.Submission, error)
	listSubmissions           func(context.Context, model.ListSubmissionsInput) (*model.ListSubmissionsResponse, error)
	completeSubmission        func(context.Context, uuid.UUID, model.CompleteSubmissionInput) (*model.Submission, error)
	failSubmission            func(context.Context, uuid.UUID, model.FailSubmissionInput) (*model.Submission, error)
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

func (f fakeStore) CreateSubmission(ctx context.Context, input model.CreateSubmissionInput) (*model.Submission, error) {
	if f.createSubmission != nil {
		return f.createSubmission(ctx, input)
	}
	return nil, errUnsupported
}

func (f fakeStore) ListSubmissions(ctx context.Context, input model.ListSubmissionsInput) (*model.ListSubmissionsResponse, error) {
	if f.listSubmissions != nil {
		return f.listSubmissions(ctx, input)
	}
	return nil, errUnsupported
}

func (f fakeStore) CompleteSubmission(ctx context.Context, id uuid.UUID, input model.CompleteSubmissionInput) (*model.Submission, error) {
	if f.completeSubmission != nil {
		return f.completeSubmission(ctx, id, input)
	}
	return nil, errUnsupported
}

func (f fakeStore) FailSubmission(ctx context.Context, id uuid.UUID, input model.FailSubmissionInput) (*model.Submission, error) {
	if f.failSubmission != nil {
		return f.failSubmission(ctx, id, input)
	}
	return nil, errUnsupported
}
