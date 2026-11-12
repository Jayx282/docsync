# Changelog

Notable changes to docsync, newest first. Versions follow [Semantic Versioning](https://semver.org/). Before 1.0, a minor version may change the config or the output format; when it does, its notes say how to upgrade.

The release workflow publishes each version's section here as its release notes, so a version's heading must start with `## v<version>`.

## v0.1.0 (unreleased)

First public release.

### Sync engine

- One document per root row, built from T-SQL fragment queries. Cardinality-one fragments become fields; cardinality-many fragments become sorted nested lists.
- Incremental sync from SQL Server Change Tracking. Each cycle reads one SQL snapshot and makes one atomic local commit (Pebble), with gap-free generation numbers.
- Declarative change routing (`references` and `referenced_by`, any number of hops). A key map finds the old parents of deleted and moved rows.
- Per-table fan-out limits that move large lookup changes to a background queue, so ordinary changes stay fresh.
- Root filters that index only the rows matching a condition. Rows enter and leave the index as their data changes.
- Automatic recovery from change-tracking retention expiry, `TRUNCATE`, change tracking being switched off and on, database restores and config changes. Schema drift pauses the pipeline until `docsync new-epoch`.
- An optional sweeper that re-checks stored documents in the background and repairs any that are stale.

### Output

- Parquet and NDJSON segment logs with an atomic manifest, base snapshots, epochs and retention. See the [consumer protocol](docs/consumer-protocol.md) (`format_version: 1`).
- An example reader in Python: [examples/reader](examples/reader).

### Studio

- A web designer served by the same binary. It lets you:
  - explore tables and how they relate;
  - design a document from suggestions;
  - add calculated fields with spreadsheet-style formulas;
  - filter which rows are indexed;
  - preview real documents as you edit;
  - run checks that also write the SQL a DBA needs;
  - deploy, with a diff of the config.
- Token sign-in, listening on `127.0.0.1` only by default.

### Operations

- An embedded dashboard and Prometheus metrics.
- The `doctor`, `introspect`, `fanout`, `assemble` and `dump` tools, and commands to control a running instance.
- Windows service support: `docsync service install`, `start`, `stop`, `status` and `uninstall`. The service restarts after failures, writes rotating log files and reports to the Event Log.
- `make demo` starts SQL Server in Docker with example data, then opens Studio.
- Release binaries for Linux, macOS and Windows, with SHA-256 checksums.
