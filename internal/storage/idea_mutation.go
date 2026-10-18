package storage

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"lexinet/internal/model"
)

const currentIdeaMutationAttempts = 3

var errCurrentIdeaChanged = errors.New("current idea changed while acquiring mutation lock")

type currentIdeaTarget struct {
	ref  string
	id   uuid.UUID
	byID bool
}

func currentIdeaRef(ref string) currentIdeaTarget {
	return currentIdeaTarget{ref: ref}
}

func currentIdeaID(id uuid.UUID) currentIdeaTarget {
	return currentIdeaTarget{id: id, byID: true}
}

func (target currentIdeaTarget) resolve(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
	if target.byID {
		if target.id == uuid.Nil {
			return uuid.Nil, ErrIdeaNotFound
		}
		return followMergedIdeaID(ctx, tx, target.id)
	}
	return resolveIdeaID(ctx, tx, target.ref)
}

// withLockedCurrentIdeas is the mutation linearization boundary shared with
// idea merge and study eligibility reads. Resolution is advisory until every
// resolved row is locked in UUID order and the original targets are resolved
// again. A merge that wins that race causes a whole-transaction retry; a
// mutation that wins is visible to the later merge while its row lock is held.
func (repository *Repository) withLockedCurrentIdeas(
	ctx context.Context,
	operation string,
	targets []currentIdeaTarget,
	mutate func(pgx.Tx, []uuid.UUID) error,
) error {
	for attempt := 0; attempt < currentIdeaMutationAttempts; attempt++ {
		err := repository.withLockedCurrentIdeasOnce(ctx, operation, targets, mutate)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errCurrentIdeaChanged) && !isRetryableIdeaTransactionError(err) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return newStudyStorageError(
		model.StudyErrorCodeServiceUnavailable,
		"idea mutation could not acquire a stable current identity",
		operation,
		true,
		errCurrentIdeaChanged,
	)
}

func (repository *Repository) withLockedCurrentIdeasOnce(
	ctx context.Context,
	operation string,
	targets []currentIdeaTarget,
	mutate func(pgx.Tx, []uuid.UUID) error,
) (resultErr error) {
	if len(targets) == 0 || mutate == nil {
		return newStudyStorageError(model.StudyErrorCodeInternal, "idea mutation target is invalid", operation, false, nil)
	}
	tx, err := repository.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite})
	if err != nil {
		return mapStudyStorageError(operation+"_begin", err)
	}
	defer tx.Rollback(ctx)

	resolved := make([]uuid.UUID, len(targets))
	for index := range targets {
		resolved[index], err = targets[index].resolve(ctx, tx)
		if err != nil {
			return err
		}
	}
	if repository.ideaMutationAfterResolve != nil {
		repository.ideaMutationAfterResolve(operation, append([]uuid.UUID(nil), resolved...))
	}
	if err := lockAndValidateCurrentIdeas(ctx, tx, resolved); err != nil {
		return err
	}
	for index := range targets {
		current, err := targets[index].resolve(ctx, tx)
		if err != nil {
			return err
		}
		if current != resolved[index] {
			return errCurrentIdeaChanged
		}
	}
	if err := mutate(tx, resolved); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func lockAndValidateCurrentIdeas(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) error {
	unique := append([]uuid.UUID(nil), ids...)
	sort.Slice(unique, func(left, right int) bool { return unique[left].String() < unique[right].String() })
	write := 0
	for _, id := range unique {
		if id == uuid.Nil {
			return ErrIdeaNotFound
		}
		if write > 0 && unique[write-1] == id {
			continue
		}
		unique[write] = id
		write++
	}
	unique = unique[:write]

	rows, err := tx.Query(ctx, `
		SELECT id, status, merged_into_id
		FROM ideas
		WHERE id = ANY($1::uuid[])
		ORDER BY id
		FOR UPDATE`, unique)
	if err != nil {
		return err
	}
	defer rows.Close()
	locked := 0
	for rows.Next() {
		var id uuid.UUID
		var status string
		var mergedInto *uuid.UUID
		if err := rows.Scan(&id, &status, &mergedInto); err != nil {
			return err
		}
		if model.IdeaStatus(status) == model.IdeaStatusMerged || mergedInto != nil {
			return errCurrentIdeaChanged
		}
		locked++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if locked != len(unique) {
		return ErrIdeaNotFound
	}
	return nil
}

func isRetryableIdeaTransactionError(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && (postgresError.Code == "40001" || postgresError.Code == "40P01")
}
