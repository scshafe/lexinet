package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratedatabase "github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lexinet/migrations"
)

const (
	studyTestPostgresDSNEnvironment = "LEXINET_STUDY_TEST_DSN"
	studyTestPostgresImage          = "postgres:16-alpine"
	studyTestPostgresNamePrefix     = "lexinet-study-test-"
)

// studyTestPostgres owns an isolated database used by the Step 2 integration
// suite. An explicit test DSN remains caller-owned and is never logged or
// removed. The default path owns one random Docker container and database.
type studyTestPostgres struct {
	dsn           string
	containerName string
	databaseName  string
	owned         bool
	rollbackSafe  bool
	ownerToken    uuid.UUID
}

func startStudyTestPostgres(t *testing.T) *studyTestPostgres {
	t.Helper()

	if explicitDSN := strings.TrimSpace(os.Getenv(studyTestPostgresDSNEnvironment)); explicitDSN != "" {
		return openExplicitStudyTestPostgres(t, explicitDSN)
	}

	randomID := strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	harness := &studyTestPostgres{
		containerName: studyTestPostgresNamePrefix + randomID,
		databaseName:  "study_" + randomID,
	}
	t.Cleanup(func() { harness.close(t) })

	startContext, cancelStart := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelStart()
	output, err := exec.CommandContext(
		startContext,
		"docker",
		"run",
		"--detach",
		"--name", harness.containerName,
		"--env", "POSTGRES_HOST_AUTH_METHOD=trust",
		"--publish", "127.0.0.1::5432",
		studyTestPostgresImage,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("STUDY_TEST_POSTGRES.START_FAILED: %v (%s)", err, boundedTestOutput(output))
	}
	harness.owned = true
	harness.rollbackSafe = true

	readyContext, cancelReady := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelReady()
	if err := waitForStudyTestPostgres(readyContext, harness.containerName); err != nil {
		t.Fatalf("STUDY_TEST_POSTGRES.READINESS_FAILED: %v", err)
	}

	hostPort, err := studyTestPostgresPort(readyContext, harness.containerName)
	if err != nil {
		t.Fatalf("STUDY_TEST_POSTGRES.PORT_RESOLUTION_FAILED: %v", err)
	}
	createOutput, err := exec.CommandContext(
		readyContext,
		"docker",
		"exec", harness.containerName,
		"createdb", "--username", "postgres", harness.databaseName,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("STUDY_TEST_POSTGRES.DATABASE_CREATE_FAILED: %v (%s)", err, boundedTestOutput(createOutput))
	}

	harness.dsn = fmt.Sprintf(
		"postgres://postgres@127.0.0.1:%s/%s?sslmode=disable",
		hostPort,
		harness.databaseName,
	)
	return harness
}

