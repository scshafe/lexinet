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

type AcronymLinkDirection string

const (
	AcronymLinkDirectionAcronym   AcronymLinkDirection = "acronym"
	AcronymLinkDirectionExpansion AcronymLinkDirection = "expansion"
)

type AcronymLink struct {
	ID              uuid.UUID             `json:"id"`
	AcronymIdeaID   uuid.UUID             `json:"acronym_idea_id"`
	AcronymName     *string               `json:"acronym_name,omitempty"`
	ExpansionIdeaID uuid.UUID             `json:"expansion_idea_id"`
	ExpansionName   *string               `json:"expansion_name,omitempty"`
	Direction       *AcronymLinkDirection `json:"direction,omitempty"`
	Note            *string               `json:"note,omitempty"`
	CreatedAt       time.Time             `json:"created_at"`
	UpdatedAt       time.Time             `json:"updated_at"`
}

type AcronymLinkSummary struct {
	ID              uuid.UUID             `json:"id"`
	AcronymIdeaID   uuid.UUID             `json:"acronym_idea_id"`
	AcronymName     *string               `json:"acronym_name,omitempty"`
	ExpansionIdeaID uuid.UUID             `json:"expansion_idea_id"`
	ExpansionName   *string               `json:"expansion_name,omitempty"`
	Direction       *AcronymLinkDirection `json:"direction,omitempty"`
}

type CreateAcronymLinkInput struct {
	AcronymIdea   string  `json:"acronym_idea"`
	ExpansionIdea string  `json:"expansion_idea"`
	Note          *string `json:"note,omitempty"`
}

type UpsertAcronymLinkInput struct {
	Acronym   string  `json:"acronym"`
	Expansion string  `json:"expansion"`
	Note      *string `json:"note,omitempty"`
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
