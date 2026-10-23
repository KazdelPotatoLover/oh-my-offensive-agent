export type ExploreKind = 'begin' | 'goal' | 'intent' | 'fact' | 'finding' | 'hint' | 'digest';

export interface GraphFinding {
  id: string;
  title: string;
  status: string;
  source_kind: string;
  verification_state: string;
  retest_verdict?: string | null;
}

export interface GraphNode {
  id: string;
  kind: ExploreKind;
  state: string;
  payload: Record<string, unknown>;
  priority?: number;
  origin?: string | null;
  created_at?: string;
  work_item_id?: string | null;
  finding_id?: string | null;
  work_status?: string | null;
  finding?: GraphFinding | null;
  delete_reason?: string | null;
  blocked_reason?: string | null;
}

export interface CanvasNodeData {
  id: string;
  kind: ExploreKind;
  state: string;
  node_state: string;
  priority?: number;
  origin?: string;
  summary: string;
  payload: Record<string, unknown>;
  created_at?: string;
  work_item_id?: string;
  finding_id?: string;
  finding?: GraphFinding;
  is_live: boolean;
  label?: string;
}

export interface CanvasEdgeData {
  id: string;
  src: string;
  dst: string;
  rel: string;
  reason?: string | null;
}

export interface ExplorationGraph {
  nodes: GraphNode[];
  edges: CanvasEdgeData[];
}

const kinds = new Set(['begin', 'goal', 'intent', 'fact', 'finding', 'hint', 'digest']);
const record = (v: unknown): v is Record<string, unknown> => v !== null && typeof v === 'object' && !Array.isArray(v);

export function parseExplorationGraph(value: unknown): ExplorationGraph {
  if (!record(value) || !Array.isArray(value.nodes) || !Array.isArray(value.edges)) throw new Error('探索图响应缺少 nodes/edges 数组');
  for (const n of value.nodes) {
    if (!record(n) || typeof n.id !== 'string' || !kinds.has(String(n.kind)) || typeof n.state !== 'string' || !record(n.payload)) {
      throw new Error('探索图节点格式错误');
    }
    if (n.finding != null && (!record(n.finding) || ['id', 'title', 'status', 'source_kind', 'verification_state'].some(k => typeof (n.finding as Record<string, unknown>)[k] !== 'string'))) {
      throw new Error('探索图发现格式错误');
    }
  }
  const ids = new Set(value.nodes.map(n => n.id));
  if (ids.size !== value.nodes.length) throw new Error('探索图节点 ID 重复');
  for (const e of value.edges) {
    if (!record(e) || ['id', 'src', 'dst', 'rel'].some(k => typeof e[k] !== 'string') || !ids.has(e.src) || !ids.has(e.dst)) {
      throw new Error('探索图关系格式错误或端点缺失');
    }
  }
  return value as unknown as ExplorationGraph;
}

function summary(n: GraphNode): string {
  if (n.kind === 'finding' && n.finding?.title.trim()) return n.finding.title;
  const fields = n.kind === 'goal' ? ['text', 'summary'] : n.kind === 'finding' ? ['title', 'name', 'summary'] : ['summary', 'text', 'body', 'description'];
  for (const field of fields) {
    const value = n.payload[field];
    if (typeof value === 'string' && value.trim()) return value;
  }
  return '暂无摘要';
}

// No inferred ancestry or proof: even isolated nodes are meaningful data.
export function buildExplorationGraph(graph: ExplorationGraph | null): { graphNodes: CanvasNodeData[]; graphEdges: CanvasEdgeData[] } {
  return {
    graphNodes: (graph?.nodes ?? []).map(n => {
      const state = n.delete_reason ? 'deleted' : n.kind === 'intent' ? n.work_status || n.state : n.state;
      return {
        id: n.id, kind: n.kind, state, node_state: n.state,
        priority: n.priority, origin: n.origin ?? undefined, summary: summary(n), payload: n.payload,
        created_at: n.created_at, work_item_id: n.work_item_id ?? undefined,
        finding_id: n.finding_id ?? undefined, finding: n.finding ?? undefined,
        is_live: n.kind === 'intent' && state === 'running',
        label: n.kind === 'fact' && n.state === 'origin' ? '任务起点' : undefined,
      };
    }),
    graphEdges: graph?.edges ?? [],
  };
}
