# docsync Studio

Studio is a web designer for docsync documents, served by the `docsync` binary. In Studio you:
1. browse a SQL Server database;
2. design a document;
3. preview it on real data;
4. check what the database needs;
5. deploy it as a running pipeline.

Everything it builds compiles to a normal docsync config, which you can read, download and run without Studio.

## Start it

```bash
export DOCSYNC_DSN='sqlserver://user:pass@host:1433?database=mydb'
docsync studio --dir ./studio
```

Studio prints a sign-in link such as `http://127.0.0.1:8090/?token=…` and opens it. The link signs the browser in by storing the token in an HttpOnly cookie.

- **The token is remembered.** It is created on first start and saved in `<dir>/.token`, so the link and your browser's sign-in keep working after restarts.
- **To sign in from another browser,** open `http://127.0.0.1:8090` and paste the token from that file. On macOS, `pbcopy < studio/.token` puts it on the clipboard.
- **To issue a new token,** delete the file. Every browser then has to sign in again.
- **To choose the token yourself,** pass `--token` or set `DOCSYNC_STUDIO_TOKEN`. Then nothing is saved.

| Flag | Default | Meaning |
|---|---|---|
| `--dir` | `./studio` | Where designs, deployed pipelines and their output live |
| `--listen` | `127.0.0.1:8090` | Address to listen on. Studio warns when this is not a loopback address |
| `--dsn-env` | `DOCSYNC_DSN` | Environment variable holding the connection string |
| `--token` | saved in `<dir>/.token` | Access token (or `DOCSYNC_STUDIO_TOKEN`) |
| `--no-browser` | off | Do not open a browser |

## Workflow

**Explore.** Search the table list. A table page shows:
- its columns, primary key and indexes;
- a graph of its neighbours. Solid edges are declared foreign keys; dashed edges are guessed from column names.

From a table with a single-column primary key, choose **Design documents from this table**.

**Design.** A design has a root and related data.
- **Root.** The root table gives one document per row. Tick the columns to include and rename any field (the box after the arrow).
- **Related data.** Add tables from the suggestions, or with **Custom join…**. Suggestions come from:
  - foreign keys in both directions;
  - tables that have a column named like the root key (child tables);
  - root columns that look like another table's key (`lab_code` → `lab.code`);
  - bridge tables that link the root to a third table (many-to-many).

  Guessed suggestions are labelled. Check the join before you add one.
- **Fields or list.**
  - *Fields* puts at most one related row's columns at the top level, or under **Nest under**. Lookup fields get the relation's name as a prefix, such as `lab_name`.
  - *List* collects every related row into a named list. Items are sorted by the primary key unless you choose sort columns.

  Studio warns when a join can match several rows but is shown as fields. Documents with more than one match then fail to build.
- **Measure on live data.** This counts items per document and how many documents one related row affects (fan-out).
- **Use only in calculated fields.** This leaves a relation's rows out of documents. Calculated fields can still summarise it, for example `COUNT(rates)`, and its changes still rebuild the documents.
- **Only rows where.** This is a filter on the root table, for example `status <> "sold"`.
  - Only matching rows become documents. A row that stops matching leaves the index, and one that starts matching joins it.
  - **Count rows** shows how many rows match.
- **Calculated fields.** These add fields computed with formulas over the root, its lookups and summaries of its lists, for example `ROUND(SUM(rates.amount) / carat, 2)` or `COUNTIF(inclusions.severity >= 3)`.
  - Click a field, list summary or function in the palette to insert it into the formula.
  - Errors appear under the formula as you type.
  - [formulas.md](formulas.md) is the reference.

**Results panel.** It updates after every edit.
- **Preview.** Three documents built from your data. Enter ids to preview specific roots.
- **Shape.** The output schema, with nested types and their SQL types.
- **Config.** The generated docsync YAML. You can copy or download it.
- **SQL.** One generated query per part of the document.
- **Checks.** Runs `doctor` against the database, then writes a script for your DBA. The script covers snapshot isolation, change tracking, missing indexes on routing columns, and grants.

**Deploy.** Shows what changes before anything runs:
- whether this is the first deployment;
- whether only settings change;
- whether the document definition changes, which starts a new epoch and rebuilds every document;
- a diff of the config.

It then runs the checks, including computing every calculated field once for every document. Finally it saves the design, writes `pipelines/<name>/docsync.yaml` and starts the pipeline. The **Pipeline** tab shows its dashboard: lag, throughput, pending work, outputs, the document inspector and admin actions. **Stop** halts it and stops it restarting with Studio.

Unsaved edits survive switching tabs and designs; reloading the page warns first. Press ⌘S or Ctrl+S to save.

## Files

```
studio/
├── designs/<name>.json            # what the designer edits
├── active.json                    # the deployed pipeline, restarted when Studio starts
└── pipelines/<name>/
    ├── docsync.yaml               # compiled config; runnable with `docsync run --config`
    ├── design.json                # the design that produced it
    ├── history/<time>.*           # every deployed version
    ├── state/                     # engine state (Pebble)
    └── out/                       # manifest, base snapshots and segments (docs/consumer-protocol.md)
```

Compiled configs read the connection string from the same environment variable (`dsn_env`). Shared rows get `hot_fanout_limit: 2000`: lookups, and rows reached through a bridge.

## Security

- Studio only answers requests whose `Host` is `localhost`, `127.0.0.1` or `::1`, which blocks DNS rebinding. It refuses cross-site writes and frames only its own pages.
- Every request needs the token, from the cookie or an `Authorization: Bearer` header.
  - The token file is readable only by you on macOS and Linux. On Windows, the folder's access rules protect it.
  - Studio adds the file to the studio folder's `.gitignore`, so committing your designs doesn't commit the token.
- The connection string never leaves the process. It is not written to designs, configs or logs.
- Studio and the pipelines it runs share one login.
  - Studio lists only the tables that login can see. Grant `VIEW DEFINITION` on the database to browse all of them.
  - Pipelines need `SELECT` and `VIEW CHANGE TRACKING` on the tables they read. The **Checks** script lists these grants.

## Current limits

- One deployed pipeline at a time, and one database per Studio process.
- Each table can appear once in a design.
- Related tables attach to the root, directly or through one bridge table. For deeper paths, edit the compiled YAML: the engine supports any number of hops.
- Lists can't contain lists.
- A root filter can use only the root table's columns.

See [studio-plan.md](studio-plan.md) for the next phases.

## API

The UI uses a JSON API under `/api/studio/`. It is handy for scripts with `Authorization: Bearer <token>`:

| Method and path | Purpose |
|---|---|
| `GET connection` · `POST schema/refresh` | Server info; reload the schema |
| `GET tables` · `GET tables/{name}` | Table list; one table with keys, indexes, foreign keys and suggestions |
| `GET suggestions?root=` · `POST profile` | Related-table suggestions; measure one relation |
| `GET/PUT/DELETE designs/{name}` · `GET designs` | Saved designs |
| `POST compile` · `preview` · `validate` · `deploy` | Body: `{"design": …, "ids": […], "limit": n}` |
| `POST deploy/plan` · `filter/count` | What deploying would change; how many rows the filter keeps. Body: `{"design": …}` |
| `GET runtime` · `POST runtime/stop` | The deployed pipeline |

The running pipeline's own dashboard and API are mounted at `/pipeline/`.
