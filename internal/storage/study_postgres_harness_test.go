package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/internal/model"
	"lexinet/internal/testsupport/studypostgres"
	"lexinet/migrations"
)

// studyTestPostgres keeps the established storage-test vocabulary while
// delegating the only database lifecycle to the reusable canonical harness.
type studyTestPostgres struct {
	*studypostgres.Harness
}

func startStudyTestPostgres(t *testing.T) *studyTestPostgres {
	t.Helper()
	return &studyTestPostgres{Harness: studypostgres.Start(t)}
}

func studyTestDatabaseError(code string, err error) error {
	return studypostgres.RedactedDatabaseError(code, err)
}

func studyTestSessionPageArtifact() model.StudySessionPageArtifact {
	canonicalJSON, err := model.EncodeStudySessionPageDocument(model.StudySessionPageDocument{
		AfterPosition: -1, NextAfterPosition: -1, Complete: true, Items: []model.StudySessionPageCardProjection{},
	})
	if err != nil {
		panic(err)
	}
	return model.StudySessionPageArtifact{
		Request: model.StudySessionPageRequest{
			AfterPosition: -1,
			Limit:         model.StudyMaximumPageItems,
		},
		CanonicalJSON:   canonicalJSON,
		CanonicalSHA256: model.SHA256Hex(canonicalJSON),
	}
}

func TestStudyTestDatabaseErrorRedactsCause(t *testing.T) {
	const sentinel = "sentinel-password-must-not-appear"
	rendered := studyTestDatabaseError(
		"STUDY_TEST_POSTGRES.MIGRATION_FAILED",
		fmt.Errorf("parse postgres://operator:%s@127.0.0.1/test", sentinel),
	).Error()
	if strings.Contains(rendered, sentinel) || strings.Contains(rendered, "postgres://") {
		t.Fatalf("STUDY_TEST_POSTGRES.ERROR_REDACTION_FAILED: %s", rendered)
	}
}

func TestPlanLexinetStep2Verify1Item4MigrateDownMigrateUpAgainAndCompareThe(t *testing.T) {
	harness := startStudyTestPostgres(t)
	harness.migrateUp(t)
	pool := harness.openPool(t)

	contractContext, cancelContract := context.WithTimeout(context.Background(), 20*time.Second)
	before, beforeDigest, err := studySchemaContract(contractContext, pool)
	cancelContract()
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.SCHEMA_CAPTURE_BEFORE_FAILED", err))
	}
	if len(before) == 0 || beforeDigest == "" {
		t.Fatal("STUDY_TEST_POSTGRES.SCHEMA_CAPTURE_BEFORE_EMPTY")
	}

	harness.migrateStudyDown(t)
	assertStudyMigrationAbsent(t, pool)
	harness.migrateUp(t)

	contractContext, cancelContract = context.WithTimeout(context.Background(), 20*time.Second)
	after, afterDigest, err := studySchemaContract(contractContext, pool)
	cancelContract()
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.SCHEMA_CAPTURE_AFTER_FAILED", err))
	}
	if beforeDigest != afterDigest || string(before) != string(after) {
		t.Fatalf(
			"STUDY_TEST_POSTGRES.SCHEMA_CONTRACT_CHANGED: before=%s after=%s",
			beforeDigest,
			afterDigest,
		)
	}
}

func (harness *studyTestPostgres) openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return harness.Harness.OpenPool(t)
}

func (harness *studyTestPostgres) migrateUp(t *testing.T) {
	t.Helper()
	if err := Migrate(context.Background(), harness.DSN(t)); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.MIGRATE_UP_FAILED", err))
	}
}

func (harness *studyTestPostgres) migrateStudyDown(t *testing.T) {
	t.Helper()
	harness.AssertDestructiveOperationAllowed(t)
	migrator := harness.newMigrator(t)
	defer closeStudyTestMigrator(t, migrator)
	// Version 6 is the explicit pre-study target. This preserves the original
	// 0007 teardown proof now that 0008 adds a second study migration.
	if err := migrator.Migrate(6); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.MIGRATE_STUDY_DOWN_FAILED", err))
	}
}

