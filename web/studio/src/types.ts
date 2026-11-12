// Mirrors the JSON of internal/studio (Go). Keep in sync with design.go, schema.go,
// suggest.go, service.go and runtime.go.

export type Shape = "fields" | "list";

export interface Field {
  column: string;
  as?: string;
}

export interface JoinPair {
  from: string; // parent column (the root, or the bridge table)
  to: string; // related table column
}

export interface Via {
  table: string;
  join: JoinPair[]; // root column (from) = bridge column (to)
}

export interface Relation {
  id: string;
  table: string;
  shape: Shape;
  join: JoinPair[];
  via?: Via;
  fields: Field[];
  path?: string;
  sort_by?: string[];
  hidden?: boolean; // used only by calculated fields, not put into documents
}

export interface Calc {
  name: string;
  expr: string;
}

export interface Design {
  version: number;
  name: string;
  root: { table: string; key: string; fields: Field[]; filter?: string };
  related: Relation[];
  calculated?: Calc[];
  output: { format?: string; include_json: boolean };
  updated_at?: string;
}

export interface Column {
  name: string;
  type: string;
  nullable: boolean;
  identity?: boolean;
  computed?: boolean;
}

export interface Index {
  name: string;
  columns: string[];
  unique: boolean;
  primary: boolean;
}

export interface ForeignKey {
  name: string;
  from_table: string;
  from_columns: string[];
  to_table: string;
  to_columns: string[];
}

export interface Table {
  name: string;
  rows: number;
  change_tracking: boolean;
  columns: Column[];
  primary_key: string[];
  indexes: Index[];
}

export interface TableRow {
  name: string;
  rows: number;
  change_tracking: boolean;
  columns: number;
  primary_key: string[];
}

export interface Suggestion {
  id: string;
  table: string;
  shape: Shape;
  join: JoinPair[];
  via?: Via;
  path?: string;
  source: string;
  guessed: boolean;
  unique: boolean;
  rows: number;
  reason: string;
}

export interface TableDetail {
  table: Table;
  references: ForeignKey[];
  referenced_by: ForeignKey[];
  related: Suggestion[];
}

export interface Profile {
  related_rows: number;
  max_per_document: number;
  avg_per_document: number;
  documents_with_any: number;
  fanout_max: number;
  fanout_avg: number;
  took_ms: number;
  explanation: string;
}

export interface Connection {
  ok: boolean;
  error?: string;
  server?: string;
  database?: string;
  edition?: string;
  version?: string;
  snapshot_isolation?: boolean;
  change_tracking?: boolean;
  tables?: number;
  loaded_at?: string;
}

export interface SchemaType {
  kind: string;
  precision?: number;
  scale?: number;
  fields?: SchemaField[];
  elem?: SchemaType;
  sql_type?: string;
}

export interface SchemaField {
  name: string;
  type: SchemaType;
}

export interface CompiledFragment {
  name: string;
  cardinality: string;
  path?: string;
  sql: string;
}

export interface PreviewDoc {
  root: string;
  json?: string;
  error?: string;
  bytes: number;
}

export interface PreviewResult {
  documents: PreviewDoc[];
  fields: SchemaField[];
  sql: CompiledFragment[];
  yaml: string;
  errors: string[];
  took_ms: number;
}

export interface Finding {
  severity: "ok" | "warn" | "FAIL";
  check: string;
  detail: string;
}

export interface ValidateResult {
  ok: boolean;
  errors: string[];
  findings: Finding[];
  script: string;
}

export type RuntimeState = "stopped" | "starting" | "running" | "error";

export interface RuntimeStatus {
  state: RuntimeState;
  design?: string;
  error?: string;
  started_at?: string;
  config_path?: string;
}

export interface FilterCount {
  errors: string[];
  total: number;
  matching: number;
  took_ms: number;
}

export interface DeployPlan {
  errors: string[];
  deployed: boolean;
  deployed_at?: string;
  current: string;
  next: string;
  same: boolean;
  rebuild: boolean;
  running: RuntimeStatus;
}
