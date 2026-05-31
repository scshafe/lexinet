package model

import (
	"time"

	"github.com/google/uuid"
)

type NameKind string

const (
	NameKindCanonical  NameKind = "canonical"
	NameKindAcronym    NameKind = "acronym"
	NameKindAlias      NameKind = "alias"
	NameKindDeprecated NameKind = "deprecated"
	NameKindLocalized  NameKind = "localized"
)

func ValidNameKinds() []NameKind {
	return []NameKind{
		NameKindCanonical,
		NameKindAcronym,
		NameKindAlias,
		NameKindDeprecated,
		NameKindLocalized,
	}
}

func (k NameKind) Valid() bool {
	switch k {
	case NameKindCanonical, NameKindAcronym, NameKindAlias, NameKindDeprecated, NameKindLocalized:
		return true
	default:
		return false
	}
}

type Name struct {
	ID              uuid.UUID `json:"id"`
	IdeaID          uuid.UUID `json:"idea_id"`
	Text            string    `json:"text"`
	Display         *string   `json:"display,omitempty"`
	Kind            NameKind  `json:"kind"`
	Expansion       *string   `json:"expansion,omitempty"`
	LangTag         *string   `json:"lang_tag,omitempty"`
	DeprecationNote *string   `json:"deprecation_note,omitempty"`
	SortKey         float64   `json:"sort_key"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type CreateNameInput struct {
	Text            string   `json:"text"`
	Display         *string  `json:"display,omitempty"`
	Kind            NameKind `json:"kind"`
	Expansion       *string  `json:"expansion,omitempty"`
	LangTag         *string  `json:"lang_tag,omitempty"`
	DeprecationNote *string  `json:"deprecation_note,omitempty"`
	SortKey         float64  `json:"sort_key,omitempty"`
}

type UpdateNameInput struct {
	Text            *string   `json:"text,omitempty"`
	Display         **string  `json:"display,omitempty"`
	Kind            *NameKind `json:"kind,omitempty"`
	Expansion       **string  `json:"expansion,omitempty"`
	LangTag         **string  `json:"lang_tag,omitempty"`
	DeprecationNote **string  `json:"deprecation_note,omitempty"`
	SortKey         *float64  `json:"sort_key,omitempty"`
}
