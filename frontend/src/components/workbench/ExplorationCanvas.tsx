'use client';

import React, { useMemo, useState, useEffect, useRef } from 'react';
import {
  ReactFlow,
  Background,
  BackgroundVariant,
  Controls,
  MiniMap,
  Panel,
  Position,
  Handle,
  MarkerType,
  useNodesState,
  useEdgesState,
  type Node as RFNode,
  type Edge as RFEdge,
  type NodeProps,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import {
  Bug,
  Compass,
  Flag,
  FlaskConical,
  Layers,
  Lightbulb,
  Target,
  X,
  Copy,
  Check,
  type LucideIcon,
} from 'lucide-react';
import { cn } from '@/lib/utils';

export type ExploreKind = 'begin' | 'goal' | 'intent' | 'fact' | 'finding' | 'hint' | 'digest';

export interface CanvasNodeData {
  id: string;
  kind: ExploreKind;
  state: string;
  priority?: number;
  origin?: string;
  summary: string;
  payload?: Record<string, unknown> | string;
  created_at?: string;
  work_item_id?: string;
  finding_id?: string;
  is_live?: boolean;
}

export interface CanvasEdgeData {
  id: string;
  src: string;
  dst: string;
  rel: string;
  animated?: boolean;
}

type TypeMeta = {
  label: string;
  icon: LucideIcon;
  iconBg: string;
  chip: string;
  hex: string;
};

export const typeMeta: Record<ExploreKind, TypeMeta> = {
  begin: { label: '起点', icon: Flag, iconBg: 'bg-slate-600', chip: 'bg-slate-500/20 text-slate-300', hex: '#64748b' },
  goal: { label: '目标', icon: Target, iconBg: 'bg-emerald-600', chip: 'bg-emerald-500/20 text-emerald-300', hex: '#10b981' },
  intent: { label: '意图', icon: Compass, iconBg: 'bg-blue-600', chip: 'bg-blue-500/20 text-blue-300', hex: '#3b82f6' },
  fact: { label: '事实', icon: FlaskConical, iconBg: 'bg-amber-600', chip: 'bg-amber-500/20 text-amber-300', hex: '#f59e0b' },
  finding: { label: '漏洞', icon: Bug, iconBg: 'bg-rose-600', chip: 'bg-rose-500/20 text-rose-300', hex: '#f43f5e' },
  hint: { label: '提示', icon: Lightbulb, iconBg: 'bg-purple-600', chip: 'bg-purple-500/20 text-purple-300', hex: '#8b5cf6' },
  digest: { label: '压缩', icon: Layers, iconBg: 'bg-teal-600', chip: 'bg-teal-500/20 text-teal-300', hex: '#14b8a6' },
};

export const relLabel: Record<string, string> = {
  spawns: '派生',
  derived_from: '意图链',
  yields: '产出',
  proves: '证明',
};

export const relColor: Record<string, string> = {
  spawns: '#10b981',
  derived_from: '#818cf8',
  yields: '#fbbf24',
  proves: '#f43f5e',
};

const COL_W = 340;
const ROW_H = 150;

type ExploreRFNode = RFNode<{ node: CanvasNodeData }, 'explore'>;

function ExploreCustomNode({ data, selected }: NodeProps<ExploreRFNode>) {
  const n = data.node;
  const meta = typeMeta[n.kind] || typeMeta.intent;
  const Icon = meta.icon;
  const isRunning = n.is_live || n.state === 'running';

  return (
    <div
      className={cn(
        'w-[250px] cursor-pointer rounded-xl border transition-all duration-150',
        selected
          ? 'border-blue-400 ring-2 ring-blue-500/40 shadow-lg shadow-blue-500/10'
          : isRunning
          ? 'border-blue-500/60 shadow-md shadow-blue-500/10'
          : 'border-gray-800 hover:border-gray-600 hover:shadow-md'
      )}
    >
      <div className="relative rounded-[11px] bg-gray-900/95 p-3 backdrop-blur-sm">
        <Handle
          type="target"
          position={Position.Left}
          className="!size-2 !border-2 !border-gray-600 !bg-gray-900 opacity-60 transition-opacity hover:opacity-100"
        />

        {/* Header */}
        <div className="flex items-center gap-2">
          <span className={cn('flex size-6 shrink-0 items-center justify-center rounded-lg shadow-sm', meta.iconBg)}>
            <Icon className="size-3.5 text-white" />
          </span>
          <span className="grow truncate text-xs font-semibold text-gray-200">
            {meta.label}
          </span>
          {isRunning && (
            <span className="relative flex size-2 shrink-0">
              <span className="absolute inline-flex size-full animate-ping rounded-full bg-blue-400 opacity-75" />
              <span className="relative inline-flex size-2 rounded-full bg-blue-500" />
            </span>
          )}
          {n.priority !== undefined && n.priority > 0 && (
            <span
              className={cn(
                'shrink-0 rounded px-1.5 py-0.5 font-mono text-[10px] font-semibold',
                n.priority >= 8
                  ? 'bg-rose-500/20 text-rose-300 border border-rose-500/30'
                  : n.priority >= 5
                  ? 'bg-amber-500/20 text-amber-300 border border-amber-500/30'
                  : 'bg-gray-800 text-gray-400'
              )}
            >
              P{n.priority}
            </span>
          )}
        </div>

        {/* Summary body */}
        <div className="mt-2 line-clamp-3 text-xs leading-relaxed text-gray-300">
          {n.summary || meta.label}
        </div>

        {/* Footer */}
        <div className="mt-3 flex items-center justify-between border-t border-gray-800/80 pt-2 text-[10px]">
          <span
            className={cn(
              'rounded px-1.5 py-0.5 font-medium',
              n.state === 'met' || n.state === 'verified' || n.state === 'done'
                ? 'bg-emerald-500/15 text-emerald-400'
                : n.state === 'running'
                ? 'bg-blue-500/15 text-blue-400'
                : n.state === 'blocked' || n.state === 'failed'
                ? 'bg-rose-500/15 text-rose-400'
                : 'bg-gray-800 text-gray-400'
            )}
          >
            {n.state}
          </span>
          <span className="font-mono text-gray-500">#{n.id.slice(0, 8)}</span>
        </div>

        <Handle
          type="source"
          position={Position.Right}
          className="!size-2 !border-2 !border-gray-600 !bg-gray-900 opacity-60 transition-opacity hover:opacity-100"
        />
      </div>
    </div>
  );
}

const nodeTypes = { explore: ExploreCustomNode };

// Cycle-safe auto-layering algorithm (similar to ARTEX computeLayout)
function computeLayout(
  nodes: CanvasNodeData[],
  edges: CanvasEdgeData[]
): Map<string, { x: number; y: number }> {
  const order = nodes.map((n) => n.id);
  const idSet = new Set(order);
  const validEdges = edges.filter((e) => idSet.has(e.src) && idSet.has(e.dst));

  const outMap = new Map<string, string[]>();
  for (const e of validEdges) {
    const list = outMap.get(e.src) || [];
    list.push(e.dst);
    outMap.set(e.src, list);
  }

  // Break cycles with DFS
  const seen = new Map<string, number>();
  const backEdges = new Set<string>();
  for (const root of order) {
    if (seen.get(root)) continue;
    const stack: Array<{ u: string; i: number }> = [{ u: root, i: 0 }];
    seen.set(root, 1);
    while (stack.length > 0) {
      const top = stack[stack.length - 1];
      const neighbors = outMap.get(top.u) || [];
      if (top.i >= neighbors.length) {
        seen.set(top.u, 2);
        stack.pop();
        continue;
      }
      const v = neighbors[top.i++];
      const st = seen.get(v) || 0;
      if (st === 1) {
        backEdges.add(`${top.u}->${v}`);
      } else if (st === 0) {
        seen.set(v, 1);
        stack.push({ u: v, i: 0 });
      }
    }
  }

  // DAG topological layering
  const dagSucc = new Map<string, string[]>();
  const inDegree = new Map<string, number>(order.map((id) => [id, 0]));
  for (const e of validEdges) {
    if (backEdges.has(`${e.src}->${e.dst}`)) continue;
    const l = dagSucc.get(e.src) || [];
    l.push(e.dst);
    dagSucc.set(e.src, l);
    inDegree.set(e.dst, (inDegree.get(e.dst) || 0) + 1);
  }

  const layer = new Map<string, number>(order.map((id) => [id, 0]));
  const queue = order.filter((id) => (inDegree.get(id) || 0) === 0);
  for (let qi = 0; qi < queue.length; qi++) {
    const u = queue[qi];
    const nbrs = dagSucc.get(u) || [];
    for (const v of nbrs) {
      layer.set(v, Math.max(layer.get(v) || 0, (layer.get(u) || 0) + 1));
      const d = (inDegree.get(v) || 0) - 1;
      inDegree.set(v, d);
      if (d === 0) queue.push(v);
    }
  }

  const maxLayer = Math.max(0, ...Array.from(layer.values()));
  const columns: string[][] = Array.from({ length: maxLayer + 1 }, () => []);
  for (const id of order) {
    columns[layer.get(id) || 0].push(id);
  }

  // Barycenter sweeps to reduce edge crossings
  const colIndex = new Map<string, number>();
  columns.forEach((ids, col) => ids.forEach((id) => colIndex.set(id, col)));

  const adj = new Map<string, string[]>();
  for (const e of validEdges) {
    const cs = colIndex.get(e.src);
    const cd = colIndex.get(e.dst);
    if (cs !== undefined && cd !== undefined && Math.abs(cs - cd) === 1) {
      const sList = adj.get(e.src) || [];
      sList.push(e.dst);
      adj.set(e.src, sList);
      const dList = adj.get(e.dst) || [];
      dList.push(e.src);
      adj.set(e.dst, dList);
    }
  }

  for (let iter = 0; iter < 4; iter++) {
    for (let c = 1; c < columns.length; c++) {
      const refMap = new Map(columns[c - 1].map((id, i) => [id, i]));
      const scored = columns[c].map((id, i) => {
        const nbrs = (adj.get(id) || [])
          .map((nid) => refMap.get(nid))
          .filter((v): v is number => v !== undefined);
        const bary = nbrs.length ? nbrs.reduce((a, b) => a + b, 0) / nbrs.length : i;
        return { id, bary, i };
      });
      scored.sort((a, b) => a.bary - b.bary || a.i - b.i);
      columns[c] = scored.map((s) => s.id);
    }
  }

  const maxCount = Math.max(1, ...columns.map((c) => c.length));
  const midline = ((maxCount - 1) * ROW_H) / 2;
  const positions = new Map<string, { x: number; y: number }>();
  columns.forEach((ids, c) => {
    const startY = midline - ((ids.length - 1) * ROW_H) / 2;
    ids.forEach((id, i) => {
      positions.set(id, { x: c * COL_W + 50, y: startY + i * ROW_H + 50 });
    });
  });

  return positions;
}

// Slide-in Node Detail Drawer
function NodeDetailDrawer({
  node,
  onClose,
}: {
  node: CanvasNodeData | null;
  onClose: () => void;
}) {
  const [copied, setCopied] = useState(false);
  if (!node) return null;

  const meta = typeMeta[node.kind] || typeMeta.intent;
  const Icon = meta.icon;

  const rawJson = typeof node.payload === 'object'
    ? JSON.stringify(node.payload, null, 2)
    : typeof node.payload === 'string'
    ? node.payload
    : JSON.stringify(node, null, 2);

  const handleCopy = () => {
    void navigator.clipboard?.writeText(rawJson).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  };

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label="节点详情"
      className="fixed inset-y-0 right-0 z-50 flex w-full max-w-md flex-col border-l border-gray-800 bg-gray-950/95 text-gray-100 shadow-2xl backdrop-blur-md transition-transform"
    >
      {/* Header */}
      <div className="flex items-center justify-between border-b border-gray-800 px-4 py-3">
        <div className="flex items-center gap-2.5">
          <span className={cn('flex size-7 items-center justify-center rounded-lg shadow-sm', meta.iconBg)}>
            <Icon className="size-4 text-white" />
          </span>
          <div>
            <h3 className="text-sm font-semibold text-gray-100">{meta.label} 详情</h3>
            <p className="font-mono text-xs text-gray-500">#{node.id}</p>
          </div>
        </div>
        <button
          type="button"
          onClick={onClose}
          className="rounded p-1 text-gray-400 hover:bg-gray-800 hover:text-gray-100"
        >
          <X className="size-4" />
        </button>
      </div>

      {/* Content */}
      <div className="flex-1 space-y-4 overflow-y-auto p-4 text-xs">
        {/* Basic Props */}
        <section className="space-y-2 rounded-lg border border-gray-800 bg-gray-900/50 p-3">
          <div className="flex justify-between">
            <span className="text-gray-400">状态</span>
            <span className="font-semibold text-gray-200">{node.state}</span>
          </div>
          {node.priority !== undefined && (
            <div className="flex justify-between">
              <span className="text-gray-400">优先级</span>
              <span className="font-mono text-gray-200">P{node.priority}</span>
            </div>
          )}
          {node.origin && (
            <div className="flex justify-between">
              <span className="text-gray-400">来源</span>
              <span className="text-gray-200">{node.origin}</span>
            </div>
          )}
          {node.created_at && (
            <div className="flex justify-between">
              <span className="text-gray-400">时间</span>
              <span className="text-gray-400">{new Date(node.created_at).toLocaleString('zh-CN')}</span>
            </div>
          )}
        </section>

        {/* Summary */}
        <section className="space-y-1">
          <h4 className="font-medium text-gray-400">摘要内容</h4>
          <p className="rounded-lg border border-gray-800 bg-gray-900/70 p-3 leading-relaxed text-gray-200 whitespace-pre-wrap">
            {node.summary || '无文本摘要'}
          </p>
        </section>

        {/* Raw payload */}
        <section className="space-y-1">
          <div className="flex items-center justify-between">
            <h4 className="font-medium text-gray-400">原始数据 (Payload)</h4>
            <button
              type="button"
              onClick={handleCopy}
              className="flex items-center gap-1 text-[11px] text-blue-400 hover:text-blue-300"
            >
              {copied ? <Check className="size-3 text-emerald-400" /> : <Copy className="size-3" />}
              {copied ? '已复制' : '复制 JSON'}
            </button>
          </div>
          <pre className="max-h-64 overflow-auto rounded-lg border border-gray-800 bg-gray-900/90 p-3 font-mono text-[11px] text-gray-300 whitespace-pre-wrap break-all">
            {rawJson}
          </pre>
        </section>
      </div>
    </div>
  );
}

