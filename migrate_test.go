package strata

import (
	"context"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aegisdesign-io/assert"
)

// writeFile creates name under dir with content, failing the test on error.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// migrationsDir returns a temporary directory holding two migrations written
// out of order, a subdirectory that itself holds a migration, a symlink to a
// migration outside the directory, a hidden file and a non-SQL
// file. The last two are not migrations and must be skipped.
func migrationsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "002_second.sql", "CREATE TABLE b (id INT);")
	writeFile(t, dir, "001_first.sql", "CREATE TABLE a (id INT);")

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sub, "000_nested.sql", "CREATE TABLE nested (id INT);")

	// Like a Kubernetes ConfigMap volume, the link points outside dir.
	target := filepath.Join(t.TempDir(), "003_link.sql")
	if err := os.WriteFile(target, []byte("CREATE TABLE c (id INT);"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "003_link.sql")); err != nil {
		t.Fatal(err)
	}

	writeFile(t, dir, ".DS_Store", "\x00\x00\x00\x01Bud1")
	writeFile(t, dir, "README.md", "# Migrations")

	return dir
}

func checksum(content string) []byte {
	sum := sha256.Sum256([]byte(content))
	return sum[:]
}

// assertMigrations checks that names holds exactly the three migrations
// written by migrationsDir, in name order, with bare names.
func assertMigrations(t *testing.T, names []string) {
	t.Helper()
	assert.With(t).
		That(names).
		IsEqualTo([]string{"001_first.sql", "002_second.sql", "003_link.sql"})
}

func TestListMigrations(t *testing.T) {
	Assert := assert.With(t)
	names, err := listMigrations(os.DirFS(migrationsDir(t)))
	Assert.
		That(err).
		IsOk()
	assertMigrations(t, names)
}

func TestListMigrations_SubFS(t *testing.T) {
	Assert := assert.With(t)
	root := t.TempDir()
	if err := os.Rename(migrationsDir(t), filepath.Join(root, "migrations")); err != nil {
		t.Fatal(err)
	}
	fsys, err := fs.Sub(os.DirFS(root), "migrations")
	Assert.
		That(err).
		IsOk()

	names, err := listMigrations(fsys)
	Assert.
		That(err).
		IsOk()

	// Names carry no directory, so they match those read from disk.
	assertMigrations(t, names)
}

func TestListMigrations_EmptyDir(t *testing.T) {
	Assert := assert.With(t)
	names, err := listMigrations(os.DirFS(t.TempDir()))
	Assert.
		That(err).
		IsOk()
	Assert.
		That(names).
		IsEmpty()
}

func TestMigrateDir_MissingDir(t *testing.T) {
	Assert := assert.With(t)
	err := MigrateDir(context.Background(), nil, filepath.Join(t.TempDir(), "missing"))
	Assert.
		That(err).
		IsError()
}

func TestListMigrations_UppercaseExtension(t *testing.T) {
	Assert := assert.With(t)
	dir := t.TempDir()
	writeFile(t, dir, "001_first.SQL", "SELECT 1;")
	writeFile(t, dir, "002_second.Sql", "SELECT 2;")
	writeFile(t, dir, ".SQL", "SELECT 3;")

	names, err := listMigrations(os.DirFS(dir))
	Assert.
		That(err).
		IsOk()
	Assert.
		That(names).
		IsEqualTo([]string{"001_first.SQL", "002_second.Sql"})
}

func TestListMigrations_RejectsDanglingSymlink(t *testing.T) {
	Assert := assert.With(t)
	dir := t.TempDir()
	writeFile(t, dir, "001_first.sql", "SELECT 1;")
	missing := filepath.Join(t.TempDir(), "gone.sql")
	if err := os.Symlink(missing, filepath.Join(dir, "002_dangling.sql")); err != nil {
		t.Fatal(err)
	}

	_, err := listMigrations(os.DirFS(dir))
	Assert.
		That(err).
		IsError()
}

// unsortedFS returns directory entries in reverse name order, which fs.ReadDir
// passes through unsorted because unsortedFS implements fs.ReadDirFS.
type unsortedFS struct{ fs.FS }

func (u unsortedFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(u.FS, name)
	slices.Reverse(entries)
	return entries, err
}

func TestListMigrations_SortsUnsortedFS(t *testing.T) {
	Assert := assert.With(t)
	dir := t.TempDir()
	writeFile(t, dir, "001_first.sql", "SELECT 1;")
	writeFile(t, dir, "002_second.sql", "SELECT 1;")

	names, err := listMigrations(unsortedFS{os.DirFS(dir)})
	Assert.
		That(err).
		IsOk()
	Assert.
		That(names).
		IsEqualTo([]string{"001_first.sql", "002_second.sql"})
}

func TestListMigrations_NoRegularFiles(t *testing.T) {
	Assert := assert.With(t)
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sub, "001_nested.sql", "SELECT 1;")

	names, err := listMigrations(os.DirFS(dir))
	Assert.
		That(err).
		IsOk()
	Assert.
		That(names).
		IsEmpty()
}
