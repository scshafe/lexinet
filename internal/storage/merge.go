package storage

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"lexinet/internal/model"
)

func (r *Repository) MergeIdea(ctx context.Context, sourceRef, targetRef string) (result *model.Idea, resultErr error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return nil, mapStudyStorageError("study_merge_transaction_begin", err)
	}
	defer tx.Rollback(ctx)
	defer func() {
		resultErr = mapRetryableStudyTransactionError("study_merge_transaction", resultErr)
	}()

	sourceID, err := resolveIdeaID(ctx, tx, sourceRef)
	if err != nil {
		return nil, err
	}
	targetID, err := resolveIdeaID(ctx, tx, targetRef)
	if err != nil {
		return nil, err
	}
	if sourceID == targetID {
		idea, err := getIdeaByID(ctx, tx, sourceID)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return idea, nil
	}
	orderedPair := orderedIdeaMergeIDs(sourceID, targetID)
	if r.studyMergeBeforeLock != nil {
		r.studyMergeBeforeLock(sourceID, targetID, orderedPair)
	}
	if err := lockAndValidateIdeaMergePair(ctx, tx, sourceID, targetID); err != nil {
		return nil, err
	}
	createsCycle, err := studyCardIdentityReaches(ctx, tx, targetID, sourceID)
	if err != nil {
		return nil, err
	}
	if createsCycle {
		return nil, newStudyStorageError(model.StudyErrorCodeStateStale, "idea merge identity is no longer current", "study_merge_reachability", false, nil)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO study_card_identity_redirects (
			historical_study_card_identity,
			survivor_study_card_identity,
			identity_redirect_reason
		) VALUES ($1, $2, 'idea_merged')`, sourceID, targetID); err != nil {
		return nil, mapStudyStorageError("study_merge_redirect_insert", err)
	}
	if err := invalidateStudyMergeProjections(ctx, tx, sourceID, targetID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE study_session_items
		SET live_source_idea_id = $2
		WHERE live_source_idea_id = $1`, sourceID, targetID); err != nil {
		return nil, mapStudyStorageError("study_merge_item_links", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE study_scheduler_card_projections
		SET live_source_idea_id = $2
		WHERE live_source_idea_id = $1`, sourceID, targetID); err != nil {
		return nil, mapStudyStorageError("study_merge_projection_links", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO idea_topics (idea_id, topic_id)
		SELECT $2, source_topics.topic_id
		FROM idea_topics source_topics
		WHERE source_topics.idea_id = $1
		ON CONFLICT (idea_id, topic_id) DO NOTHING`, sourceID, targetID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM idea_topics WHERE idea_id = $1`, sourceID); err != nil {
		return nil, err
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
	if err := consolidateIdeaMergeAcronymLinks(ctx, tx, sourceID, targetID); err != nil {
		return nil, err
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

func mapRetryableStudyTransactionError(operation string, err error) error {
	if err == nil {
		return nil
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && (postgresError.Code == "40001" || postgresError.Code == "40P01") {
		return mapStudyStorageError(operation, err)
	}
	return err
}

func lockAndValidateIdeaMergePair(ctx context.Context, tx pgx.Tx, sourceID, targetID uuid.UUID) error {
	orderedPair := orderedIdeaMergeIDs(sourceID, targetID)
	ordered := orderedPair[:]
	rows, err := tx.Query(ctx, `
		SELECT id, status, merged_into_id
		FROM ideas
		WHERE id = ANY($1::uuid[])
		ORDER BY id
		FOR UPDATE`, ordered)
	if err != nil {
		return err
	}
	defer rows.Close()
	type state struct {
		status     model.IdeaStatus
		mergedInto *uuid.UUID
	}
	states := make(map[uuid.UUID]state, 2)
	for rows.Next() {
		var id uuid.UUID
		var status string
		var mergedInto *uuid.UUID
		if err := rows.Scan(&id, &status, &mergedInto); err != nil {
			return err
		}
		states[id] = state{status: model.IdeaStatus(status), mergedInto: mergedInto}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(states) != 2 {
		return ErrIdeaNotFound
	}
	for _, id := range []uuid.UUID{sourceID, targetID} {
		current := states[id]
		if current.status == model.IdeaStatusMerged || current.mergedInto != nil {
			return newStudyStorageError(model.StudyErrorCodeServiceUnavailable, "idea merge source or target changed while acquiring locks", "study_merge_lock", true, nil)
		}
	}
	return nil
}

func orderedIdeaMergeIDs(sourceID, targetID uuid.UUID) [2]uuid.UUID {
	ordered := [2]uuid.UUID{sourceID, targetID}
	if ordered[1].String() < ordered[0].String() {
		ordered[0], ordered[1] = ordered[1], ordered[0]
	}
	return ordered
}

func studyCardIdentityReaches(ctx context.Context, tx pgx.Tx, historicalID, soughtID uuid.UUID) (bool, error) {
	var reaches bool
	err := tx.QueryRow(ctx, `
		WITH RECURSIVE identity_path(identity, visited, depth) AS (
			SELECT $1::uuid, ARRAY[$1::uuid], 0
			UNION ALL
			SELECT redirect.survivor_study_card_identity,
				path.visited || redirect.survivor_study_card_identity,
				path.depth + 1
			FROM identity_path path
			JOIN study_card_identity_redirects redirect
			  ON redirect.historical_study_card_identity = path.identity
			WHERE NOT redirect.survivor_study_card_identity = ANY(path.visited)
			  AND path.depth < 1024
		)
		SELECT EXISTS (SELECT 1 FROM identity_path WHERE identity = $2)`, historicalID, soughtID).Scan(&reaches)
	if err != nil {
		return false, mapStudyStorageError("study_merge_reachability", err)
	}
	return reaches, nil
}

func invalidateStudyMergeProjections(ctx context.Context, tx pgx.Tx, sourceID, targetID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		DELETE FROM study_scheduler_card_projections
		WHERE projected_study_card_identity = ANY($1::uuid[])`, []uuid.UUID{sourceID, targetID}); err != nil {
		return mapStudyStorageError("study_merge_projection_invalidate", err)
	}
	return nil
}

