package storage

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/model"
	searchcfg "lexinet/internal/search"
)

var (
	ErrIdeaNotFound                = errors.New("idea not found")
	ErrIdeaRefAmbiguous            = errors.New("idea reference ambiguous")
	ErrInvalidIdeaRef              = errors.New("invalid idea reference")
	ErrInvalidNameKind             = errors.New("invalid name kind")
	ErrNameNotFound                = errors.New("name not found")
	ErrCanonicalName               = errors.New("canonical name must be promoted or replaced first")
	ErrDefinitionNotFound          = errors.New("definition not found")
	ErrExampleNotFound             = errors.New("example not found")
	ErrNoteNotFound                = errors.New("note not found")
	ErrRelationshipNotFound        = errors.New("relationship not found")
	ErrTopicNotFound               = errors.New("topic not found")
	ErrAcronymLinkNotFound         = errors.New("acronym link not found")
	ErrSubmissionNotFound          = errors.New("submission not found")
	ErrInvalidSubmissionTransition = errors.New("invalid submission status transition")
)

type AmbiguousIdeaRefError struct {
	Ref        string
	Candidates []uuid.UUID
}

func (e *AmbiguousIdeaRefError) Error() string {
	return fmt.Sprintf("%s: %q matched %d ideas", ErrIdeaRefAmbiguous, e.Ref, len(e.Candidates))
}

func (e *AmbiguousIdeaRefError) Unwrap() error {
	return ErrIdeaRefAmbiguous
}

