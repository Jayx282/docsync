# Architecture

docsync turns changes in SQL Server tables into complete documents, one per root row, and publishes them as an append-only log of generations.

```
             ┌──────────────────────── one cycle (one SQL snapshot) ────────────────────────┐
SQL Server ──► versions ─► capture CT changes ─► route to roots ─► pick roots ─► run fragments ─┐
             └───────────────────────────────────────────────────────────────────────────────┘
                                                                                             │
                ┌────────── one atomic Pebble batch ───────────┐                             ▼
  state  ◄──────┤ documents · key map · pending set · outbox ·  ├◄── assemble + compare ◄──────┘
                │ checkpoint (CT version, generation, epoch)    │
                └───────────────────────────────────────────────┘
                                   │
                        publisher  ▼
                     sinks: manifest + base + segments (Parquet / NDJSON)
```

## The cycle

Cycles run one at a time, so every generation reflects a newer database snapshot than the one before it.

1. **Prepare temp tables.** On a pinned connection, empty `#roots` and one `#chg_<n>` table per tracked table. This happens *before* the transaction, because snapshot transactions reject some DDL. Then begin a `SNAPSHOT` transaction.
2. **Read versions.** Read each table's `CHANGE_TRACKING_MIN_VALID_VERSION` and begin version, the database's `CHANGE_TRACKING_CURRENT_VERSION()` (`@to`) and the recovery fork GUID.
   - If any of them shows the stored state can't be trusted, resnapshot (see Lifecycle).
   - If change tracking is off on a table, fail the cycle and retry with backoff until it is back on.
3. **Capture.** For each tracked table, copy `CHANGETABLE(CHANGES t, @from)` into `#chg_<n>`, all on the server. A change version above `@to` is a fatal invariant violation.
4. **Route.** Join each table's captured keys to its current rows, then walk the routing path to root keys (see Routing). Update the key map in the same batch.
5. **Pick roots.** Take changed roots first, then the pending set by priority, up to `assemble_budget`. A share (`low_priority_share`) is reserved for low-priority work whenever it exists. Overflow goes to the pending set.
6. **Assemble.** Bulk-insert the roots into `#roots`; SQL Server's primary key, with `IGNORE_DUP_KEY` and the column's own collation, removes duplicates.
   - Read back the canonical ids.
   - Find ids missing from the root table; those documents are deleted.
   - Run every fragment once.
   - Commit the read-only SQL transaction.
7. **Compare.** Build documents (lists sorted by `sort_key`, ties broken by full encoding), encode them canonically and compare with the last emitted bytes. Unchanged documents produce no output.
8. **Commit, in one Pebble batch synced to disk.** The batch holds:
   - new and deleted documents;
   - key map updates;
   - pending-set removals and additions;
   - the outbox entry for generation `g+1` (only if something changed);
   - the checkpoint (`@to`, generation, epoch, fork GUID, per-table begin versions).

   The generation number is assigned here, never at publish time.
9. **Publish.** Each sink knows the last generation it durably wrote: the manifest's `latest_generation`. The publisher reads newer generations from the outbox and writes one segment per `segment_interval`. Re-writing a generation is a no-op, so a crash anywhere is safe.

### Why every rebuild happens inside a capture cycle

If documents were assembled at a snapshot the key map had not seen, a row could move twice between snapshots. A parent could then keep a stale child forever. All rebuild work therefore runs as part of a cycle that also captured every change up to its own snapshot:
- fan-out overflow;
- full loads;
- manual rebuilds;
- resnapshots.

Each kind of work only adds roots to the pending set.

## Routing

Each tracked table declares the next table on its path to the root and how they join:

- **`references`**: this table holds the next table's columns (`inclusion.stoneid → stone.stoneid`).
- **`referenced_by`**: the next table holds this table's columns (`stone.lab_code → lab.code`, so one lab reaches many stones).

Routing starts from the values of the first hop's columns on the changed rows:
- **New values** come from the current row (none if it was deleted).
- **Old values** come from the **key map**, a local copy of each row's routing columns. Change tracking reports only the key of a deleted row and never old column values, so without the key map, deletes and moves would miss the old parent.

When the routing columns are part of the primary key (`order_stone(order_id, stoneid)`), the key itself carries them and no key map entry is kept.

Each later hop is a lookup in the same SQL snapshot: rows of the next table whose join columns match the current values. When the next hop starts from the same columns, the values pass straight through. A change to an `order_line` therefore walks `order_line → order_header → order_stone → stone`.

`hot_fanout_limit` caps how many roots one table's changes may add to a cycle's hot set. The rest go to the pending set's fan-out class, so a lookup rename touching 100,000 documents does not delay ordinary changes.

## The pending set

A durable priority queue of root ids, in Pebble, with lazy deletion. From highest priority to lowest:

| Class | Fed by |
|---|---|
| `overflow` | changed roots that did not fit in their cycle's budget |
| `fanout` | roots beyond a table's `hot_fanout_limit` |
| `rebuild` | `docsync rebuild` |
| `snapshot` | full loads and resnapshots |
| `sweep` | reserved for the anti-entropy sweeper |

A root that is assembled for any reason is removed from every class.

## Lifecycle

| Event | Detected by | Response |
|---|---|---|
| First start | no checkpoint | snapshot, epoch 1 |
| Change-tracking history expired | checkpoint < table's minimum valid version | resnapshot |
| `TRUNCATE` | minimum valid version / begin version moves past the checkpoint | resnapshot |
| Tracking switched off | minimum valid version is NULL | wait (retry with backoff) |
| Tracking switched back on | table begin version differs from the recorded one | resnapshot |
| Database restored | current version < checkpoint, or recovery fork GUID changed | resnapshot |
| Config (documents or routing) changed | content hash differs | resnapshot, new epoch |
| Fragment result columns changed | schema hash or column mismatch | pause until `new-epoch` |

A **resnapshot** runs one short SQL snapshot that:
- reads the current version;
- rescans the key map (key columns only);
- queues every root id, plus every stored document id, so vanished roots get deleted.

Normal cycles then drain the queue, and only documents that actually changed are emitted. Once the queue is empty, each sink gets a fresh base.

## Crash safety

- The SQL side is read-only. A crash before the Pebble commit just repeats the cycle.
- Pebble commits are atomic and synced: state, outbox and checkpoint move together.
- Sinks write files under a temp name, sync, rename, then replace the manifest. Unreferenced files are cleaned on open. Generations already written are skipped on replay.
- The randomized integration test injects failures:
  - after the SQL commit;
  - before and after the state commit;
  - before and after sink writes.

  After restarts, the output must equal a full rebuild.

## Packages

| Package | Role |
|---|---|
| `internal/config` | YAML config, defaults, validation |
| `internal/source/sqlserver` | catalog, session, change capture, lookups, fragments |
| `internal/routing` | routing plan and hop-by-hop propagation |
| `internal/assemble` | fragment rows → documents, drift detection |
| `internal/schema` | SQL types → logical types, schema hash |
| `internal/doc`, `internal/keyenc` | canonical value encodings |
| `internal/state` | Pebble layout: checkpoint, docs, key map, pending set, outbox |
| `internal/engine` | cycle loop, snapshots, publishing, commands, metrics |
| `internal/sink`, `internal/sink/parquet` | segment logs, manifest, Parquet writer |
| `internal/dashboard` | web UI, admin API, `/metrics` |
| `internal/tools` | doctor, introspect, fanout, assemble |
