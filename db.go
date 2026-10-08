package strata

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
)

// schemaSQL creates the strata_migrations table and the functions that
// operate on it.
//
//go:embed schema.sql
var schemaSQL string

// execer is implemented by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// querier is implemented by both *sql.DB and *sql.Tx.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// MigrationStatus is the result of checking a migration file against the
// strata_migrations table.
type MigrationStatus uint8

const (
	// MigrationNotApplied indicates the migration is new, and has yet to be
	// applied.
	MigrationNotApplied MigrationStatus = iota

	// MigrationApplied indicates the migration was previously applied.
	MigrationApplied

	// MigrationChecksumMismatch indicates the migration was applied, but the
	// source file has changed and is now invalid.
	MigrationChecksumMismatch
)

// migrationLockSQL takes an advisory lock that serializes database calls
// from CreateMigrationsTable and applyMigration. The key is generated from
// this project's name (strata) and current schema to prevent conflicts.
const migrationLockSQL = "SELECT pg_advisory_xact_lock(hashtextextended('strata:' || $1, 0))"

// withMigrationLock runs fn in a READ COMMITTED transaction holding the
// migration lock, and commits if fn succeeds. READ COMMITTED lets fn see rows
// committed by other processes while it waited for the lock, which a
// REPEATABLE READ or SERIALIZABLE snapshot would not. The lock is released
// when the transaction commits or rolls back.
func withMigrationLock(ctx context.Context, db *sql.DB, op string, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("%s: begin: %w", op, err)
	}
	// Rollback is a no-op once the transaction has committed.
	defer func() { _ = tx.Rollback() }()

	// The advisory lock serializes callers across processes. This is
	// important at scale when multiple instances of a service may be
	// starting simultaneously. current_schema() is NULL when no schema on
	// search_path exists, and pg_advisory_xact_lock(NULL) silently takes no
	// lock, so reject that case.
	var schema sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return fmt.Errorf("%s: current schema: %w", op, err)
	}
	if !schema.Valid {
		return fmt.Errorf("%s: no current schema; check search_path", op)
	}
	if _, err := tx.ExecContext(ctx, migrationLockSQL, schema.String); err != nil {
		return fmt.Errorf("%s: lock: %w", op, err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}

	return nil
}

// CreateMigrationsTable creates the strata plumbing if it does not already
// exist. It holds the migration lock while it runs, so it is safe to call
// from several processes at once.
func CreateMigrationsTable(ctx context.Context, db *sql.DB) error {
	return withMigrationLock(ctx, db, "create migrations schema", func(tx *sql.Tx) error {
		// Run all plumbing SQL statements at once.
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("create migrations schema: %w", err)
		}

		return nil
	})
}

// CheckMigration reports the current status of migration `fileName`.
func CheckMigration(ctx context.Context, db querier, fileName string, checksum []byte) (MigrationStatus, error) {
	var status MigrationStatus
	err := db.QueryRowContext(ctx, "SELECT strata_check_migration($1, $2)", fileName, checksum).Scan(&status)
	if err != nil {
		return 0, fmt.Errorf("check migration %q: %w", fileName, err)
	}

	return status, nil
}

// RecordMigration adds a migration file and hash to the `strata_migrations`
// table. Pass the same `*sql.Tx` used to run the migration as `db` so the
// migration and its record can commit or roll back together.
func RecordMigration(ctx context.Context, db execer, fileName string, checksum []byte) error {
	_, err := db.ExecContext(ctx, "SELECT strata_record_migration($1, $2)", fileName, checksum)
	if err != nil {
		return fmt.Errorf("record migration %q: %w", fileName, err)
	}

	return nil
}

// checksumMismatchError reports that fileName was applied with a different
// checksum.
func checksumMismatchError(fileName string) error {
	return fmt.Errorf("migration %q: already applied with a different checksum", fileName)
}

// outOfOrderError reports that fileName is unapplied but sorts before the
// applied migration later.
func outOfOrderError(fileName, later string) error {
	return fmt.Errorf("migration %q: not applied, but sorts before applied migration %q", fileName, later)
}

// applyMigration runs migrationSQL against db and records fileName with
// checksum in strata_migrations. Both happen in a single transaction, so if
// either fails, nothing is applied. migrationSQL may contain several
// statements, but none that cannot run inside a transaction block, such as
// CREATE INDEX CONCURRENTLY or VACUUM, and no transaction control statements
// such as BEGIN or COMMIT. A migration whose name sorts before an applied
// migration is rejected, since it was written against an older schema.
//
// Also, each migration file is run within a transaction-level advisory lock.
// This prevents concurrent clients from applying the same migration at the
// same time. This would commonly happen if the same service was starting in
// multiple cluster nodes.
func applyMigration(ctx context.Context, db *sql.DB, fileName string, migrationSQL string, checksum []byte) error {
	return withMigrationLock(ctx, db, fmt.Sprintf("apply migration %q", fileName), func(tx *sql.Tx) error {
		return applyLocked(ctx, tx, fileName, migrationSQL, checksum)
	})
}

// applyLocked is applyMigration's body, run under the migration lock.
func applyLocked(ctx context.Context, tx *sql.Tx, fileName string, migrationSQL string, checksum []byte) error {
	status, err := CheckMigration(ctx, tx, fileName, checksum)
	if err != nil {
		return err
	}

	switch status {
	case MigrationApplied:
		// Another caller applied it while we waited for the lock.
		return nil
	case MigrationChecksumMismatch:
		return checksumMismatchError(fileName)
	case MigrationNotApplied:
	default:
		return fmt.Errorf("migration %q: unknown status %d", fileName, status)
	}

	// Check the order under the lock, so a later migration applied by
	// another process since the caller last looked is seen. COLLATE "C"
	// compares bytes, matching Go's string ordering.
	var later string
	err = tx.QueryRowContext(ctx,
		`SELECT file_name FROM strata_migrations
		WHERE file_name COLLATE "C" > $1 COLLATE "C"
		ORDER BY file_name COLLATE "C" DESC LIMIT 1`, fileName).Scan(&later)
	switch {
	case err == nil:
		return outOfOrderError(fileName, later)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("apply migration %q: check order: %w", fileName, err)
	}

	// migrationSQL is sent without arguments, so the driver uses the simple
	// query protocol, which allows multiple statements.
	if _, err := tx.ExecContext(ctx, migrationSQL); err != nil {
		return fmt.Errorf("apply migration %q: %w", fileName, err)
	}

	return RecordMigration(ctx, tx, fileName, checksum)
}
