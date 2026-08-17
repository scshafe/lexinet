// Package studypostgres owns the reusable, destructive-safe disposable
// Postgres lifecycle used by every study integration suite.
package studypostgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	migratedatabase "github.com/golang-migrate/migrate/v4/database"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DSNEnvironment         = "LEXINET_STUDY_TEST_DSN"
	postgresImage          = "postgres:16-alpine"
	postgresDataTmpfsMount = "type=tmpfs,destination=/var/lib/postgresql/data,tmpfs-size=536870912,tmpfs-mode=0700"
	namePrefix             = "lexinet-study-test-"
)

// Harness owns one isolated study integration database. An explicit DSN stays
// caller-owned; a default harness owns one random Docker container.
//
// @canonical study_disposable_postgres_test_harness -- storage, server, and
// CLI integration suites must use this lifecycle rather than starting their
// own database container.
type Harness struct {
	dsn           string
	containerName string
	databaseName  string
	owned         bool
	rollbackSafe  bool
	ownerToken    uuid.UUID
}

// Start creates or safely claims one disposable study database and registers
// cleanup on t. Failures contain stable codes and never expose a DSN.
func Start(t *testing.T) *Harness {
	t.Helper()

	if explicitDSN := strings.TrimSpace(os.Getenv(DSNEnvironment)); explicitDSN != "" {
		return openExplicit(t, explicitDSN)
	}

	randomID := strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	harness := &Harness{
		containerName: namePrefix + randomID,
		databaseName:  "study_" + randomID,
	}
	t.Cleanup(func() { harness.close(t) })

	startContext, cancelStart := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelStart()
	output, err := exec.CommandContext(
		startContext,
		"docker",
		ownedContainerRunArguments(harness.containerName)...,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("STUDY_TEST_POSTGRES.START_FAILED: cause_type=%T output=%s", err, boundedOutput(output))
	}
	harness.owned = true
	harness.rollbackSafe = true

	readyContext, cancelReady := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelReady()
	if err := waitForPostgres(readyContext, harness.containerName); err != nil {
		t.Fatalf("STUDY_TEST_POSTGRES.READINESS_FAILED: %v", err)
	}

	hostPort, err := postgresPort(readyContext, harness.containerName)
	if err != nil {
		t.Fatalf("STUDY_TEST_POSTGRES.PORT_RESOLUTION_FAILED: %v", err)
	}
	if err := createDatabase(readyContext, harness.containerName, harness.databaseName); err != nil {
		t.Fatal(err)
	}

	harness.dsn = fmt.Sprintf(
		"postgres://postgres@127.0.0.1:%s/%s?sslmode=disable",
		hostPort,
		harness.databaseName,
	)
	if err := waitForDatabase(readyContext, harness.dsn); err != nil {
		t.Fatal(err)
	}
	return harness
}

// DSN returns the connection string for the isolated database. Callers must
// never log it or place it in a command argument.
func (harness *Harness) DSN(t *testing.T) string {
	t.Helper()
	if harness == nil || strings.TrimSpace(harness.dsn) == "" {
		t.Fatal("STUDY_TEST_POSTGRES.DSN_MISSING")
	}
	return harness.dsn
}

// OpenPool opens and registers cleanup for a bounded pool to the isolated
// database. Driver failures are redacted to stable structured evidence.
func (harness *Harness) OpenPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := harness.DSN(t)
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.DSN_INVALID", err))
	}
	poolConfig.MaxConns = 12
	connectContext, cancelConnect := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelConnect()
	pool, err := pgxpool.NewWithConfig(connectContext, poolConfig)
	if err != nil {
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.CONNECT_FAILED", err))
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(connectContext); err != nil {
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.PING_FAILED", err))
	}
	return pool
}

