package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"lexinet/internal/model"
	"lexinet/internal/storage"
)

type Store interface {
	CreateIdea(context.Context, model.CreateIdeaInput) (*model.Idea, error)
	ListIdeas(context.Context, model.IdeaListInput) (*model.IdeaListResponse, error)
	GetIdeaByRef(context.Context, string) (*model.Idea, error)
	UpdateIdea(context.Context, string, model.UpdateIdeaInput) (*model.Idea, error)
	DeleteIdea(context.Context, string) error
	MergeIdea(context.Context, string, string) (*model.Idea, error)
	ResolveIdeaID(context.Context, string) (uuid.UUID, error)

	AddNameByRef(context.Context, string, model.CreateNameInput) (*model.Name, error)
	ListNamesByRef(context.Context, string) ([]model.Name, error)
	UpdateName(context.Context, uuid.UUID, model.UpdateNameInput) (*model.Name, error)
	DeleteName(context.Context, uuid.UUID) error
	PromoteName(context.Context, uuid.UUID, string) (*model.Name, error)

	CreateDefinition(context.Context, string, model.CreateDefinitionInput) (*model.Definition, error)
	ListDefinitions(context.Context, string) ([]model.Definition, error)
	UpdateDefinition(context.Context, uuid.UUID, model.UpdateDefinitionInput) (*model.Definition, error)
	DeleteDefinition(context.Context, uuid.UUID) error
	CreateExample(context.Context, string, model.CreateExampleInput) (*model.Example, error)
	ListExamples(context.Context, string) ([]model.Example, error)
	UpdateExample(context.Context, uuid.UUID, model.UpdateExampleInput) (*model.Example, error)
	DeleteExample(context.Context, uuid.UUID) error
	CreateNote(context.Context, string, model.CreateNoteInput) (*model.Note, error)
	ListNotes(context.Context, string) ([]model.Note, error)
	UpdateNote(context.Context, uuid.UUID, model.UpdateNoteInput) (*model.Note, error)
	DeleteNote(context.Context, uuid.UUID) error

	UpsertRelationship(context.Context, string, string, model.RelationKind, *string) (*model.Relationship, error)
	ListRelationships(context.Context, string, *model.RelationKind, model.RelationDirection) ([]model.Relationship, error)
	DeleteRelationship(context.Context, uuid.UUID) error
	TraverseRelationships(context.Context, string, model.TraverseRelationshipsInput) (*model.RelationshipTraversal, error)

	CreateTopic(context.Context, model.CreateTopicInput) (*model.Topic, error)
	ListTopics(context.Context) ([]model.Topic, error)
	UpdateTopic(context.Context, string, model.UpdateTopicInput) (*model.Topic, error)
	DeleteTopic(context.Context, string) error
	AssignTopic(context.Context, string, string) (*model.Topic, error)
	UnassignTopic(context.Context, string, string) error
	ListTopicsByIdeaRef(context.Context, string) ([]model.TopicSummary, error)
	ListIdeasByTopic(context.Context, string, model.IdeaListInput) (*model.IdeaListResponse, error)

	UpsertAcronymLink(context.Context, model.UpsertAcronymLinkInput) (*model.AcronymLink, error)
	ListAcronymLinksByIdeaRef(context.Context, string) ([]model.AcronymLink, error)
	DeleteAcronymLink(context.Context, uuid.UUID) error

	CreateSubmission(context.Context, model.CreateSubmissionInput) (*model.Submission, error)
	ListSubmissions(context.Context, model.ListSubmissionsInput) (*model.ListSubmissionsResponse, error)
	CompleteSubmission(context.Context, uuid.UUID, model.CompleteSubmissionInput) (*model.Submission, error)
	FailSubmission(context.Context, uuid.UUID, model.FailSubmissionInput) (*model.Submission, error)

	Search(context.Context, model.SearchInput) (*model.SearchResponse, error)
}

