package model

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeCreateAcronymLinkInputTrimsRefs(t *testing.T) {
	got, err := NormalizeCreateAcronymLinkInput(CreateAcronymLinkInput{
		AcronymIdea:   "  TUI  ",
		ExpansionIdea: " Terminal User Interface ",
	})
	if err != nil {
		t.Fatalf("expected acronym link input to normalize, got %v", err)
	}
	if got.AcronymIdea != "TUI" || got.ExpansionIdea != "Terminal User Interface" {
		t.Fatalf("expected refs to trim, got %#v", got)
	}
}

func TestNormalizeCreateAcronymLinkInputRejectsSameRef(t *testing.T) {
	_, err := NormalizeCreateAcronymLinkInput(CreateAcronymLinkInput{
		AcronymIdea:   "TUI",
		ExpansionIdea: " tui ",
	})
	if !errors.Is(err, ErrSelfAcronymLink) {
		t.Fatalf("expected self-link error, got %v", err)
	}
}

func TestValidateAcronymLinkIdeasRejectsSelfLink(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	if err := ValidateAcronymLinkIdeas(id, id); !errors.Is(err, ErrSelfAcronymLink) {
		t.Fatalf("expected self-link error, got %v", err)
	}
}

func TestValidateAcronymLinkIdeasRejectsMissingEndpoint(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	if err := ValidateAcronymLinkIdeas(uuid.Nil, id); !errors.Is(err, ErrAcronymIdeaRequired) {
		t.Fatalf("expected missing acronym idea error, got %v", err)
	}
	if err := ValidateAcronymLinkIdeas(id, uuid.Nil); !errors.Is(err, ErrExpansionIdeaRequired) {
		t.Fatalf("expected missing expansion idea error, got %v", err)
	}
}
