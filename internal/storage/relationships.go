package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"lexinet/internal/model"
)

func (r *Repository) UpsertRelationship(ctx context.Context, fromRef, toRef string, kind model.RelationKind, note *string) (*model.Relationship, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	fromIdeaID, err := resolveIdeaID(ctx, tx, fromRef)
	if err != nil {
		return nil, err
	}
	toIdeaID, err := resolveIdeaID(ctx, tx, toRef)
	if err != nil {
		return nil, err
	}
	normalized, err := model.NormalizeRelation(fromIdeaID, toIdeaID, kind)
	if err != nil {
		return nil, err
	}

	relationship, err := scanRelationship(tx.QueryRow(ctx, `
		INSERT INTO relationships (from_idea_id, to_idea_id, kind, note)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (from_idea_id, to_idea_id, kind)
		DO UPDATE SET note = EXCLUDED.note
		RETURNING id, from_idea_id, to_idea_id, kind, note, created_at, updated_at`,
		normalized.FromIdeaID, normalized.ToIdeaID, normalized.Kind, note))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return relationship, nil
}

func (r *Repository) ListRelationships(ctx context.Context, ideaRef string, kind *model.RelationKind, direction model.RelationDirection) ([]model.Relationship, error) {
	ideaID, err := r.ResolveIdeaID(ctx, ideaRef)
	if err != nil {
		return nil, err
	}
	storedKind, direction, err := model.NormalizeRelationFilter(kind, direction, model.RelationDirectionBoth)
	if err != nil {
		return nil, err
	}
	return queryAdjacentRelationships(ctx, r.db, ideaID, storedKind, direction)
}

func (r *Repository) DeleteRelationship(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM relationships WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrRelationshipNotFound
	}
	return nil
}

func (r *Repository) TraverseRelationships(ctx context.Context, ideaRef string, input model.TraverseRelationshipsInput) (*model.RelationshipTraversal, error) {
	rootIdeaID, err := r.ResolveIdeaID(ctx, ideaRef)
	if err != nil {
		return nil, err
	}
	depth := model.ClampTraversalDepth(input.Depth, input.MaxDepth)
	direction, err := model.NormalizeRelationDirection(input.Direction, model.RelationDirectionOut)
	if err != nil {
		return nil, err
	}
	kinds, err := normalizeStoredRelationKinds(input.Kinds)
	if err != nil {
		return nil, err
	}

	visited := map[uuid.UUID]int{rootIdeaID: 0}
	edgesSeen := map[uuid.UUID]struct{}{}
	queue := []traversalNode{{id: rootIdeaID, depth: 0}}
	relationships := make([]model.Relationship, 0)
	truncated := false

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		adjacent, err := queryAdjacentRelationships(ctx, r.db, current.id, nil, direction)
		if err != nil {
			return nil, err
		}
		for _, relationship := range adjacent {
			if !relationshipKindAllowed(relationship.Kind, kinds) {
				continue
			}
			neighborID, ok := relationshipNeighborID(relationship, current.id)
			if !ok {
				continue
			}
			nextDepth := current.depth + 1
			if nextDepth > depth {
				if _, seen := visited[neighborID]; !seen {
					truncated = true
				}
				continue
			}
			if _, seen := edgesSeen[relationship.ID]; !seen {
				edgesSeen[relationship.ID] = struct{}{}
				relationships = append(relationships, relationship)
			}
			if _, seen := visited[neighborID]; seen {
				continue
			}
			visited[neighborID] = nextDepth
			queue = append(queue, traversalNode{id: neighborID, depth: nextDepth})
		}
	}

	visitedIdeaIDs := make([]uuid.UUID, 0, len(visited))
	for id := range visited {
		visitedIdeaIDs = append(visitedIdeaIDs, id)
	}
	sort.Slice(visitedIdeaIDs, func(i, j int) bool {
		return visitedIdeaIDs[i].String() < visitedIdeaIDs[j].String()
	})

	return &model.RelationshipTraversal{
		RootIdeaID:     rootIdeaID,
		Depth:          depth,
		Direction:      direction,
		Relationships:  relationships,
		VisitedIdeaIDs: visitedIdeaIDs,
		Truncated:      truncated,
	}, nil
}

type traversalNode struct {
	id    uuid.UUID
	depth int
}

func queryAdjacentRelationships(ctx context.Context, q dbRunner, ideaID uuid.UUID, kind *model.RelationKind, direction model.RelationDirection) ([]model.Relationship, error) {
	if direction == "" {
		direction = model.RelationDirectionBoth
	}
	where := "(from_idea_id = $1 OR to_idea_id = $1)"
	switch direction {
	case model.RelationDirectionOut:
		where = "(from_idea_id = $1 OR (kind IN ('synonym_of','related_to') AND to_idea_id = $1))"
	case model.RelationDirectionIn:
		where = "(to_idea_id = $1 OR (kind IN ('synonym_of','related_to') AND from_idea_id = $1))"
	case model.RelationDirectionBoth:
		where = "(from_idea_id = $1 OR to_idea_id = $1)"
	default:
		return nil, fmt.Errorf("invalid relation direction %q", direction)
	}

	args := []any{ideaID}
	query := `
		SELECT id, from_idea_id, to_idea_id, kind, note, created_at, updated_at
		FROM relationships
		WHERE ` + where
	if kind != nil {
		args = append(args, *kind)
		query += " AND kind = $2"
	}
	query += " ORDER BY kind, from_idea_id, to_idea_id"

	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	relationships := make([]model.Relationship, 0)
	for rows.Next() {
		relationship, err := scanRelationship(rows)
		if err != nil {
			return nil, err
		}
		relationship.Label = relationshipLabelForIdea(*relationship, ideaID)
		relationships = append(relationships, *relationship)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return relationships, nil
}

func normalizeStoredRelationKinds(kinds []model.RelationKind) (map[model.RelationKind]struct{}, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	normalized := make(map[model.RelationKind]struct{}, len(kinds))
	for _, kind := range kinds {
		mapping, err := model.ParseRelationKind(kind)
		if err != nil {
			return nil, err
		}
		normalized[mapping.Stored] = struct{}{}
	}
	return normalized, nil
}

func relationshipKindAllowed(kind model.RelationKind, allowed map[model.RelationKind]struct{}) bool {
	if len(allowed) == 0 {
		return true
	}
	_, ok := allowed[kind]
	return ok
}

func relationshipNeighborID(relationship model.Relationship, ideaID uuid.UUID) (uuid.UUID, bool) {
	if relationship.FromIdeaID == ideaID {
		return relationship.ToIdeaID, true
	}
	if relationship.ToIdeaID == ideaID {
		return relationship.FromIdeaID, true
	}
	return uuid.Nil, false
}

func relationshipLabelForIdea(relationship model.Relationship, ideaID uuid.UUID) model.RelationKind {
	if relationship.ToIdeaID == ideaID {
		return model.ReverseRelationKind(relationship.Kind)
	}
	return relationship.Kind
}

func scanRelationship(row pgx.Row) (*model.Relationship, error) {
	var relationship model.Relationship
	var kind string
	var note pgtype.Text
	if err := row.Scan(
		&relationship.ID,
		&relationship.FromIdeaID,
		&relationship.ToIdeaID,
		&kind,
		&note,
		&relationship.CreatedAt,
		&relationship.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRelationshipNotFound
		}
		return nil, err
	}
	relationship.Kind = model.RelationKind(kind)
	relationship.Label = relationship.Kind
	relationship.Note = textPtr(note)
	return &relationship, nil
}
