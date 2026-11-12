# Configuration reference

docsync reads one YAML file (`--config`, default `docsync.yaml`, or `DOCSYNC_CONFIG`). Unknown keys are errors. Relative directories resolve against the config file's location.

## source

| Key | Default | Meaning |
|---|---|---|
| `type` | required | `sqlserver` |
| `dsn_env` | | Environment variable holding the connection string (recommended) |
| `dsn` | | Connection string written inline (avoid committing secrets) |
| `poll_interval` | `1s` | How long an idle engine waits before the next cycle |
| `assemble_budget` | `5000` | Maximum documents assembled per cycle |
| `low_priority_share` | `0.2` | Share of the budget reserved for rebuild, snapshot and sweep work while such work is queued |
| `max_cycle_retries` | `5` | Retries after transient SQL errors (deadlocks, snapshot conflicts) |
| `sweep_per_minute` | `0` (off) | Re-check this many stored documents per minute against the database, cycling forever. A document that comes out different reveals a missed change; it is corrected and counted (`docsync_sweep_corrections_total`) |

Connection strings use the go-mssqldb format, for example `sqlserver://user:pass@host:1433?database=db`. Windows integrated authentication and Entra ID are supported by the driver.

## document

| Key | Meaning |
|---|---|
| `name` | Document type name (appears in manifests and metrics) |
| `root.table` | Table whose rows are the documents, e.g. `dbo.stone` |
| `root.key` | One column: the root table's primary key |
| `root.filter` | Optional. A SQL condition on the root table; only matching rows become documents (see below) |
| `fragments` | List of fragment queries (below) |

### Root filter

`root.filter` keeps only some rows of the root table, for example `filter: "t.status <> 'sold'"`. Write it as a `WHERE` condition on the root table, using bare column names or the alias `t`.

- A row that stops matching is deleted from the output. A row that starts matching is added.
- It may use only the root table's own columns. The root table is always tracked, so every change that matters re-checks the row. `docsync doctor` refuses a filter that reads other tables, and reports how many rows it keeps.
- Changing the filter changes the document definition, so docsync starts a new epoch and rebuilds every document.
- Avoid time-dependent conditions such as `GETDATE()`. Documents are re-checked only when their data changes, so they would not leave the index when time passes.

### fragments

| Key | Default | Meaning |
|---|---|---|
| `name` | required | Unique name |
| `cardinality` | `one` | `one`: at most one row per root; columns become top-level fields (or a struct under `path`). `many`: rows become a list under `path` |
| `path` | | Field name for a `many` list (required), or for a `one` struct (optional) |
| `sort_key` | | Required for `many`: columns that order the list. docsync sorts in Go, so the order is deterministic whatever plan SQL Server picks |
| `sql` | required | T-SQL query (rules below) |

Fragment SQL rules:

- Join the `{{roots}}` placeholder, a temp table with one column `id`, and select it as `_root`: `SELECT r.id AS _root, … FROM {{roots}} r JOIN …`.
- List columns explicitly; `SELECT *` is rejected so schema changes are detected.
- Every column needs a unique name (`AS`) and a supported type (see the [consumer protocol](consumer-protocol.md)). Cast anything else, for example `CAST(geo AS nvarchar(max))`.
- A `one` fragment that returns two rows for a root records an error for that root (shown on the dashboard) and keeps its previous document.
- Calculated fields are ordinary SQL: joins, subqueries, `CASE`, window functions.
- Every table a fragment reads must be tracked (in `routing`, or the root table) or marked `static`. `docsync doctor` checks this using SQL Server's query plans.

## routing

One entry per table that documents depend on, other than the root table.

| Key | Meaning |
|---|---|
| `table` | Table name |
| `key` | The table's primary key columns, in order |
| `next.table` | Next table on the path to the root (the root, or another routing table) |
| `next.join` | Map of this table's columns to the next table's columns |
| `next.kind` | `references` (this row holds the next table's columns) or `referenced_by` (rows of the next table hold this row's columns) |
| `hot_fanout_limit` | Optional. Roots beyond this count, from one cycle's changes to this table, go to a background queue |
| `static` | `true` for tables fragments read but that are not tracked (no `key`/`next`). Rebuild documents yourself after changing them |

Every tracked table needs change tracking enabled and a primary key. Paths must reach the root without cycles.

Examples:

```yaml
routing:
  # child table holding the root key
  - { table: dbo.inclusion, key: [inclusion_id], next: { table: dbo.stone, join: { stoneid: stoneid }, kind: references } }
  # shared lookup referenced by a non-key column of the root
  - { table: dbo.lab, key: [lab_id], next: { table: dbo.stone, join: { code: lab_code }, kind: referenced_by }, hot_fanout_limit: 2000 }
  # bridge table, then its parent, then a grandchild (three hops from order_line)
  - { table: dbo.order_stone,  key: [order_id, stoneid], next: { table: dbo.stone, join: { stoneid: stoneid }, kind: references } }
  - { table: dbo.order_header, key: [order_id], next: { table: dbo.order_stone, join: { order_id: order_id }, kind: referenced_by } }
  - { table: dbo.order_line,   key: [line_id],  next: { table: dbo.order_header, join: { order_id: order_id }, kind: references } }
  - { table: dbo.currency, static: true }
```

## sinks

| Key | Default | Meaning |
|---|---|---|
| `type` | required | `parquet_log` or `ndjson` |
| `name` | type | Unique name |
| `dir` | required | Output directory |
| `segment_interval` | `2s` | Minimum time between segment files (generations in between are combined) |
| `base_every` | `15m` | Write a fresh base this often once the sink is caught up |
| `retain_segments` | `1h` | Keep segments (and old bases) at least this long after a newer base covers them |
| `include_json` | `false` | Add `_doc_json` with the full canonical document (Parquet) |
| `base_file_rows` | `500000` | Rows per base file |

## state

| Key | Default | Meaning |
|---|---|---|
| `dir` | `./state` | Pebble state directory. One docsync instance per directory |
| `outbox_retention` | `24h` | Keep published generations this long for re-delivery |

## dashboard

| Key | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:8088` | Address of the dashboard, admin API and `/metrics` |
| `disabled` | `false` | Turn the HTTP server off |
| `basic_auth_user` | | Require HTTP basic auth with this user… |
| `basic_auth_password_env` | | …and the password from this environment variable |

The admin API accepts only `application/json` POSTs. Keep the dashboard on a private address or behind authentication: it can trigger full rebuilds.