func openExplicitStudyTestPostgres(t *testing.T, dsn string) *studyTestPostgres {
	t.Helper()
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.EXPLICIT_DSN_INVALID", err))
	}
	if !strings.HasPrefix(poolConfig.ConnConfig.Database, "lexinet_study_test_") {
		t.Fatal("STUDY_TEST_POSTGRES.EXPLICIT_DATABASE_NAME_UNSAFE")
	}

	verifyContext, cancelVerify := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelVerify()
	pool, err := pgxpool.NewWithConfig(verifyContext, poolConfig)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.EXPLICIT_CONNECT_FAILED", err))
	}
	defer pool.Close()

	var currentDatabase string
	var userRelationCount int
	err = pool.QueryRow(verifyContext, `
		SELECT current_database(), (
			SELECT count(*)
			FROM pg_class AS relation
			JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname NOT IN ('pg_catalog', 'information_schema')
			  AND namespace.nspname NOT LIKE 'pg_toast%'
			  AND relation.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
		)`).Scan(&currentDatabase, &userRelationCount)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.EXPLICIT_INSPECTION_FAILED", err))
	}
	if currentDatabase != poolConfig.ConnConfig.Database || userRelationCount != 0 {
		t.Fatal("STUDY_TEST_POSTGRES.EXPLICIT_DATABASE_NOT_EMPTY")
	}
	ownerToken := uuid.New()
	ownershipTransaction, err := pool.Begin(verifyContext)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_TRANSACTION_FAILED", err))
	}
	defer ownershipTransaction.Rollback(verifyContext)
	if _, err := ownershipTransaction.Exec(verifyContext, `
		CREATE TABLE lexinet_study_test_harness_owner (
			owner_token UUID PRIMARY KEY,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_CREATE_FAILED", err))
	}
	if _, err := ownershipTransaction.Exec(
		verifyContext,
		`INSERT INTO lexinet_study_test_harness_owner (owner_token) VALUES ($1)`,
		ownerToken,
	); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_BIND_FAILED", err))
	}
	if err := ownershipTransaction.Commit(verifyContext); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_COMMIT_FAILED", err))
	}

	harness := &studyTestPostgres{
		dsn:          dsn,
		databaseName: currentDatabase,
		rollbackSafe: true,
		ownerToken:   ownerToken,
	}
	t.Cleanup(func() { harness.close(t) })
	return harness
}

func studyTestDatabaseError(code string, err error) error {
	if err == nil {
		return errors.New(code)
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return studyTestPostgresError(code, 0, postgresError)
	}
	if line, original, ok := studyTestMigrationOriginalError(err); ok {
		if errors.As(original, &postgresError) {
			return studyTestPostgresError(code, line, postgresError)
		}
		return fmt.Errorf("%s: migration_line=%d cause_type=%T", code, line, original)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: deadline_exceeded", code)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: canceled", code)
	}
	return fmt.Errorf("%s: cause_type=%T", code, err)
}

func studyTestMigrationOriginalError(err error) (uint, error, bool) {
	var pointerError *migratedatabase.Error
	if errors.As(err, &pointerError) && pointerError != nil {
		return pointerError.Line, pointerError.OrigErr, true
	}
	var valueError migratedatabase.Error
	if errors.As(err, &valueError) {
		return valueError.Line, valueError.OrigErr, true
	}
	return 0, nil, false
}

func studyTestPostgresError(code string, line uint, postgresError *pgconn.PgError) error {
	if postgresError == nil {
		return fmt.Errorf("%s: migration_line=%d", code, line)
	}
	return fmt.Errorf(
		"%s: migration_line=%d sqlstate=%s table=%s constraint=%s column=%s",
		code,
		line,
		postgresError.Code,
		postgresError.TableName,
		postgresError.ConstraintName,
		postgresError.ColumnName,
	)
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
	if harness == nil || strings.TrimSpace(harness.dsn) == "" {
		t.Fatal("STUDY_TEST_POSTGRES.DSN_MISSING")
	}
	poolConfig, err := pgxpool.ParseConfig(harness.dsn)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.DSN_INVALID", err))
	}
	poolConfig.MaxConns = 12
	connectContext, cancelConnect := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelConnect()
	pool, err := pgxpool.NewWithConfig(connectContext, poolConfig)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.CONNECT_FAILED", err))
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(connectContext); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.PING_FAILED", err))
	}
	return pool
}

func (harness *studyTestPostgres) migrateUp(t *testing.T) {
	t.Helper()
	if err := Migrate(context.Background(), harness.dsn); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.MIGRATE_UP_FAILED", err))
	}
}

func (harness *studyTestPostgres) migrateStudyDown(t *testing.T) {
	t.Helper()
	if harness == nil || !harness.rollbackSafe {
		t.Fatal("STUDY_TEST_POSTGRES.DESTRUCTIVE_MIGRATION_NOT_PROVEN_SAFE")
	}
	if !harness.owned {
		harness.assertExplicitOwnership(t)
	}
	migrator := harness.newMigrator(t)
	defer closeStudyTestMigrator(t, migrator)
	if err := migrator.Steps(-1); err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.MIGRATE_STUDY_DOWN_FAILED", err))
	}
}

func (harness *studyTestPostgres) newMigrator(t *testing.T) *migrate.Migrate {
	t.Helper()
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.MIGRATION_SOURCE_FAILED", err))
	}
	migrator, err := migrate.NewWithSourceInstance("iofs", source, RewriteMigrateDSN(harness.dsn))
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

func (harness *studyTestPostgres) close(t *testing.T) {
	t.Helper()
	if harness == nil {
		return
	}
	if !harness.owned {
		harness.releaseExplicitOwnership(t)
		return
	}
	if !strings.HasPrefix(harness.containerName, studyTestPostgresNamePrefix) {
		t.Errorf("STUDY_TEST_POSTGRES.CLEANUP_TARGET_INVALID")
		return
	}
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelCleanup()
	output, err := exec.CommandContext(
		cleanupContext,
		"docker", "rm", "--force", harness.containerName,
	).CombinedOutput()
	if err != nil {
		t.Errorf("STUDY_TEST_POSTGRES.CLEANUP_FAILED: %v (%s)", err, boundedTestOutput(output))
	}
	harness.owned = false
}

func (harness *studyTestPostgres) assertExplicitOwnership(t *testing.T) {
	t.Helper()
	poolConfig, err := pgxpool.ParseConfig(harness.dsn)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_DSN_INVALID", err))
	}
	verifyContext, cancelVerify := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelVerify()
	pool, err := pgxpool.NewWithConfig(verifyContext, poolConfig)
	if err != nil {
		t.Fatal(studyTestDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_CONNECT_FAILED", err))
	}
	defer pool.Close()
	var boundToken uuid.UUID
	err = pool.QueryRow(
		verifyContext,
		`SELECT owner_token FROM lexinet_study_test_harness_owner`,
	).Scan(&boundToken)
	if err != nil || boundToken != harness.ownerToken {
		t.Fatal("STUDY_TEST_POSTGRES.EXPLICIT_OWNERSHIP_LOST")
	}
}

func (harness *studyTestPostgres) releaseExplicitOwnership(t *testing.T) {
	t.Helper()
	if harness.ownerToken == uuid.Nil || strings.TrimSpace(harness.dsn) == "" {
		return
	}
	poolConfig, err := pgxpool.ParseConfig(harness.dsn)
	if err != nil {
		t.Error(studyTestDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_RELEASE_DSN_INVALID", err))
		return
	}
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCleanup()
	pool, err := pgxpool.NewWithConfig(cleanupContext, poolConfig)
	if err != nil {
		t.Error(studyTestDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_RELEASE_CONNECT_FAILED", err))
		return
	}
	defer pool.Close()
	command, err := pool.Exec(
		cleanupContext,
		`DELETE FROM lexinet_study_test_harness_owner WHERE owner_token = $1`,
		harness.ownerToken,
	)
	if err != nil || command.RowsAffected() != 1 {
		t.Error("STUDY_TEST_POSTGRES.OWNERSHIP_RELEASE_FAILED")
		return
	}
	if _, err := pool.Exec(cleanupContext, `DROP TABLE lexinet_study_test_harness_owner`); err != nil {
		t.Error(studyTestDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_DROP_FAILED", err))
		return
	}
	harness.ownerToken = uuid.Nil
	harness.rollbackSafe = false
}

func waitForStudyTestPostgres(ctx context.Context, containerName string) error {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		output, err := exec.CommandContext(
			ctx,
			"docker", "exec", containerName,
			"pg_isready", "--username", "postgres", "--dbname", "postgres",
		).CombinedOutput()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("readiness deadline: %w (%s)", ctx.Err(), boundedTestOutput(output))
		case <-ticker.C:
		}
	}
}

func studyTestPostgresPort(ctx context.Context, containerName string) (string, error) {
	output, err := exec.CommandContext(
		ctx,
		"docker", "port", containerName, "5432/tcp",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker port: %w (%s)", err, boundedTestOutput(output))
	}
	lines := strings.Fields(strings.TrimSpace(string(output)))
	if len(lines) != 1 {
		return "", fmt.Errorf("unexpected mapped-port count: %d", len(lines))
	}
	host, port, err := net.SplitHostPort(lines[0])
	if err != nil {
		return "", fmt.Errorf("parse mapped port: %w", err)
	}
	if host != "127.0.0.1" || port == "" {
		return "", fmt.Errorf("mapped port is not loopback-only")
	}
	return port, nil
}

func boundedTestOutput(output []byte) string {
	const maximum = 2048
	normalized := strings.TrimSpace(string(output))
	if len(normalized) <= maximum {
		return normalized
	}
	return normalized[len(normalized)-maximum:]
}

// studySchemaContract returns timestamp-free canonical catalog evidence for
// every study-owned column, constraint, index, trigger, and function. Comparing
// the bytes before and after 0007 down/up proves the same schema contract, not
// merely that both migration commands returned success.
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
