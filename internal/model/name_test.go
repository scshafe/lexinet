package model

import "testing"

func TestNameKindValid(t *testing.T) {
	for _, kind := range ValidNameKinds() {
		if !kind.Valid() {
			t.Fatalf("expected %q to be valid", kind)
		}
	}
	if NameKind("alternative").Valid() {
		t.Fatal("alternative must not be a valid v1 name kind; use alias")
	}
}

func TestIdeaStatusValid(t *testing.T) {
	for _, status := range ValidIdeaStatuses() {
		if !status.Valid() {
			t.Fatalf("expected %q to be valid", status)
		}
	}
	if IdeaStatus("deleted").Valid() {
		t.Fatal("deleted must not be a valid v1 idea status")
	}
}
