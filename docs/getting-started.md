# Getting started

This guide takes you from nothing to a running sync in about 15 minutes, using example data. Part 2 then explains how to do the same with your own database.

You only need **Studio**, docsync's web app. The command-line tools (`doctor`, `fanout` and the rest) are for running docsync without Studio, and you can ignore them for now.

## The idea

```
SQL Server tables                 docsync                         your search service
stone, lab, rate, inclusion …  →  one document per stone,     →   reads the files in out/
                                  kept up to date               (Parquet)
```

- **You describe the document once.** "One document per stone, with its lab's name, its inclusions, its prices and a price per carat."
- **docsync builds every document,** then watches SQL Server for changes. When a row changes, docsync rebuilds only the documents that use it, within a few seconds. Renaming a lab rebuilds the stones from that lab, and nothing else.
- **The documents are written as files** your search service reads: a full snapshot, then batches of changes.

Three words you will see everywhere:

| Word | Meaning |
|---|---|
| **Root table** | The table whose rows become documents: one document per row (one per stone in `dbo.stone`). |
| **Related data** | Other tables pulled into each document. **Fields:** one matching row, like the lab's name. **List:** many rows, like all inclusions of the stone. |
| **Pipeline** | A design that is deployed and running: it keeps the documents up to date. |

## Part 1: try it with example data