export function ExplorationCanvas({
  nodes,
  edges,
  className,
  emptyHint = '当前 Run 暂无探索图节点',
}: {
  nodes: CanvasNodeData[];
  edges: CanvasEdgeData[];
  className?: string;
  emptyHint?: string;
}) {
  const [rfNodes, setRfNodes, onNodesChange] = useNodesState<ExploreRFNode>([]);
  const [rfEdges, setRfEdges, onEdgesChange] = useEdgesState<RFEdge>([]);
  const [selectedNode, setSelectedNode] = useState<CanvasNodeData | null>(null);

  const prevNodesRef = useRef<Map<string, { x: number; y: number }>>(new Map());

  useEffect(() => {
    const layout = computeLayout(nodes, edges);

    // Keep user dragged positions or apply computed layout
    const newRfNodes: ExploreRFNode[] = nodes.map((n) => {
      const prevPos = prevNodesRef.current.get(n.id);
      const pos = prevPos || layout.get(n.id) || { x: 50, y: 50 };
      return {
        id: n.id,
        type: 'explore' as const,
        position: pos,
        data: { node: n },
        sourcePosition: Position.Right,
        targetPosition: Position.Left,
      };
    });

    const liveDst = new Set(
      nodes.filter((n) => n.is_live || n.state === 'running').map((n) => n.id)
    );

    const newRfEdges: RFEdge[] = edges.map((e, idx) => {
      const color = relColor[e.rel] || '#94a3b8';
      const isAnimated = e.animated || liveDst.has(e.dst);
      return {
        id: e.id || `e-${idx}-${e.src}-${e.dst}`,
        source: e.src,
        target: e.dst,
        label: relLabel[e.rel] || e.rel,
        type: 'default',
        animated: isAnimated,
        labelShowBg: false,
        labelStyle: { fontSize: 10, fontWeight: 600, fill: color },
        style: { stroke: color, strokeWidth: 2, strokeOpacity: isAnimated ? 0.95 : 0.6 },
        markerEnd: { type: MarkerType.ArrowClosed, color, width: 14, height: 14 },
      };
    });

    setRfNodes(newRfNodes);
    setRfEdges(newRfEdges);
  }, [nodes, edges, setRfNodes, setRfEdges]);

  // Track position updates when dragged
  const handleNodesChange = (changes: any) => {
    onNodesChange(changes);
    for (const c of changes) {
      if (c.type === 'position' && c.position && c.id) {
        prevNodesRef.current.set(c.id, c.position);
      }
    }
  };

  return (
    <div className={cn('relative h-full w-full bg-gray-950', className)} data-testid="exploration-canvas">
      <ReactFlow
        nodes={rfNodes}
        edges={rfEdges}
        onNodesChange={handleNodesChange}
        onEdgesChange={onEdgesChange}
        onNodeClick={(_, node) => setSelectedNode(node.data.node)}
        nodeTypes={nodeTypes}
        fitView
        fitViewOptions={{ padding: 0.2 }}
        proOptions={{ hideAttribution: true }}
        minZoom={0.15}
        maxZoom={2.5}
        className="!bg-gray-950"
      >
        <Background variant={BackgroundVariant.Dots} gap={20} size={1} color="#374151" />
        <Controls
          showInteractive={false}
          className="!rounded-lg !border !border-gray-800 !bg-gray-900 !shadow-lg [&>button]:!border-gray-800 [&>button]:!bg-gray-900 [&>button]:!text-gray-200 [&>button:hover]:!bg-gray-800 [&_svg]:!fill-gray-200"
        />
        <MiniMap
          pannable
          zoomable
          className="!rounded-lg !border !border-gray-800 !bg-gray-900/90 !shadow-lg"
          maskColor="rgba(0, 0, 0, 0.45)"
          nodeColor={(node) => {
            const data = (node.data as { node?: CanvasNodeData })?.node;
            const k = data?.kind || 'intent';
            return typeMeta[k]?.hex || '#64748b';
          }}
          nodeStrokeWidth={0}
          nodeBorderRadius={4}
        />

        {/* Legend / Info Panel */}
        <Panel position="top-left">
          <div className="flex flex-col gap-2 rounded-lg border border-gray-800 bg-gray-900/90 p-2.5 text-xs shadow-lg backdrop-blur-md">
            {nodes.length === 0 && <span className="text-gray-400">{emptyHint}</span>}
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
              {(['goal', 'intent', 'fact', 'finding', 'hint'] as ExploreKind[]).map((k) => {
                const m = typeMeta[k];
                const Icon = m.icon;
                return (
                  <span key={k} className="inline-flex items-center gap-1 text-[11px] text-gray-300">
                    <span className={cn('flex size-3.5 items-center justify-center rounded', m.iconBg)}>
                      <Icon className="size-2 text-white" />
                    </span>
                    {m.label}
                  </span>
                );
              })}
            </div>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-gray-800 pt-1.5 text-[11px] text-gray-400">
              {Object.entries(relLabel).map(([k, label]) => (
                <span key={k} className="inline-flex items-center gap-1">
                  <span className="h-0.5 w-3 rounded-full" style={{ backgroundColor: relColor[k] }} />
                  {label}
                </span>
              ))}
            </div>
          </div>
        </Panel>
      </ReactFlow>

      {/* Detail Drawer */}
      <NodeDetailDrawer node={selectedNode} onClose={() => setSelectedNode(null)} />
    </div>
  );
}
