package strata

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aegisdesign-io/assert"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// testDatabaseURLEnv names the environment variable holding the URL of a
// PostgreSQL 18 database for the integration tests, for example
// postgres://user@localhost:5432/strata_test. The tests are skipped when it
// is unset.
const testDatabaseURLEnv = "STRATA_TEST_DATABASE_URL"

// openTestDB returns a connection to the test database whose search_path is a
// new, empty schema, so each test has its own strata_migrations table and
// functions. The schema is dropped when the test ends.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	rawURL := os.Getenv(testDatabaseURLEnv)
	if rawURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}
	ctx := context.Background()

	admin, err := sql.Open("pgx", rawURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	schema := "strata_test_" + hex.EncodeToString(suffix)
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})

	// pgx sends unrecognized URL parameters as run-time parameters, so every
	// pooled connection starts with this search_path.
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

// openMigrationsDB is openTestDB with the migrations schema already created.
func openMigrationsDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openTestDB(t)
	if err := CreateMigrationsTable(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

// queryInt runs query, which must return a single integer.
func queryInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func recordedCount(t *testing.T, db *sql.DB, fileName string) int {
	t.Helper()
	return queryInt(t, db, "SELECT count(*) FROM strata_migrations WHERE file_name = $1", fileName)
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	return queryInt(t, db, "SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1", table) == 1
}

func TestCreateMigrationsTable_IsIdempotent(t *testing.T) {
	Assert := assert.With(t)
	db := openTestDB(t)
	ctx := context.Background()

	Assert.
		That(CreateMigrationsTable(ctx, db)).
		IsOk()
	Assert.
		That(CreateMigrationsTable(ctx, db)).
		IsOk()
	Assert.
		That(tableExists(t, db, "strata_migrations")).
		IsTrue()
}

func TestCheckMigration_Statuses(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()
	sum := checksum("SELECT 1;")

	status, err := CheckMigration(ctx, db, "001.sql", sum)
	Assert.
		That(err).
		IsOk()
	Assert.
		That(status).
		IsEqualTo(MigrationNotApplied)

	Assert.
		That(RecordMigration(ctx, db, "001.sql", sum)).
		IsOk()

	status, err = CheckMigration(ctx, db, "001.sql", sum)
	Assert.
		That(err).
		IsOk()
	Assert.
		That(status).
		IsEqualTo(MigrationApplied)

	status, err = CheckMigration(ctx, db, "001.sql", checksum("SELECT 2;"))
	Assert.
		That(err).
		IsOk()
	Assert.
		That(status).
		IsEqualTo(MigrationChecksumMismatch)
}

func TestCheckMigration_WithoutSchema(t *testing.T) {
	Assert := assert.With(t)
	db := openTestDB(t)

	_, err := CheckMigration(context.Background(), db, "001.sql", checksum("SELECT 1;"))
	Assert.
		That(err).
		IsError()
}

func TestRecordMigration_RejectsDuplicateFileName(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()

	Assert.
		That(RecordMigration(ctx, db, "001.sql", checksum("SELECT 1;"))).
		IsOk()
	Assert.
		That(RecordMigration(ctx, db, "001.sql", checksum("SELECT 1;"))).
		IsError()
	Assert.
		That(recordedCount(t, db, "001.sql")).
		IsEqualTo(1)
}

func TestRecordMigration_RejectsShortChecksum(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)

	err := RecordMigration(context.Background(), db, "001.sql", []byte{1, 2, 3})
	Assert.
		That(err).
		IsError()
	Assert.
		That(recordedCount(t, db, "001.sql")).
		IsEqualTo(0)
}

func TestApplyMigration_AppliesAndRecords(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	migration := "CREATE TABLE a (id INT); INSERT INTO a VALUES (1), (2);"

	err := applyMigration(context.Background(), db, "001.sql", migration, checksum(migration))
	Assert.
		That(err).
		IsOk()
	Assert.
		That(queryInt(t, db, "SELECT count(*) FROM a")).
		IsEqualTo(2)
	Assert.
		That(recordedCount(t, db, "001.sql")).
		IsEqualTo(1)
}

func TestApplyMigration_FailureRollsBack(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	// The first statement succeeds and the second fails, so the whole
	// migration must be rolled back.
	migration := "CREATE TABLE a (id INT); SELECT * FROM missing;"

	err := applyMigration(context.Background(), db, "001.sql", migration, checksum(migration))
	Assert.
		That(err).
		IsError()
	Assert.
		That(tableExists(t, db, "a")).
		IsFalse()
	Assert.
		That(recordedCount(t, db, "001.sql")).
		IsEqualTo(0)
}