func consolidateIdeaMergeAcronymLinks(ctx context.Context, tx pgx.Tx, sourceID, targetID uuid.UUID) error {
	rows, err := tx.Query(ctx, `
		SELECT acronym_idea_id, expansion_idea_id, note
		FROM acronym_links
		WHERE acronym_idea_id = $1 OR expansion_idea_id = $1
		ORDER BY id
		FOR UPDATE`, sourceID)
	if err != nil {
		return err
	}
	type link struct {
		acronymID   uuid.UUID
		expansionID uuid.UUID
		note        *string
	}
	links := make([]link, 0)
	for rows.Next() {
		var current link
		if err := rows.Scan(&current.acronymID, &current.expansionID, &current.note); err != nil {
			rows.Close()
			return err
		}
		links = append(links, current)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if _, err := tx.Exec(ctx, `
		DELETE FROM acronym_links
		WHERE acronym_idea_id = $1 OR expansion_idea_id = $1`, sourceID); err != nil {
		return err
	}
	for _, current := range links {
		if current.acronymID == sourceID {
			current.acronymID = targetID
		}
		if current.expansionID == sourceID {
			current.expansionID = targetID
		}
		if current.acronymID == current.expansionID {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO acronym_links (acronym_idea_id, expansion_idea_id, note)
			VALUES ($1, $2, $3)
			ON CONFLICT (acronym_idea_id, expansion_idea_id)
			DO UPDATE SET note = coalesce(acronym_links.note, EXCLUDED.note)`,
			current.acronymID, current.expansionID, current.note); err != nil {
			return err
		}
	}
	return nil
}