func (harness *studyTestPostgres) newMigrator(t *testing.T) *migrate.Migrate {
	t.Helper()
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.MIGRATION_SOURCE_FAILED", err))
	}
	migrator, err := migrate.NewWithSourceInstance("iofs", source, RewriteMigrateDSN(harness.DSN(t)))
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.MIGRATOR_CREATE_FAILED", err))
	}
	return migrator
}

func closeStudyTestMigrator(t *testing.T, migrator *migrate.Migrate) {
	t.Helper()
	if migrator == nil {
		return
	}
	sourceErr, databaseErr := migrator.Close()
	if sourceErr != nil || databaseErr != nil {
		t.Errorf(
			"STUDY_TEST_POSTGRES.MIGRATOR_CLOSE_FAILED: source_failed=%t database_failed=%t",
			sourceErr != nil,
			databaseErr != nil,
		)
	}
}

// studySchemaContract returns timestamp-free canonical catalog evidence for
// every study-owned column, constraint, index, trigger, and function. Comparing
// the bytes before and after an explicit study migration down/up proves the
// same schema contract, not merely that both migration commands returned success.
func studySchemaContract(ctx context.Context, pool *pgxpool.Pool) ([]byte, string, error) {
	if pool == nil {
		return nil, "", errors.New("pool is required")
	}
	queries := []struct {
		kind string
		sql  string
	}{
		{
			kind: "relation",
			sql: `
				SELECT relation.relname, relation.relkind::text,
				       relation.relpersistence::text,
				       relation.relrowsecurity::text,
				       relation.relforcerowsecurity::text,
				       pg_get_userbyid(relation.relowner)
				FROM pg_class AS relation
				JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public' AND relation.relname LIKE 'study\_%' ESCAPE '\'
				ORDER BY relation.relname`,
		},
		{
			kind: "column",
			sql: `
				SELECT table_name, ordinal_position::text, column_name, data_type,
				       udt_name, is_nullable, coalesce(column_default, ''),
				       coalesce(character_maximum_length::text, '')
				FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name LIKE 'study\_%' ESCAPE '\'
				ORDER BY table_name, ordinal_position`,
		},
		{
			kind: "constraint",
			sql: `
				SELECT relation.relname, constraint_row.conname,
				       pg_get_constraintdef(constraint_row.oid, true)
				FROM pg_constraint AS constraint_row
				JOIN pg_class AS relation ON relation.oid = constraint_row.conrelid
				JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public' AND relation.relname LIKE 'study\_%' ESCAPE '\'
				ORDER BY relation.relname, constraint_row.conname`,
		},
		{
			kind: "index",
			sql: `
				SELECT relation.relname, index_relation.relname,
				       pg_get_indexdef(index_row.indexrelid)
				FROM pg_index AS index_row
				JOIN pg_class AS relation ON relation.oid = index_row.indrelid
				JOIN pg_class AS index_relation ON index_relation.oid = index_row.indexrelid
				JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public' AND relation.relname LIKE 'study\_%' ESCAPE '\'
				ORDER BY relation.relname, index_relation.relname`,
		},
		{
			kind: "trigger",
			sql: `
				SELECT relation.relname, trigger_row.tgname,
				       pg_get_triggerdef(trigger_row.oid, true)
				FROM pg_trigger AS trigger_row
				JOIN pg_class AS relation ON relation.oid = trigger_row.tgrelid
				JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public'
				  AND relation.relname LIKE 'study\_%' ESCAPE '\'
				  AND NOT trigger_row.tgisinternal
				ORDER BY relation.relname, trigger_row.tgname`,
		},
		{
			kind: "function",
			sql: `
				SELECT procedure.proname, pg_get_function_identity_arguments(procedure.oid),
				       pg_get_functiondef(procedure.oid)
				FROM pg_proc AS procedure
				JOIN pg_namespace AS namespace ON namespace.oid = procedure.pronamespace
				WHERE namespace.nspname = 'public' AND procedure.proname LIKE 'study\_%' ESCAPE '\'
				ORDER BY procedure.proname, pg_get_function_identity_arguments(procedure.oid)`,
		},
		{
			kind: "comment",
			sql: `
				SELECT relation.relname, 'table', '', coalesce(obj_description(relation.oid, 'pg_class'), '')
				FROM pg_class AS relation
				JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public' AND relation.relname LIKE 'study\_%' ESCAPE '\'
				UNION ALL
				SELECT relation.relname, 'column', attribute.attname,
				       coalesce(col_description(relation.oid, attribute.attnum), '')
				FROM pg_class AS relation
				JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				JOIN pg_attribute AS attribute ON attribute.attrelid = relation.oid
				WHERE namespace.nspname = 'public'
				  AND relation.relname LIKE 'study\_%' ESCAPE '\'
				  AND attribute.attnum > 0 AND NOT attribute.attisdropped
				ORDER BY 1, 2, 3`,
		},
		{
			kind: "policy",
			sql: `
				SELECT relation.relname, policy.polname, policy.polcmd::text,
				       policy.polpermissive::text, policy.polroles::text,
				       coalesce(pg_get_expr(policy.polqual, policy.polrelid), ''),
				       coalesce(pg_get_expr(policy.polwithcheck, policy.polrelid), '')
				FROM pg_policy AS policy
				JOIN pg_class AS relation ON relation.oid = policy.polrelid
				JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public' AND relation.relname LIKE 'study\_%' ESCAPE '\'
				ORDER BY relation.relname, policy.polname`,
		},
		{
			kind: "grant",
			sql: `
				SELECT table_name, grantor, grantee, privilege_type, is_grantable
				FROM information_schema.table_privileges
				WHERE table_schema = 'public' AND table_name LIKE 'study\_%' ESCAPE '\'
				ORDER BY table_name, grantor, grantee, privilege_type`,
		},
	}

	records := make([]string, 0, 128)
	for _, query := range queries {
		rows, err := pool.Query(ctx, query.sql)
		if err != nil {
			return nil, "", fmt.Errorf("query %s schema: %w", query.kind, err)
		}
		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				rows.Close()
				return nil, "", fmt.Errorf("read %s schema: %w", query.kind, err)
			}
			parts := make([]string, len(values)+1)
			parts[0] = query.kind
			for index, value := range values {
				parts[index+1] = fmt.Sprint(value)
			}
			records = append(records, strings.Join(parts, "\x1f"))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("iterate %s schema: %w", query.kind, err)
		}
		rows.Close()
	}

	sort.Strings(records)
	canonical := []byte(strings.Join(records, "\n") + "\n")
	digest := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(digest[:]), nil
}