type dbRunner interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type dbPool interface {
	dbRunner
	Begin(context.Context) (pgx.Tx, error)
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

type Repository struct {
	db                       dbPool
	searchStrategies         searchcfg.StrategyProvider
	studyMergeBeforeLock     func(uuid.UUID, uuid.UUID, [2]uuid.UUID)
	ideaMutationAfterResolve func(string, []uuid.UUID)
}

type RepositoryOption func(*Repository)

func NewRepository(pool *pgxpool.Pool, opts ...RepositoryOption) *Repository {
	repo := &Repository{db: pool, searchStrategies: searchcfg.BuiltinProvider()}
	for _, opt := range opts {
		opt(repo)
	}
	return repo
}

func WithSearchStrategyProvider(provider searchcfg.StrategyProvider) RepositoryOption {
	return func(repo *Repository) {
		if provider != nil {
			repo.searchStrategies = provider
		}
	}
}

func (r *Repository) CreateIdea(ctx context.Context, input model.CreateIdeaInput) (*model.Idea, error) {
	if len(input.Names) == 0 {
		return nil, fmt.Errorf("create idea: at least one name is required")
	}
	names := append([]model.CreateNameInput(nil), input.Names...)
	canonicalIdx := chooseCanonicalName(names)
	for i := range names {
		if i == canonicalIdx {
			names[i].Kind = model.NameKindCanonical
			names[i].Expansion = nil
			names[i].LangTag = nil
			names[i].DeprecationNote = nil
		} else if names[i].Kind == "" {
			names[i].Kind = model.NameKindAlias
		}
		if !names[i].Kind.Valid() {
			return nil, fmt.Errorf("%w: %q", ErrInvalidNameKind, names[i].Kind)
		}
		normalizeNameInput(&names[i])
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	idea, err := scanIdea(tx.QueryRow(ctx, `
		INSERT INTO ideas (slug, summary, domain)
		VALUES ($1, $2, $3)
		RETURNING id, slug, summary, domain, status, canonical_name_id, merged_into_id, created_at, updated_at`,
		input.Slug, input.Summary, input.Domain))
	if err != nil {
		return nil, err
	}

	var canonicalID uuid.UUID
	for _, nameInput := range names {
		name, err := insertName(ctx, tx, idea.ID, nameInput)
		if err != nil {
			return nil, err
		}
		if name.Kind == model.NameKindCanonical {
			canonicalID = name.ID
		}
	}
	if canonicalID == uuid.Nil {
		return nil, fmt.Errorf("create idea: canonical name was not created")
	}

	idea, err = scanIdea(tx.QueryRow(ctx, `
		UPDATE ideas
		SET canonical_name_id = $2
		WHERE id = $1
		RETURNING id, slug, summary, domain, status, canonical_name_id, merged_into_id, created_at, updated_at`,
		idea.ID, canonicalID))
	if err != nil {
		return nil, err
	}
	for _, input := range input.Definitions {
		if strings.TrimSpace(input.Body) == "" {
			return nil, fmt.Errorf("create definition: body is required")
		}
		if _, err := insertDefinition(ctx, tx, idea.ID, input); err != nil {
			return nil, err
		}
	}
	for _, input := range input.Examples {
		if strings.TrimSpace(input.Body) == "" {
			return nil, fmt.Errorf("create example: body is required")
		}
		if _, err := insertExample(ctx, tx, idea.ID, input); err != nil {
			return nil, err
		}
	}
	for _, input := range input.Notes {
		if strings.TrimSpace(input.Body) == "" {
			return nil, fmt.Errorf("create note: body is required")
		}
		if _, err := insertNote(ctx, tx, idea.ID, input); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return idea, nil
}

func (r *Repository) ListIdeas(ctx context.Context, input model.IdeaListInput) (*model.IdeaListResponse, error) {
	if input.Limit <= 0 {
		input.Limit = 20
	}
	if input.Limit > 100 {
		input.Limit = 100
	}
	if input.Offset < 0 {
		input.Offset = 0
	}
	query := strings.TrimSpace(input.Query)
	slug := strings.TrimSpace(input.Slug)
	topic := strings.TrimSpace(input.Topic)
	filterByTopic := false
	var topicID uuid.UUID
	if topic != "" {
		var err error
		topicID, err = resolveTopicID(ctx, r.db, topic)
		if err != nil {
			return nil, err
		}
		filterByTopic = true
	}
	rows, err := r.db.Query(ctx, `
		WITH filtered AS (
			SELECT i.id, i.slug, i.summary, i.domain, i.status, i.created_at, i.updated_at, cn.text::text AS canonical_name
			FROM ideas i
			LEFT JOIN names cn ON cn.id = i.canonical_name_id
			WHERE ($1 = '' OR i.slug = $1)
			  AND ($2 = '' OR i.summary ILIKE '%' || $2 || '%' OR i.domain ILIKE '%' || $2 || '%' OR cn.text::text ILIKE '%' || $2 || '%')
			  AND (NOT $5::boolean OR EXISTS (
				  SELECT 1 FROM idea_topics it WHERE it.idea_id = i.id AND it.topic_id = $6
			  ))
		)
		SELECT count(*) OVER(), id, slug, summary, domain, status, canonical_name, created_at, updated_at
		FROM filtered
		ORDER BY coalesce(canonical_name, ''), created_at, id
		LIMIT $3 OFFSET $4`, slug, query, input.Limit, input.Offset, filterByTopic, topicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	response := &model.IdeaListResponse{Limit: input.Limit, Offset: input.Offset, Ideas: make([]model.IdeaSummary, 0)}
	for rows.Next() {
		var total int64
		var idea model.IdeaSummary
		var slug, summary, domain, canonicalName pgtype.Text
		var status string
		if err := rows.Scan(&total, &idea.ID, &slug, &summary, &domain, &status, &canonicalName, &idea.CreatedAt, &idea.UpdatedAt); err != nil {
			return nil, err
		}
		response.Total = int(total)
		idea.Slug = textPtr(slug)
		idea.Summary = textPtr(summary)
		idea.Domain = textPtr(domain)
		idea.Status = model.IdeaStatus(status)
		idea.CanonicalName = textPtr(canonicalName)
		response.Ideas = append(response.Ideas, idea)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := r.enrichIdeaSummaries(ctx, response.Ideas); err != nil {
		return nil, err
	}
	return response, nil
}

func (r *Repository) UpdateIdea(ctx context.Context, ref string, input model.UpdateIdeaInput) (*model.Idea, error) {
	if input.Status != nil && *input.Status == model.IdeaStatusMerged {
		return nil, newStudyStorageError(model.StudyErrorCodeSchemaInvalid, "merged status is reserved for idea merge", "idea_update", false, nil)
	}
	var idea *model.Idea
	err := r.withLockedCurrentIdeas(ctx, "idea_update", []currentIdeaTarget{currentIdeaRef(ref)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		current, err := getIdeaByID(ctx, tx, ids[0])
		if err != nil {
			return err
		}
		slug := current.Slug
		if input.Slug != nil {
			slug = *input.Slug
		}
		summary := current.Summary
		if input.Summary != nil {
			summary = *input.Summary
		}
		domain := current.Domain
		if input.Domain != nil {
			domain = *input.Domain
		}
		status := current.Status
		if input.Status != nil {
			if !input.Status.Valid() {
				return fmt.Errorf("update idea: invalid status %q", *input.Status)
			}
			status = *input.Status
		}
		idea, err = scanIdea(tx.QueryRow(ctx, `
			UPDATE ideas
			SET slug = $2, summary = $3, domain = $4, status = $5
			WHERE id = $1
			RETURNING id, slug, summary, domain, status, canonical_name_id, merged_into_id, created_at, updated_at`,
			ids[0], slug, summary, domain, status))
		return err
	})
	if err != nil {
		return nil, err
	}
	return idea, nil
}

func (r *Repository) DeleteIdea(ctx context.Context, ref string) error {
	return r.withLockedCurrentIdeas(ctx, "idea_delete", []currentIdeaTarget{currentIdeaRef(ref)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		tag, err := tx.Exec(ctx, `DELETE FROM ideas WHERE id = $1`, ids[0])
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrIdeaNotFound
		}
		return nil
	})
}

func (r *Repository) AddName(ctx context.Context, ideaID uuid.UUID, input model.CreateNameInput) (*model.Name, error) {
	var name *model.Name
	err := r.withLockedCurrentIdeas(ctx, "name_add", []currentIdeaTarget{currentIdeaID(ideaID)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		var err error
		name, err = addNameInTx(ctx, tx, ids[0], input)
		return err
	})
	if err != nil {
		return nil, err
	}
	return name, nil
}

func (r *Repository) AddNameByRef(ctx context.Context, ideaRef string, input model.CreateNameInput) (*model.Name, error) {
	var name *model.Name
	err := r.withLockedCurrentIdeas(ctx, "name_add", []currentIdeaTarget{currentIdeaRef(ideaRef)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		var err error
		name, err = addNameInTx(ctx, tx, ids[0], input)
		return err
	})
	if err != nil {
		return nil, err
	}
	return name, nil
}

func (r *Repository) GetIdea(ctx context.Context, id uuid.UUID) (*model.Idea, error) {
	id, err := followMergedIdeaID(ctx, r.db, id)
	if err != nil {
		return nil, err
	}
	return getIdeaByID(ctx, r.db, id)
}

func (r *Repository) GetIdeaByRef(ctx context.Context, ref string) (*model.Idea, error) {
	id, err := r.ResolveIdeaID(ctx, ref)
	if err != nil {
		return nil, err
	}
	return getIdeaByID(ctx, r.db, id)
}

func getIdeaByID(ctx context.Context, q dbRunner, id uuid.UUID) (*model.Idea, error) {
	idea, err := scanIdea(q.QueryRow(ctx, `
		SELECT id, slug, summary, domain, status, canonical_name_id, merged_into_id, created_at, updated_at
		FROM ideas
		WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIdeaNotFound
	}
	return idea, err
}

func (r *Repository) ListNames(ctx context.Context, ideaID uuid.UUID) ([]model.Name, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, idea_id, text::text, display, kind, expansion, lang_tag, deprecation_note, sort_key, created_at, updated_at
		FROM names
		WHERE idea_id = $1
		ORDER BY kind = 'canonical' DESC, sort_key, text`, ideaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []model.Name
	for rows.Next() {
		name, err := scanNameRows(rows)
		if err != nil {
			return nil, err
		}
		names = append(names, *name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return names, nil
}

func (r *Repository) ListNamesByRef(ctx context.Context, ideaRef string) ([]model.Name, error) {
	ideaID, err := r.ResolveIdeaID(ctx, ideaRef)
	if err != nil {
		return nil, err
	}
	return r.ListNames(ctx, ideaID)
}

func (r *Repository) GetName(ctx context.Context, id uuid.UUID) (*model.Name, error) {
	name, err := scanName(r.db.QueryRow(ctx, `
		SELECT id, idea_id, text::text, display, kind, expansion, lang_tag, deprecation_note, sort_key, created_at, updated_at
		FROM names
		WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNameNotFound
	}
	return name, err
}

func (r *Repository) UpdateName(ctx context.Context, id uuid.UUID, input model.UpdateNameInput) (*model.Name, error) {
	initial, err := r.GetName(ctx, id)
	if err != nil {
		return nil, err
	}
	var name *model.Name
	err = r.withLockedCurrentIdeas(ctx, "name_update", []currentIdeaTarget{currentIdeaID(initial.IdeaID)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		current, err := scanName(tx.QueryRow(ctx, `
			SELECT id, idea_id, text::text, display, kind, expansion, lang_tag, deprecation_note, sort_key, created_at, updated_at
			FROM names
			WHERE id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNameNotFound
		}
		if err != nil {
			return err
		}
		if current.IdeaID != ids[0] {
			return errCurrentIdeaChanged
		}
		text := current.Text
		if input.Text != nil {
			text = strings.TrimSpace(*input.Text)
			if text == "" {
				return fmt.Errorf("update name: text is required")
			}
		}
		display := current.Display
		if input.Display != nil {
			display = *input.Display
		}
		kind := current.Kind
		if input.Kind != nil {
			if !input.Kind.Valid() {
				return fmt.Errorf("%w: %q", ErrInvalidNameKind, *input.Kind)
			}
			kind = *input.Kind
		}
		expansion := current.Expansion
		if input.Expansion != nil {
			expansion = *input.Expansion
		}
		langTag := current.LangTag
		if input.LangTag != nil {
			langTag = *input.LangTag
		}
		deprecationNote := current.DeprecationNote
		if input.DeprecationNote != nil {
			deprecationNote = *input.DeprecationNote
		}
		if kind != model.NameKindAcronym {
			expansion = nil
		}
		if kind != model.NameKindLocalized {
			langTag = nil
		}
		if kind != model.NameKindDeprecated {
			deprecationNote = nil
		}
		if kind == model.NameKindCanonical {
			if _, err := tx.Exec(ctx, `
				UPDATE names
				SET kind = 'alias'
				WHERE idea_id = $1 AND kind = 'canonical' AND id <> $2`, current.IdeaID, id); err != nil {
				return err
			}
		}
		sortKey := current.SortKey
		if input.SortKey != nil {
			sortKey = *input.SortKey
		}
		name, err = scanName(tx.QueryRow(ctx, `
			UPDATE names
			SET text = $2, display = $3, kind = $4, expansion = $5, lang_tag = $6, deprecation_note = $7, sort_key = $8
			WHERE id = $1
			RETURNING id, idea_id, text::text, display, kind, expansion, lang_tag, deprecation_note, sort_key, created_at, updated_at`,
			id, text, display, kind, expansion, langTag, deprecationNote, sortKey))
		if err != nil {
			return err
		}
		if name.Kind == model.NameKindCanonical {
			_, err = tx.Exec(ctx, `
				UPDATE ideas
				SET canonical_name_id = $2
				WHERE id = $1`, name.IdeaID, name.ID)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return name, nil
}

func (r *Repository) DeleteName(ctx context.Context, id uuid.UUID) error {
	initial, err := r.GetName(ctx, id)
	if err != nil {
		return err
	}
	return r.withLockedCurrentIdeas(ctx, "name_delete", []currentIdeaTarget{currentIdeaID(initial.IdeaID)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		var ownerID uuid.UUID
		var kind string
		err := tx.QueryRow(ctx, `SELECT idea_id, kind FROM names WHERE id = $1`, id).Scan(&ownerID, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNameNotFound
		}
		if err != nil {
			return err
		}
		if ownerID != ids[0] {
			return errCurrentIdeaChanged
		}
		if model.NameKind(kind) == model.NameKindCanonical {
			return ErrCanonicalName
		}
		tag, err := tx.Exec(ctx, `DELETE FROM names WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNameNotFound
		}
		return nil
	})
}

func (r *Repository) ResolveIdeaID(ctx context.Context, ref string) (uuid.UUID, error) {
	return resolveIdeaID(ctx, r.db, ref)
}

func (r *Repository) PromoteName(ctx context.Context, ideaID uuid.UUID, nameText string) (*model.Name, error) {
	nameText = strings.TrimSpace(nameText)
	if nameText == "" {
		return nil, fmt.Errorf("%w: empty name", ErrNameNotFound)
	}

	var name *model.Name
	err := r.withLockedCurrentIdeas(ctx, "name_promote", []currentIdeaTarget{currentIdeaID(ideaID)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		var targetID uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT id
			FROM names
			WHERE idea_id = $1 AND text = $2`, ids[0], nameText).Scan(&targetID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNameNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE names
			SET kind = 'alias'
			WHERE idea_id = $1 AND kind = 'canonical' AND id <> $2`, ids[0], targetID); err != nil {
			return err
		}
		name, err = scanName(tx.QueryRow(ctx, `
			UPDATE names
			SET kind = 'canonical', expansion = NULL, lang_tag = NULL, deprecation_note = NULL
			WHERE id = $1
			RETURNING id, idea_id, text::text, display, kind, expansion, lang_tag, deprecation_note, sort_key, created_at, updated_at`, targetID))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE ideas
			SET canonical_name_id = $2
			WHERE id = $1`, ids[0], name.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return name, nil
}

func (r *Repository) CreateDefinition(ctx context.Context, ideaRef string, input model.CreateDefinitionInput) (*model.Definition, error) {
	if strings.TrimSpace(input.Body) == "" {
		return nil, fmt.Errorf("create definition: body is required")
	}
	var definition *model.Definition
	err := r.withLockedCurrentIdeas(ctx, "definition_create", []currentIdeaTarget{currentIdeaRef(ideaRef)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		var err error
		definition, err = insertDefinition(ctx, tx, ids[0], input)
		return err
	})
	if err != nil {
		return nil, err
	}
	return definition, nil
}

func (r *Repository) ListDefinitions(ctx context.Context, ideaRef string) ([]model.Definition, error) {
	ideaID, err := r.ResolveIdeaID(ctx, ideaRef)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, idea_id, body, register, source, is_primary, sort_key, created_at, updated_at
		FROM definitions
		WHERE idea_id = $1
		ORDER BY is_primary DESC, sort_key, created_at`, ideaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var definitions []model.Definition
	for rows.Next() {
		definition, err := scanDefinition(rows)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, *definition)
	}
	return definitions, rows.Err()
}

func (r *Repository) GetDefinition(ctx context.Context, id uuid.UUID) (*model.Definition, error) {
	definition, err := scanDefinition(r.db.QueryRow(ctx, `
		SELECT id, idea_id, body, register, source, is_primary, sort_key, created_at, updated_at
		FROM definitions
		WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDefinitionNotFound
	}
	return definition, err
}

func (r *Repository) UpdateDefinition(ctx context.Context, id uuid.UUID, input model.UpdateDefinitionInput) (*model.Definition, error) {
	initial, err := r.GetDefinition(ctx, id)
	if err != nil {
		return nil, err
	}
	var definition *model.Definition
	err = r.withLockedCurrentIdeas(ctx, "definition_update", []currentIdeaTarget{currentIdeaID(initial.IdeaID)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		current, err := scanDefinition(tx.QueryRow(ctx, `
			SELECT id, idea_id, body, register, source, is_primary, sort_key, created_at, updated_at
			FROM definitions WHERE id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDefinitionNotFound
		}
		if err != nil {
			return err
		}
		if current.IdeaID != ids[0] {
			return errCurrentIdeaChanged
		}
		body := current.Body
		if input.Body != nil {
			body = strings.TrimSpace(*input.Body)
			if body == "" {
				return fmt.Errorf("update definition: body is required")
			}
		}
		register := current.Register
		if input.Register != nil {
			register = *input.Register
		}
		source := current.Source
		if input.Source != nil {
			source = *input.Source
		}
		isPrimary := current.IsPrimary
		if input.IsPrimary != nil {
			isPrimary = *input.IsPrimary
		}
		sortKey := current.SortKey
		if input.SortKey != nil {
			sortKey = *input.SortKey
		}
		definition, err = scanDefinition(tx.QueryRow(ctx, `
			UPDATE definitions
			SET body = $2, register = $3, source = $4, is_primary = $5, sort_key = $6
			WHERE id = $1
			RETURNING id, idea_id, body, register, source, is_primary, sort_key, created_at, updated_at`,
			id, body, register, source, isPrimary, sortKey))
		return err
	})
	if err != nil {
		return nil, err
	}
	return definition, nil
}

func (r *Repository) DeleteDefinition(ctx context.Context, id uuid.UUID) error {
	initial, err := r.GetDefinition(ctx, id)
	if err != nil {
		return err
	}
	return r.withLockedCurrentIdeas(ctx, "definition_delete", []currentIdeaTarget{currentIdeaID(initial.IdeaID)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		var ownerID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT idea_id FROM definitions WHERE id = $1`, id).Scan(&ownerID); errors.Is(err, pgx.ErrNoRows) {
			return ErrDefinitionNotFound
		} else if err != nil {
			return err
		}
		if ownerID != ids[0] {
			return errCurrentIdeaChanged
		}
		tag, err := tx.Exec(ctx, `DELETE FROM definitions WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrDefinitionNotFound
		}
		return nil
	})
}

func (r *Repository) CreateExample(ctx context.Context, ideaRef string, input model.CreateExampleInput) (*model.Example, error) {
	if strings.TrimSpace(input.Body) == "" {
		return nil, fmt.Errorf("create example: body is required")
	}
	var example *model.Example
	err := r.withLockedCurrentIdeas(ctx, "example_create", []currentIdeaTarget{currentIdeaRef(ideaRef)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		var err error
		example, err = insertExample(ctx, tx, ids[0], input)
		return err
	})
	if err != nil {
		return nil, err
	}
	return example, nil
}

func (r *Repository) ListExamples(ctx context.Context, ideaRef string) ([]model.Example, error) {
	ideaID, err := r.ResolveIdeaID(ctx, ideaRef)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, idea_id, body, lang, caption, sort_key, created_at, updated_at
		FROM examples
		WHERE idea_id = $1
		ORDER BY sort_key, created_at`, ideaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var examples []model.Example
	for rows.Next() {
		example, err := scanExample(rows)
		if err != nil {
			return nil, err
		}
		examples = append(examples, *example)
	}
	return examples, rows.Err()
}

func (r *Repository) GetExample(ctx context.Context, id uuid.UUID) (*model.Example, error) {
	example, err := scanExample(r.db.QueryRow(ctx, `
		SELECT id, idea_id, body, lang, caption, sort_key, created_at, updated_at
		FROM examples
		WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExampleNotFound
	}
	return example, err
}

func (r *Repository) UpdateExample(ctx context.Context, id uuid.UUID, input model.UpdateExampleInput) (*model.Example, error) {
	initial, err := r.GetExample(ctx, id)
	if err != nil {
		return nil, err
	}
	var example *model.Example
	err = r.withLockedCurrentIdeas(ctx, "example_update", []currentIdeaTarget{currentIdeaID(initial.IdeaID)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		current, err := scanExample(tx.QueryRow(ctx, `
			SELECT id, idea_id, body, lang, caption, sort_key, created_at, updated_at
			FROM examples WHERE id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrExampleNotFound
		}
		if err != nil {
			return err
		}
		if current.IdeaID != ids[0] {
			return errCurrentIdeaChanged
		}
		body := current.Body
		if input.Body != nil {
			body = strings.TrimSpace(*input.Body)
			if body == "" {
				return fmt.Errorf("update example: body is required")
			}
		}
		lang := current.Lang
		if input.Lang != nil {
			lang = *input.Lang
		}
		caption := current.Caption
		if input.Caption != nil {
			caption = *input.Caption
		}
		sortKey := current.SortKey
		if input.SortKey != nil {
			sortKey = *input.SortKey
		}
		example, err = scanExample(tx.QueryRow(ctx, `
			UPDATE examples
			SET body = $2, lang = $3, caption = $4, sort_key = $5
			WHERE id = $1
			RETURNING id, idea_id, body, lang, caption, sort_key, created_at, updated_at`,
			id, body, lang, caption, sortKey))
		return err
	})
	if err != nil {
		return nil, err
	}
	return example, nil
}

func (r *Repository) DeleteExample(ctx context.Context, id uuid.UUID) error {
	initial, err := r.GetExample(ctx, id)
	if err != nil {
		return err
	}
	return r.withLockedCurrentIdeas(ctx, "example_delete", []currentIdeaTarget{currentIdeaID(initial.IdeaID)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		var ownerID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT idea_id FROM examples WHERE id = $1`, id).Scan(&ownerID); errors.Is(err, pgx.ErrNoRows) {
			return ErrExampleNotFound
		} else if err != nil {
			return err
		}
		if ownerID != ids[0] {
			return errCurrentIdeaChanged
		}
		tag, err := tx.Exec(ctx, `DELETE FROM examples WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrExampleNotFound
		}
		return nil
	})
}

func (r *Repository) CreateNote(ctx context.Context, ideaRef string, input model.CreateNoteInput) (*model.Note, error) {
	if strings.TrimSpace(input.Body) == "" {
		return nil, fmt.Errorf("create note: body is required")
	}
	var note *model.Note
	err := r.withLockedCurrentIdeas(ctx, "note_create", []currentIdeaTarget{currentIdeaRef(ideaRef)}, func(tx pgx.Tx, ids []uuid.UUID) error {
		var err error
		note, err = insertNote(ctx, tx, ids[0], input)
		return err
	})
	if err != nil {
		return nil, err
	}
	return note, nil
}

func (r *Repository) ListNotes(ctx context.Context, ideaRef string) ([]model.Note, error) {
	ideaID, err := r.ResolveIdeaID(ctx, ideaRef)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, idea_id, body, kind, sort_key, created_at, updated_at
		FROM notes
		WHERE idea_id = $1
		ORDER BY sort_key, created_at`, ideaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []model.Note
	for rows.Next() {
		note, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes = append(notes, *note)
	}
	return notes, rows.Err()
}

func (r *Repository) GetNote(ctx context.Context, id uuid.UUID) (*model.Note, error) {
	note, err := scanNote(r.db.QueryRow(ctx, `
		SELECT id, idea_id, body, kind, sort_key, created_at, updated_at
		FROM notes
		WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoteNotFound
	}
	return note, err
}

func (r *Repository) UpdateNote(ctx context.Context, id uuid.UUID, input model.UpdateNoteInput) (*model.Note, error) {
	current, err := r.GetNote(ctx, id)
	if err != nil {
		return nil, err
	}
	body := current.Body
	if input.Body != nil {
		body = strings.TrimSpace(*input.Body)
		if body == "" {
			return nil, fmt.Errorf("update note: body is required")
		}
	}
	kind := current.Kind
	if input.Kind != nil {
		kind = *input.Kind
	}
	sortKey := current.SortKey
	if input.SortKey != nil {
		sortKey = *input.SortKey
	}
	note, err := scanNote(r.db.QueryRow(ctx, `
		UPDATE notes
		SET body = $2, kind = $3, sort_key = $4
		WHERE id = $1
		RETURNING id, idea_id, body, kind, sort_key, created_at, updated_at`,
		id, body, kind, sortKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoteNotFound
	}
	return note, err
}

func (r *Repository) DeleteNote(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM notes WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoteNotFound
	}
	return nil
}

func normalizeNameInput(input *model.CreateNameInput) {
	if input.Kind != model.NameKindAcronym {
		input.Expansion = nil
	}
	if input.Kind != model.NameKindLocalized {
		input.LangTag = nil
	}
	if input.Kind != model.NameKindDeprecated {
		input.DeprecationNote = nil
	}
}

func addNameInTx(ctx context.Context, tx pgx.Tx, ideaID uuid.UUID, input model.CreateNameInput) (*model.Name, error) {
	if input.Kind == "" {
		input.Kind = model.NameKindAlias
	}
	if !input.Kind.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidNameKind, input.Kind)
	}
	normalizeNameInput(&input)
	if input.Kind == model.NameKindCanonical {
		if _, err := tx.Exec(ctx, `
			UPDATE names
			SET kind = 'alias'
			WHERE idea_id = $1 AND kind = 'canonical'`, ideaID); err != nil {
			return nil, err
		}
	}

	name, err := insertName(ctx, tx, ideaID, input)
	if err != nil {
		return nil, err
	}
	if name.Kind == model.NameKindCanonical {
		if _, err := tx.Exec(ctx, `
			UPDATE ideas
			SET canonical_name_id = $2
			WHERE id = $1`, ideaID, name.ID); err != nil {
			return nil, err
		}
	}
	return name, nil
}

func insertName(ctx context.Context, q dbRunner, ideaID uuid.UUID, input model.CreateNameInput) (*model.Name, error) {
	input.Text = strings.TrimSpace(input.Text)
	if input.Text == "" {
		return nil, fmt.Errorf("insert name: text is required")
	}
	return scanName(q.QueryRow(ctx, `
		INSERT INTO names (idea_id, text, display, kind, expansion, lang_tag, deprecation_note, sort_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, idea_id, text::text, display, kind, expansion, lang_tag, deprecation_note, sort_key, created_at, updated_at`,
		ideaID, input.Text, input.Display, input.Kind, input.Expansion, input.LangTag, input.DeprecationNote, input.SortKey))
}

func insertDefinition(ctx context.Context, q dbRunner, ideaID uuid.UUID, input model.CreateDefinitionInput) (*model.Definition, error) {
	return scanDefinition(q.QueryRow(ctx, `
		INSERT INTO definitions (idea_id, body, register, source, is_primary, sort_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, idea_id, body, register, source, is_primary, sort_key, created_at, updated_at`,
		ideaID, strings.TrimSpace(input.Body), input.Register, input.Source, input.IsPrimary, input.SortKey))
}

func insertExample(ctx context.Context, q dbRunner, ideaID uuid.UUID, input model.CreateExampleInput) (*model.Example, error) {
	return scanExample(q.QueryRow(ctx, `
		INSERT INTO examples (idea_id, body, lang, caption, sort_key)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, idea_id, body, lang, caption, sort_key, created_at, updated_at`,
		ideaID, strings.TrimSpace(input.Body), input.Lang, input.Caption, input.SortKey))
}

func insertNote(ctx context.Context, q dbRunner, ideaID uuid.UUID, input model.CreateNoteInput) (*model.Note, error) {
	return scanNote(q.QueryRow(ctx, `
		INSERT INTO notes (idea_id, body, kind, sort_key)
		VALUES ($1, $2, $3, $4)
		RETURNING id, idea_id, body, kind, sort_key, created_at, updated_at`,
		ideaID, strings.TrimSpace(input.Body), input.Kind, input.SortKey))
}

func resolveIdeaID(ctx context.Context, q dbRunner, ref string) (uuid.UUID, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return uuid.Nil, fmt.Errorf("%w: empty ref", ErrInvalidIdeaRef)
	}

	candidates := make([]uuid.UUID, 0, 2)
	if looksLikeUUIDPrefix(ref) {
		ids, err := queryIdeaIDPrefix(ctx, q, ref)
		if err != nil {
			return uuid.Nil, err
		}
		candidates = append(candidates, ids...)
	}

	ids, err := queryIdeaName(ctx, q, ref)
	if err != nil {
		return uuid.Nil, err
	}
	candidates = append(candidates, ids...)

	ids, err = queryIdeaSlug(ctx, q, ref)
	if err != nil {
		return uuid.Nil, err
	}
	candidates = append(candidates, ids...)

	id, err := uniqueIdeaCandidate(ref, candidates)
	if err != nil {
		return uuid.Nil, err
	}
	return followMergedIdeaID(ctx, q, id)
}

func queryIdeaIDPrefix(ctx context.Context, q dbRunner, ref string) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		SELECT id
		FROM ideas
		WHERE left(lower(id::text), length($1)) = lower($1)
		ORDER BY id
		LIMIT 2`, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectUUIDRows(rows)
}

func queryIdeaName(ctx context.Context, q dbRunner, ref string) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		SELECT idea_id
		FROM names
		WHERE text = $1
		LIMIT 2`, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectUUIDRows(rows)
}

func queryIdeaSlug(ctx context.Context, q dbRunner, ref string) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		SELECT id
		FROM ideas
		WHERE slug = $1
		LIMIT 2`, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectUUIDRows(rows)
}

func collectUUIDRows(rows pgx.Rows) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func uniqueIdeaCandidate(ref string, candidates []uuid.UUID) (uuid.UUID, error) {
	seen := make(map[uuid.UUID]struct{}, len(candidates))
	unique := make([]uuid.UUID, 0, len(candidates))
	for _, id := range candidates {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].String() < unique[j].String()
	})
	switch len(unique) {
	case 0:
		return uuid.Nil, ErrIdeaNotFound
	case 1:
		return unique[0], nil
	default:
		return uuid.Nil, &AmbiguousIdeaRefError{Ref: ref, Candidates: unique}
	}
}

func followMergedIdeaID(ctx context.Context, q dbRunner, id uuid.UUID) (uuid.UUID, error) {
	seen := map[uuid.UUID]struct{}{}
	for range 16 {
		if _, ok := seen[id]; ok {
			return uuid.Nil, fmt.Errorf("merged idea cycle at %s", id)
		}
		seen[id] = struct{}{}

		var status string
		var mergedInto pgtype.UUID
		err := q.QueryRow(ctx, `
			SELECT status, merged_into_id
			FROM ideas
			WHERE id = $1`, id).Scan(&status, &mergedInto)
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrIdeaNotFound
		}
		if err != nil {
			return uuid.Nil, err
		}
		if model.IdeaStatus(status) != model.IdeaStatusMerged {
			return id, nil
		}
		if !mergedInto.Valid {
			return uuid.Nil, fmt.Errorf("merged idea %s has no survivor", id)
		}
		id = uuid.UUID(mergedInto.Bytes)
	}
	return uuid.Nil, fmt.Errorf("merged idea chain too deep")
}

var uuidPrefixPattern = regexp.MustCompile(`^[0-9a-fA-F-]{1,36}$`)

func looksLikeUUIDPrefix(ref string) bool {
	if len(ref) > 36 || !uuidPrefixPattern.MatchString(ref) {
		return false
	}
	for _, part := range strings.Split(ref, "-") {
		if len(part) == 0 {
			return false
		}
	}
	return true
}

func chooseCanonicalName(names []model.CreateNameInput) int {
	for i, name := range names {
		if name.Kind == model.NameKindCanonical {
			return i
		}
	}
	return 0
}

func scanIdea(row pgx.Row) (*model.Idea, error) {
	var idea model.Idea
	var status string
	var slug, summary, domain pgtype.Text
	var canonicalNameID, mergedIntoID pgtype.UUID
	if err := row.Scan(
		&idea.ID,
		&slug,
		&summary,
		&domain,
		&status,
		&canonicalNameID,
		&mergedIntoID,
		&idea.CreatedAt,
		&idea.UpdatedAt,
	); err != nil {
		return nil, err
	}
	idea.Slug = textPtr(slug)
	idea.Summary = textPtr(summary)
	idea.Domain = textPtr(domain)
	idea.Status = model.IdeaStatus(status)
	idea.CanonicalNameID = uuidPtr(canonicalNameID)
	idea.MergedIntoID = uuidPtr(mergedIntoID)
	return &idea, nil
}

func scanName(row pgx.Row) (*model.Name, error) {
	var name model.Name
	var kind string
	var display, expansion, langTag, deprecationNote pgtype.Text
	if err := row.Scan(
		&name.ID,
		&name.IdeaID,
		&name.Text,
		&display,
		&kind,
		&expansion,
		&langTag,
		&deprecationNote,
		&name.SortKey,
		&name.CreatedAt,
		&name.UpdatedAt,
	); err != nil {
		return nil, err
	}
	name.Display = textPtr(display)
	name.Kind = model.NameKind(kind)
	name.Expansion = textPtr(expansion)
	name.LangTag = textPtr(langTag)
	name.DeprecationNote = textPtr(deprecationNote)
	return &name, nil
}

func scanNameRows(rows pgx.Rows) (*model.Name, error) {
	return scanName(rows)
}

func scanDefinition(row pgx.Row) (*model.Definition, error) {
	var definition model.Definition
	var register, source pgtype.Text
	if err := row.Scan(
		&definition.ID,
		&definition.IdeaID,
		&definition.Body,
		&register,
		&source,
		&definition.IsPrimary,
		&definition.SortKey,
		&definition.CreatedAt,
		&definition.UpdatedAt,
	); err != nil {
		return nil, err
	}
	definition.Register = textPtr(register)
	definition.Source = textPtr(source)
	return &definition, nil
}

func scanExample(row pgx.Row) (*model.Example, error) {
	var example model.Example
	var lang, caption pgtype.Text
	if err := row.Scan(
		&example.ID,
		&example.IdeaID,
		&example.Body,
		&lang,
		&caption,
		&example.SortKey,
		&example.CreatedAt,
		&example.UpdatedAt,
	); err != nil {
		return nil, err
	}
	example.Lang = textPtr(lang)
	example.Caption = textPtr(caption)
	return &example, nil
}

func scanNote(row pgx.Row) (*model.Note, error) {
	var note model.Note
	var kind pgtype.Text
	if err := row.Scan(
		&note.ID,
		&note.IdeaID,
		&note.Body,
		&kind,
		&note.SortKey,
		&note.CreatedAt,
		&note.UpdatedAt,
	); err != nil {
		return nil, err
	}
	note.Kind = textPtr(kind)
	return &note, nil
}

func textPtr(v pgtype.Text) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

func uuidPtr(v pgtype.UUID) *uuid.UUID {
	if !v.Valid {
		return nil
	}
	id := uuid.UUID(v.Bytes)
	return &id
}
