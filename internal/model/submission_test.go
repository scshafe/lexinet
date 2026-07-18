package model

import (
	"errors"
	"testing"
)

func TestNormalizeCreateSubmissionInput(t *testing.T) {
	contextText := "  seen in menu bar capture  "
	input, err := NormalizeCreateSubmissionInput(CreateSubmissionInput{Term: "  TUI  ", Context: &contextText})
	if err != nil {
		t.Fatalf("expected valid create input, got %v", err)
	}
	if input.Term != "TUI" || input.Context == nil || *input.Context != "seen in menu bar capture" {
		t.Fatalf("unexpected normalized input: %#v", input)
	}

	if _, err := NormalizeCreateSubmissionInput(CreateSubmissionInput{Term: "   "}); !errors.Is(err, ErrSubmissionTermRequired) {
		t.Fatalf("expected term required error, got %v", err)
	}
}

func TestNormalizeListSubmissionsInput(t *testing.T) {
	status := SubmissionStatus(" pending ")
	input, err := NormalizeListSubmissionsInput(ListSubmissionsInput{Status: &status, Limit: 200, Offset: -1})
	if err != nil {
		t.Fatalf("expected valid list input, got %v", err)
	}
	if input.Status == nil || *input.Status != SubmissionStatusPending || input.Limit != 100 || input.Offset != 0 {
		t.Fatalf("unexpected normalized list input: %#v", input)
	}

	bad := SubmissionStatus("done")
	if _, err := NormalizeListSubmissionsInput(ListSubmissionsInput{Status: &bad}); !errors.Is(err, ErrInvalidSubmissionStatus) {
		t.Fatalf("expected invalid status error, got %v", err)
	}
}

func TestNormalizeCompleteAndFailSubmissionInput(t *testing.T) {
	note := "  linked by enrichment agent  "
	complete, err := NormalizeCompleteSubmissionInput(CompleteSubmissionInput{Idea: "  Terminal User Interface  ", Note: &note})
	if err != nil {
		t.Fatalf("expected valid complete input, got %v", err)
	}
	if complete.Idea != "Terminal User Interface" || complete.Note == nil || *complete.Note != "linked by enrichment agent" {
		t.Fatalf("unexpected normalized complete input: %#v", complete)
	}

	if _, err := NormalizeCompleteSubmissionInput(CompleteSubmissionInput{Idea: "   "}); !errors.Is(err, ErrSubmissionIdeaRequired) {
		t.Fatalf("expected idea required error, got %v", err)
	}

	emptyNote := "   "
	fail := NormalizeFailSubmissionInput(FailSubmissionInput{Note: &emptyNote})
	if fail.Note != nil {
		t.Fatalf("expected blank note to normalize to nil, got %#v", fail.Note)
	}
}
