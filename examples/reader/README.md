# Example reader (Python)

[`read_output.py`](read_output.py) reads a docsync output directory the way a consumer should, as the [consumer protocol](../../docs/consumer-protocol.md) describes:

1. Load the base named in `manifest.json`.
2. Apply the segments after it, one whole generation at a time.
3. Keep polling the manifest. Start again from the base when the epoch changes, or when segments it still needs have been removed.

It reads both `parquet_log` and `ndjson` output. It's a way to look at what docsync wrote and a starting point for your own loader. It keeps every document in memory as a Python dict.

From the repository root, after `make demo` has deployed a pipeline:

```bash
pip install pyarrow   # needed for parquet_log output only
python examples/reader/read_output.py studio/pipelines/stone/out                # what the directory holds
python examples/reader/read_output.py studio/pipelines/stone/out --id 1000001   # one document
python examples/reader/read_output.py studio/pipelines/stone/out --dump > documents.ndjson
python examples/reader/read_output.py studio/pipelines/stone/out --follow       # then print each generation as it arrives
```

```
docsync output in studio/pipelines/stone/out
  document   stone, keyed by stoneid
  epoch      1
  base       generation 1, 2,000 documents
  segments   none after the base
  applied    generation 1: 2,000 documents
following studio/pipelines/stone/out, Ctrl-C to stop
generation 2: 3 upserts, 0 deletes; 2,000 documents
```

Documents print as JSON in docsync's style:

- keys sorted;
- decimals exact;
- dates and times in ISO 8601;
- bytes in base64.

It was tested with Python 3.9 and pyarrow 21.