func TestApplyMigration_RecordFailureRollsBack(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	migration := "CREATE TABLE a (id INT);"

	// A short checksum fails the CHECK constraint when recording, after the
	// migration itself has run.
	err := applyMigration(context.Background(), db, "001.sql", migration, []byte{1, 2, 3})
	Assert.
		That(err).
		IsError()
	Assert.
		That(tableExists(t, db, "a")).
		IsFalse()
}

func TestApplyMigration_AlreadyAppliedIsSkipped(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE runs (id INT)"); err != nil {
		t.Fatal(err)
	}
	migration := "INSERT INTO runs VALUES (1);"

	Assert.
		That(applyMigration(ctx, db, "001.sql", migration, checksum(migration))).
		IsOk()
	Assert.
		That(applyMigration(ctx, db, "001.sql", migration, checksum(migration))).
		IsOk()
	Assert.
		That(queryInt(t, db, "SELECT count(*) FROM runs")).
		IsEqualTo(1)
}

func TestApplyMigration_ChecksumMismatch(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE runs (id INT)"); err != nil {
		t.Fatal(err)
	}
	migration := "INSERT INTO runs VALUES (1);"
	changed := "INSERT INTO runs VALUES (2);"

	Assert.
		That(applyMigration(ctx, db, "001.sql", migration, checksum(migration))).
		IsOk()
	Assert.
		That(applyMigration(ctx, db, "001.sql", changed, checksum(changed))).
		IsError()
	Assert.
		That(queryInt(t, db, "SELECT count(*) FROM runs")).
		IsEqualTo(1)
}

func TestApplyMigration_ConcurrentCallersApplyOnce(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE runs (id INT)"); err != nil {
		t.Fatal(err)
	}
	// CREATE TABLE fails if run twice, and the insert counts the runs.
	migration := "CREATE TABLE a (id INT); INSERT INTO runs VALUES (1);"

	const callers = 8
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			errs[i] = applyMigration(ctx, db, "001.sql", migration, checksum(migration))
		})
	}
	wg.Wait()

	for _, err := range errs {
		Assert.
			That(err).
			IsOk()
	}
	Assert.
		That(queryInt(t, db, "SELECT count(*) FROM runs")).
		IsEqualTo(1)
	Assert.
		That(recordedCount(t, db, "001.sql")).
		IsEqualTo(1)
}

func TestMigrateDir_AppliesInOrderAndIsRerunnable(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	// 002 depends on 001, so they must be applied in name order.
	writeFile(t, dir, "002_insert.sql", "INSERT INTO a VALUES (1);")
	writeFile(t, dir, "001_create.sql", "CREATE TABLE a (id INT);")

	Assert.
		That(MigrateDir(ctx, db, dir)).
		IsOk()
	Assert.
		That(MigrateDir(ctx, db, dir)).
		IsOk()
	Assert.
		That(queryInt(t, db, "SELECT count(*) FROM a")).
		IsEqualTo(1)
	Assert.
		That(queryInt(t, db, "SELECT count(*) FROM strata_migrations")).
		IsEqualTo(2)
}

func TestMigrateDir_ChangedFileIsRejected(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, dir, "001_create.sql", "CREATE TABLE a (id INT);")

	Assert.
		That(MigrateDir(ctx, db, dir)).
		IsOk()

	writeFile(t, dir, "001_create.sql", "CREATE TABLE a (id BIGINT);")
	Assert.
		That(MigrateDir(ctx, db, dir)).
		IsError()
}

func TestMigrateDir_StopsAtFirstFailure(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	dir := t.TempDir()
	writeFile(t, dir, "001_ok.sql", "CREATE TABLE a (id INT);")
	writeFile(t, dir, "002_bad.sql", "SELECT * FROM missing;")
	writeFile(t, dir, "003_ok.sql", "CREATE TABLE c (id INT);")

	Assert.
		That(MigrateDir(context.Background(), db, dir)).
		IsError()
	Assert.
		That(recordedCount(t, db, "001_ok.sql")).
		IsEqualTo(1)
	Assert.
		That(recordedCount(t, db, "002_bad.sql")).
		IsEqualTo(0)
	Assert.
		That(tableExists(t, db, "c")).
		IsFalse()
}

