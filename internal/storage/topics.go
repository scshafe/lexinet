package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"lexinet/internal/model"
)

func (r *Repository) CreateTopic(ctx context.Context, input model.CreateTopicInput) (*model.Topic, error) {
	slug := strings.TrimSpace(input.Slug)
	name := strings.TrimSpace(input.Name)
	if slug == "" || name == "" {
		return nil, fmt.Errorf("create topic: slug and name are required")
	}
	return scanTopic(r.db.QueryRow(ctx, `
		INSERT INTO topics (slug, name, description)
		VALUES ($1, $2, $3)
		RETURNING id, slug::text, name::text, description, created_at, updated_at`, slug, name, input.Description))
}

func (r *Repository) ListTopics(ctx context.Context) ([]model.Topic, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, slug::text, name::text, description, created_at, updated_at
		FROM topics
		ORDER BY name, slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	topics := make([]model.Topic, 0)
	for rows.Next() {
		topic, err := scanTopic(rows)
		if err != nil {
			return nil, err
		}
		topics = append(topics, *topic)
	}
	return topics, rows.Err()
}

func (r *Repository) UpdateTopic(ctx context.Context, ref string, input model.UpdateTopicInput) (*model.Topic, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	topicID, err := resolveTopicID(ctx, tx, ref)
	if err != nil {
		return nil, err
	}
	current, err := getTopicByID(ctx, tx, topicID)
	if err != nil {
		return nil, err
	}
	slug := current.Slug
	if input.Slug != nil {
		slug = strings.TrimSpace(*input.Slug)
		if slug == "" {
			return nil, fmt.Errorf("update topic: slug is required")
		}
	}
	name := current.Name
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
		if name == "" {
			return nil, fmt.Errorf("update topic: name is required")
		}
	}
	description := current.Description
	if input.Description != nil {
		description = *input.Description
	}
	topic, err := scanTopic(tx.QueryRow(ctx, `
		UPDATE topics
		SET slug = $2, name = $3, description = $4
		WHERE id = $1
		RETURNING id, slug::text, name::text, description, created_at, updated_at`, topicID, slug, name, description))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return topic, nil
}

func (r *Repository) DeleteTopic(ctx context.Context, ref string) error {
	for attempt := 0; attempt < currentIdeaMutationAttempts; attempt++ {
		err := r.deleteTopicOnce(ctx, ref)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errCurrentIdeaChanged) && !isRetryableIdeaTransactionError(err) {
			return err
		}
	}
	return newStudyStorageError(model.StudyErrorCodeServiceUnavailable, "topic deletion could not acquire stable eligibility locks", "topic_delete", true, errCurrentIdeaChanged)
}

func (r *Repository) deleteTopicOnce(ctx context.Context, ref string) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	topicID, err := resolveTopicID(ctx, tx, ref)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT idea_id FROM idea_topics WHERE topic_id = $1 ORDER BY idea_id`, topicID)
	if err != nil {
		return err
	}
	ideaIDs, err := collectUUIDRows(rows)
	rows.Close()
	if err != nil {
		return err
	}
	if r.ideaMutationAfterResolve != nil && len(ideaIDs) > 0 {
		r.ideaMutationAfterResolve("topic_delete", append([]uuid.UUID(nil), ideaIDs...))
	}
	if len(ideaIDs) > 0 {
		if err := lockAndValidateCurrentIdeas(ctx, tx, ideaIDs); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `DELETE FROM topics WHERE id = $1`, topicID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTopicNotFound
	}
	return tx.Commit(ctx)
}

func (r *Repository) AssignTopic(ctx context.Context, ideaRef, topicRef string) (*model.Topic, error) {
	var topic *model.Topic
	err := r.withLockedCurrentIdeas(ctx, "topic_assign", []currentIdeaTarget{currentIdeaRef(ideaRef)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		topicID, err := resolveTopicID(ctx, tx, topicRef)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO idea_topics (idea_id, topic_id)
			VALUES ($1, $2)
			ON CONFLICT (idea_id, topic_id) DO NOTHING`, ids[0], topicID); err != nil {
			return err
		}
		topic, err = getTopicByID(ctx, tx, topicID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return topic, nil
}

