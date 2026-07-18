package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"lexinet/internal/model"
)

func (r *Repository) CreateSubmission(ctx context.Context, input model.CreateSubmissionInput) (*model.Submission, error) {
	input, err := model.NormalizeCreateSubmissionInput(input)
	if err != nil {
		return nil, err
	}
	return scanSubmission(r.db.QueryRow(ctx, `
		INSERT INTO submissions (term, context)
		VALUES ($1, $2)
		RETURNING id, term, context, status, idea_id, note, completed_at, created_at, updated_at`, input.Term, input.Context))
}

func (r *Repository) ListSubmissions(ctx context.Context, input model.ListSubmissionsInput) (*model.ListSubmissionsResponse, error) {
	input, err := model.NormalizeListSubmissionsInput(input)
	if err != nil {
		return nil, err
	}
	status := ""
	if input.Status != nil {
		status = string(*input.Status)
	}
	rows, err := r.db.Query(ctx, `
		WITH filtered AS (
			SELECT id, term, context, status, idea_id, note, completed_at, created_at, updated_at
			FROM submissions
			WHERE ($1 = '' OR status = $1)
		)
		SELECT count(*) OVER(), id, term, context, status, idea_id, note, completed_at, created_at, updated_at
		FROM filtered
		ORDER BY created_at, id
		LIMIT $2 OFFSET $3`, status, input.Limit, input.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	response := &model.ListSubmissionsResponse{Limit: input.Limit, Offset: input.Offset, Submissions: make([]model.Submission, 0)}
	for rows.Next() {
		var total int64
		submission, err := scanSubmissionWithTotal(rows, &total)
		if err != nil {
			return nil, err
		}
		response.Total = int(total)
		response.Submissions = append(response.Submissions, *submission)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return response, nil
}

func (r *Repository) CompleteSubmission(ctx context.Context, id uuid.UUID, input model.CompleteSubmissionInput) (*model.Submission, error) {
	input, err := model.NormalizeCompleteSubmissionInput(input)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	ideaID, err := resolveIdeaID(ctx, tx, input.Idea)
	if err != nil {
		return nil, err
	}
	submission, err := scanSubmission(tx.QueryRow(ctx, `
		UPDATE submissions
		SET status = 'enriched', idea_id = $2, note = $3, completed_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING id, term, context, status, idea_id, note, completed_at, created_at, updated_at`, id, ideaID, input.Note))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, submissionTransitionError(ctx, tx, id)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return submission, nil
}

func (r *Repository) FailSubmission(ctx context.Context, id uuid.UUID, input model.FailSubmissionInput) (*model.Submission, error) {
	input = model.NormalizeFailSubmissionInput(input)
	submission, err := scanSubmission(r.db.QueryRow(ctx, `
		UPDATE submissions
		SET status = 'failed', note = $2, completed_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING id, term, context, status, idea_id, note, completed_at, created_at, updated_at`, id, input.Note))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, submissionTransitionError(ctx, r.db, id)
	}
	return submission, err
}

func submissionTransitionError(ctx context.Context, q dbRunner, id uuid.UUID) error {
	var status string
	err := q.QueryRow(ctx, `SELECT status FROM submissions WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSubmissionNotFound
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: submission %s is %s", ErrInvalidSubmissionTransition, id, status)
}

func scanSubmissionWithTotal(row pgx.Row, total *int64) (*model.Submission, error) {
	var submission model.Submission
	var contextText, note pgtype.Text
	var status string
	var ideaID pgtype.UUID
	var completedAt pgtype.Timestamptz
	if err := row.Scan(total, &submission.ID, &submission.Term, &contextText, &status, &ideaID, &note, &completedAt, &submission.CreatedAt, &submission.UpdatedAt); err != nil {
		return nil, err
	}
	populateSubmission(&submission, contextText, status, ideaID, note, completedAt)
	return &submission, nil
}

func scanSubmission(row pgx.Row) (*model.Submission, error) {
	var submission model.Submission
	var contextText, note pgtype.Text
	var status string
	var ideaID pgtype.UUID
	var completedAt pgtype.Timestamptz
	if err := row.Scan(&submission.ID, &submission.Term, &contextText, &status, &ideaID, &note, &completedAt, &submission.CreatedAt, &submission.UpdatedAt); err != nil {
		return nil, err
	}
	populateSubmission(&submission, contextText, status, ideaID, note, completedAt)
	return &submission, nil
}

func populateSubmission(submission *model.Submission, contextText pgtype.Text, status string, ideaID pgtype.UUID, note pgtype.Text, completedAt pgtype.Timestamptz) {
	submission.Context = textPtr(contextText)
	submission.Status = model.SubmissionStatus(status)
	submission.IdeaID = uuidPtr(ideaID)
	submission.Note = textPtr(note)
	if completedAt.Valid {
		completed := completedAt.Time
		submission.CompletedAt = &completed
	}
}
