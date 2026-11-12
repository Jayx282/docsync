# Consumer protocol (parquet_log and ndjson sinks)

This is the contract between docsync and whatever reads its output, for example a Rust serving index built on Arrow and DataFusion. It is stable within `format_version: 1`.

## Directory layout

```
<sink dir>/
  manifest.json                                   # the commit point; replaced atomically
  base/<generation, 20 digits>/part-00000.parquet # full snapshot of every document at that generation
  base/<generation>/part-00001.parquet ...
  segments/<from>-<to>.parquet                    # document changes for generations from..to (inclusive)
```

Files are written under a `.tmp` name, synced to disk and renamed. A file that the manifest does not mention is not committed: ignore it, and expect docsync to delete it. The manifest is written last, so anything it lists is complete.

## manifest.json

```json
{
  "format": "docsync-parquet-log",
  "format_version": 1,
  "document": "stone",
  "root_key": "stoneid",
  "epoch": 1,
  "schema_hash": "fe87a281df01…",
  "base": {
    "generation": 57,
    "path": "base/00000000000000000057",
    "files": ["part-00000.parquet"],
    "documents": 5128,
    "created_at": "2026-11-07T16:17:32.4Z"
  },
  "segments": [
    { "from": 58, "to": 60, "path": "segments/00000000000000000058-00000000000000000060.parquet",
      "upserts": 41, "deletes": 1, "created_at": "2026-11-07T16:17:34.1Z" }
  ],
  "latest_generation": 72,
  "updated_at": "2026-11-07T16:17:45.9Z"
}
```

## Generations

- A **generation** is one committed sync cycle: an atomic batch of document upserts and deletes.
- Generation numbers increase by exactly one, with no gaps, across the whole life of a state directory.
- A segment covers a contiguous range of generations. Ranges of consecutive segments touch (`to + 1 == next.from`).
- A generation is a consistent batch, but the index as a whole is eventually consistent. Some documents may still be queued (for example after a huge fan-out). They arrive in later generations.

## Epochs

The **epoch** changes when the document shape or definition changes (a config change, or `docsync new-epoch` after schema drift). When the epoch changes:

1. docsync re-queues every document and writes a new base for the new epoch.
2. Only then does the manifest switch to the new epoch, with that base and an empty segment list.
3. Consumers must discard their state and reload from the new base. Columns may have changed.

Until the new base exists, the manifest keeps describing the previous epoch, so consumers keep serving it.

## File columns

Base and segment files share one Arrow schema:

| Column | Type | Meaning |
|---|---|---|
| `_gen` | int64 | Generation that produced this row. In a base it is the generation of the document's last change |
| `_op` | utf8 | `upsert` or `delete` |
| `_root` | key type | The document's root key (for example int64 `stoneid`) |
| `_doc_json` | utf8, nullable | The full document as canonical JSON (present when `include_json: true`) |
| one column per document field | typed | Fragment columns. `cardinality: many` fragments are `list<struct<…>>` |

Rules:
- **Upserts carry the complete document.** Replace whatever you had for that root. There are no partial updates.
- **Deletes** have all document columns null.
- The Arrow schema metadata carries `docsync.document`, `docsync.root_key` and `docsync.schema_hash`.

Type mapping from SQL Server:

| SQL Server | Arrow |
|---|---|
| bit | bool |
| tinyint, smallint, int, bigint | uint8, int16, int32, int64 |
| real, float | float32, float64 |
| decimal(p,s), numeric | decimal128(p,s) |
| money | decimal128(19,4) |
| char, varchar, nchar, nvarchar, text, xml | utf8 |
| uniqueidentifier | utf8 (upper-case canonical form) |
| date | date32 |
| datetime, datetime2, smalldatetime | timestamp[us] (no zone; datetime2(7) is truncated to microseconds) |
| datetimeoffset | timestamp[us, UTC] |
| time | time64[us] |
| binary, varbinary, rowversion | binary |

## NDJSON files

An `ndjson` sink uses the same layout, manifest and rules, with `.ndjson` files and `"format": "docsync-ndjson-log"`. Each line is one row:

```json
{"gen":58,"op":"upsert","root":1042,"doc":{"carat":1.250,"inclusions":[{"kind":"cloud","position":1}],"stoneid":1042}}
{"gen":58,"op":"delete","root":977,"doc":null}
```

`doc` is the document as canonical JSON, the same text as `_doc_json`:

- keys sorted;
- decimals as exact numbers;
- dates and times as ISO 8601 strings;
- bytes as base64.

## Consumer algorithm

```text
state: epoch, applied_generation, documents

on start, or when manifest.epoch != epoch:
    m = read manifest.json
    documents = load every file in m.base.files
    epoch = m.epoch
    applied_generation = m.base.generation

loop every ~500 ms:
    m = read manifest.json
    if m.epoch != epoch: restart from the base (above)
    for seg in m.segments ordered by from:
        if seg.to <= applied_generation: skip
        if seg.from > applied_generation + 1: missing data -> restart from the base
        for row in seg where row._gen > applied_generation, grouped by _gen in order:
            apply all rows of one generation together (upsert replaces, delete removes)
            applied_generation = row._gen
    publish a new serving generation once a whole generation (or batch) is applied
```

## Retention

- Segments are deleted only after a newer base covers them **and** they are older than `retain_segments` (default 1 hour).
- Old base directories are deleted after the same period.
- A consumer that falls further behind than that must reload from the current base.
- docsync also keeps recent generations in its own state (`outbox_retention`). If a sink falls behind the outbox, docsync writes it a new base.

## Reading with arrow-rs (sketch)

```rust
let file = std::fs::File::open(path)?;
let reader = parquet::arrow::arrow_reader::ParquetRecordBatchReaderBuilder::try_new(file)?.build()?;
for batch in reader {
    let batch = batch?;
    let gens = batch.column_by_name("_gen").unwrap().as_primitive::<Int64Type>();
    let ops  = batch.column_by_name("_op").unwrap().as_string::<i32>();
    // apply rows with gens.value(i) > applied_generation ...
}
```

## A complete reader

[examples/reader/read_output.py](../examples/reader/read_output.py) implements this protocol in plain Python, for Parquet and NDJSON output. It handles epochs, gaps after retention, and following a live directory.
