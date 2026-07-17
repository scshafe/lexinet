package model

import (
	"time"

	"github.com/google/uuid"
)

type IdeaStatus string

const (
	IdeaStatusDraft    IdeaStatus = "draft"
	IdeaStatusActive   IdeaStatus = "active"
	IdeaStatusMerged   IdeaStatus = "merged"
	IdeaStatusArchived IdeaStatus = "archived"
)

func ValidIdeaStatuses() []IdeaStatus {
	return []IdeaStatus{
		IdeaStatusDraft,
		IdeaStatusActive,
		IdeaStatusMerged,
		IdeaStatusArchived,
	}
}

func (s IdeaStatus) Valid() bool {
	switch s {
	case IdeaStatusDraft, IdeaStatusActive, IdeaStatusMerged, IdeaStatusArchived:
		return true
	default:
		return false
	}
}

type Idea struct {
	ID              uuid.UUID  `json:"id"`
	Slug            *string    `json:"slug,omitempty"`
	Summary         *string    `json:"summary,omitempty"`
	Domain          *string    `json:"domain,omitempty"`
	Status          IdeaStatus `json:"status"`
	CanonicalNameID *uuid.UUID `json:"canonical_name_id,omitempty"`
	MergedIntoID    *uuid.UUID `json:"merged_into_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type CreateIdeaInput struct {
	Slug        *string                 `json:"slug,omitempty"`
	Summary     *string                 `json:"summary,omitempty"`
	Domain      *string                 `json:"domain,omitempty"`
	Names       []CreateNameInput       `json:"names"`
	Definitions []CreateDefinitionInput `json:"definitions,omitempty"`
	Examples    []CreateExampleInput    `json:"examples,omitempty"`
	Notes       []CreateNoteInput       `json:"notes,omitempty"`
}

type UpdateIdeaInput struct {
	Slug    **string    `json:"slug,omitempty"`
	Summary **string    `json:"summary,omitempty"`
	Domain  **string    `json:"domain,omitempty"`
	Status  *IdeaStatus `json:"status,omitempty"`
}

type IdeaListInput struct {
	Limit  int
	Offset int
	Query  string
	Slug   string
	Topic  string
}

type IdeaSummary struct {
	ID            uuid.UUID            `json:"id"`
	Slug          *string              `json:"slug,omitempty"`
	Summary       *string              `json:"summary,omitempty"`
	Domain        *string              `json:"domain,omitempty"`
	Status        IdeaStatus           `json:"status"`
	CanonicalName *string              `json:"canonical_name,omitempty"`
	Topics        []TopicSummary       `json:"topics,omitempty"`
	AcronymLinks  []AcronymLinkSummary `json:"acronym_links,omitempty"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

type IdeaListResponse struct {
	Total  int           `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
	Ideas  []IdeaSummary `json:"ideas"`
}
