package model

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrAcronymIdeaRequired   = errors.New("acronym idea is required")
	ErrExpansionIdeaRequired = errors.New("expansion idea is required")
	ErrSelfAcronymLink       = errors.New("acronym link cannot connect an idea to itself")
)

type AcronymLink struct {
	ID              uuid.UUID `json:"id"`
	AcronymIdeaID   uuid.UUID `json:"acronym_idea_id"`
	ExpansionIdeaID uuid.UUID `json:"expansion_idea_id"`
	Note            *string   `json:"note,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type CreateAcronymLinkInput struct {
	AcronymIdea   string  `json:"acronym_idea"`
	ExpansionIdea string  `json:"expansion_idea"`
	Note          *string `json:"note,omitempty"`
}

type UpdateAcronymLinkInput struct {
	Note **string `json:"note,omitempty"`
}

type AcronymLinkedIdea struct {
	ID            uuid.UUID `json:"id"`
	Slug          *string   `json:"slug,omitempty"`
	CanonicalName *string   `json:"canonical_name,omitempty"`
	Summary       *string   `json:"summary,omitempty"`
}

type AcronymLinkSummary struct {
	ID                     uuid.UUID `json:"id"`
	AcronymIdeaID          uuid.UUID `json:"acronym_idea_id"`
	ExpansionIdeaID        uuid.UUID `json:"expansion_idea_id"`
	AcronymCanonicalName   *string   `json:"acronym_canonical_name,omitempty"`
	ExpansionCanonicalName *string   `json:"expansion_canonical_name,omitempty"`
	Note                   *string   `json:"note,omitempty"`
}

type AcronymLinkView struct {
	ID        uuid.UUID         `json:"id"`
	Acronym   AcronymLinkedIdea `json:"acronym"`
	Expansion AcronymLinkedIdea `json:"expansion"`
	Note      *string           `json:"note,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type IdeaAcronymLinksView struct {
	IdeaID       uuid.UUID         `json:"idea_id"`
	AcronymLinks []AcronymLinkView `json:"acronym_links"`
}

func NormalizeCreateAcronymLinkInput(input CreateAcronymLinkInput) (CreateAcronymLinkInput, error) {
	input.AcronymIdea = strings.TrimSpace(input.AcronymIdea)
	input.ExpansionIdea = strings.TrimSpace(input.ExpansionIdea)
	if input.AcronymIdea == "" {
		return input, ErrAcronymIdeaRequired
	}
	if input.ExpansionIdea == "" {
		return input, ErrExpansionIdeaRequired
	}
	if strings.EqualFold(input.AcronymIdea, input.ExpansionIdea) {
		return input, ErrSelfAcronymLink
	}
	return input, nil
}

func ValidateAcronymLinkIdeas(acronymIdeaID, expansionIdeaID uuid.UUID) error {
	if acronymIdeaID == uuid.Nil {
		return ErrAcronymIdeaRequired
	}
	if expansionIdeaID == uuid.Nil {
		return ErrExpansionIdeaRequired
	}
	if acronymIdeaID == expansionIdeaID {
		return ErrSelfAcronymLink
	}
	return nil
}
