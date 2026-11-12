# Migrating from a view-based search sync

Many teams feed Elasticsearch (or another search index) from a custom service that polls "sync views". These views list rows changed since the last run, and the service maps each view onto parts of a document. This guide moves that setup to docsync without losing document semantics.

## 1. Inventory what the documents read

The pending-change views usually hide the base tables. List them:

```bash
export DOCSYNC_DSN='sqlserver://…?database=yourdb'
docsync introspect --root dbo.your_root_table --key your_root_id --views 'sync.%'
```

The draft contains:

- **routing rules** for every table connected to the root by a foreign key or a column named like the root key (marked `GUESSED` when inferred from the name);
- **skeleton fragments** for direct child tables;
- **a TODO list** of tables the views read that could not be connected automatically. Write those routing rules by hand: they are usually indirect paths through orders or transactions, or lookups.

## 2. Turn view logic into fragments

Sync views are incremental by design: rows disappear once processed. Fragments must instead describe the **complete** current state for a set of roots. For each mapping:

1. Take the view's `SELECT` and drop the "pending"/"processed" filter and any change-queue join.
2. Join `{{roots}} r` and select `r.id AS _root`.
3. Choose the shape:
   - one row per root → a `cardinality: one` fragment (fields at top level, or a struct with `path`);
   - many rows per root → `cardinality: many` with `path` (the nested field name used in the old document) and a `sort_key` (the order the old service produced, or the child's primary key).
4. Keep calculated fields in SQL (`CASE`, joins, aggregates), so the same business rules apply.
5. Rename columns with `AS` to match the old document field names exactly.

Check each document against the old index while you work:

```bash
docsync assemble --id 12345 > new.json      # compare with the old index's _source for id 12345
```

## 3. Enable change tracking on the base tables

```sql
ALTER DATABASE CURRENT SET ALLOW_SNAPSHOT_ISOLATION ON;
ALTER DATABASE CURRENT SET CHANGE_TRACKING = ON (CHANGE_RETENTION = 3 DAYS, AUTO_CLEANUP = ON);
ALTER TABLE dbo.child_table ENABLE CHANGE_TRACKING;   -- for every tracked table
GRANT VIEW CHANGE TRACKING ON dbo.child_table TO docsync_login;
```

- Change tracking needs a primary key on every tracked table.
- Its write overhead is similar to maintaining one more index. Measure it on a copy of production with a replayed workload first.
- Choose `CHANGE_RETENTION` longer than the longest outage docsync should survive without a full resnapshot.

## 4. Validate

```bash
docsync doctor    # permissions, change tracking, missing indexes, and tables fragments read but routing misses
docsync fanout    # how many documents one row of each table reaches
```

`fanout` tells you which lookups have wide reach. Give those a `hot_fanout_limit`, so a mass update to them drains in the background instead of delaying ordinary changes.

## 5. Run side by side

1. Run docsync next to the old service, writing to a scratch directory.
2. Compare a sample of documents (or all of them) with the old index's `_source`. Expect a few differences: an incremental service that applied partial updates may have left stale fields that a full assembly does not have.
3. Enable `sweep_per_minute` (for example 1,000) for continuous verification. Any sweeper correction on the dashboard means a change was missed, usually because a table is missing from routing.

## 6. Cut over

1. Point the serving index (for example a Rust/DataFusion service) at the docsync sink: it loads the base, then tails segments (see [consumer-protocol.md](consumer-protocol.md)).
2. Watch `docsync_seconds_since_last_change`, `docsync_sink_lag_generations` and `docsync_pending_roots`.
3. Retire the old views and service.
