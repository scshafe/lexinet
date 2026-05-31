package storage

import (
	"context"
	"fmt"

	"lexinet/internal/model"
)

func (r *Repository) MergeIdea(ctx context.Context, sourceRef, targetRef string) (*model.Idea, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	sourceID, err := resolveIdeaID(ctx, tx, sourceRef)
	if err != nil {
		return nil, err
	}
	targetID, err := resolveIdeaID(ctx, tx, targetRef)
	if err != nil {
		return nil, err
	}
	if sourceID == targetID {
		return nil, fmt.Errorf("merge idea: source and target are the same idea")
	}

	rows, err := tx.Query(ctx, `
		SELECT id, from_idea_id, to_idea_id, kind, note, created_at, updated_at
		FROM relationships
		WHERE from_idea_id = $1 OR to_idea_id = $1`, sourceID)
	if err != nil {
		return nil, err
	}
	relationships := make([]model.Relationship, 0)
	for rows.Next() {
		relationship, err := scanRelationship(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		relationships = append(relationships, *relationship)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if _, err := tx.Exec(ctx, `DELETE FROM relationships WHERE from_idea_id = $1 OR to_idea_id = $1`, sourceID); err != nil {
		return nil, err
	}
	for _, relationship := range relationships {
		fromID := relationship.FromIdeaID
		toID := relationship.ToIdeaID
		if fromID == sourceID {
			fromID = targetID
		}
		if toID == sourceID {
			toID = targetID
		}
		if fromID == toID {
			continue
		}
		normalized, err := model.NormalizeRelation(fromID, toID, relationship.Kind)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO relationships (from_idea_id, to_idea_id, kind, note)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (from_idea_id, to_idea_id, kind)
			DO UPDATE SET note = coalesce(EXCLUDED.note, relationships.note)`,
			normalized.FromIdeaID, normalized.ToIdeaID, normalized.Kind, relationship.Note); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE definitions
		SET is_primary = false
		WHERE idea_id = $1 AND is_primary AND EXISTS (
			SELECT 1 FROM definitions WHERE idea_id = $2 AND is_primary
		)`, sourceID, targetID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE definitions SET idea_id = $2 WHERE idea_id = $1`, sourceID, targetID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE examples SET idea_id = $2 WHERE idea_id = $1`, sourceID, targetID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE notes SET idea_id = $2 WHERE idea_id = $1`, sourceID, targetID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE names n
		SET kind = 'alias', expansion = NULL, lang_tag = NULL, deprecation_note = NULL
		WHERE n.idea_id = $1
		  AND (n.kind = 'canonical' OR (n.kind = 'localized' AND EXISTS (
			SELECT 1 FROM names target WHERE target.idea_id = $2 AND target.kind = 'localized' AND target.lang_tag = n.lang_tag
		  )))`, sourceID, targetID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE names SET idea_id = $2 WHERE idea_id = $1`, sourceID, targetID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE ideas
		SET status = 'merged', merged_into_id = $2
		WHERE id = $1`, sourceID, targetID); err != nil {
		return nil, err
	}

	idea, err := getIdeaByID(ctx, tx, targetID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return idea, nil
}