type expandedIdea struct {
	*model.Idea
	Names         []model.Name         `json:"names"`
	Definitions   []model.Definition   `json:"definitions,omitempty"`
	Examples      []model.Example      `json:"examples,omitempty"`
	Notes         []model.Note         `json:"notes,omitempty"`
	Relationships []model.Relationship `json:"relationships,omitempty"`
	Topics        []model.TopicSummary `json:"topics"`
	AcronymLinks  []model.AcronymLink  `json:"acronym_links"`
}

func registerAPIRoutes(r chi.Router, store Store) {
	r.Post("/ideas", createIdeaHandler(store))
	r.Get("/ideas", listIdeasHandler(store))
	r.Get("/ideas/{ref}", getIdeaHandler(store))
	r.Patch("/ideas/{ref}", updateIdeaHandler(store))
	r.Delete("/ideas/{ref}", deleteIdeaHandler(store))
	r.Post("/ideas/{ref}/merge", mergeIdeaHandler(store))
	r.Post("/ideas/{ref}/names", addNameHandler(store))
	r.Get("/ideas/{ref}/names", listNamesHandler(store))
	r.Post("/ideas/{ref}/names/promote", promoteNameHandler(store))
	r.Patch("/names/{id}", updateNameHandler(store))
	r.Delete("/names/{id}", deleteNameHandler(store))

	r.Post("/ideas/{ref}/definitions", createDefinitionHandler(store))
	r.Get("/ideas/{ref}/definitions", listDefinitionsHandler(store))
	r.Patch("/definitions/{id}", updateDefinitionHandler(store))
	r.Delete("/definitions/{id}", deleteDefinitionHandler(store))
	r.Post("/ideas/{ref}/examples", createExampleHandler(store))
	r.Get("/ideas/{ref}/examples", listExamplesHandler(store))
	r.Patch("/examples/{id}", updateExampleHandler(store))
	r.Delete("/examples/{id}", deleteExampleHandler(store))
	r.Post("/ideas/{ref}/notes", createNoteHandler(store))
	r.Get("/ideas/{ref}/notes", listNotesHandler(store))
	r.Patch("/notes/{id}", updateNoteHandler(store))
	r.Delete("/notes/{id}", deleteNoteHandler(store))

	r.Post("/relationships", createRelationshipHandler(store))
	r.Get("/relationships", listRelationshipsHandler(store))
	r.Delete("/relationships/{id}", deleteRelationshipHandler(store))
	r.Get("/ideas/{ref}/graph", graphHandler(store))
	r.Post("/topics", createTopicHandler(store))
	r.Get("/topics", listTopicsHandler(store))
	r.Patch("/topics/{ref}", updateTopicHandler(store))
	r.Delete("/topics/{ref}", deleteTopicHandler(store))
	r.Get("/topics/{ref}/ideas", listIdeasByTopicHandler(store))
	r.Post("/ideas/{ref}/topics", assignTopicHandler(store))
	r.Get("/ideas/{ref}/topics", listIdeaTopicsHandler(store))
	r.Delete("/ideas/{ref}/topics/{topic}", unassignTopicHandler(store))
	r.Post("/acronym-links", upsertAcronymLinkHandler(store))
	r.Delete("/acronym-links/{id}", deleteAcronymLinkHandler(store))
	r.Get("/ideas/{ref}/acronym-links", listIdeaAcronymLinksHandler(store))
	r.Post("/submissions", createSubmissionHandler(store))
	r.Get("/submissions", listSubmissionsHandler(store))
	r.Post("/submissions/{id}/complete", completeSubmissionHandler(store))
	r.Post("/submissions/{id}/fail", failSubmissionHandler(store))
	r.Get("/search", searchHandler(store))
}

func createIdeaHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input model.CreateIdeaInput
		if !decodeJSON(w, r, &input) {
			return
		}
		idea, err := store.CreateIdea(r.Context(), input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, idea)
	}
}

func listIdeasHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, ok := queryInt(w, r, "limit", 20)
		if !ok {
			return
		}
		offset, ok := queryInt(w, r, "offset", 0)
		if !ok {
			return
		}
		response, err := store.ListIdeas(r.Context(), model.IdeaListInput{Limit: limit, Offset: offset, Query: r.URL.Query().Get("q"), Slug: r.URL.Query().Get("slug"), Topic: r.URL.Query().Get("topic")})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func getIdeaHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ref := chi.URLParam(r, "ref")
		idea, err := store.GetIdeaByRef(r.Context(), ref)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		response := expandedIdea{Idea: idea}
		response.Names, err = store.ListNamesByRef(r.Context(), ref)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		response.Topics, err = store.ListTopicsByIdeaRef(r.Context(), ref)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		response.AcronymLinks, err = store.ListAcronymLinksByIdeaRef(r.Context(), ref)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		expands := expandSet(r.URL.Query().Get("expand"))
		if expands["definitions"] {
			response.Definitions, err = store.ListDefinitions(r.Context(), ref)
		}
		if err == nil && expands["examples"] {
			response.Examples, err = store.ListExamples(r.Context(), ref)
		}
		if err == nil && expands["notes"] {
			response.Notes, err = store.ListNotes(r.Context(), ref)
		}
		if err == nil && expands["relationships"] {
			response.Relationships, err = store.ListRelationships(r.Context(), ref, nil, model.RelationDirectionBoth)
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func updateIdeaHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input model.UpdateIdeaInput
		if !decodeJSON(w, r, &input) {
			return
		}
		idea, err := store.UpdateIdea(r.Context(), chi.URLParam(r, "ref"), input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, idea)
	}
}

func deleteIdeaHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.DeleteIdea(r.Context(), chi.URLParam(r, "ref")); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func mergeIdeaHandler(store Store) http.HandlerFunc {
	type request struct {
		Into string `json:"into"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var input request
		if !decodeJSON(w, r, &input) {
			return
		}
		if strings.TrimSpace(input.Into) == "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "into is required")
			return
		}
		idea, err := store.MergeIdea(r.Context(), chi.URLParam(r, "ref"), input.Into)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, idea)
	}
}

func addNameHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input model.CreateNameInput
		if !decodeJSON(w, r, &input) {
			return
		}
		name, err := store.AddNameByRef(r.Context(), chi.URLParam(r, "ref"), input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, name)
	}
}

func listNamesHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		names, err := store.ListNamesByRef(r.Context(), chi.URLParam(r, "ref"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"names": names})
	}
}

func promoteNameHandler(store Store) http.HandlerFunc {
	type request struct {
		Text string `json:"text"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var input request
		if !decodeJSON(w, r, &input) {
			return
		}
		ideaID, err := store.ResolveIdeaID(r.Context(), chi.URLParam(r, "ref"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		name, err := store.PromoteName(r.Context(), ideaID, input.Text)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, name)
	}
}

func updateNameHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		var input model.UpdateNameInput
		if !decodeJSON(w, r, &input) {
			return
		}
		name, err := store.UpdateName(r.Context(), id, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, name)
	}
}

func deleteNameHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		if err := store.DeleteName(r.Context(), id); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func createDefinitionHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input model.CreateDefinitionInput
		if !decodeJSON(w, r, &input) {
			return
		}
		definition, err := store.CreateDefinition(r.Context(), chi.URLParam(r, "ref"), input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, definition)
	}
}

func listDefinitionsHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		definitions, err := store.ListDefinitions(r.Context(), chi.URLParam(r, "ref"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"definitions": definitions})
	}
}

func updateDefinitionHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		var input model.UpdateDefinitionInput
		if !decodeJSON(w, r, &input) {
			return
		}
		definition, err := store.UpdateDefinition(r.Context(), id, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, definition)
	}
}

func deleteDefinitionHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		if err := store.DeleteDefinition(r.Context(), id); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func createExampleHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input model.CreateExampleInput
		if !decodeJSON(w, r, &input) {
			return
		}
		example, err := store.CreateExample(r.Context(), chi.URLParam(r, "ref"), input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, example)
	}
}

func listExamplesHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		examples, err := store.ListExamples(r.Context(), chi.URLParam(r, "ref"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"examples": examples})
	}
}

func updateExampleHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		var input model.UpdateExampleInput
		if !decodeJSON(w, r, &input) {
			return
		}
		example, err := store.UpdateExample(r.Context(), id, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, example)
	}
}

func deleteExampleHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		if err := store.DeleteExample(r.Context(), id); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func createNoteHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input model.CreateNoteInput
		if !decodeJSON(w, r, &input) {
			return
		}
		note, err := store.CreateNote(r.Context(), chi.URLParam(r, "ref"), input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, note)
	}
}

func listNotesHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		notes, err := store.ListNotes(r.Context(), chi.URLParam(r, "ref"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
	}
}

func updateNoteHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		var input model.UpdateNoteInput
		if !decodeJSON(w, r, &input) {
			return
		}
		note, err := store.UpdateNote(r.Context(), id, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, note)
	}
}

func deleteNoteHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		if err := store.DeleteNote(r.Context(), id); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func createRelationshipHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input model.CreateRelationshipInput
		if !decodeJSON(w, r, &input) {
			return
		}
		relationship, err := store.UpsertRelationship(r.Context(), input.From, input.To, input.Kind, input.Note)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, relationship)
	}
}

func listRelationshipsHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idea := r.URL.Query().Get("idea")
		if strings.TrimSpace(idea) == "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "idea query parameter is required")
			return
		}
		kind, ok := relationKindQuery(w, r)
		if !ok {
			return
		}
		relationships, err := store.ListRelationships(r.Context(), idea, kind, model.RelationDirection(r.URL.Query().Get("direction")))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"relationships": relationships})
	}
}

func deleteRelationshipHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		if err := store.DeleteRelationship(r.Context(), id); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func graphHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		depth, ok := queryInt(w, r, "depth", model.DefaultTraversalDepth)
		if !ok {
			return
		}
		kinds := make([]model.RelationKind, 0)
		for _, value := range r.URL.Query()["kind"] {
			for _, part := range strings.Split(value, ",") {
				part = strings.TrimSpace(part)
				if part != "" {
					kinds = append(kinds, model.RelationKind(part))
				}
			}
		}
		traversal, err := store.TraverseRelationships(r.Context(), chi.URLParam(r, "ref"), model.TraverseRelationshipsInput{
			Depth:     depth,
			MaxDepth:  model.MaxServiceTraversalDepth,
			Kinds:     kinds,
			Direction: model.RelationDirection(r.URL.Query().Get("direction")),
		})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, traversal)
	}
}

func createTopicHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input model.CreateTopicInput
		if !decodeJSON(w, r, &input) {
			return
		}
		topic, err := store.CreateTopic(r.Context(), input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, topic)
	}
}

func listTopicsHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		topics, err := store.ListTopics(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"topics": topics})
	}
}

func updateTopicHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input model.UpdateTopicInput
		if !decodeJSON(w, r, &input) {
			return
		}
		topic, err := store.UpdateTopic(r.Context(), chi.URLParam(r, "ref"), input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, topic)
	}
}

func deleteTopicHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.DeleteTopic(r.Context(), chi.URLParam(r, "ref")); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func assignTopicHandler(store Store) http.HandlerFunc {
	type request struct {
		Topic string `json:"topic"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var input request
		if !decodeJSON(w, r, &input) {
			return
		}
		if strings.TrimSpace(input.Topic) == "" {
			writeError(w, http.StatusBadRequest, "BAD_REQUEST", "topic is required")
			return
		}
		topic, err := store.AssignTopic(r.Context(), chi.URLParam(r, "ref"), input.Topic)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, topic)
	}
}

func unassignTopicHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.UnassignTopic(r.Context(), chi.URLParam(r, "ref"), chi.URLParam(r, "topic")); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func listIdeaTopicsHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		topics, err := store.ListTopicsByIdeaRef(r.Context(), chi.URLParam(r, "ref"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"topics": topics})
	}
}

func listIdeasByTopicHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, ok := queryInt(w, r, "limit", 20)
		if !ok {
			return
		}
		offset, ok := queryInt(w, r, "offset", 0)
		if !ok {
			return
		}
		response, err := store.ListIdeasByTopic(r.Context(), chi.URLParam(r, "ref"), model.IdeaListInput{Limit: limit, Offset: offset, Query: r.URL.Query().Get("q"), Slug: r.URL.Query().Get("slug")})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func upsertAcronymLinkHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input model.UpsertAcronymLinkInput
		if !decodeJSON(w, r, &input) {
			return
		}
		link, err := store.UpsertAcronymLink(r.Context(), input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, link)
	}
}

func listIdeaAcronymLinksHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		links, err := store.ListAcronymLinksByIdeaRef(r.Context(), chi.URLParam(r, "ref"))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"acronym_links": links})
	}
}

func deleteAcronymLinkHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		if err := store.DeleteAcronymLink(r.Context(), id); err != nil {
			writeStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func createSubmissionHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input model.CreateSubmissionInput
		if !decodeJSON(w, r, &input) {
			return
		}
		input, err := model.NormalizeCreateSubmissionInput(input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		submission, err := store.CreateSubmission(r.Context(), input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, submission)
	}
}

func listSubmissionsHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, ok := queryInt(w, r, "limit", 20)
		if !ok {
			return
		}
		offset, ok := queryInt(w, r, "offset", 0)
		if !ok {
			return
		}
		input := model.ListSubmissionsInput{Limit: limit, Offset: offset}
		if statusValue := strings.TrimSpace(r.URL.Query().Get("status")); statusValue != "" {
			status := model.SubmissionStatus(statusValue)
			input.Status = &status
		}
		input, err := model.NormalizeListSubmissionsInput(input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		response, err := store.ListSubmissions(r.Context(), input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func completeSubmissionHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		var input model.CompleteSubmissionInput
		if !decodeJSON(w, r, &input) {
			return
		}
		input, err := model.NormalizeCompleteSubmissionInput(input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		submission, err := store.CompleteSubmission(r.Context(), id, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, submission)
	}
}

func failSubmissionHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := uuidParam(w, r, "id")
		if !ok {
			return
		}
		var input model.FailSubmissionInput
		if !decodeJSON(w, r, &input) {
			return
		}
		input = model.NormalizeFailSubmissionInput(input)
		submission, err := store.FailSubmission(r.Context(), id, input)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, submission)
	}
}

func searchHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, ok := queryInt(w, r, "limit", 20)
		if !ok {
			return
		}
		offset, ok := queryInt(w, r, "offset", 0)
		if !ok {
			return
		}
		threshold, ok := queryFloat(w, r, "threshold", 0.3)
		if !ok {
			return
		}
		fuzzy, ok := queryBool(w, r, "fuzzy", false)
		if !ok {
			return
		}
		kinds := make([]model.NameKind, 0)
		for _, value := range r.URL.Query()["kind"] {
			for _, part := range strings.Split(value, ",") {
				part = strings.TrimSpace(part)
				if part != "" {
					kinds = append(kinds, model.NameKind(part))
				}
			}
		}
		response, err := store.Search(r.Context(), model.SearchInput{
			Query:     r.URL.Query().Get("q"),
			Scope:     model.SearchScope(r.URL.Query().Get("scope")),
			Fuzzy:     fuzzy,
			Threshold: threshold,
			Kinds:     kinds,
			Limit:     limit,
			Offset:    offset,
		})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("invalid JSON body: %v", err))
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body: multiple JSON values")
		return false
	}
	return true
}

func uuidParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid UUID")
		return uuid.Nil, false
	}
	return id, true
}

func queryInt(w http.ResponseWriter, r *http.Request, key string, fallback int) (int, bool) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback, true
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid "+key)
		return 0, false
	}
	return parsed, true
}

func queryFloat(w http.ResponseWriter, r *http.Request, key string, fallback float64) (float64, bool) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback, true
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid "+key)
		return 0, false
	}
	return parsed, true
}

func queryBool(w http.ResponseWriter, r *http.Request, key string, fallback bool) (bool, bool) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback, true
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid "+key)
		return false, false
	}
	return parsed, true
}

func relationKindQuery(w http.ResponseWriter, r *http.Request) (*model.RelationKind, bool) {
	value := strings.TrimSpace(r.URL.Query().Get("kind"))
	if value == "" {
		return nil, true
	}
	kind := model.RelationKind(value)
	return &kind, true
}

func expandSet(value string) map[string]bool {
	set := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			set[part] = true
		}
	}
	return set
}

func writeStoreError(w http.ResponseWriter, err error) {
	var ambiguous *storage.AmbiguousIdeaRefError
	if errors.As(err, &ambiguous) {
		candidates := make([]string, 0, len(ambiguous.Candidates))
		for _, id := range ambiguous.Candidates {
			candidates = append(candidates, id.String())
		}
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "IDEA_REF_AMBIGUOUS", Message: "idea reference is ambiguous", Candidates: candidates})
		return
	}
	if errors.Is(err, storage.ErrIdeaNotFound) || errors.Is(err, storage.ErrInvalidIdeaRef) {
		writeError(w, http.StatusNotFound, "IDEA_NOT_FOUND", "no idea matches that id, prefix, name, or slug")
		return
	}
	if errors.Is(err, storage.ErrNameNotFound) {
		writeError(w, http.StatusNotFound, "NAME_NOT_FOUND", "name not found")
		return
	}
	if errors.Is(err, storage.ErrDefinitionNotFound) || errors.Is(err, storage.ErrExampleNotFound) || errors.Is(err, storage.ErrNoteNotFound) {
		writeError(w, http.StatusNotFound, "BODY_NOT_FOUND", "body row not found")
		return
	}
	if errors.Is(err, storage.ErrRelationshipNotFound) {
		writeError(w, http.StatusNotFound, "RELATIONSHIP_NOT_FOUND", "relationship not found")
		return
	}
	if errors.Is(err, storage.ErrTopicNotFound) {
		writeError(w, http.StatusNotFound, "TOPIC_NOT_FOUND", "topic not found")
		return
	}
	if errors.Is(err, storage.ErrAcronymLinkNotFound) {
		writeError(w, http.StatusNotFound, "ACRONYM_LINK_NOT_FOUND", "acronym link not found")
		return
	}
	if errors.Is(err, storage.ErrSubmissionNotFound) {
		writeError(w, http.StatusNotFound, "SUBMISSION_NOT_FOUND", "submission not found")
		return
	}
	if errors.Is(err, storage.ErrInvalidSubmissionTransition) {
		writeError(w, http.StatusConflict, "SUBMISSION_INVALID_TRANSITION", "submission is not pending")
		return
	}
	if errors.Is(err, storage.ErrInvalidNameKind) {
		writeError(w, http.StatusBadRequest, "INVALID_NAME_KIND", "invalid name kind")
		return
	}
	if errors.Is(err, model.ErrInvalidSubmissionStatus) {
		writeError(w, http.StatusBadRequest, "INVALID_SUBMISSION_STATUS", "invalid submission status")
		return
	}
	if errors.Is(err, model.ErrInvalidRelationKind) {
		writeError(w, http.StatusBadRequest, "INVALID_RELATIONSHIP_KIND", "invalid relationship kind")
		return
	}
	if errors.Is(err, model.ErrSelfRelationship) {
		writeError(w, http.StatusBadRequest, "RELATIONSHIP_SELF_EDGE", "relationship cannot connect an idea to itself")
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
		switch pgErr.ConstraintName {
		case "ideas_slug_key":
			writeError(w, http.StatusConflict, "SLUG_EXISTS", "slug already exists")
		default:
			writeError(w, http.StatusConflict, "NAME_EXISTS", "name already exists")
		}
		return
	}
	if strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "same idea") {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
}

func writeAPIError(w http.ResponseWriter, status int, apiError APIError) {
	apiError.Code = strings.ToUpper(apiError.Code)
	writeJSON(w, status, ErrorEnvelope{Error: apiError})
}
