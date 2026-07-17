package model

import (
	"errors"
	"testing"
)

func TestNormalizeCreateTopicInputDerivesSlugFromName(t *testing.T) {
	got, err := NormalizeCreateTopicInput(CreateTopicInput{Name: "  Software Architecture  "})
	if err != nil {
		t.Fatalf("expected topic input to normalize, got %v", err)
	}
	if got.Name != "Software Architecture" {
		t.Fatalf("expected trimmed name, got %q", got.Name)
	}
	if got.Slug != "software-architecture" {
		t.Fatalf("expected slug derived from name, got %q", got.Slug)
	}
}

func TestNormalizeTopicSlugCompressesSeparators(t *testing.T) {
	got, err := NormalizeTopicSlug("  Software / Systems_Design  ")
	if err != nil {
		t.Fatalf("expected slug to normalize, got %v", err)
	}
	if got != "software-systems-design" {
		t.Fatalf("expected normalized slug, got %q", got)
	}
}

func TestNormalizeTopicRejectsBlankName(t *testing.T) {
	_, err := NormalizeCreateTopicInput(CreateTopicInput{Name: " \t "})
	if !errors.Is(err, ErrTopicNameRequired) {
		t.Fatalf("expected topic name error, got %v", err)
	}
}

func TestNormalizeTopicRejectsSlugWithoutAsciiWord(t *testing.T) {
	_, err := NormalizeTopicSlug("---")
	if !errors.Is(err, ErrInvalidTopicSlug) {
		t.Fatalf("expected invalid slug error, got %v", err)
	}
}

func TestValidTopicSlug(t *testing.T) {
	for _, slug := range []string{"software", "systems-design", "go-123"} {
		if !ValidTopicSlug(slug) {
			t.Fatalf("expected %q to be valid", slug)
		}
	}
	for _, slug := range []string{"", "Software", "software--design", "software_"} {
		if ValidTopicSlug(slug) {
			t.Fatalf("expected %q to be invalid", slug)
		}
	}
}