func TestMigrateDir_StopsAtUnreadableFile(t *testing.T) {
	Assert := assert.With(t)
	if os.Geteuid() == 0 {
		t.Skip("root can read files without read permission")
	}
	db := openMigrationsDB(t)
	dir := t.TempDir()
	writeFile(t, dir, "001_ok.sql", "CREATE TABLE a (id INT);")
	writeFile(t, dir, "002_locked.sql", "CREATE TABLE b (id INT);")
	writeFile(t, dir, "003_ok.sql", "CREATE TABLE c (id INT);")
	if err := os.Chmod(filepath.Join(dir, "002_locked.sql"), 0o000); err != nil {
		t.Fatal(err)
	}

	Assert.
		That(MigrateDir(context.Background(), db, dir)).
		IsError()
	Assert.
		That(recordedCount(t, db, "001_ok.sql")).
		IsEqualTo(1)
	Assert.
		That(recordedCount(t, db, "002_locked.sql")).
		IsEqualTo(0)
	Assert.
		That(tableExists(t, db, "c")).
		IsFalse()
}

func TestMigrateDir_RejectsOutOfOrderFile(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, dir, "001_create.sql", "CREATE TABLE a (id INT);")
	writeFile(t, dir, "003_create.sql", "CREATE TABLE c (id INT);")
	Assert.
		That(MigrateDir(ctx, db, dir)).
		IsOk()

	// 002 arrives after 003 was applied, for example from a merged branch.
	writeFile(t, dir, "002_create.sql", "CREATE TABLE b (id INT);")
	writeFile(t, dir, "004_create.sql", "CREATE TABLE d (id INT);")
	Assert.
		That(MigrateDir(ctx, db, dir)).
		IsError()
	Assert.
		That(tableExists(t, db, "b")).
		IsFalse()
	Assert.
		That(tableExists(t, db, "d")).
		IsFalse()
}

func TestMigrateDir_EmptyDirCreatesTable(t *testing.T) {
	Assert := assert.With(t)
	db := openTestDB(t)

	Assert.
		That(MigrateDir(context.Background(), db, t.TempDir())).
		IsOk()
	Assert.
		That(tableExists(t, db, "strata_migrations")).
		IsTrue()
}

// Another process may apply a later migration after Migrate reads the
// applied set, so applyMigration repeats the order check under the lock.
func TestApplyMigration_RejectsOutOfOrder(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()
	later := "CREATE TABLE c (id INT);"
	earlier := "CREATE TABLE b (id INT);"

	Assert.
		That(applyMigration(ctx, db, "003_create.sql", later, checksum(later))).
		IsOk()
	Assert.
		That(applyMigration(ctx, db, "002_create.sql", earlier, checksum(earlier))).
		IsError()
	Assert.
		That(tableExists(t, db, "b")).
		IsFalse()
	Assert.
		That(recordedCount(t, db, "002_create.sql")).
		IsEqualTo(0)
}

func TestMigrate_RecognizesFilesAppliedFromDisk(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "001_create.sql", "CREATE TABLE a (id INT);")

	Assert.
		That(MigrateDir(ctx, db, dir)).
		IsOk()
	// Rerunning from an fs.FS would fail on CREATE TABLE if the file were
	// not recognized as already applied.
	fsys, err := fs.Sub(os.DirFS(root), "migrations")
	Assert.
		That(err).
		IsOk()
	Assert.
		That(Migrate(ctx, db, fsys)).
		IsOk()
	Assert.
		That(recordedCount(t, db, "001_create.sql")).
		IsEqualTo(1)
}

func TestMigrateDir_RejectsRenamedMigration(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	// Idempotent content, so rerunning it under the new name would succeed
	// and only the rename detection can reject it.
	writeFile(t, dir, "002_add_col.sql", "SELECT 1;")
	Assert.
		That(MigrateDir(ctx, db, dir)).
		IsOk()

	if err := os.Rename(filepath.Join(dir, "002_add_col.sql"), filepath.Join(dir, "002_add_column.sql")); err != nil {
		t.Fatal(err)
	}
	Assert.
		That(MigrateDir(ctx, db, dir)).
		IsError()
	Assert.
		That(recordedCount(t, db, "002_add_column.sql")).
		IsEqualTo(0)
}

func TestMigrateDir_AllowsNewerAppliedMigration(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, dir, "001_a.sql", "SELECT 1;")
	writeFile(t, dir, "002_b.sql", "SELECT 1;")
	Assert.
		That(MigrateDir(ctx, db, dir)).
		IsOk()

	// An older release, without 002_b.sql, must still start.
	if err := os.Remove(filepath.Join(dir, "002_b.sql")); err != nil {
		t.Fatal(err)
	}
	Assert.
		That(MigrateDir(ctx, db, dir)).
		IsOk()
}

func TestCreateMigrationsTable_RejectsNullCurrentSchema(t *testing.T) {
	Assert := assert.With(t)
	db := openMigrationsDB(t)
	ctx := context.Background()

	// A single connection, so the search_path applies to the call below.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "SET search_path = strata_no_such_schema"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.ExecContext(ctx, "RESET search_path") }()

	Assert.
		That(CreateMigrationsTable(ctx, db)).
		IsError()
}
