package strata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
)

// Migrate applies every .sql file in the root of fsys to db as a migration,
// in lexicographic order by file name. Hidden files, files without a .sql
// extension (matched case-insensitively) and subdirectories are skipped;
// subdirectories are not descended into. Symbolic links are followed, and a
// link whose target does not exist is an error. Files already applied are
// skipped. An unapplied file whose name sorts before an applied migration is
// an error, as is an applied migration with no file in fsys that sorts before
// the newest file, such as one that was renamed; in either case nothing is
// applied. Applied migrations that sort after every file, such as those from
// a newer release, are ignored.
// Migrate creates the strata_migrations table first if it does not exist.
// Iteration stops at the first error, which is returned to the caller.
//
// To read migrations from a subdirectory of an embed.FS, pass the result of
// fs.Sub.
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	names, err := listMigrations(fsys)
	if err != nil {
		return err
	}

	return applyMigrations(ctx, db, fsys, names)
}

// MigrateDir is a helper that reads migrations from the directory specified
// by `dir` on disk.
func MigrateDir(ctx context.Context, db *sql.DB, dir string) error {
	return Migrate(ctx, db, os.DirFS(dir))
}

// isValidMigrationFileName reports whether `name` looks like a valid
// migration file, which is a `.sql` file that is not hidden. The extension
// is matched case-insensitively, so `001_init.SQL` is not silently skipped.
func isValidMigrationFileName(name string) bool {
	const ext = ".sql"
	return !strings.HasPrefix(name, ".") &&
		len(name) > len(ext) &&
		strings.EqualFold(name[len(name)-len(ext):], ext)
}

// listMigrations returns the name of each migration file in the root of
// fsys, in name order, without reading any file. Entries are skipped unless
// isValidMigrationFileName accepts them, and they are, or link to, a regular
// file. A link whose target does not exist is an error, since skipping it
// would let later migrations apply first and leave it permanently out of
// order.
// Migrations are named by entry name alone, so the same file is
// recognized whatever fsys it is read from.
func listMigrations(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !isValidMigrationFileName(name) {
			continue
		}
		mode := entry.Type()
		if mode&fs.ModeSymlink != 0 {
			// Follow the link, as with Kubernetes ConfigMap volumes, whose
			// files are symlinks into a hidden data directory.
			info, err := fs.Stat(fsys, name)
			if err != nil {
				return nil, fmt.Errorf("stat %q: %w", name, err)
			}
			mode = info.Mode()
		}
		if !mode.IsRegular() {
			continue
		}

		names = append(names, name)
	}

	// fs.ReadDir only sorts when fsys does not implement fs.ReadDirFS, so
	// sort here to guarantee the apply order.
	slices.Sort(names)

	return names, nil
}

// applyMigrations reads, hashes, and applies each migration in `names` from
// fsys, in the order given. An unapplied file whose name sorts before the
// last-applied migration stops the run before the database changes, since it
// was written against an older schema, as does an applied migration missing
// from names that sorts before the newest name. Otherwise, the run stops at
// the first file that cannot be read, has changed since it was applied, or fails to
// apply. Migrations before it remain applied. The order check is repeated by
// applyMigration under the migration lock, since another process may apply
// a later migration after the snapshot below is read.
func applyMigrations(ctx context.Context, db *sql.DB, fsys fs.FS, names []string) error {
	if err := CreateMigrationsTable(ctx, db); err != nil {
		return err
	}

	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return err
	}

	onDisk := make(map[string]bool, len(names))
	for _, name := range names {
		onDisk[name] = true
	}

	// An applied migration missing from names is only an error if it sorts
	// before the newest file in names, so it must have been renamed or
	// deleted. One that sorts after was applied by a newer release, and an
	// older release must still be able to start alongside it.
	var newest string
	if len(names) > 0 {
		newest = names[len(names)-1]
	}
	var last string
	for name := range applied {
		if !onDisk[name] && name < newest {
			return fmt.Errorf("migration %q: applied, but no longer present; was it renamed or deleted?", name)
		}
		last = max(last, name)
	}
	for _, name := range names {
		if _, ok := applied[name]; !ok && name < last {
			return outOfOrderError(name, last)
		}
	}

	for _, name := range names {
		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read %q: %w", name, err)
		}
		checksum := sha256.Sum256(content)

		sum, ok := applied[name]
		switch {
		case !ok:
			if err := applyMigration(ctx, db, name, string(content), checksum[:]); err != nil {
				return err
			}
		case !bytes.Equal(sum, checksum[:]):
			return checksumMismatchError(name)
		}
	}

	return nil
}

// appliedMigrations returns the checksum of every migration recorded in
// strata_migrations, keyed by file name.
func appliedMigrations(ctx context.Context, db *sql.DB) (map[string][]byte, error) {
	rows, err := db.QueryContext(ctx, "SELECT file_name, checksum FROM strata_migrations")
	if err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string][]byte)
	for rows.Next() {
		var name string
		var sum []byte
		if err := rows.Scan(&name, &sum); err != nil {
			return nil, fmt.Errorf("list applied migrations: %w", err)
		}
		applied[name] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}

	return applied, nil
}
