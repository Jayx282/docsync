# docsync Studio: plan for a visual index designer

Status (2026-11-10):
- **Phase 1 (Designer MVP) is implemented.** See [Phase 1 as built](#phase-1-as-built) and the user guide [studio.md](studio.md).
- **Phase 2 is partly done:**
  - formulas, with summaries over lists ([formulas.md](formulas.md));
  - root filters, which need the engine's new `document.root.filter`;
  - relations used only by calculations;
  - a deploy preview with a config diff and rebuild impact.
- **Still open in Phase 2:** lists inside lists, custom SQL nodes, design versions and rollback.
- **Phases 3–4** are proposals. The engine described in [architecture.md](architecture.md) exists; this plan adds a designer and a multi-index control plane on top of it.

## 1. Where we are

**The engine is already generic.** It syncs any SQL Server database, any root table and any related tables, with any calculation expressible in SQL. The stone index is only the bundled example.

**Its interface is YAML and SQL written by hand**, and the engine has four limits:
- one index per process;
- a single-column root key;
- nested lists only one level deep;
- SQL Server as the only source.

**Studio** is the missing layer. You:
1. connect to SQL Server;
2. see every table;
3. pick the root and the related data;
4. pick fields and add formulas;
5. preview real documents;
6. validate;
7. deploy;
8. operate.

## 2. Research summary: what exists

| Tool | What to learn | Why it doesn't already solve this |
|---|---|---|
| Elastic connectors (Kibana) | Draft → validate → apply for sync rules; an audit trail per sync job | Flat rows only, with no nesting. MSSQL has no incremental sync. Elastic License 2.0 |
| Hasura console | Join suggestions from foreign keys with "track all"; **object vs array relationships**; metadata YAML you can diff | Builds an API layer, not an index. Computed fields aren't available on SQL Server |
| NocoDB | **Formula, lookup and rollup fields**; formula → SQL compilation with a typed validator and a function allow-list | SQL Server is a paid add-on; the license changed to Sustainable Use (not open source) in 2026 |
| Power Query | Applied steps with a preview at every step; merge → nested table → expand or aggregate; **folding indicators** that show what runs in the database | Proprietary; Microsoft-only |
| Airbyte / Fivetran / Estuary | Column-selection grids; a policy for new columns; review of pending schema changes | Flat replication, not documents |
| ADF mapping data flows | Nested arrays via `collect()`; inspecting the generated script | Paid Spark runtime; on-prem setup is complex |
| Apache Hop / KNIME / Tableau Prep | Excel-style formula transforms; "evaluate on the first 10 rows"; join summaries | Generic ETL, not incremental document sync |
| PGSync | Declarative nested documents (`children`, object vs scalar) | No SQL Server, no UI |
| Azimutt / tbls / ChartDB | Big schemas: search, expand step by step, saved viewpoints. A full diagram of 262 tables is "almost unreadable" | Diagrams only |

**Gap confirmed.** No free tool combines all of these:
- SQL Server as the source;
- nested documents, one per root;
- incremental updates in seconds;
- Parquet output;
- a visual designer.

**Patterns to adopt.** These recur across the best tools:
1. Suggest relationships from foreign keys.
2. The visual model is always inspectable as code.
3. Preview on a sample at every step.
4. Show what runs at the source.
5. Nest by relating tables, then collecting child rows.
6. Column grids with a policy for new columns.
7. Draft → validate → apply, with a diff.
8. Search, expand step by step and save subsets for big schemas.

## 3. Product principles

1. **Code underneath.**
   - Everything the UI builds compiles to a normal docsync config (YAML + SQL).
   - You can view, diff, export and commit it to git.
   - The UI never stores anything the engine cannot read.
2. **All logic runs as SQL.**
   - Joins, filters, formulas and rollups compile to T-SQL fragments.
   - This keeps the engine's guarantee that incremental output equals a full rebuild.
   - It also uses the database's own indexes.
   - The generated SQL is always visible.
3. **Preview with real data at every step.** Assemble actual documents for sample root ids before anything is deployed.
4. **Draft → validate → deploy, with versions and rollback.** Deploys report their impact (for example, "this change starts a new epoch").
5. **Safe by default.**
   - Design with a read-only login; enabling change tracking is a separate, explicit step for a DBA.
   - Listen on localhost unless authentication is configured.
6. **Free and open.** Apache-2.0, MIT-licensed UI dependencies, no paid services.

## 4. User workflow

1. **Connections.**
   - Authentication: SQL login, Windows single sign-on (no stored password; supported by go-mssqldb) or Entra ID.
   - "Test" reports the version, edition, change-tracking and snapshot-isolation status, and missing permissions.
2. **Schema explorer.**
   - Search across all tables and columns.
   - A detail panel per table: columns, types, keys, foreign keys in and out, indexes, row count, size, change-tracking status.
   - A relationship graph that starts from one table and expands on click (never all tables at once).
   - Saved viewpoints.
3. **New index, step 1: choose the root (the grain).** "One document per row of `dbo.stone`." The key is detected from the primary key, and the document count is shown.
4. **Step 2: add related data.**
   - Suggestion cards come from three sources:
     - foreign keys;
     - column-name matches (the `introspect` logic);
     - the base tables behind existing views.
   - Each card shows the measured shape, for example "each stone has 0–12 inclusions (average 3)".
   - The user chooses how each relation appears:
     - **Fields**: a lookup or one-to-one relation, as an object relationship;
     - **List**: one-to-many, as an array relationship;
     - **Through a bridge table**: many-to-many.
   - A manual join builder covers databases without foreign keys.
5. **Step 3: pick fields.**
   - Checkboxes per table, with output names and placement (top level or a nested object).
   - Type badges show the SQL → Parquet mapping.
   - A policy decides what happens to columns added later.
6. **Step 4: calculated fields.**
   - A formula editor with autocomplete, inline type errors and "evaluate on a sample".
   - Rollups (count, sum, min/max/avg, any/all, join text) over any list, optionally filtered.
   - Lookups through dotted paths (`lab.name`).
7. **Step 5: filters and ordering.** Which root rows become documents (`status <> 'deleted'`); sort order and limits for lists.
8. **Step 6: output.** Sink type and directory, the `_doc_json` option, and segment/base timings.
9. **Live preview, throughout.**
   - Three sample documents as JSON and as columns.
   - Document size.
   - "Show generated SQL", with every element marked as running in SQL. Custom SQL is marked opaque.
10. **Validate.**
    - The `doctor` checks, with generated scripts for the DBA: `ALTER TABLE … ENABLE CHANGE_TRACKING`, `CREATE INDEX …` and `GRANT VIEW CHANGE TRACKING`.
    - `fanout` for every relationship, with a suggested `hot_fanout_limit`.
    - Formula type checks.
    - Estimates: number of documents, Parquet size and initial-load time (from measured throughput).
11. **Deploy.**
    - Diff against the running version, classified as either no rebuild needed or a rebuild (new epoch) with the reason.
    - Initial-load progress.
    - Rollback to any previous version.
12. **Operate.**
    - The existing dashboard, per index: freshness, fan-out, inspector, actions.
    - Alerts through webhook or email.
    - Sweeper corrections.
    - Guided handling of schema drift: what changed, then "start new epoch".
13. **Consume.**
    - Each index gets a page with the output path, a field-by-field schema reference, and sample reader code (Rust arrow-rs, Python pyarrow, .NET, Go).
    - The page links to [consumer-protocol.md](consumer-protocol.md).

## 5. Formulas

**No reusable library exists.** We found no maintained, permissively licensed formula → T-SQL library:
- NocoDB's SQL Server mapping is a paid add-on under a non-open-source license;
- PRQL lists MSSQL as unsupported;
- cel2sql has no SQL Server target.

We build a small compiler, using NocoDB's architecture as the model: parser → name resolution → type checker → per-function T-SQL mapping, with an allow-list.

- **Syntax:** Excel/Airtable style, which users already know.
  - Field names, `{Field Name}` for names with spaces, and dotted paths across relationships.
  - `IF`, `IFS`, `SWITCH`, `AND`/`OR`/`NOT`, `ISBLANK`, `COALESCE`, arithmetic, `&` or `CONCAT`.
  - Text functions (`LEFT`, `UPPER`, `TRIM`, `REPLACE`…) and date functions (`YEAR`, `DATEADD`, `DATEDIFF`…).
- **Rollups over a list:** `COUNT(inclusions)`, `SUM(rates.amount)`, `MAX(inclusions.severity)`, `COUNTIF(inclusions, kind = "feather")`, `JOIN(orders.party, ", ")`.
  - Compiled to correlated subqueries or `OUTER APPLY` per list.
  - Both use index seeks for small batches of roots, which matches how the engine runs.
- **Semantics, defined explicitly:** SQL NULL rules, plus blank-handling functions.
  - Over an empty list, `COUNT` and `SUM` give 0; `MIN`, `MAX` and `AVG` give blank (as in NocoDB).
  - The documented differences from Excel are listed in the editor's help.
- **Determinism:** `TODAY()` and `NOW()` are rejected. Documents must not depend on when they were built, or incremental output would stop matching a full rebuild. Time-relative logic belongs at query time.
- **Safety:**
  - Identifiers come from the catalog and are bracket-quoted.
  - Literals become parameters or `N'…'` with escaping.
  - Functions come from the allow-list; there is no string concatenation of user SQL.
- **Type checking:** in the compiler, from catalog types, then confirmed by SQL Server. The engine runs each fragment with an empty `#roots` to read its result columns. `sp_describe_first_result_set` can't be used here, because it doesn't support temp tables.
- **Escape hatch: raw T-SQL expressions** for power users.
  - Parsed with a T-SQL grammar (bytebase/parser, ANTLR, BSD-3) to make sure it is one scalar expression.
  - Dependencies checked with the existing SHOWPLAN check in `doctor`.
- **Later, maybe:** document-level formulas evaluated in Go with expr-lang/expr (MIT; sandboxed; has sum/filter/any) for logic SQL can't express. Not in v1: one execution model is easier to keep correct.

## 6. Web or desktop: web first, served by the same binary

The research favours a web UI served by the docsync binary itself.

**Reasons:**
- **The engine runs on the server anyway.** The designer needs live access to the same SQL Server, pipelines and state. Keeping them together avoids copying designs between machines.
- **Zero distribution cost:**
  - Desktop apps need code signing to avoid SmartScreen warnings, and Windows 11 Smart App Control blocks unsigned, unknown apps.
  - Azure Artifact Signing ($9.99/month) is open to individual developers only in the USA and Canada.
  - OV/EV certificates cost about $150–400+ per year, and EV no longer skips SmartScreen.
  - SignPath Foundation is free for open-source projects, but signs as "SignPath Foundation" and has strict rules.
  - macOS needs the $99/year Apple Developer Program.
- **Multi-user from day one.** The team designs and monitors from any browser on the intranet, and updates mean replacing one binary.
- **It still feels local.** `docsync studio` opens the browser at `http://127.0.0.1:…` with a one-time token, the same pattern Jupyter uses.

**Later, if needed:** for offline single-user design, wrap the same UI with **Wails v3**: Go-native, stable since October 2026, with a built-in signed updater and NSIS/MSIX/DMG packaging. No rewrite.

## 7. Architecture

```
Browser ── React + TypeScript SPA (schema explorer, canvas, formula editor, previews, dashboards)
   │ REST + Server-Sent Events
docsync server (one Go binary, Windows or Linux)
 ├─ HTTP: auth (localhost token / local accounts / OIDC), Origin + CSRF checks
 ├─ Studio services
 │    catalog & introspection · relationship inference & profiling · index compiler
 │    formula compiler · preview runner · validator (doctor, fanout) · deploy & versions
 ├─ Control-plane store: SQLite (pure-Go driver) — connections, index definitions + versions,
 │    users, audit log   (optional GitOps mode: definitions as files in a repo)
 └─ Runtime supervisor: one docsync engine per index → Parquet sinks
         │
   SQL Server (Change Tracking)
```

**Index definition.** The UI's model is a tree, compiled to a docsync config:
- **Root node:** table, key, optional filter.
- **Related nodes:** one per related table, each with:
  - a relationship: fields, list or through a bridge;
  - joins;
  - picked fields with output names;
  - list ordering.
- **Calculated fields.**

The compiler emits:
- one root fragment holding scalar fields, lookups and rollups;
- one fragment per list;
- routing rules for every table touched, including bridge and lookup tables.

Hand-written YAML can be imported. Custom SQL fragments appear as opaque nodes with editable routing.

**Libraries (all free):**

| Need | Choice | Why |
|---|---|---|
| UI | React 19 + TypeScript, built with Vite, embedded with `go:embed` (`all:` prefix) | standard, MIT |
| Relationship graph | React Flow (`@xyflow/react`, MIT) + `@dagrejs/dagre` (MIT) | node graphs with custom nodes; expanding on demand keeps node counts low |
| Formula/SQL editor | CodeMirror 6 (MIT) with `@codemirror/lang-sql` (MSSQL dialect, schema autocomplete) and `@codemirror/lint` | smaller than Monaco; Monaco has no T-SQL language package |
| Preview grids | TanStack Table + TanStack Virtual (MIT) | headless, no paid tier; AG Grid Community (MIT) is the alternative |
| Store | SQLite via `modernc.org/sqlite` | pure Go, so still a static binary for Windows |
| Auth | `coreos/go-oidc`, `x/crypto/argon2` for local accounts, Go's `http.CrossOriginProtection` | standard, maintained |
| Secrets | Windows DPAPI (`x/sys/windows`), `zalando/go-keyring`; Windows SSO preferred | no plaintext passwords |
| T-SQL parsing (escape hatch) | `bytebase/parser` (ANTLR T-SQL, BSD-3); SQL Server as the final validator | only Go option; evaluate maturity first |

## 8. Engine work this needs

1. **Multi-index supervisor:** many pipelines per process, each with its own state, sinks and budgets, sharing connection pools.
2. **Index definition model and compiler,** plus YAML import and export.
3. **Lists inside lists** (orders → lines): fragments that declare a parent list and key; assembler and schema support.
4. **Root filters:** an include predicate. Documents leaving the filter are deleted; columns the predicate uses are routed.
5. **Preview API** for unsaved definitions (generalizes `assemble`).
6. **Profiling API:** relationship cardinality and fan-out on samples, with time limits.
7. **Versioned deploys** with impact analysis and hot swap. Epoch handling already exists.
8. **Auth, roles** (viewer, editor, admin) and an **audit log.**
9. **Alerts:** webhook and email.
10. **Composite root keys** (later).

## 9. Roadmap

Rough sizes assume one experienced developer.

| Phase | Scope | Rough size |
|---|---|---|
| 1. Designer MVP | Connections, schema explorer, root and relationship picker with suggestions, field picker, one level of lists, live preview, validation (doctor/fanout), generated SQL view, export YAML and deploy to one pipeline | 4–6 weeks |
| 2. Formulas and richer shapes | Formula compiler and editor, rollups, root filters, lists inside lists, custom SQL nodes, versions, diff, impact analysis, rollback | 4–6 weeks |
| 3. Multi-index platform | Supervisor, per-index dashboards, control-plane store, local and OIDC auth, roles, audit, alerts, consumer docs and snippets | 3–5 weeks |
| 4. Reach | PostgreSQL and MySQL sources, gRPC and Elasticsearch/OpenSearch sinks (with a parity checker), Wails desktop build, plugin interface | ongoing |

### Phase 1 as built

**What exists**

| Plan item | Built |
|---|---|
| Connections | One connection per Studio process, from an environment variable (never stored). A connection manager comes with Phase 3's control plane |
| Schema explorer | Searchable table list. Each table page shows columns, keys, indexes, foreign keys and a neighbour graph (React Flow + dagre): declared and guessed relations, click to navigate |
| Root and relationship picker with suggestions | Suggestions from foreign keys, child tables (column named like the root key), lookups (`lab_code` → `lab.code`) and bridges. Guesses are labelled. Custom joins can be direct or through a bridge |
| Field picker, one level of lists | Column checklists with renaming. Relations show as fields (optionally nested in an object) or as sorted lists |
| Live preview | Rebuilt after every edit from real rows (or chosen ids), with the output schema, generated SQL and config |
| Validation | `doctor` checks, a uniqueness warning for "fields" joins, live fan-out and items-per-document measurement, and a DBA script (snapshot isolation, change tracking, indexes, grants) |
| Export YAML, deploy to one pipeline | The compiled config shows effective settings in readable key order and can be downloaded. Deploy keeps a history and restarts with Studio. The pipeline dashboard is embedded |

**Code**
- `internal/studio`: schema snapshot, design model and validation, compiler, suggester, profiler, service, runtime and HTTP server.
- `web/studio`: React 19 + TypeScript + Vite. The build is embedded with `go:embed`.

**Security:** loopback-only `Host`, a one-time token stored in an HttpOnly SameSite=Strict cookie, cross-origin write protection, and a CSP.

**Tests**
- Unit tests cover suggestions, the compiler, validation, the store and HTTP auth.
- `TestStudio` (integration) builds the stones design from the suggester's output, then previews and validates it. It checks that the compiled config keeps incremental output equal to a full rebuild under random writes. It deploys and stops the design through the HTTP API.

**Known limits, carried into Phase 2**
- A design can use each table once (the engine routes each table along one path).
- Related tables attach to the root, directly or through one bridge.
- No formulas, filters or lists inside lists.
- One running pipeline.
- The randomized *design* generator from the quality gate below is not written yet. Today one fixed design runs under random writes.

**Quality gate for every phase:** the randomized invariant test grows with the compiler. Generate random index definitions over the example schema, compile them, and check that incremental output equals a full rebuild under random writes and injected crashes. Any designer mistake that would produce wrong documents fails CI.

## 10. Open questions

1. **Who designs indexes?** Engineers only, or analysts too? The answer decides whether formulas or SQL is the main surface.
2. **Where do index definitions live?** In Studio's own store (UI first), in a git repository (GitOps), or both?
3. **One admin or many users?** This decides when auth and roles are needed.
4. **Deployment.** Should Studio run next to the engine on the database server, or on a machine of its own?
5. **Other sources.** How soon are PostgreSQL and MySQL needed?

Use cases and opinions on any of these are welcome in an issue.

## Sources (selected)
- **Elastic connectors:**
  - [Kibana connectors UI](https://www.elastic.co/docs/reference/search-connectors/connectors-ui-in-kibana)
  - [Sync rules](https://www.elastic.co/docs/reference/search-connectors/es-sync-rules)
  - [MSSQL connector](https://www.elastic.co/docs/reference/search-connectors/es-connectors-ms-sql)
- **Hasura:**
  - [Relationships](https://hasura.io/docs/2.0/schema/ms-sql-server/table-relationships/index/)
  - [Feature support](https://hasura.io/docs/2.0/databases/feature-support/)
  - [Metadata](https://hasura.io/docs/2.0/migrations-metadata-seeds/manage-metadata/)
- **NocoDB:**
  - [Formulas](https://nocodb.com/docs/product-docs/fields/field-types/formula/formula)
  - [Rollup](https://nocodb.com/docs/product-docs/fields/field-types/links-based/rollup)
  - [Formula builder](https://github.com/nocodb/nocodb/blob/develop/packages/nocodb/src/db/formulav2/formulaQueryBuilderv2.ts)
  - [License](https://nocodb.com/docs/self-hosting/license)
- **Power Query:**
  - [Merge](https://learn.microsoft.com/en-us/power-query/merge-queries-overview)
  - [Folding indicators](https://learn.microsoft.com/en-us/power-query/step-folding-indicators)
- **Column selection:**
  - [Fivetran schema tab](https://fivetran.com/docs/using-fivetran/fivetran-dashboard/connectors/schema)
  - [Airbyte schema configuration](https://docs.airbyte.com/platform/using-airbyte/configuring-schema)
  - [Estuary field selection](https://docs.estuary.dev/guides/customize-materialization-fields/)
- **Big schemas:**
  - [Azimutt](https://azimutt.app/use-cases/explore)
  - [tbls](https://github.com/k1LoW/tbls)
  - [SchemaSpy large-schema issue](https://github.com/schemaspy/schemaspy/issues/155)
- **ADF:** [SQL to nested Cosmos DB documents](https://learn.microsoft.com/en-us/azure/data-factory/how-to-sqldb-to-cosmosdb)
- **Code signing:**
  - [Windows code signing options](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/code-signing-options)
  - [SmartScreen reputation](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/smartscreen-reputation)
  - [SignPath Foundation](https://signpath.org/terms)
  - [Apple Developer Program](https://developer.apple.com/programs/)
- **Wails v3:**
  - [Release notes](https://github.com/wailsapp/wails/releases/tag/v3.0.0)
  - [Updater](https://v3.wails.io/guides/updater)
- **UI libraries:**
  - [React Flow performance](https://reactflow.dev/learn/advanced-use/performance)
  - [CodeMirror lang-sql](https://github.com/codemirror/lang-sql)
  - [AG Grid community vs enterprise](https://www.ag-grid.com/javascript-data-grid/community-vs-enterprise/)
- **Local web UI security:**
  - [Jupyter security](https://jupyter-server.readthedocs.io/en/latest/operators/security.html)
  - [Go CrossOriginProtection](https://pkg.go.dev/net/http#CrossOriginProtection)
- **Formulas:**
  - [expr-lang](https://github.com/expr-lang/expr)
  - [cel-go](https://github.com/cel-expr/cel-go)
  - [Power Fx open source](https://www.microsoft.com/en-us/power-platform/blog/power-apps/power-fx-open-source-now-available/)
  - [PRQL targets](https://prql-lang.org/book/project/target.html)
  - [cel2sql](https://pkg.go.dev/github.com/spandigital/cel2sql/v3)
- **SQL Server:**
  - [sp_describe_first_result_set](https://learn.microsoft.com/en-us/sql/relational-databases/system-stored-procedures/sp-describe-first-result-set-transact-sql)
  - [bytebase/parser](https://github.com/bytebase/parser)
  - [Subquery decorrelation](https://learn.microsoft.com/en-us/archive/blogs/craigfr/decorrelating-subqueries)
