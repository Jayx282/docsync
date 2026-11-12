# Contributing to docsync

Bug reports, fixes, docs, new sources and new sinks are all welcome.

## Before you start

- **Bugs.** Open an issue using the bug template. Include the output of `docsync version`, and remove connection strings and passwords.
- **Larger changes**, such as a new source, a new sink or a new config option: open an issue first, so the design is agreed before you write the code.
- **Security problems** go through [SECURITY.md](SECURITY.md), not a public issue.

## Set up

You need:

- Go, at the version in `go.mod`.
- Node.js 22 or later, only if you change the Studio UI in `web/studio`.
- Docker, only for the integration tests and `make demo`. On macOS without Docker Desktop, the demo script can use Lima instead.

```bash
make build   # bin/docsync
make test    # go vet and the unit tests
make demo    # SQL Server in Docker with example data, then Studio
```

`make help` lists every target.

## Tests

The unit tests need only Go. The integration tests need a SQL Server on which they can create and drop databases:

```bash
docker run -d --name docsync-mssql --platform linux/amd64 -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD='<a strong password>' -p 1433:1433 mcr.microsoft.com/mssql/server:2022-latest
export DOCSYNC_TEST_DSN='sqlserver://sa:<the password>@127.0.0.1:1433?database=master&encrypt=disable'
make test-integration
```

The integration tests are randomized:

- Each test logs its seed. Set `DOCSYNC_TEST_SEED` to replay a failure exactly.
- `DOCSYNC_TEST_FAULTS=1` injects crashes.
- `DOCSYNC_TEST_ROUNDS=1000` runs longer.

### The invariant

docsync's central promise is that **incremental output always equals a fresh full rebuild**. The integration tests check this under:

- concurrent writes;
- crashes;
- lifecycle events: retention expiry, `TRUNCATE`, restores and config changes.

Every change to capture, routing, assembly, state or sinks must keep them passing. If you fix a missed change, add the kind of write that caused it to the random writer in `examples/stones/stonegen`, or to a test, so it stays fixed.

## Code

- Run `gofmt` and `go vet ./...`. CI checks both, with and without the `integration` build tag.
- Keep the binary free of cgo. Release builds use `CGO_ENABLED=0`.
- Write error messages that say what to do next. Users mostly meet docsync through its errors and its dashboard.
- Update `docs/` when you change behaviour, a config option or the output format.
- A change to [docs/consumer-protocol.md](docs/consumer-protocol.md) that existing readers can't handle needs a new `format_version`. Discuss it in an issue first.

## Studio UI

The UI is React and TypeScript in `web/studio`, built with Vite. Its build output in `internal/studio/ui/dist` is committed, so `go build` needs no Node.

After changing the UI, rebuild it and commit the output:

```bash
make ui
```

CI rebuilds the UI and fails if the committed output differs.

For live reloading, run these in two terminals:

1. `docsync studio --token dev --no-browser`
2. `make ui-dev`

## Pull requests

- Keep each pull request to one change, with tests.
- Say what changed and how you tested it. The template has a short checklist.
- For anything users will notice, add a line to the top section of `CHANGELOG.md`.

## Releases

1. A maintainer gives the top section of `CHANGELOG.md` its version and date, for example `## v0.2.0 - 2026-12-01`.
2. The maintainer pushes a tag:

   ```bash
   git tag v0.2.0 && git push origin v0.2.0
   ```

3. The release workflow then runs the tests and cross-compiles for Linux, macOS and Windows with the version stamped in. It publishes the binaries and `SHA256SUMS`, using that version's changelog section as the release notes.

## License

By contributing, you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).
