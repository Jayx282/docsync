# docsync

[![CI](https://github.com/Jayx282/docsync/actions/workflows/ci.yml/badge.svg)](https://github.com/Jayx282/docsync/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/Jayx282/docsync)](go.mod)

**Keep a document index in sync with SQL Server, incrementally.**

`docsync` maintains one document per *root* row (for example one document per product, customer or stone), assembled from any number of related SQL Server tables, and keeps it up to date within seconds as those tables change. Documents are published as Parquet (or NDJSON) segment logs that a search or serving engine can load and tail.

It replaces the usual "custom sync service + sync views" plumbing behind Elasticsearch-style indexes with a single configurable binary.

**New here?** Run `make demo` and follow [docs/getting-started.md](docs/getting-started.md). In about 15 minutes you design a document in the browser, deploy it and watch it update.

> **Status: early.** v0.1 is the first release. Randomized tests against SQL Server 2022 check it thoroughly, but it has no production track record yet. The config and the output format may change before 1.0; the [changelog](CHANGELOG.md) will say how to upgrade.

```
SQL Server tables ──(Change Tracking)──► docsync ──► Parquet segment log ──► your index / query engine
                                            │
                                            └─► web dashboard, metrics, CLI, Studio (visual designer)
```

## Why

Keeping denormalized documents correct is harder than it looks:

- A change in a child table (an order line) has to find its root through several hops (order line → order → order/stone link → stone).
- Changing a shared lookup row (a lab name) can affect thousands of documents.
- Deletes and rows moved between parents have to rebuild the *old* parent as well. SQL Server Change Tracking only reports the key of a deleted row.
- Full rebuilds and incremental updates have to produce exactly the same documents, through crashes, restores, `TRUNCATE`s and schema changes.

docsync handles all of this generically from a YAML file. A randomized test against a real SQL Server checks that incremental output always equals a fresh full rebuild, with concurrent writers and injected crashes.

## Features

- **SQL-first documents.** Each document is built from *fragment* queries you write in plain T-SQL, so existing view logic ports directly. Cardinality-one fragments become fields; cardinality-many fragments become nested lists. Calculated fields are just SQL.
- **Root filters.** Index only the rows you want, for example unsold stones. Rows enter and leave the index as their data changes.
- **Declarative change routing.** You declare how each table reaches the root (`references` / `referenced_by`, any number of hops). docsync turns every change into the exact set of affected roots, including old parents of deleted or moved rows.
- **Only affected documents are rebuilt.** Unchanged results are not re-emitted.
- **Fan-out control.** A per-table limit moves huge lookup fan-outs to a background queue, so ordinary changes stay fresh.
- **Consistent and durable.**
  - One SQL snapshot per cycle.
  - One atomic local commit (Pebble) per cycle.
  - Gap-free generation numbers.
  - Idempotent, crash-safe publishing.
- **Self-healing.** It detects and recovers automatically from:
  - change-tracking retention expiry;
  - `TRUNCATE`;
  - change tracking being switched off and on;
  - database restores;
  - configuration changes.

  When the database schema changes under it, it pauses until an operator starts a new epoch.
- **Operable.**
  - Embedded dashboard: lag, fan-out per table, document inspector, actions.
  - Prometheus metrics.
  - `doctor`, `introspect` and `fanout` diagnostics.
  - An optional sweeper that re-checks documents in the background and repairs any that a missed change left stale.
- **Zero infrastructure.** One Go binary and a local state directory. No Kafka, Spark or cloud services.

## Install

- **Download a binary** for Linux, macOS or Windows from [Releases](https://github.com/Jayx282/docsync/releases), and check it against `SHA256SUMS`.
  - Rename it to `docsync`, or `docsync.exe` on Windows.
  - On Linux and macOS, make it executable with `chmod +x docsync`.
  - On macOS, a binary downloaded in a browser is quarantined until you run `xattr -d com.apple.quarantine docsync`.
- **Or build it with Go**, at the version in `go.mod` or later:

  ```bash
  go install github.com/Jayx282/docsync/cmd/docsync@latest
  ```

- **Or try it first with only Docker installed:** clone this repository and run `make demo`.

On Windows Server, run docsync as a service. See [docs/windows-service.md](docs/windows-service.md).

## Quick start

The fastest way in is the demo: `make demo` starts SQL Server in Docker with example data and opens Studio (see [docs/getting-started.md](docs/getting-started.md)). The steps below set docsync up by hand.

Requirements: SQL Server 2016 or later (tested on 2022), with change tracking and snapshot isolation enabled on the database. It works on every edition, including Express.

```bash
docsync init                          # writes docsync.yaml + schema.sql (a demo stone inventory)
export DOCSYNC_DSN='sqlserver://user:pass@host:1433?database=mydb'
docsync introspect --root dbo.stone   # optional: draft routing from foreign keys and column names
docsync doctor                        # checks permissions, change tracking, indexes, undeclared tables
docsync fanout                        # how many documents one row of each table reaches
docsync run                           # sync + dashboard at http://127.0.0.1:8088
```

Each tracked table needs change tracking and a primary key:

```sql
ALTER DATABASE CURRENT SET ALLOW_SNAPSHOT_ISOLATION ON;
ALTER DATABASE CURRENT SET CHANGE_TRACKING = ON (CHANGE_RETENTION = 3 DAYS, AUTO_CLEANUP = ON);
ALTER TABLE dbo.stone ENABLE CHANGE_TRACKING;   -- repeat for every table the documents read
GRANT VIEW CHANGE TRACKING ON dbo.stone TO docsync_login;
```

To try it end to end with generated data:

```bash
go run ./examples/stones/stonegen --create-database stones_demo --stones 10000 --churn 1000
```

## Studio: design documents visually

`docsync studio` opens a web designer, served by the same binary, so you don't have to write the YAML and SQL by hand:

```bash
export DOCSYNC_DSN='sqlserver://user:pass@host:1433?database=mydb'
docsync studio            # prints a sign-in link and opens it in your browser
```

![Studio's design editor: the root table's columns on the left, real documents built from the data on the right](docs/images/studio-design.png)

- **Explore.** Search every table. Each one shows its columns, keys, indexes and a graph of related tables, from foreign keys and from columns named like a key.
- **Design.**
  - Pick the root table: one document per row.
  - Tick the columns you want and rename them.
  - Add related tables from suggestions, or with a custom join. A table can come in as fields (a lookup such as `lab_name`) or as a nested list (`inclusions`), directly or through a bridge table.
- **Calculate.** Add fields with spreadsheet-style formulas over the row, its lookups and summaries of its lists, for example `ROUND(SUM(rates.amount) / carat, 2)` or `COUNTIF(inclusions.severity >= 3)`. Add a filter such as `status <> "sold"` to index only some rows. See [docs/formulas.md](docs/formulas.md).
- **Preview.** Every edit rebuilds a few real documents from your data. It also shows the output schema, the generated SQL and the docsync config.
- **Checks and deploy.** Checks run `doctor`, compute every formula once and write the SQL your DBA needs: change tracking, indexes and grants. The deploy dialog shows a diff of the config and says whether every document will be rebuilt. Then the pipeline starts, and its dashboard opens inside Studio.

| Calculated fields | Explore |
|---|---|
| ![Calculated fields with formulas, an insert palette and the live preview](docs/images/studio-calculated.png) | ![A table's columns and a graph of related tables](docs/images/studio-explore.png) |

Studio listens on `127.0.0.1` only. Sign in with the link it prints, or paste the token saved in `studio/.token`. The connection string is read from the environment and is never written to disk. See [docs/studio.md](docs/studio.md).

## Configuration at a glance

```yaml
document:
  name: stone
  root: { table: dbo.stone, key: [stoneid] }
  fragments:
    - name: details                    # one row per root -> top-level fields
      sql: |
        SELECT r.id AS _root, s.carat, s.color, l.name AS lab_name
        FROM {{roots}} r
        JOIN dbo.stone s ON s.stoneid = r.id
        LEFT JOIN dbo.lab l ON l.code = s.lab_code
    - name: inclusions                 # many rows per root -> nested list
      cardinality: many
      path: inclusions
      sort_key: [position, inclusion_id]
      sql: SELECT r.id AS _root, i.inclusion_id, i.kind, i.position FROM {{roots}} r JOIN dbo.inclusion i ON i.stoneid = r.id
routing:
  - { table: dbo.inclusion, key: [inclusion_id], next: { table: dbo.stone, join: { stoneid: stoneid }, kind: references } }
  - { table: dbo.lab,       key: [lab_id],       next: { table: dbo.stone, join: { code: lab_code },  kind: referenced_by }, hot_fanout_limit: 2000 }
sinks:
  - { type: parquet_log, dir: ./out, include_json: true }
```

See [docs/config-reference.md](docs/config-reference.md) for every option and [examples/stones/docsync.yaml](examples/stones/docsync.yaml) for a complete example. It covers lookups, a bridge table and a three-hop path.

## Output

Each sink directory holds a `manifest.json`, a base snapshot and segment files. Every generation is an atomic batch of document upserts and deletes; consumers load the base, then apply segments in order. The format and the consumer algorithm are specified in [docs/consumer-protocol.md](docs/consumer-protocol.md).

To look at an output directory, or to start your own loader, use the Python reader in [examples/reader](examples/reader):

```bash
python examples/reader/read_output.py studio/pipelines/stone/out --id 1000001
```

## Commands

| Command | Purpose |
|---|---|
| `run` | Run the engine and the dashboard |
| `studio` | Open the visual designer: explore tables, design, preview, check and deploy |
| `service install`, `start`, `stop`, `status`, `uninstall` | Run a pipeline as a Windows service that starts with the machine and restarts after failures ([docs/windows-service.md](docs/windows-service.md)) |
| `doctor` | Check database settings, permissions, indexes and undeclared table dependencies |
| `introspect` | Draft a config from foreign keys, column names and existing views |
| `fanout` | Measure documents reached per source row, per table |
| `assemble --id X` | Print one document as assembled from the database now |
| `dump FILE` | Print a Parquet segment or base file as JSON |
| `status`, `pause`, `resume`, `rebuild`, `resnapshot`, `new-epoch`, `compact` | Control a running instance |
| `init` | Write the example config and schema |

## Migrating from sync views

If you feed a search index from SQL "sync views" today, [docs/migrating-from-sync-views.md](docs/migrating-from-sync-views.md) walks through the move:
- inventory with `introspect --views`;
- port views to fragments;
- enable change tracking;
- validate with `doctor` and `fanout`;
- run side by side with the sweeper on.

## How it works

[docs/architecture.md](docs/architecture.md) explains:
- the cycle;
- routing and the key map;
- the pending set;
- generations;
- crash safety;
- lifecycle handling.

## Performance

These numbers were measured on an Apple-silicon laptop, running SQL Server 2022 in a Linux container under x86 emulation. A real server should do better.

| What | Result |
|---|---|
| Freshness at 1,000 changed documents per minute, from the source commit to the change being readable in the output | p50 0.35 s, p95 0.60 s, slowest 0.66 s |
| Full load | 100,000 documents in about 10 s |

- Both runs use the example stone document: a lookup and three nested lists, one of them through a bridge table.
- The freshness run polls every 500 ms and writes a segment at most every 500 ms. It is `TestFreshness` in `test/integration`.
- Your numbers depend on your fragment queries and your indexes. `docsync doctor` points out missing indexes.

## Limitations

- SQL Server only (Change Tracking). The source interface is designed for CDC, PostgreSQL and MySQL later.
- One root key column per document type; one document type per instance.
- Nesting is one level deep (fields and lists of structs). Return JSON from SQL for deeper shapes.
- Sinks: Parquet and NDJSON segment logs. gRPC and Elasticsearch/OpenSearch sinks are planned.
- Studio runs one deployed pipeline at a time. A design can use each table once and attaches related tables to the root, directly or through one bridge table. Lists inside lists and multiple pipelines are planned ([docs/studio-plan.md](docs/studio-plan.md)).

## Contributing

Issues and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) covers:

- building docsync;
- the test suite, including the randomized invariant tests against SQL Server;
- working on the Studio UI.

Report security problems privately, as described in [SECURITY.md](SECURITY.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
