import { useMemo } from "react";
import { Background, Controls, Handle, MarkerType, Position, ReactFlow } from "@xyflow/react";
import type { Edge, Node, NodeProps } from "@xyflow/react";
import dagre from "@dagrejs/dagre";
import { go, useTables } from "../hooks";
import type { TableDetail } from "../types";
import { eqi, fmtCompact, lc, schemaOf, shortName } from "../util";

type TableNodeData = { name: string; rows?: number; center: boolean };
type TableNode = Node<TableNodeData, "table">;

const W = 190;
const H = 48;
const MAX_NODES = 40;

function TableNodeView({ data }: NodeProps<TableNode>) {
  return (
    <div className={"tnode" + (data.center ? " center" : "")} title={data.name}>
      <Handle type="target" position={Position.Left} isConnectable={false} />
      <div className="tnode-name">{shortName(data.name)}</div>
      <div className="tnode-meta">
        {schemaOf(data.name)}
        {data.rows !== undefined ? ` · ${fmtCompact(data.rows)} rows` : ""}
      </div>
      <Handle type="source" position={Position.Right} isConnectable={false} />
    </div>
  );
}

const nodeTypes = { table: TableNodeView };

/** Label for join columns: "stoneid" when both sides match, else "lab_code → code". */
function joinLabel(from: string[], to: string[]): string {
  return from.map((c, i) => (eqi(c, to[i] ?? "") ? c : `${c} → ${to[i]}`)).join(", ");
}

function build(detail: TableDetail, rowsOf: (name: string) => number | undefined) {
  const center = detail.table.name;
  const nodes = new Map<string, TableNode>();
  const edges = new Map<string, Edge>();
  const node = (name: string) => {
    const k = lc(name);
    if (!nodes.has(k)) {
      nodes.set(k, {
        id: k,
        type: "table",
        position: { x: 0, y: 0 },
        data: { name, rows: rowsOf(name), center: eqi(name, center) },
      });
    }
    return k;
  };
  // Edges point from the table holding the reference to the table it references.
  const edge = (from: string, to: string, label: string, guessed: boolean) => {
    if (eqi(from, to)) return;
    const s = node(from);
    const t = node(to);
    const id = `${s}->${t}`;
    const prev = edges.get(id);
    if (prev && !(prev.data as { guessed: boolean }).guessed) return; // declared keys win
    edges.set(id, {
      id,
      source: s,
      target: t,
      label,
      data: { guessed },
      className: guessed ? "guessed" : undefined,
      markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14 },
      labelBgPadding: [4, 2],
      labelBgBorderRadius: 3,
    });
  };

  node(center);
  for (const fk of detail.references) edge(fk.from_table, fk.to_table, joinLabel(fk.from_columns, fk.to_columns), false);
  for (const fk of detail.referenced_by) edge(fk.from_table, fk.to_table, joinLabel(fk.from_columns, fk.to_columns), false);
  const rootKey = detail.table.primary_key.length === 1 ? detail.table.primary_key[0] : "";
  for (const s of detail.related) {
    const from = s.join.map((j) => j.from);
    const to = s.join.map((j) => j.to);
    if (s.via) {
      edge(s.via.table, center, joinLabel(s.via.join.map((j) => j.to), s.via.join.map((j) => j.from)), s.guessed);
      edge(s.via.table, s.table, joinLabel(from, to), s.guessed);
    } else if (rootKey && from.length === 1 && eqi(from[0], rootKey)) {
      edge(s.table, center, joinLabel(to, from), s.guessed); // child rows hold the key
    } else {
      edge(center, s.table, joinLabel(from, to), s.guessed); // lookup
    }
  }

  let list = [...nodes.values()];
  let hidden = 0;
  if (list.length > MAX_NODES) {
    hidden = list.length - MAX_NODES;
    list = list.slice(0, MAX_NODES);
  }
  const keep = new Set(list.map((n) => n.id));
  const edgeList = [...edges.values()].filter((e) => keep.has(e.source) && keep.has(e.target));

  const g = new dagre.graphlib.Graph();
  g.setGraph({ rankdir: "LR", nodesep: 18, ranksep: 50, edgesep: 12, marginx: 10, marginy: 10 });
  g.setDefaultEdgeLabel(() => ({}));
  list.forEach((n) => g.setNode(n.id, { width: W, height: H }));
  // Reserve room for join labels so they do not sit on top of tables.
  edgeList.forEach((e) => g.setEdge(e.source, e.target, { width: String(e.label ?? "").length * 6.6 + 16, height: 16, labelpos: "c" }));
  dagre.layout(g);
  const placed = list.map((n) => {
    const p = g.node(n.id) as { x: number; y: number };
    return { ...n, position: { x: p.x - W / 2, y: p.y - H / 2 } };
  });
  return { nodes: placed, edges: edgeList, hidden };
}

/** Neighbourhood of one table: declared foreign keys plus relations guessed from names. */
export default function TableGraph({ detail }: { detail: TableDetail }) {
  const tables = useTables();
  const { nodes, edges, hidden } = useMemo(() => {
    const rows = new Map((tables.data ?? []).map((t) => [lc(t.name), t.rows]));
    return build(detail, (n) => rows.get(lc(n)));
  }, [detail, tables.data]);

  if (nodes.length <= 1) {
    return (
      <div className="graph-empty muted">
        No related tables found: no foreign keys point to or from this table, and no other table has a column named like
        its key.
      </div>
    );
  }
  const height = Math.min(560, Math.max(280, Math.ceil(nodes.length / 3) * 72 + 110));
  return (
    <div className="graph">
      <div className="graph-canvas" style={{ height }}>
      <ReactFlow
        // Uncontrolled (draggable) nodes: remount when the inputs change.
        key={`${detail.table.name}:${nodes.length}:${tables.data ? "rows" : ""}`}
        defaultNodes={nodes}
        defaultEdges={edges}
        nodeTypes={nodeTypes}
        fitView
        fitViewOptions={{ padding: 0.12, maxZoom: 1.1 }}
        colorMode="system"
        nodesConnectable={false}
        edgesFocusable={false}
        minZoom={0.2}
        onNodeClick={(_, n) => {
          const name = (n.data as TableNodeData).name;
          if (!eqi(name, detail.table.name)) go(`explore/${name}`);
        }}
      >
        <Background gap={22} size={1} />
        <Controls showInteractive={false} position="top-right" />
      </ReactFlow>
      </div>
      <div className="graph-legend">
        <span>
          <i className="line solid" /> foreign key
        </span>
        <span>
          <i className="line dashed" /> guessed from column names
        </span>
        <span>arrows point at the referenced table · click a table to open it</span>
        {hidden > 0 && <span>{hidden} more tables not shown</span>}
      </div>
    </div>
  );
}
