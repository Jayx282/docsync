# Formulas

Studio uses formulas in two places:
- **Calculated fields** add fields to every document, such as a price per carat or a count of severe inclusions.
- **The root filter** ("Only rows where") decides which root rows become documents.

A formula reads like a spreadsheet formula and compiles to T-SQL, which you can see in Studio's **SQL** tab. Only the functions listed here exist, so a formula can't run arbitrary SQL.

```text
ROUND(SUM(rates.amount) / carat, 2)
COUNTIF(inclusions.severity >= 3)
IF(carat >= 2, "large", "small")
UPPER(lab_name) & " (" & COALESCE(lab.country, "?") & ")"
DATEDIFF("day", certified_on, updated_at)
```

## Names

| You write | It means |
|---|---|
| `carat` | A field of the document: a root column you selected, or a lookup field such as `lab_name`. Any root column works, even one you didn't select. |
| `lab.country` | A column of a related table, using the relation's name (shown on its card). Any column works, selected or not. |
| `stone.carat` | A root column, written with the root table's name. |
| `price_per_carat` | Another calculated field. Fields can use each other; loops are reported. |
| `[unit price]` | A name with unusual characters, in brackets. |

Columns of a **list** (many rows per document) can only appear inside a summary such as `MAX(inclusions.severity)`.

## Values and operators

- **Values:**
  - Numbers: `2`, `0.5`.
  - Text in double or single quotes: `"sold"`, `'it''s'`.
  - `TRUE`, `FALSE` and `NULL`, which means empty.
  - Dates written as text: `"2024-12-31"`, `"2024-12-31T18:00:00"`.
- **Arithmetic:** `+ - * / %`.
  - `/` always divides as real numbers, so `5 / 2` is `2.5`.
  - Dividing by zero gives an empty value, not an error.
  - `%` needs whole numbers.
- **Joining text:** `&`. An empty value joins as empty text.
- **Comparisons:** `= <> != < <= > >=`, `IN ("a", "b")`, `NOT IN (…)`, `IS NULL`, `IS NOT NULL`. Writing `x = NULL` is read as `x IS NULL`.
- **Logic:** `AND`, `OR`, `NOT`, and parentheses.
- **Precedence**, from loosest to tightest: `OR`, `AND`, `NOT`, comparisons, `&`, `+ -`, `* / %`, then unary `-`.

A condition used as a field value, like `carat >= 2`, becomes `true` or `false`, or empty when a value it uses is empty.

## Summaries of lists

| Function | Result |
|---|---|
| `COUNT(rates)` | Number of rows in the list |
| `COUNT(rates.amount)` | Number of rows where the value isn't empty |
| `COUNTIF(inclusions.severity >= 3)` | Number of rows that meet the condition |
| `SUM(rates.amount)`, `AVG(…)`, `MIN(…)`, `MAX(…)` | Total, average, smallest and largest |

Rules for summaries:
- **Any list works,** including one reached through a bridge table, for example `COUNT(orders)`.
- **Use only in calculated fields:** turn this on in a relation's card to summarise its rows without putting them in documents.
- **One list per summary.** A summary can also use root and lookup fields, for example `SUM(rates.amount / carat)`.
- **No nesting:** a summary can't contain another summary.
- **Empty lists:** `COUNT` gives `0`; `SUM`, `AVG`, `MIN` and `MAX` give an empty value. Write `COALESCE(SUM(rates.amount), 0)` to get `0` instead.
- **Exact arithmetic:** `SUM` and `AVG` use exact decimals (six decimal places for averages and for sums of non-decimal values). A document then comes out identical whatever order the rows are read in, which keeps incremental output equal to a full rebuild.

## Functions

| Function | Result |
|---|---|
| `IF(condition, then, else)` | `then` when the condition holds, otherwise `else` (empty if omitted). Both must be the same kind of value. |
| `COALESCE(a, b, …)` | The first value that isn't empty |
| `NULLIF(a, b)` | Empty when `a` equals `b`, otherwise `a` |
| `ISBLANK(x)` | True when `x` is empty; for text, also when it is blank |
| `ROUND(x, digits)`, `FLOOR(x)`, `CEILING(x)`, `ABS(x)`, `SQRT(x)` | Number functions. `SQRT` of a negative number is empty. |
| `UPPER(t)`, `LOWER(t)`, `TRIM(t)`, `LEN(t)` | Text functions |
| `LEFT(t, n)`, `RIGHT(t, n)`, `MID(t, start, n)` | Parts of a text. `n` and `start` must be numbers written in the formula. |
| `REPLACE(t, find, with)`, `CONCAT(a, b, …)` | Replace part of a text; join values |
| `CONTAINS(t, part)`, `STARTSWITH(t, p)`, `ENDSWITH(t, p)` | Conditions on text |
| `YEAR(d)`, `MONTH(d)`, `DAY(d)` | Parts of a date |
| `DATEDIFF("day", start, end)` | Whole days between two dates. Also takes `"year"`, `"quarter"`, `"month"`, `"week"`, `"hour"`, `"minute"` or `"second"`. |
| `DATEADD("month", n, d)` | `d` plus `n` units. `n` must be written in the formula. |
| `NUMBER(t)`, `INTEGER(x)`, `TEXT(x)` | Conversions. A text that isn't a number gives an empty value. Dates convert to ISO text. |

## What is not allowed, and why

- **The current date or time:** `NOW()`, `TODAY()`, `GETDATE()` and similar.
  - A document is rebuilt only when its data changes, so a field like "days since certified" would quietly go stale.
  - Compare dates stored in the data instead.
  - To use a moving reference date, keep it in a table that docsync tracks.
- **Mixing kinds of values,** such as comparing text with a number or adding to a date. SQL Server would fail on those rows at runtime. Studio catches them while you type.
- **Root filters that read other tables.** A change in another table wouldn't re-check which rows are documents. A filter can use only the root table's columns.

## Checks before you deploy

**Checks** (and the deploy dialog) computes every calculated field for every document once. Data that would make a formula fail at runtime, such as a value too large for its type, shows up there rather than in the running pipeline.

## How formulas run

- **One query.** All calculated fields compile into one query named `calculated`, which runs alongside the other parts of the document.
- **Lookups** are joined with `LEFT JOIN`, so a missing lookup gives empty values.
- **Summaries** become correlated subqueries, one per summary. Keep the join columns indexed (the **Checks** script suggests the indexes).
- **The root filter** is added to docsync's existence check. A row that stops matching is deleted from the output, and one that starts matching is added.
- **Changing a formula or the filter** changes the document definition. The deploy dialog says when a deployment will rebuild every document.