// AssertDestructiveOperationAllowed proves that a destructive migration can
// target only the random owned container or the exact explicit ownership
// marker installed by this harness.
func (harness *Harness) AssertDestructiveOperationAllowed(t *testing.T) {
	t.Helper()
	if harness == nil || !harness.rollbackSafe {
		t.Fatal("STUDY_TEST_POSTGRES.DESTRUCTIVE_MIGRATION_NOT_PROVEN_SAFE")
	}
	if harness.owned {
		if !strings.HasPrefix(harness.containerName, namePrefix) {
			t.Fatal("STUDY_TEST_POSTGRES.DESTRUCTIVE_TARGET_INVALID")
		}
		return
	}
	harness.assertExplicitOwnership(t)
}

// RedactedDatabaseError converts database and migration failures into bounded,
// machine-readable evidence without driver messages, DSNs, or credentials.
func RedactedDatabaseError(code string, err error) error {
	if err == nil {
		return errors.New(code)
	}
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return redactedPostgresError(code, 0, postgresError)
	}
	if line, original, ok := migrationOriginalError(err); ok {
		if errors.As(original, &postgresError) {
			return redactedPostgresError(code, line, postgresError)
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

func createDatabase(ctx context.Context, containerName, databaseName string) error {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	var lastOutput []byte
	for {
		output, err := exec.CommandContext(
			ctx,
			"docker",
			"exec", containerName,
			"createdb", "--username", "postgres", databaseName,
		).CombinedOutput()
		if err == nil {
			return nil
		}
		lastOutput = output
		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"STUDY_TEST_POSTGRES.DATABASE_CREATE_FAILED: deadline output=%s",
				boundedOutput(lastOutput),
			)
		case <-ticker.C:
		}
	}
}

func waitForDatabase(ctx context.Context, dsn string) error {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			err = pool.Ping(ctx)
			pool.Close()
		}
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"STUDY_TEST_POSTGRES.FINAL_DATABASE_READINESS_FAILED: cause_type=%T",
				lastErr,
			)
		case <-ticker.C:
		}
	}
}

