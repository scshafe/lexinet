package model

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrSubmissionTermRequired  = errors.New("submission term is required")
	ErrInvalidSubmissionStatus = errors.New("invalid submission status")
	ErrSubmissionIdeaRequired  = errors.New("submission idea is required")
)

type SubmissionStatus string

const (
	SubmissionStatusPending  SubmissionStatus = "pending"
	SubmissionStatusEnriched SubmissionStatus = "enriched"
	SubmissionStatusFailed   SubmissionStatus = "failed"
)

func (s SubmissionStatus) Valid() bool {
	switch s {
	case SubmissionStatusPending, SubmissionStatusEnriched, SubmissionStatusFailed:
		return true
	default:
		return false
	}
}

type Submission struct {
	ID          uuid.UUID        `json:"id"`
	Term        string           `json:"term"`
	Context     *string          `json:"context,omitempty"`
	Status      SubmissionStatus `json:"status"`
	IdeaID      *uuid.UUID       `json:"idea_id,omitempty"`
	Note        *string          `json:"note,omitempty"`
	CompletedAt *time.Time       `json:"completed_at,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

type CreateSubmissionInput struct {
	Term    string  `json:"term"`
	Context *string `json:"context,omitempty"`
}

type ListSubmissionsInput struct {
	Status *SubmissionStatus
	Limit  int
	Offset int
}

type CompleteSubmissionInput struct {
	Idea string  `json:"idea"`
	Note *string `json:"note,omitempty"`
}

type FailSubmissionInput struct {
	Note *string `json:"note,omitempty"`
}

type ListSubmissionsResponse struct {
	Total       int          `json:"total"`
	Limit       int          `json:"limit"`
	Offset      int          `json:"offset"`
	Submissions []Submission `json:"submissions"`
}

func NormalizeCreateSubmissionInput(input CreateSubmissionInput) (CreateSubmissionInput, error) {
	term := strings.TrimSpace(input.Term)
	if term == "" {
		return input, ErrSubmissionTermRequired
	}
	input.Term = term
	input.Context = normalizeOptionalText(input.Context)
	return input, nil
}

func NormalizeListSubmissionsInput(input ListSubmissionsInput) (ListSubmissionsInput, error) {
	if input.Status != nil {
		status := SubmissionStatus(strings.TrimSpace(string(*input.Status)))
		if !status.Valid() {
			return input, ErrInvalidSubmissionStatus
		}
		input.Status = &status
	}
	if input.Limit <= 0 {
		input.Limit = 20
	}
	if input.Limit > 100 {
		input.Limit = 100
	}
	if input.Offset < 0 {
		input.Offset = 0
	}
	return input, nil
}

func NormalizeCompleteSubmissionInput(input CompleteSubmissionInput) (CompleteSubmissionInput, error) {
	input.Idea = strings.TrimSpace(input.Idea)
	if input.Idea == "" {
		return input, ErrSubmissionIdeaRequired
	}
	input.Note = normalizeOptionalText(input.Note)
	return input, nil
}

func NormalizeFailSubmissionInput(input FailSubmissionInput) FailSubmissionInput {
	input.Note = normalizeOptionalText(input.Note)
	return input
}

func normalizeOptionalText(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
