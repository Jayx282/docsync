#!/usr/bin/env python3
"""Read a docsync output directory: load the base, apply the segments, keep up.

This is the consumer algorithm from docs/consumer-protocol.md, written out plainly so
every step is visible. Use it to look at what docsync wrote, or as the starting point
of your own loader. It keeps every document in memory as a Python dict; a real index
would keep the Arrow columns instead.

    pip install pyarrow                       # for parquet_log output; ndjson needs nothing
    python read_output.py ./out               # what the directory holds
    python read_output.py ./out --id 1042     # one document
    python read_output.py ./out --dump        # every document, one JSON line each
    python read_output.py ./out --follow      # keep applying generations as they arrive
"""

import argparse
import base64
import datetime
import decimal
import itertools
import json
import math
import os
import signal
import sys
import time

FORMATS = ("docsync-parquet-log", "docsync-ndjson-log")
META = ("_gen", "_op", "_root", "_doc_json")  # Parquet columns that are not document fields


class Reload(Exception):
    """Something needed to catch up is gone, so start again from the current base."""


class Output:
    """The documents in one docsync sink directory, as of generation `applied`."""

    def __init__(self, directory):
        self.dir = directory
        self.manifest = {}
        self.epoch = None
        self.applied = 0  # the newest generation applied
        self.docs = {}  # root key -> document

    def read_manifest(self):
        with open(os.path.join(self.dir, "manifest.json"), encoding="utf-8") as f:
            m = json.load(f)
        if m.get("format") not in FORMATS or m.get("format_version") != 1:
            raise SystemExit(f"{self.dir}: this reader knows format_version 1 of {FORMATS}, "
                             f"not {m.get('format')} version {m.get('format_version')}")
        return m

    def load(self):
        """Load the base named by the manifest, then apply every segment after it."""
        m = self.read_manifest()
        self.docs, self.applied = {}, 0
        base = m.get("base")
        if base:  # no base yet: the first full load is still running
            for name in base["files"]:
                for row in self.rows(m, base["path"] + "/" + name):
                    self.apply(row)
            self.applied = base["generation"]
        self.epoch = m["epoch"]
        self.catch_up(m)

    def poll(self, on_generation=None):
        """Apply whatever is new. Returns "reloaded" if it had to start again from a base."""
        m = self.read_manifest()
        if m["epoch"] == self.epoch:
            try:
                self.catch_up(m, on_generation)
                return None
            except Reload:
                pass  # fell too far behind: segments we still needed were removed
        # A new epoch (the document definition changed) or a gap: start from the base.
        self.load()
        return "reloaded"

    def catch_up(self, m, on_generation=None):
        for seg in sorted(m.get("segments") or [], key=lambda s: s["from"]):
            if seg["to"] <= self.applied:
                continue  # applied already
            if seg["from"] > self.applied + 1:
                raise Reload(f"generations {self.applied + 1} to {seg['from'] - 1} are no longer kept")
            rows = [r for r in self.rows(m, seg["path"]) if r["gen"] > self.applied]
            rows.sort(key=lambda r: r["gen"])  # a stable sort keeps file order within a generation
            for gen, batch in itertools.groupby(rows, key=lambda r: r["gen"]):
                batch = list(batch)
                for row in batch:  # a generation is applied whole, never in part
                    self.apply(row)
                self.applied = gen
                if on_generation:
                    on_generation(gen, batch)
            self.applied = seg["to"]
        self.manifest = m

    def apply(self, row):
        if row["op"] == "delete":
            self.docs.pop(row["root"], None)
        else:
            self.docs[row["root"]] = row["doc"]  # an upsert carries the whole document

    def rows(self, m, path):
        """Yield {gen, op, root, doc} for each row of one base or segment file."""
        try:
            if m["format"] == "docsync-ndjson-log":
                yield from ndjson_rows(os.path.join(self.dir, path))
            else:
                yield from parquet_rows(os.path.join(self.dir, path))
        except FileNotFoundError as e:  # removed by retention after we read the manifest
            raise Reload(str(e)) from e

    def get(self, key):
        """The document with this root key, given as text (as on a command line)."""
        if key in self.docs:
            return self.docs[key]
        try:
            return self.docs.get(int(key))
        except ValueError:
            return None

    def summary(self):
        m, base = self.manifest, self.manifest.get("base")
        segs = m.get("segments") or []
        return "\n".join([
            f"docsync output in {self.dir}",
            f"  document   {m.get('document')}, keyed by {m.get('root_key')}",
            f"  epoch      {self.epoch}",
            "  base       " + (f"generation {base['generation']}, {base['documents']:,} documents" if base
                               else "none yet (the first full load is still running)"),
            "  segments   " + (f"{len(segs)}, generations {min(s['from'] for s in segs)} to {max(s['to'] for s in segs)}"
                               if segs else "none after the base"),
            f"  applied    generation {self.applied}: {len(self.docs):,} documents",
        ])


def parquet_rows(path):
    import pyarrow.parquet as pq  # imported here so that ndjson output needs no pyarrow

    table = pq.read_table(path)
    fields = [n for n in table.column_names if n not in META]
    for batch in table.to_batches():
        for r in batch.to_pylist():
            # Typed columns: Decimal, datetime, date, time, bytes, and lists of dicts for
            # nested lists. _doc_json, when present, holds the same document as JSON.
            doc = None if r["_op"] == "delete" else {k: r[k] for k in fields}
            yield {"gen": r["_gen"], "op": r["_op"], "root": r["_root"], "doc": doc}


