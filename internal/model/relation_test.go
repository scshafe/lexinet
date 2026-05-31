package model

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeRelationReverseLabelSwapsEndpoints(t *testing.T) {
	whole := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	part := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	got, err := NormalizeRelation(whole, part, RelationHasPart)
	if err != nil {
		t.Fatalf("expected reverse label to normalize, got %v", err)
	}
	if got.Kind != RelationPartOf || got.FromIdeaID != part || got.ToIdeaID != whole {
		t.Fatalf("expected part --part_of--> whole, got %#v", got)
	}
}

func TestNormalizeRelationSymmetricCanonicalOrder(t *testing.T) {
	high := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	low := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	got, err := NormalizeRelation(high, low, RelationRelatedTo)
	if err != nil {
		t.Fatalf("expected symmetric relation to normalize, got %v", err)
	}
	if got.FromIdeaID != low || got.ToIdeaID != high || got.Kind != RelationRelatedTo {
		t.Fatalf("expected ascending UUID order, got %#v", got)
	}
}

func TestNormalizeRelationRejectsSelfEdge(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	_, err := NormalizeRelation(id, id, RelationIsA)
	if !errors.Is(err, ErrSelfRelationship) {
		t.Fatalf("expected self relationship error, got %v", err)
	}
}

func TestNormalizeRelationFilterSwapsReverseDirection(t *testing.T) {
	kind := RelationGeneralizationOf
	stored, direction, err := NormalizeRelationFilter(&kind, RelationDirectionOut, RelationDirectionBoth)
	if err != nil {
		t.Fatalf("expected filter to normalize, got %v", err)
	}
	if stored == nil || *stored != RelationIsA || direction != RelationDirectionIn {
		t.Fatalf("expected is_a incoming filter, got kind=%v direction=%q", stored, direction)
	}
}

func TestClampTraversalDepth(t *testing.T) {
	if got := ClampTraversalDepth(0, MaxServiceTraversalDepth); got != DefaultTraversalDepth {
		t.Fatalf("expected default depth, got %d", got)
	}
	if got := ClampTraversalDepth(99, MaxServiceTraversalDepth); got != MaxServiceTraversalDepth {
		t.Fatalf("expected service max depth, got %d", got)
	}
}
