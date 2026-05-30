package storage

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

func TestUniqueIdeaCandidateDedupeAndAmbiguity(t *testing.T) {
	id1 := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	id2 := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	got, err := uniqueIdeaCandidate("same-ref", []uuid.UUID{id1, id1})
	if err != nil {
		t.Fatalf("expected duplicate candidate to resolve, got %v", err)
	}
	if got != id1 {
		t.Fatalf("expected %s, got %s", id1, got)
	}

	_, err = uniqueIdeaCandidate("ambiguous-ref", []uuid.UUID{id2, id1})
	var ambiguous *AmbiguousIdeaRefError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("expected AmbiguousIdeaRefError, got %T %v", err, err)
	}
	if !errors.Is(err, ErrIdeaRefAmbiguous) {
		t.Fatalf("expected ErrIdeaRefAmbiguous unwrap, got %v", err)
	}
	if len(ambiguous.Candidates) != 2 || ambiguous.Candidates[0] != id1 || ambiguous.Candidates[1] != id2 {
		t.Fatalf("expected sorted candidates [%s %s], got %v", id1, id2, ambiguous.Candidates)
	}
}

func TestUniqueIdeaCandidateNotFound(t *testing.T) {
	_, err := uniqueIdeaCandidate("missing", nil)
	if !errors.Is(err, ErrIdeaNotFound) {
		t.Fatalf("expected ErrIdeaNotFound, got %v", err)
	}
}

func TestLooksLikeUUIDPrefix(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{"11111111-1111", true},
		{"ABCDEF", true},
		{"grpc", false},
		{"bad--dash", false},
		{"11111111-1111-1111-1111-111111111111-extra", false},
	}
	for _, tc := range cases {
		if got := looksLikeUUIDPrefix(tc.ref); got != tc.want {
			t.Fatalf("looksLikeUUIDPrefix(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

func TestChooseCanonicalName(t *testing.T) {
	names := []model.CreateNameInput{
		{Text: "REST", Kind: model.NameKindAcronym},
		{Text: "Representational State Transfer", Kind: model.NameKindCanonical},
	}
	if got := chooseCanonicalName(names); got != 1 {
		t.Fatalf("expected explicit canonical at index 1, got %d", got)
	}
	if got := chooseCanonicalName([]model.CreateNameInput{{Text: "first"}, {Text: "second"}}); got != 0 {
		t.Fatalf("expected fallback canonical at index 0, got %d", got)
	}
}
