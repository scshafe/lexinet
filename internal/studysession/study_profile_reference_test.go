package studysession

import (
	"strings"
	"testing"
)

func TestValidStudyProfileRefAcceptsExactGrammarBoundaries(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"a",
		strings.Repeat("a", 128),
		"Learner-01.primary:active_profile",
	} {
		if !ValidStudyProfileRef(value) {
			t.Fatalf("valid profile ref rejected at length %d", len(value))
		}
	}
}

func TestValidStudyProfileRefRejectsOutsideGrammarBoundaries(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"",
		strings.Repeat("a", 129),
		".leading-punctuation",
		"learner profile",
		"léarner",
	} {
		if ValidStudyProfileRef(value) {
			t.Fatalf("invalid profile ref accepted at length %d", len(value))
		}
	}
}
