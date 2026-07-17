package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"lexinet/internal/model"
)

func (r *Repository) UpsertAcronymLink(ctx context.Context, input model.UpsertAcronymLinkInput) (*model.AcronymLink, error) {
	acronymRef := strings.TrimSpace(input.Acronym)
	expansionRef := strings.TrimSpace(input.Expansion)
	if acronymRef == "" || expansionRef == "" {
		return nil, fmt.Errorf("upsert acronym link: acronym and expansion are required")
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	acronymID, err := resolveIdeaID(ctx, tx, acronymRef)
	if err != nil {
		return nil, err
	}
	expansionID, err := resolveIdeaID(ctx, tx, expansionRef)
	if err != nil {
		return nil, err
	}
	if acronymID == expansionID {
		return nil, fmt.Errorf("upsert acronym link: acronym and expansion cannot be the same idea")
	}
	link, err := scanAcronymLink(tx.QueryRow(ctx, `
		WITH upserted AS (
			INSERT INTO acronym_links (acronym_idea_id, expansion_idea_id, note)
			VALUES ($1, $2, $3)
			ON CONFLICT (acronym_idea_id, expansion_idea_id)
			DO UPDATE SET note = EXCLUDED.note
			RETURNING id
		)
		`+acronymLinkSelectPrefix+`
		JOIN upserted u ON u.id = al.id`, acronymID, expansionID, input.Note))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return link, nil
}

func (r *Repository) DeleteAcronymLink(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM acronym_links WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAcronymLinkNotFound
	}
	return nil
}

func (r *Repository) ListAcronymLinksByIdeaRef(ctx context.Context, ideaRef string) ([]model.AcronymLink, error) {
	ideaID, err := r.ResolveIdeaID(ctx, ideaRef)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx, acronymLinkSelectPrefix+`
		WHERE al.acronym_idea_id = $1 OR al.expansion_idea_id = $1
		ORDER BY direction, coalesce(acn.text::text, ''), coalesce(exn.text::text, ''), al.id`, ideaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	links := make([]model.AcronymLink, 0)
	for rows.Next() {
		link, err := scanAcronymLink(rows)
		if err != nil {
			return nil, err
		}
		direction := model.AcronymLinkDirectionExpansion
		if link.AcronymIdeaID == ideaID {
			direction = model.AcronymLinkDirectionAcronym
		}
		link.Direction = &direction
		links = append(links, *link)
	}
	return links, rows.Err()
}

func loadAcronymLinkSummariesByIdeaIDs(ctx context.Context, q dbRunner, ideaIDs []uuid.UUID) (map[uuid.UUID][]model.AcronymLinkSummary, error) {
	result := make(map[uuid.UUID][]model.AcronymLinkSummary, len(ideaIDs))
	if len(ideaIDs) == 0 {
		return result, nil
	}
	rows, err := q.Query(ctx, `
		WITH target AS (SELECT unnest($1::uuid[]) AS idea_id)
		SELECT target.idea_id, al.id, al.acronym_idea_id, acn.text::text, al.expansion_idea_id, exn.text::text,
		       CASE WHEN al.acronym_idea_id = target.idea_id THEN 'acronym' ELSE 'expansion' END AS direction
		FROM target
		JOIN acronym_links al ON al.acronym_idea_id = target.idea_id OR al.expansion_idea_id = target.idea_id
		LEFT JOIN ideas ai ON ai.id = al.acronym_idea_id
		LEFT JOIN names acn ON acn.id = ai.canonical_name_id
		LEFT JOIN ideas ei ON ei.id = al.expansion_idea_id
		LEFT JOIN names exn ON exn.id = ei.canonical_name_id
		ORDER BY direction, coalesce(acn.text::text, ''), coalesce(exn.text::text, ''), al.id`, ideaIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ideaID uuid.UUID
		var summary model.AcronymLinkSummary
		var acronymName, expansionName, direction pgtype.Text
		if err := rows.Scan(&ideaID, &summary.ID, &summary.AcronymIdeaID, &acronymName, &summary.ExpansionIdeaID, &expansionName, &direction); err != nil {
			return nil, err
		}
		summary.AcronymName = textPtr(acronymName)
		summary.ExpansionName = textPtr(expansionName)
		if direction.Valid {
			d := model.AcronymLinkDirection(direction.String)
			summary.Direction = &d
		}
		result[ideaID] = append(result[ideaID], summary)
	}
	return result, rows.Err()
}

func (r *Repository) enrichIdeaSummaries(ctx context.Context, ideas []model.IdeaSummary) error {
	ids := make([]uuid.UUID, 0, len(ideas))
	for _, idea := range ideas {
		ids = append(ids, idea.ID)
	}
	topics, err := loadTopicSummariesByIdeaIDs(ctx, r.db, ids)
	if err != nil {
		return err
	}
	links, err := loadAcronymLinkSummariesByIdeaIDs(ctx, r.db, ids)
	if err != nil {
		return err
	}
	for i := range ideas {
		ideas[i].Topics = topics[ideas[i].ID]
		ideas[i].AcronymLinks = links[ideas[i].ID]
	}
	return nil
}

func (r *Repository) enrichSearchResults(ctx context.Context, results []model.SearchResult) error {
	ids := make([]uuid.UUID, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.Idea.ID)
	}
	topics, err := loadTopicSummariesByIdeaIDs(ctx, r.db, ids)
	if err != nil {
		return err
	}
	links, err := loadAcronymLinkSummariesByIdeaIDs(ctx, r.db, ids)
	if err != nil {
		return err
	}
	for i := range results {
		results[i].Idea.Topics = topics[results[i].Idea.ID]
		results[i].Idea.AcronymLinks = links[results[i].Idea.ID]
	}
	return nil
}

const acronymLinkSelectPrefix = `
	SELECT al.id, al.acronym_idea_id, acn.text::text, al.expansion_idea_id, exn.text::text, al.note, al.created_at, al.updated_at,
	       CASE WHEN false THEN 'acronym' ELSE NULL END AS direction
	FROM acronym_links al
	LEFT JOIN ideas ai ON ai.id = al.acronym_idea_id
	LEFT JOIN names acn ON acn.id = ai.canonical_name_id
	LEFT JOIN ideas ei ON ei.id = al.expansion_idea_id
	LEFT JOIN names exn ON exn.id = ei.canonical_name_id
`

func scanAcronymLink(row pgx.Row) (*model.AcronymLink, error) {
	var link model.AcronymLink
	var acronymName, expansionName, note, direction pgtype.Text
	if err := row.Scan(&link.ID, &link.AcronymIdeaID, &acronymName, &link.ExpansionIdeaID, &expansionName, &note, &link.CreatedAt, &link.UpdatedAt, &direction); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAcronymLinkNotFound
		}
		return nil, err
	}
	link.AcronymName = textPtr(acronymName)
	link.ExpansionName = textPtr(expansionName)
	link.Note = textPtr(note)
	if direction.Valid {
		d := model.AcronymLinkDirection(direction.String)
		link.Direction = &d
	}
	return &link, nil
}
