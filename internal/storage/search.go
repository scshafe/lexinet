package storage

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"lexinet/internal/model"
	searchcfg "lexinet/internal/search"
)

func (r *Repository) Search(ctx context.Context, input model.SearchInput) (*model.SearchResponse, error) {
	input, err := model.NormalizeSearchInputWithProvider(input, r.searchStrategies)
	if err != nil {
		return nil, err
	}
	exact := matcherConfig(input.StrategyConfig, searchcfg.MatcherExact)
	prefix := matcherConfig(input.StrategyConfig, searchcfg.MatcherPrefix)
	fullText := matcherConfig(input.StrategyConfig, searchcfg.MatcherFullText)
	fuzzyName := matcherConfig(input.StrategyConfig, searchcfg.MatcherFuzzyName)
	fuzzyBody := matcherConfig(input.StrategyConfig, searchcfg.MatcherFuzzyBody)

	nameKinds := make([]string, 0, len(input.Kinds))
	for _, kind := range input.Kinds {
		nameKinds = append(nameKinds, string(kind))
	}

	rows, err := r.db.Query(ctx, searchSQL,
		input.Query,
		input.Scope == model.SearchScopeAll,
		input.Fuzzy,
		input.Threshold,
		input.Limit,
		input.Offset,
		nameKinds,
		exact.Enabled,
		exact.Weight,
		exact.Priority,
		prefix.Enabled,
		prefix.Weight,
		prefix.Priority,
		fullText.Enabled,
		fullText.Weight,
		fullText.Priority,
		fuzzyName.Enabled,
		fuzzyName.Weight,
		fuzzyName.Priority,
		fuzzyBody.Enabled,
		fuzzyBody.Weight,
		fuzzyBody.Priority,
		fuzzyThreshold(fuzzyName, input.Threshold),
		fuzzyThreshold(fuzzyBody, input.Threshold),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	response := &model.SearchResponse{
		Query:      input.Query,
		Scope:      input.Scope,
		Fuzzy:      input.Fuzzy,
		Limit:      input.Limit,
		Offset:     input.Offset,
		Results:    make([]model.SearchResult, 0),
		Strategy:   input.Strategy,
		Provenance: model.SearchResponseProvenance(input),
	}
	for rows.Next() {
		result, total, err := scanSearchResult(rows)
		if err != nil {
			return nil, err
		}
		result.Provenance = model.SearchResultProvenance(input, *result)
		response.Total = total
		response.Results = append(response.Results, *result)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := r.enrichSearchResults(ctx, response.Results); err != nil {
		return nil, err
	}
	return response, nil
}

func matcherConfig(strategy searchcfg.Strategy, name searchcfg.MatcherName) searchcfg.MatcherConfig {
	matcher, ok := searchcfg.MatcherByName(strategy, name)
	if ok {
		return matcher
	}
	return searchcfg.MatcherConfig{Name: name}
}

func fuzzyThreshold(matcher searchcfg.MatcherConfig, fallback float64) float64 {
	if matcher.Threshold > 0 {
		return matcher.Threshold
	}
	return fallback
}

func scanSearchResult(row pgx.Row) (*model.SearchResult, int, error) {
	var result model.SearchResult
	var total int64
	var slug, canonicalName, summary, matchedName, matchedNameType, headline pgtype.Text
	var matchType string
	var similarity pgtype.Float8
	if err := row.Scan(
		&total,
		&result.Idea.ID,
		&slug,
		&canonicalName,
		&summary,
		&result.Score,
		&result.MatchedVia,
		&result.MatchedField,
		&matchedName,
		&matchedNameType,
		&matchType,
		&similarity,
		&headline,
	); err != nil {
		return nil, 0, err
	}
	result.Idea.Slug = textPtr(slug)
	result.Idea.CanonicalName = textPtr(canonicalName)
	result.Idea.Summary = textPtr(summary)
	result.MatchedName = textPtr(matchedName)
	if matchedNameType.Valid {
		kind := model.NameKind(matchedNameType.String)
		result.MatchedNameType = &kind
	}
	result.MatchType = model.SearchMatchType(matchType)
	if similarity.Valid {
		result.Similarity = &similarity.Float64
	}
	result.Headline = textPtr(headline)
	return &result, int(total), nil
}

const searchSQL = `
WITH params AS (
    SELECT
        $1::text AS q,
        lower($1::text) AS q_lower,
        $2::boolean AS include_all,
        $3::boolean AS fuzzy,
        $4::double precision AS threshold,
        CASE WHEN cardinality($7::text[]) = 0 THEN NULL ELSE $7::text[] END AS kinds,
        $8::boolean AS exact_enabled,
        $9::double precision AS exact_weight,
        $10::integer AS exact_priority,
        $11::boolean AS prefix_enabled,
        $12::double precision AS prefix_weight,
        $13::integer AS prefix_priority,
        $14::boolean AS fts_enabled,
        $15::double precision AS fts_weight,
        $16::integer AS fts_priority,
        $17::boolean AS fuzzy_name_enabled,
        $18::double precision AS fuzzy_name_weight,
        $19::integer AS fuzzy_name_priority,
        $20::boolean AS fuzzy_body_enabled,
        $21::double precision AS fuzzy_body_weight,
        $22::integer AS fuzzy_body_priority,
        $23::double precision AS fuzzy_name_threshold,
        $24::double precision AS fuzzy_body_threshold,
        websearch_to_tsquery('english', $1::text) AS tsq_english,
        websearch_to_tsquery('simple', $1::text) AS tsq_simple,
        websearch_to_tsquery('english', $1::text) || websearch_to_tsquery('simple', $1::text) AS tsq_all
), name_matches AS (
    SELECT
        coalesce(source_idea.merged_into_id, n.idea_id) AS idea_id,
        n.idea_id AS source_idea_id,
        n.text::text AS matched_name,
        n.kind AS matched_name_type,
        CASE WHEN n.kind = 'canonical' THEN 'canonical_name' ELSE n.kind || '_name' END AS matched_field,
        'exact' AS match_type,
        NULL::double precision AS similarity,
        NULL::text AS headline,
        (p.exact_weight - CASE WHEN n.kind = 'deprecated' THEN 0.15 WHEN n.kind = 'canonical' THEN 0.0 ELSE 0.05 END)::double precision AS score,
        p.exact_priority AS priority
    FROM names n
    JOIN ideas source_idea ON source_idea.id = n.idea_id
    CROSS JOIN params p
    WHERE p.exact_enabled
      AND n.text = p.q
      AND (p.kinds IS NULL OR n.kind = ANY(p.kinds))

    UNION ALL

    SELECT
        coalesce(source_idea.merged_into_id, n.idea_id) AS idea_id,
        n.idea_id AS source_idea_id,
        n.text::text AS matched_name,
        n.kind AS matched_name_type,
        CASE WHEN n.kind = 'canonical' THEN 'canonical_name' ELSE n.kind || '_name' END AS matched_field,
        'prefix' AS match_type,
        NULL::double precision AS similarity,
        NULL::text AS headline,
        (p.prefix_weight * char_length(p.q)::double precision / greatest(char_length(n.text::text), 1)
            - CASE WHEN n.kind = 'deprecated' THEN 0.15 WHEN n.kind = 'canonical' THEN 0.0 ELSE 0.05 END)::double precision AS score,
        p.prefix_priority AS priority
    FROM names n
    JOIN ideas source_idea ON source_idea.id = n.idea_id
    CROSS JOIN params p
    WHERE p.prefix_enabled
      AND lower(n.text::text) LIKE p.q_lower || '%'
      AND n.text <> p.q
      AND (p.kinds IS NULL OR n.kind = ANY(p.kinds))

    UNION ALL

    SELECT
        coalesce(source_idea.merged_into_id, n.idea_id) AS idea_id,
        n.idea_id AS source_idea_id,
        n.text::text AS matched_name,
        n.kind AS matched_name_type,
        CASE WHEN n.kind = 'canonical' THEN 'canonical_name' ELSE n.kind || '_name' END AS matched_field,
        'fuzzy' AS match_type,
        similarity(n.text::text, p.q) AS similarity,
        NULL::text AS headline,
        (p.fuzzy_name_weight * similarity(n.text::text, p.q)
            - CASE WHEN n.kind = 'deprecated' THEN 0.15 WHEN n.kind = 'canonical' THEN 0.0 ELSE 0.05 END)::double precision AS score,
        p.fuzzy_name_priority AS priority
    FROM names n
    JOIN ideas source_idea ON source_idea.id = n.idea_id
    CROSS JOIN params p
    WHERE p.fuzzy AND p.fuzzy_name_enabled
      AND similarity(n.text::text, p.q) >= p.fuzzy_name_threshold
      AND n.text::text % p.q
      AND (p.kinds IS NULL OR n.kind = ANY(p.kinds))
), fts_matches AS (
    SELECT
        coalesce(source_idea.merged_into_id, i.id) AS idea_id,
        i.id AS source_idea_id,
        evidence.matched_name,
        evidence.matched_name_type,
        evidence.matched_field,
        'fts' AS match_type,
        NULL::double precision AS similarity,
        evidence.headline,
        (p.fts_weight * ts_rank_cd(i.search_doc, p.tsq_all))::double precision AS score,
        p.fts_priority AS priority
    FROM ideas i
    JOIN ideas source_idea ON source_idea.id = i.id
    CROSS JOIN params p
    CROSS JOIN LATERAL (
        SELECT
            coalesce(name_hit.matched_name, NULL::text) AS matched_name,
            name_hit.matched_name_type,
            coalesce(name_hit.matched_field, definition_hit.matched_field, example_hit.matched_field, note_hit.matched_field, 'text') AS matched_field,
            coalesce(definition_hit.headline, example_hit.headline, note_hit.headline) AS headline
        FROM (SELECT 1) anchor
        LEFT JOIN LATERAL (
            SELECT
                n.text::text AS matched_name,
                n.kind AS matched_name_type,
                CASE WHEN n.kind = 'canonical' THEN 'canonical_name' ELSE n.kind || '_name' END AS matched_field
            FROM names n
            WHERE n.idea_id = i.id
              AND to_tsvector('simple', coalesce(n.display, n.text::text) || ' ' || n.text::text) @@ p.tsq_simple
              AND (p.kinds IS NULL OR n.kind = ANY(p.kinds))
            ORDER BY n.kind = 'canonical' DESC, n.sort_key, n.text::text
            LIMIT 1
        ) name_hit ON true
        LEFT JOIN LATERAL (
            SELECT
                'definition' AS matched_field,
                ts_headline('english', d.body, p.tsq_english, 'MaxFragments=2,MinWords=5,MaxWords=18') AS headline
            FROM definitions d
            WHERE d.idea_id = i.id AND to_tsvector('english', d.body) @@ p.tsq_english
            ORDER BY d.is_primary DESC, d.sort_key, d.created_at
            LIMIT 1
        ) definition_hit ON true
        LEFT JOIN LATERAL (
            SELECT
                'example' AS matched_field,
                ts_headline('english', e.body, p.tsq_english, 'MaxFragments=2,MinWords=5,MaxWords=18') AS headline
            FROM examples e
            WHERE e.idea_id = i.id AND to_tsvector('english', e.body) @@ p.tsq_english
            ORDER BY e.sort_key, e.created_at
            LIMIT 1
        ) example_hit ON true
        LEFT JOIN LATERAL (
            SELECT
                'note' AS matched_field,
                ts_headline('english', nt.body, p.tsq_english, 'MaxFragments=2,MinWords=5,MaxWords=18') AS headline
            FROM notes nt
            WHERE nt.idea_id = i.id AND to_tsvector('english', nt.body) @@ p.tsq_english
            ORDER BY nt.sort_key, nt.created_at
            LIMIT 1
        ) note_hit ON true
    ) evidence
    WHERE p.include_all AND p.fts_enabled
      AND i.search_doc @@ p.tsq_all
), fuzzy_body_matches AS (
    SELECT
        coalesce(source_idea.merged_into_id, d.idea_id) AS idea_id,
        d.idea_id AS source_idea_id,
        NULL::text AS matched_name,
        NULL::text AS matched_name_type,
        'definition' AS matched_field,
        'fuzzy' AS match_type,
        similarity(d.body, p.q) AS similarity,
        ts_headline('english', d.body, p.tsq_english, 'MaxFragments=2,MinWords=5,MaxWords=18') AS headline,
        (p.fuzzy_body_weight * similarity(d.body, p.q))::double precision AS score,
        p.fuzzy_body_priority AS priority
    FROM definitions d
    JOIN ideas source_idea ON source_idea.id = d.idea_id
    CROSS JOIN params p
    WHERE p.include_all AND p.fuzzy AND p.fuzzy_body_enabled AND similarity(d.body, p.q) >= p.fuzzy_body_threshold AND d.body % p.q

    UNION ALL

    SELECT
        coalesce(source_idea.merged_into_id, e.idea_id) AS idea_id,
        e.idea_id AS source_idea_id,
        NULL::text AS matched_name,
        NULL::text AS matched_name_type,
        'example' AS matched_field,
        'fuzzy' AS match_type,
        similarity(e.body, p.q) AS similarity,
        ts_headline('english', e.body, p.tsq_english, 'MaxFragments=2,MinWords=5,MaxWords=18') AS headline,
        (p.fuzzy_body_weight * similarity(e.body, p.q))::double precision AS score,
        p.fuzzy_body_priority AS priority
    FROM examples e
    JOIN ideas source_idea ON source_idea.id = e.idea_id
    CROSS JOIN params p
    WHERE p.include_all AND p.fuzzy AND p.fuzzy_body_enabled AND similarity(e.body, p.q) >= p.fuzzy_body_threshold AND e.body % p.q

    UNION ALL

    SELECT
        coalesce(source_idea.merged_into_id, nt.idea_id) AS idea_id,
        nt.idea_id AS source_idea_id,
        NULL::text AS matched_name,
        NULL::text AS matched_name_type,
        'note' AS matched_field,
        'fuzzy' AS match_type,
        similarity(nt.body, p.q) AS similarity,
        ts_headline('english', nt.body, p.tsq_english, 'MaxFragments=2,MinWords=5,MaxWords=18') AS headline,
        (p.fuzzy_body_weight * similarity(nt.body, p.q))::double precision AS score,
        p.fuzzy_body_priority AS priority
    FROM notes nt
    JOIN ideas source_idea ON source_idea.id = nt.idea_id
    CROSS JOIN params p
    WHERE p.include_all AND p.fuzzy AND p.fuzzy_body_enabled AND similarity(nt.body, p.q) >= p.fuzzy_body_threshold AND nt.body % p.q
), candidates AS (
    SELECT * FROM name_matches
    UNION ALL SELECT * FROM fts_matches
    UNION ALL SELECT * FROM fuzzy_body_matches
), ranked AS (
    SELECT
        candidates.*,
        row_number() OVER (
            PARTITION BY idea_id
            ORDER BY score DESC, priority, matched_field, source_idea_id
        ) AS rownum
    FROM candidates
), best AS (
    SELECT * FROM ranked WHERE rownum = 1
), counted AS (
    SELECT count(*) AS total FROM best
)
SELECT
    counted.total,
    i.id,
    i.slug::text,
    coalesce(canonical.display, canonical.text::text) AS canonical_name,
    i.summary,
    best.score,
    CASE WHEN best.matched_name IS NULL THEN 'text' ELSE 'name' END AS matched_via,
    best.matched_field,
    best.matched_name,
    best.matched_name_type,
    best.match_type,
    best.similarity,
    best.headline
FROM best
CROSS JOIN counted
JOIN ideas i ON i.id = best.idea_id
LEFT JOIN names canonical ON canonical.id = i.canonical_name_id
ORDER BY best.score DESC, best.priority, canonical.kind = 'canonical' DESC, i.updated_at DESC, i.id
LIMIT $5 OFFSET $6`
