[![tests](https://github.com/aegisdesign-io/strata/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/aegisdesign-io/strata/actions/workflows/test.yml)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/aegisdesign-io/strata/badges/coverage.json)](https://github.com/aegisdesign-io/strata/actions/workflows/test.yml)

# strata

Database migration library

## Purpose

A simple database migration library that reads SQL files in lexical order,
validates them with the existing database, then applies only new migrations.
The files may be read from the filesystem, or as embedded files using go's
`embed.FS`.

## Usage

Migrations are read from the root of an `fs.FS`:

```go
// From disk
err := strata.MigrateDir(ctx, db, "migrations")

// Embedded
//go:embed migrations/*.sql
var migrations embed.FS

sub, err := fs.Sub(migrations, "migrations")
if err != nil {
	return err
}
err = strata.Migrate(ctx, db, sub)
```

## Testing

### Unit tests

Unit tests do not need a running database, but do not exercise all the code:

```sh
make test
```

### Integration tests

Integration tests run against a real PostgreSQL 18 or later database (the
schema uses `uuidv7()`). They are skipped unless `STRATA_TEST_DATABASE_URL`
is set.

With Docker running, `make test-integration` starts a temporary
`postgres:18-alpine` container on a free local port, runs the unit and
integration tests against it, and removes the container afterwards, even if
the tests fail:

```sh
make test-integration
```

**NOTE:** an easy way to get docker running on macOS is with
[colima](https://colima.run). Install colima and the docker client:

```sh
brew install colima docker docker-credential-helper
```

Colima can start and stop as needed:

```sh
# The --vm-type vz argument is needed only the first time colima is started.
colima start --vm-type vz
...
colima stop
```

To run them against an existing database instead, set the URL and run
`go test` directly:

```sh
STRATA_TEST_DATABASE_URL=postgres://user:pass@host:5432/db go test -count=1 ./...
```

Each test creates its own schema, named `strata_test_<random>`, and drops it
when the test ends, so tests do not interfere with each other or with other
data in the database. The connecting role needs permission to create schemas.

### Coverage

```sh
make coverage
```

Like `make test-integration`, this starts a temporary `postgres:18-alpine`
container, runs the unit and integration tests against it, removes the
container, and writes `coverage.html`.

## Contributing

1.  Fork it
2.  Create a feature branch (`git checkout -b new-feature`)
3.  Commit changes (`git commit -am "Added new feature xyz"`)
4.  Push the branch (`git push origin new-feature`)
5.  Create a new pull request.

## Maintainers

* [Aegis Design](http://github.com/aegisdesign-io)

## License

Copyright 2026 AegisDesign.io

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