func assertStudyMigrationAbsent(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if pool == nil {
		t.Fatal("STUDY_TEST_POSTGRES.POOL_MISSING")
	}
	verifyContext, cancelVerify := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelVerify()

	var ownedObjectCount int
	err := pool.QueryRow(verifyContext, `
		SELECT (
			SELECT count(*)
			FROM pg_class AS relation
			JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public' AND relation.relname LIKE 'study\_%' ESCAPE '\'
		) + (
			SELECT count(*)
			FROM pg_proc AS procedure
			JOIN pg_namespace AS namespace ON namespace.oid = procedure.pronamespace
			WHERE namespace.nspname = 'public' AND procedure.proname LIKE 'study\_%' ESCAPE '\'
		)`).Scan(&ownedObjectCount)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.DOWN_INSPECTION_FAILED", err))
	}
	if ownedObjectCount != 0 {
		t.Fatalf("STUDY_TEST_POSTGRES.DOWN_LEFT_OWNED_OBJECTS: count=%d", ownedObjectCount)
	}

	var version int
	var dirty bool
	if err := pool.QueryRow(verifyContext, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.VERSION_INSPECTION_FAILED", err))
	}
	if version != 6 || dirty {
		t.Fatalf("STUDY_TEST_POSTGRES.DOWN_VERSION_INVALID: version=%d dirty=%t", version, dirty)
	}
}