You need [Go](https://go.dev/dl/) 1.26 or later and Docker (Docker Desktop, Colima or Lima). Run everything from the repository folder.

### 1. Start the demo

```bash
make demo
```

This does everything for you:
1. It starts SQL Server 2022 in Docker. The first run downloads it, about 1.5 GB.
2. It creates a database with 2,000 fictional stones, their labs, inclusions, prices and orders.
3. It opens Studio in your browser, already signed in.

Leave this terminal open. Studio runs until you press Ctrl+C there.

> Studio asks for a token if the browser isn't signed in. The terminal prints a sign-in link, and the token is also saved in `studio/.token`.

### 2. Look at the database (Explore)

![Explore: a table's columns and the tables connected to it](images/studio-explore.png)

The left side lists every table. Click **dbo.stone**.
- **The graph** shows the tables connected to stone. Arrows point at the table being referred to. Dashed lines are links guessed from column names; this demo database declares no foreign keys.
- **Below the graph** are its columns, keys and indexes.

Now click **Design documents from this table**.

### 3. Choose the fields (Design)

![Design: the document's fields on the left, real documents built from the data on the right](images/studio-design.png)

This is where you spend most of your time:
- **The left side is your design.** Every column of `stone` is ticked. Untick what you don't need, and type a new name after the arrow to rename a field.
- **The right side shows real documents,** rebuilt from the database after every change you make.

### 4. Add related data

![Add related data: tables connected to stone, each with an Add button](images/studio-suggestions.png)

Scroll down to **Add related data**. Studio lists the tables connected to stone and explains each one. Click **Add** on:
- **lab** (fields). Each stone gets `lab_name`, `lab_country` and so on.
- **inclusion** (list). Each stone gets its list of inclusions.
- **rate** (list). Each stone gets its prices.
- **order_header via order_stone** (list). Each stone gets the orders it is part of.

Each added table gets a card. There you choose its columns, rename them and, for lists, set the sort order. **Measure on live data** tells you how many items a document has. The preview on the right updates every time.

### 5. Add a calculated field

![Calculated fields: formulas, a palette to insert names, and the preview](images/studio-calculated.png)

Click **+ Add calculated field**, name it `price_per_carat` and type this formula:

```text
ROUND(SUM(rate.amount) / carat, 2)
```

- **The palette** below the formulas inserts field names, list summaries and functions where your cursor is.
- **Mistakes show up under the formula** while you type, for example "rate has no column amout (did you mean amount?)".
- **More ideas:** `COUNT(inclusion)`, `COUNTIF(inclusion.severity >= 3)` and `IF(carat >= 2, "large", "small")`. The full list is in [formulas.md](formulas.md).

Optional: on the **rate** card, tick **Use only in calculated fields**. The prices still feed `price_per_carat`, but the list of rates is left out of the documents.

### 6. Index only some stones (optional)

Under the root's columns, in **Only rows where**, type:

```text
status <> "sold"
```

Click **Count rows** to see how many stones match. A stone that is sold later leaves the index on its own, and comes back if its status changes again.

### 7. Deploy

![Deploy: what changes, and the checks](images/studio-deploy.png)

Click **Deploy…**. Studio shows what will happen and checks that the database is ready: change tracking, keys, indexes and every formula. On the demo database every check passes.

Click **Save and deploy**. Studio builds all the documents and opens the **Pipeline** tab:

![Pipeline: the running sync, its outputs, the changes per table and a document inspector](images/studio-pipeline.png)

### 8. Watch documents update

In a **second terminal**, run:

```bash
make demo-change
```

It renames a lab, raises the prices of one stone and marks another stone as sold, then tells you which ones. Within a few seconds the Pipeline tab shows:
- **Source tables:** the changes per table. A renamed lab affects every stone from that lab.
- **Generation:** the batch number, which goes up.

Type one of the stone numbers into **Inspect a document** to see its current document. A stone that was just marked sold is no longer a document, if you added the filter in step 6.

### 9. Look at the output

The documents are written to `studio/pipelines/stone/out`:

```
out/manifest.json     what to read: the latest snapshot and the batches after it
out/base/…            every document at one moment (Parquet files)
out/segments/…        batches of changes since then (Parquet files)
```

To print a few documents from the latest batch:

```bash
./bin/docsync dump "$(ls -t studio/pipelines/stone/out/segments/*.parquet | head -1)" --limit 3
```

Your search service loads the snapshot, then applies the batches in order, and keeps checking the manifest for new ones. [consumer-protocol.md](consumer-protocol.md) describes exactly what to read.

### 10. Stop, and come back later

- **Ctrl+C** in the first terminal stops Studio and the pipeline.
- **`make demo-stop`** stops SQL Server. Lima users can also run `limactl stop docker`.
- **`make demo`** brings everything back. Your designs are kept in `./studio`, and a deployed pipeline continues where it stopped.

## Part 2: use it with your own database

1. **Get a login.** Ask your DBA for a SQL Server login for docsync that can read the tables you need (`SELECT`) and their changes (`VIEW CHANGE TRACKING`). To see every table in Studio, it also needs `VIEW DEFINITION`. Step 4 produces the exact `GRANT` statements.
2. **Start Studio against your database.** Use a folder of its own, so its designs don't mix with the demo's:
   ```bash
   export DOCSYNC_DSN='sqlserver://docsync_login:PASSWORD@your-server:1433?database=YourDatabase'
   ./bin/docsync studio --dir ./studio-mydb
   ```
   The connection string stays in your shell. Studio never saves it.
3. **Design** as in part 1, starting from your root table. Suggestions come from your foreign keys if you have them, and from column names.
4. **Let the DBA prepare the database.** Open **Checks** and click **Run checks**. It lists what is missing (snapshot isolation, change tracking, indexes) and writes the SQL under **SQL for your DBA**. The DBA reviews it and runs it once.
5. **Deploy from Studio** and watch the Pipeline tab, as in part 1.
6. **Run it on the server without Studio.** A deployed pipeline is a self-contained folder: `studio-mydb/pipelines/<name>/` with `docsync.yaml`. Copy `docsync.yaml` to the server and run:
   ```bash
   export DOCSYNC_DSN='…'
   docsync run --config docsync.yaml
   ```
   - It keeps its state in `state/` and writes documents to `out/`, next to `docsync.yaml`.
   - Its dashboard is at http://127.0.0.1:8088.
   - Install it as a service so it starts with the machine and restarts after failures. On Windows, `docsync service install` does this, and connects with Windows authentication so no password is stored. On Linux, use a systemd unit. [windows-service.md](windows-service.md) covers both.
7. **Point your search service at `out/`,** following [consumer-protocol.md](consumer-protocol.md).

## Words you may run into

| Word | Meaning |
|---|---|
| Change tracking | A SQL Server feature that records which rows changed. docsync reads it every second. |
| Lookup | Related data with one matching row, such as the lab of a stone. |
| Bridge table | A table that links two others, like `order_stone` between orders and stones. |
| Calculated field | A field computed with a formula. See [formulas.md](formulas.md). |
| Generation | The number of a batch of changes. It only goes up. |
| Base snapshot | A full copy of every document, written now and then, so readers don't have to replay every batch. |
| Epoch | Starts when the document definition changes, for example new fields or formulas. docsync then rebuilds every document, and Studio's deploy dialog warns you first. |

## Where to go next

- [studio.md](studio.md): everything Studio can do.
- [formulas.md](formulas.md): the formula language.
- [config-reference.md](config-reference.md): the config file Studio writes, if you want to edit it by hand.
- [consumer-protocol.md](consumer-protocol.md): how to read the output.
- [architecture.md](architecture.md): how docsync works inside.
