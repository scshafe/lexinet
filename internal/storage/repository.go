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
)

var (
	ErrIdeaNotFound     = errors.New("idea not found")
	ErrIdeaRefAmbiguous = errors.New("idea reference ambiguous")
	ErrInvalidIdeaRef   = errors.New("invalid idea reference")
	ErrInvalidNameKind  = errors.New("invalid name kind")
	ErrNameNotFound     = errors.New("name not found")
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
}

type Repository struct {
	db dbPool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{db: pool}
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
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return idea, nil
}

func (r *Repository) AddName(ctx context.Context, ideaID uuid.UUID, input model.CreateNameInput) (*model.Name, error) {
	if input.Kind == "" {
		input.Kind = model.NameKindAlias
	}
	if !input.Kind.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidNameKind, input.Kind)
	}
	if input.Kind == model.NameKindCanonical {
		input.Expansion = nil
		input.LangTag = nil
		input.DeprecationNote = nil
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

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
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return name, nil
}

func (r *Repository) GetIdea(ctx context.Context, id uuid.UUID) (*model.Idea, error) {
	idea, err := scanIdea(r.db.QueryRow(ctx, `
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

func (r *Repository) ResolveIdeaID(ctx context.Context, ref string) (uuid.UUID, error) {
	return resolveIdeaID(ctx, r.db, ref)
}

func (r *Repository) PromoteName(ctx context.Context, ideaID uuid.UUID, nameText string) (*model.Name, error) {
	nameText = strings.TrimSpace(nameText)
	if nameText == "" {
		return nil, fmt.Errorf("%w: empty name", ErrNameNotFound)
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var targetID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id
		FROM names
		WHERE idea_id = $1 AND text = $2`, ideaID, nameText).Scan(&targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNameNotFound
	}
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE names
		SET kind = 'alias'
		WHERE idea_id = $1 AND kind = 'canonical' AND id <> $2`, ideaID, targetID); err != nil {
		return nil, err
	}

	name, err := scanName(tx.QueryRow(ctx, `
		UPDATE names
		SET kind = 'canonical', expansion = NULL, lang_tag = NULL, deprecation_note = NULL
		WHERE id = $1
		RETURNING id, idea_id, text::text, display, kind, expansion, lang_tag, deprecation_note, sort_key, created_at, updated_at`, targetID))
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE ideas
		SET canonical_name_id = $2
		WHERE id = $1`, ideaID, name.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
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
