package model

import (
	"time"

	"github.com/google/uuid"
)

type Definition struct {
	ID        uuid.UUID `json:"id"`
	IdeaID    uuid.UUID `json:"idea_id"`
	Body      string    `json:"body"`
	Register  *string   `json:"register,omitempty"`
	Source    *string   `json:"source,omitempty"`
	IsPrimary bool      `json:"is_primary"`
	SortKey   float64   `json:"sort_key"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateDefinitionInput struct {
	Body      string  `json:"body"`
	Register  *string `json:"register,omitempty"`
	Source    *string `json:"source,omitempty"`
	IsPrimary bool    `json:"is_primary,omitempty"`
	SortKey   float64 `json:"sort_key,omitempty"`
}

type UpdateDefinitionInput struct {
	Body      *string  `json:"body,omitempty"`
	Register  **string `json:"register,omitempty"`
	Source    **string `json:"source,omitempty"`
	IsPrimary *bool    `json:"is_primary,omitempty"`
	SortKey   *float64 `json:"sort_key,omitempty"`
}

type Example struct {
	ID        uuid.UUID `json:"id"`
	IdeaID    uuid.UUID `json:"idea_id"`
	Body      string    `json:"body"`
	Lang      *string   `json:"lang,omitempty"`
	Caption   *string   `json:"caption,omitempty"`
	SortKey   float64   `json:"sort_key"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateExampleInput struct {
	Body    string  `json:"body"`
	Lang    *string `json:"lang,omitempty"`
	Caption *string `json:"caption,omitempty"`
	SortKey float64 `json:"sort_key,omitempty"`
}

type UpdateExampleInput struct {
	Body    *string  `json:"body,omitempty"`
	Lang    **string `json:"lang,omitempty"`
	Caption **string `json:"caption,omitempty"`
	SortKey *float64 `json:"sort_key,omitempty"`
}

type Note struct {
	ID        uuid.UUID `json:"id"`
	IdeaID    uuid.UUID `json:"idea_id"`
	Body      string    `json:"body"`
	Kind      *string   `json:"kind,omitempty"`
	SortKey   float64   `json:"sort_key"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateNoteInput struct {
	Body    string  `json:"body"`
	Kind    *string `json:"kind,omitempty"`
	SortKey float64 `json:"sort_key,omitempty"`
}

type UpdateNoteInput struct {
	Body    *string  `json:"body,omitempty"`
	Kind    **string `json:"kind,omitempty"`
	SortKey *float64 `json:"sort_key,omitempty"`
}