def ndjson_rows(path):
    with open(path, encoding="utf-8") as f:
        for line in f:
            if line.strip():  # each line is already {"gen", "op", "root", "doc"}
                yield json.loads(line, parse_float=decimal.Decimal)


def to_json(v, indent=None, depth=0):
    """JSON in docsync's style: keys sorted, decimals exact, times in ISO 8601, bytes in base64."""
    if isinstance(v, (dict, list)):
        if not v:
            return "{}" if isinstance(v, dict) else "[]"
        pad = "" if indent is None else "\n" + " " * indent * (depth + 1)
        end = "" if indent is None else "\n" + " " * indent * depth
        if isinstance(v, dict):
            colon = ":" if indent is None else ": "
            items = [json.dumps(k, ensure_ascii=False) + colon + to_json(v[k], indent, depth + 1) for k in sorted(v)]
            return "{" + pad + ("," + pad).join(items) + end + "}"
        return "[" + pad + ("," + pad).join(to_json(x, indent, depth + 1) for x in v) + end + "]"
    if isinstance(v, decimal.Decimal):
        return str(v)
    if isinstance(v, float) and not math.isfinite(v):
        return '"NaN"' if math.isnan(v) else ('"Infinity"' if v > 0 else '"-Infinity"')
    if isinstance(v, datetime.datetime):
        if v.tzinfo is not None:  # datetimeoffset: docsync writes UTC with a Z
            return '"' + clock(v.astimezone(datetime.timezone.utc), "%Y-%m-%dT%H:%M:%S") + 'Z"'
        return '"' + clock(v, "%Y-%m-%dT%H:%M:%S") + '"'
    if isinstance(v, datetime.date):
        return '"' + v.isoformat() + '"'
    if isinstance(v, datetime.time):
        return '"' + clock(v, "%H:%M:%S") + '"'
    if isinstance(v, bytes):
        return '"' + base64.b64encode(v).decode("ascii") + '"'
    return json.dumps(v, ensure_ascii=False)  # str, int, float, bool, None


def clock(v, fmt):
    """Seconds, then microseconds without trailing zeros."""
    frac = f"{v.microsecond:06d}".rstrip("0")
    return v.strftime(fmt) + ("." + frac if frac else "")


def dump(out):
    """One JSON line per document, ordered by root key, shaped like the ndjson sink's lines."""
    for root in sorted(out.docs):
        yield '{"root":' + to_json(root) + ',"doc":' + to_json(out.docs[root]) + "}"


def follow(out, interval):
    def count(n, word):
        return f"{n:,} {word}" + ("" if n == 1 else "s")

    def report(gen, batch):
        deletes = sum(1 for r in batch if r["op"] == "delete")
        print(f"generation {gen}: {count(len(batch) - deletes, 'upsert')}, {count(deletes, 'delete')}; "
              f"{count(len(out.docs), 'document')}", flush=True)

    print(f"following {out.dir}, Ctrl-C to stop", flush=True)
    while True:
        time.sleep(interval)
        try:
            if out.poll(report) == "reloaded":
                print(f"reloaded from the base: epoch {out.epoch}, generation {out.applied}, "
                      f"{len(out.docs):,} documents", flush=True)
        except (Reload, OSError, ValueError) as e:  # caught mid-change: try again next time
            print(f"will retry: {e}", file=sys.stderr, flush=True)


def main():
    p = argparse.ArgumentParser(description="Read a docsync output directory (a parquet_log or ndjson sink).")
    p.add_argument("dir", help="the sink directory, which holds manifest.json")
    what = p.add_mutually_exclusive_group()
    what.add_argument("--id", metavar="KEY", help="print the document with this root key")
    what.add_argument("--dump", action="store_true", help='print every document as a JSON line {"root": ..., "doc": ...}')
    p.add_argument("--follow", action="store_true", help="then keep polling, printing each generation as it is applied")
    p.add_argument("--interval", type=float, default=0.5, metavar="SECONDS",
                   help="time between polls with --follow (default 0.5)")
    a = p.parse_args()
    if hasattr(signal, "SIGPIPE"):  # exit quietly when piped into head or less
        signal.signal(signal.SIGPIPE, signal.SIG_DFL)

    out = Output(a.dir)
    try:
        out.load()
    except FileNotFoundError:
        raise SystemExit(f"{a.dir}: no manifest.json. Point at a sink directory, for example "
                         "studio/pipelines/<name>/out, once docsync has written to it.")
    except Reload as e:
        raise SystemExit(f"{a.dir}: the output changed while it was being read ({e}). Run again.")

    if a.id is not None:
        doc = out.get(a.id)
        if doc is None:
            raise SystemExit(f"no document with root key {a.id}")
        print(to_json(doc, indent=2))
    elif a.dump:
        for line in dump(out):
            print(line)
    else:
        print(out.summary())

    if a.follow:
        try:
            follow(out, a.interval)
        except KeyboardInterrupt:
            pass


if __name__ == "__main__":
    main()