func (r *Repository) UnassignTopic(ctx context.Context, ideaRef, topicRef string) error {
	return r.withLockedCurrentIdeas(ctx, "topic_unassign", []currentIdeaTarget{currentIdeaRef(ideaRef)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		topicID, err := resolveTopicID(ctx, tx, topicRef)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM idea_topics WHERE idea_id = $1 AND topic_id = $2`, ids[0], topicID)
		return err
	})
}

func (r *Repository) ListTopicsByIdeaRef(ctx context.Context, ideaRef string) ([]model.TopicSummary, error) {
	ideaID, err := r.ResolveIdeaID(ctx, ideaRef)
	if err != nil {
		return nil, err
	}
	byIdea, err := loadTopicSummariesByIdeaIDs(ctx, r.db, []uuid.UUID{ideaID})
	if err != nil {
		return nil, err
	}
	return byIdea[ideaID], nil
}

func (r *Repository) ListIdeasByTopic(ctx context.Context, topicRef string, input model.IdeaListInput) (*model.IdeaListResponse, error) {
	input.Topic = topicRef
	return r.ListIdeas(ctx, input)
}

func getTopicByID(ctx context.Context, q dbRunner, id uuid.UUID) (*model.Topic, error) {
	topic, err := scanTopic(q.QueryRow(ctx, `
		SELECT id, slug::text, name::text, description, created_at, updated_at
		FROM topics
		WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTopicNotFound
	}
	return topic, err
}

func resolveTopicID(ctx context.Context, q dbRunner, ref string) (uuid.UUID, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return uuid.Nil, fmt.Errorf("%w: empty topic ref", ErrTopicNotFound)
	}
	candidates := make([]uuid.UUID, 0, 2)
	if looksLikeUUIDPrefix(ref) {
		rows, err := q.Query(ctx, `
			SELECT id
			FROM topics
			WHERE left(lower(id::text), length($1)) = lower($1)
			ORDER BY id
			LIMIT 2`, ref)
		if err != nil {
			return uuid.Nil, err
		}
		ids, err := collectUUIDRows(rows)
		rows.Close()
		if err != nil {
			return uuid.Nil, err
		}
		candidates = append(candidates, ids...)
	}
	rows, err := q.Query(ctx, `
		SELECT id
		FROM topics
		WHERE slug = $1 OR name = $1
		ORDER BY id
		LIMIT 2`, ref)
	if err != nil {
		return uuid.Nil, err
	}
	ids, err := collectUUIDRows(rows)
	rows.Close()
	if err != nil {
		return uuid.Nil, err
	}
	candidates = append(candidates, ids...)
	seen := make(map[uuid.UUID]struct{}, len(candidates))
	unique := make([]uuid.UUID, 0, len(candidates))
	for _, id := range candidates {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i].String() < unique[j].String() })
	switch len(unique) {
	case 0:
		return uuid.Nil, ErrTopicNotFound
	case 1:
		return unique[0], nil
	default:
		return uuid.Nil, fmt.Errorf("%w: %q is ambiguous", ErrTopicNotFound, ref)
	}
}

func loadTopicSummariesByIdeaIDs(ctx context.Context, q dbRunner, ideaIDs []uuid.UUID) (map[uuid.UUID][]model.TopicSummary, error) {
	result := make(map[uuid.UUID][]model.TopicSummary, len(ideaIDs))
	if len(ideaIDs) == 0 {
		return result, nil
	}
	rows, err := q.Query(ctx, `
		SELECT it.idea_id, t.id, t.slug::text, t.name::text
		FROM idea_topics it
		JOIN topics t ON t.id = it.topic_id
		WHERE it.idea_id = ANY($1)
		ORDER BY t.name, t.slug`, ideaIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ideaID uuid.UUID
		var topic model.TopicSummary
		if err := rows.Scan(&ideaID, &topic.ID, &topic.Slug, &topic.Name); err != nil {
			return nil, err
		}
		result[ideaID] = append(result[ideaID], topic)
	}
	return result, rows.Err()
}

func scanTopic(row pgx.Row) (*model.Topic, error) {
	var topic model.Topic
	var description pgtype.Text
	if err := row.Scan(&topic.ID, &topic.Slug, &topic.Name, &description, &topic.CreatedAt, &topic.UpdatedAt); err != nil {
		return nil, err
	}
	topic.Description = textPtr(description)
	return &topic, nil
}
