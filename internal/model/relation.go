package model

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidRelationKind = errors.New("invalid relation kind")
	ErrSelfRelationship    = errors.New("relationship cannot connect an idea to itself")
)

type RelationKind string

const (
	RelationIsA                RelationKind = "is_a"
	RelationPartOf             RelationKind = "part_of"
	RelationDependsOn          RelationKind = "depends_on"
	RelationUsedInDefinitionOf RelationKind = "used_in_definition_of"
	RelationImplements         RelationKind = "implements"
	RelationSupersedes         RelationKind = "supersedes"
	RelationSynonymOf          RelationKind = "synonym_of"
	RelationRelatedTo          RelationKind = "related_to"

	RelationGeneralizationOf RelationKind = "generalization_of"
	RelationHasPart          RelationKind = "has_part"
	RelationDependedOnBy     RelationKind = "depended_on_by"
	RelationDefinedUsing     RelationKind = "defined_using"
	RelationSupersededBy     RelationKind = "superseded_by"
)

type RelationDirection string

const (
	RelationDirectionOut  RelationDirection = "out"
	RelationDirectionIn   RelationDirection = "in"
	RelationDirectionBoth RelationDirection = "both"
)

const (
	DefaultTraversalDepth    = 1
	MaxServiceTraversalDepth = 5
	MaxCLITraversalDepth     = 6
)

type Relationship struct {
	ID         uuid.UUID    `json:"id"`
	FromIdeaID uuid.UUID    `json:"from_idea_id"`
	ToIdeaID   uuid.UUID    `json:"to_idea_id"`
	Kind       RelationKind `json:"kind"`
	Label      RelationKind `json:"label,omitempty"`
	Note       *string      `json:"note,omitempty"`
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
}

type CreateRelationshipInput struct {
	From string       `json:"from"`
	To   string       `json:"to"`
	Kind RelationKind `json:"kind"`
	Note *string      `json:"note,omitempty"`
}

type TraverseRelationshipsInput struct {
	Depth     int               `json:"depth,omitempty"`
	MaxDepth  int               `json:"max_depth,omitempty"`
	Kinds     []RelationKind    `json:"kinds,omitempty"`
	Direction RelationDirection `json:"direction,omitempty"`
}

type RelationshipTraversal struct {
	RootIdeaID     uuid.UUID         `json:"root_idea_id"`
	Depth          int               `json:"depth"`
	Direction      RelationDirection `json:"direction"`
	Relationships  []Relationship    `json:"relationships"`
	VisitedIdeaIDs []uuid.UUID       `json:"visited_idea_ids"`
	Truncated      bool              `json:"truncated"`
}

type NormalizedRelation struct {
	FromIdeaID uuid.UUID
	ToIdeaID   uuid.UUID
	Kind       RelationKind
}

type RelationKindMapping struct {
	Label   RelationKind
	Stored  RelationKind
	Reverse bool
}

func ValidRelationKinds() []RelationKind {
	return []RelationKind{
		RelationIsA,
		RelationPartOf,
		RelationDependsOn,
		RelationUsedInDefinitionOf,
		RelationImplements,
		RelationSupersedes,
		RelationSynonymOf,
		RelationRelatedTo,
	}
}

func AcceptedRelationLabels() []RelationKind {
	return []RelationKind{
		RelationIsA,
		RelationPartOf,
		RelationDependsOn,
		RelationUsedInDefinitionOf,
		RelationImplements,
		RelationSupersedes,
		RelationSynonymOf,
		RelationRelatedTo,
		RelationGeneralizationOf,
		RelationHasPart,
		RelationDependedOnBy,
		RelationDefinedUsing,
		RelationSupersededBy,
	}
}

func (k RelationKind) Valid() bool {
	switch k {
	case RelationIsA, RelationPartOf, RelationDependsOn, RelationUsedInDefinitionOf, RelationImplements, RelationSupersedes, RelationSynonymOf, RelationRelatedTo:
		return true
	default:
		return false
	}
}

func (k RelationKind) Symmetric() bool {
	switch k {
	case RelationSynonymOf, RelationRelatedTo:
		return true
	default:
		return false
	}
}

func (d RelationDirection) Valid() bool {
	switch d {
	case RelationDirectionOut, RelationDirectionIn, RelationDirectionBoth:
		return true
	default:
		return false
	}
}

func NormalizeRelationDirection(direction RelationDirection, fallback RelationDirection) (RelationDirection, error) {
	if direction == "" {
		direction = fallback
	}
	if !direction.Valid() {
		return "", errors.New("invalid relation direction")
	}
	return direction, nil
}