func openExplicit(t *testing.T, dsn string) *Harness {
	t.Helper()
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.EXPLICIT_DSN_INVALID", err))
	}
	if !strings.HasPrefix(poolConfig.ConnConfig.Database, "lexinet_study_test_") {
		t.Fatal("STUDY_TEST_POSTGRES.EXPLICIT_DATABASE_NAME_UNSAFE")
	}

	verifyContext, cancelVerify := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelVerify()
	pool, err := pgxpool.NewWithConfig(verifyContext, poolConfig)
	if err != nil {
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.EXPLICIT_CONNECT_FAILED", err))
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
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.EXPLICIT_INSPECTION_FAILED", err))
	}
	if currentDatabase != poolConfig.ConnConfig.Database || userRelationCount != 0 {
		t.Fatal("STUDY_TEST_POSTGRES.EXPLICIT_DATABASE_NOT_EMPTY")
	}
	ownerToken := uuid.New()
	ownershipTransaction, err := pool.Begin(verifyContext)
	if err != nil {
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_TRANSACTION_FAILED", err))
	}
	defer ownershipTransaction.Rollback(verifyContext)
	if _, err := ownershipTransaction.Exec(verifyContext, `
		CREATE TABLE lexinet_study_test_harness_owner (
			owner_token UUID PRIMARY KEY,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_CREATE_FAILED", err))
	}
	if _, err := ownershipTransaction.Exec(
		verifyContext,
		`INSERT INTO lexinet_study_test_harness_owner (owner_token) VALUES ($1)`,
		ownerToken,
	); err != nil {
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_BIND_FAILED", err))
	}
	if err := ownershipTransaction.Commit(verifyContext); err != nil {
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_COMMIT_FAILED", err))
	}

	harness := &Harness{
		dsn:          dsn,
		databaseName: currentDatabase,
		rollbackSafe: true,
		ownerToken:   ownerToken,
	}
	t.Cleanup(func() { harness.close(t) })
	return harness
}

func (harness *Harness) assertExplicitOwnership(t *testing.T) {
	t.Helper()
	poolConfig, err := pgxpool.ParseConfig(harness.dsn)
	if err != nil {
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_DSN_INVALID", err))
	}
	verifyContext, cancelVerify := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelVerify()
	pool, err := pgxpool.NewWithConfig(verifyContext, poolConfig)
	if err != nil {
		t.Fatal(RedactedDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_CONNECT_FAILED", err))
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

func (harness *Harness) close(t *testing.T) {
	t.Helper()
	if harness == nil {
		return
	}
	if !harness.owned {
		harness.releaseExplicitOwnership(t)
		return
	}
	if !strings.HasPrefix(harness.containerName, namePrefix) {
		t.Errorf("STUDY_TEST_POSTGRES.CLEANUP_TARGET_INVALID")
		return
	}
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelCleanup()
	output, err := exec.CommandContext(
		cleanupContext,
		"docker", ownedContainerCleanupArguments(harness.containerName)...,
	).CombinedOutput()
	if err != nil {
		t.Errorf("STUDY_TEST_POSTGRES.CLEANUP_FAILED: cause_type=%T output=%s", err, boundedOutput(output))
	}
	harness.owned = false
}

func ownedContainerRunArguments(containerName string) []string {
	return []string{
		"run",
		"--detach",
		"--name", containerName,
		"--env", "POSTGRES_HOST_AUTH_METHOD=trust",
		"--publish", "127.0.0.1::5432",
		"--mount", postgresDataTmpfsMount,
		postgresImage,
	}
}

func ownedContainerCleanupArguments(containerName string) []string {
	return []string{"rm", "--force", "--volumes", containerName}
}

func (harness *Harness) releaseExplicitOwnership(t *testing.T) {
	t.Helper()
	if harness.ownerToken == uuid.Nil || strings.TrimSpace(harness.dsn) == "" {
		return
	}
	poolConfig, err := pgxpool.ParseConfig(harness.dsn)
	if err != nil {
		t.Error(RedactedDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_RELEASE_DSN_INVALID", err))
		return
	}
	cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCleanup()
	pool, err := pgxpool.NewWithConfig(cleanupContext, poolConfig)
	if err != nil {
		t.Error(RedactedDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_RELEASE_CONNECT_FAILED", err))
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
		t.Error(RedactedDatabaseError("STUDY_TEST_POSTGRES.OWNERSHIP_DROP_FAILED", err))
		return
	}
	harness.ownerToken = uuid.Nil
	harness.rollbackSafe = false
}

func waitForPostgres(ctx context.Context, containerName string) error {
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
			return fmt.Errorf("readiness deadline: %w (output=%s)", ctx.Err(), boundedOutput(output))
		case <-ticker.C:
		}
	}
}

func postgresPort(ctx context.Context, containerName string) (string, error) {
	output, err := exec.CommandContext(
		ctx,
		"docker", "port", containerName, "5432/tcp",
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker port: cause_type=%T output=%s", err, boundedOutput(output))
	}
	lines := strings.Fields(strings.TrimSpace(string(output)))
	if len(lines) != 1 {
		return "", fmt.Errorf("unexpected mapped-port count: %d", len(lines))
	}
	host, port, err := net.SplitHostPort(lines[0])
	if err != nil {
		return "", fmt.Errorf("parse mapped port: cause_type=%T", err)
	}
	if host != "127.0.0.1" || port == "" {
		return "", errors.New("mapped port is not loopback-only")
	}
	return port, nil
}

func migrationOriginalError(err error) (uint, error, bool) {
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

func redactedPostgresError(code string, line uint, postgresError *pgconn.PgError) error {
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

func boundedOutput(output []byte) string {
	const maximum = 2048
	normalized := strings.TrimSpace(string(output))
	if len(normalized) <= maximum {
		return normalized
	}
	return normalized[len(normalized)-maximum:]
}
