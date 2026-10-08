-- Schema for tracking applied migrations. Requires PostgreSQL 18 or later for
-- uuidv7().

-- strata_migrations records each migration file that has been applied.
CREATE TABLE IF NOT EXISTS strata_migrations (
	id         UUID          PRIMARY KEY DEFAULT uuidv7(),
	created_at TIMESTAMP     NOT NULL DEFAULT (now() AT TIME ZONE 'UTC'),
	file_name  VARCHAR(1024) NOT NULL UNIQUE,
	checksum   BYTEA         NOT NULL CHECK (octet_length(checksum) = 32)
);

-- strata_check_migration reports whether a migration file has been applied
-- and, if so, whether its checksum matches the recorded one. The return values
-- correspond to the MigrationStatus constants in db.go.
CREATE OR REPLACE FUNCTION strata_check_migration(p_file_name VARCHAR(1024), p_checksum BYTEA)
RETURNS SMALLINT
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
	v_checksum BYTEA;
BEGIN
	SELECT checksum INTO v_checksum
	FROM strata_migrations
	WHERE file_name = p_file_name;

	IF NOT FOUND THEN
		RETURN 0;
	ELSIF v_checksum = p_checksum THEN
		RETURN 1;
	ELSE
		RETURN 2;
	END IF;
END;
$$;

-- strata_record_migration records a migration file as applied.
CREATE OR REPLACE FUNCTION strata_record_migration(p_file_name VARCHAR(1024), p_checksum BYTEA)
RETURNS VOID
LANGUAGE sql
AS $$
	INSERT INTO strata_migrations (file_name, checksum) VALUES (p_file_name, p_checksum);
$$;