func ClampTraversalDepth(depth, maxDepth int) int {
	if maxDepth <= 0 {
		maxDepth = MaxServiceTraversalDepth
	}
	if depth <= 0 {
		depth = DefaultTraversalDepth
	}
	if depth > maxDepth {
		return maxDepth
	}
	return depth
}

func ParseRelationKind(label RelationKind) (RelationKindMapping, error) {
	switch normalizedRelationKind(label) {
	case RelationIsA:
		return RelationKindMapping{Label: RelationIsA, Stored: RelationIsA}, nil
	case RelationPartOf:
		return RelationKindMapping{Label: RelationPartOf, Stored: RelationPartOf}, nil
	case RelationDependsOn:
		return RelationKindMapping{Label: RelationDependsOn, Stored: RelationDependsOn}, nil
	case RelationUsedInDefinitionOf:
		return RelationKindMapping{Label: RelationUsedInDefinitionOf, Stored: RelationUsedInDefinitionOf}, nil
	case RelationImplements:
		return RelationKindMapping{Label: RelationImplements, Stored: RelationImplements}, nil
	case RelationSupersedes:
		return RelationKindMapping{Label: RelationSupersedes, Stored: RelationSupersedes}, nil
	case RelationSynonymOf:
		return RelationKindMapping{Label: RelationSynonymOf, Stored: RelationSynonymOf}, nil
	case RelationRelatedTo:
		return RelationKindMapping{Label: RelationRelatedTo, Stored: RelationRelatedTo}, nil
	case RelationGeneralizationOf:
		return RelationKindMapping{Label: RelationGeneralizationOf, Stored: RelationIsA, Reverse: true}, nil
	case RelationHasPart:
		return RelationKindMapping{Label: RelationHasPart, Stored: RelationPartOf, Reverse: true}, nil
	case RelationDependedOnBy:
		return RelationKindMapping{Label: RelationDependedOnBy, Stored: RelationDependsOn, Reverse: true}, nil
	case RelationDefinedUsing:
		return RelationKindMapping{Label: RelationDefinedUsing, Stored: RelationUsedInDefinitionOf, Reverse: true}, nil
	case RelationSupersededBy:
		return RelationKindMapping{Label: RelationSupersededBy, Stored: RelationSupersedes, Reverse: true}, nil
	default:
		return RelationKindMapping{}, ErrInvalidRelationKind
	}
}

func NormalizeRelation(fromIdeaID, toIdeaID uuid.UUID, label RelationKind) (NormalizedRelation, error) {
	mapping, err := ParseRelationKind(label)
	if err != nil {
		return NormalizedRelation{}, err
	}
	if mapping.Reverse {
		fromIdeaID, toIdeaID = toIdeaID, fromIdeaID
	}
	if fromIdeaID == toIdeaID {
		return NormalizedRelation{}, ErrSelfRelationship
	}
	if mapping.Stored.Symmetric() && fromIdeaID.String() > toIdeaID.String() {
		fromIdeaID, toIdeaID = toIdeaID, fromIdeaID
	}
	return NormalizedRelation{FromIdeaID: fromIdeaID, ToIdeaID: toIdeaID, Kind: mapping.Stored}, nil
}

func ReverseRelationKind(kind RelationKind) RelationKind {
	switch kind {
	case RelationIsA:
		return RelationGeneralizationOf
	case RelationPartOf:
		return RelationHasPart
	case RelationDependsOn:
		return RelationDependedOnBy
	case RelationUsedInDefinitionOf:
		return RelationDefinedUsing
	case RelationSupersedes:
		return RelationSupersededBy
	default:
		return kind
	}
}

func ReverseRelationDirection(direction RelationDirection) RelationDirection {
	switch direction {
	case RelationDirectionOut:
		return RelationDirectionIn
	case RelationDirectionIn:
		return RelationDirectionOut
	default:
		return direction
	}
}

func NormalizeRelationFilter(label *RelationKind, direction RelationDirection, fallback RelationDirection) (*RelationKind, RelationDirection, error) {
	direction, err := NormalizeRelationDirection(direction, fallback)
	if err != nil {
		return nil, "", err
	}
	if label == nil || *label == "" {
		return nil, direction, nil
	}
	mapping, err := ParseRelationKind(*label)
	if err != nil {
		return nil, "", err
	}
	if mapping.Reverse {
		direction = ReverseRelationDirection(direction)
	}
	if mapping.Stored.Symmetric() {
		direction = RelationDirectionBoth
	}
	return &mapping.Stored, direction, nil
}

func normalizedRelationKind(kind RelationKind) RelationKind {
	return RelationKind(strings.ToLower(strings.TrimSpace(string(kind))))
}
