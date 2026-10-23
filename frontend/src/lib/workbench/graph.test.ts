import assert from 'node:assert/strict';
import test from 'node:test';
import { buildExplorationGraph, parseExplorationGraph, type ExplorationGraph } from './graph';

const graph: ExplorationGraph = {
  nodes: [
    { id: 'goal-1', kind: 'goal', state: 'open', payload: { text: '第一个目标' } },
    { id: 'goal-2', kind: 'goal', state: 'met', payload: { text: '第二个目标' } },
    { id: 'intent-node', kind: 'intent', state: 'open', work_status: 'completed', work_item_id: 'work-id', payload: { text: '历史方向' } },
    { id: 'fact-1', kind: 'fact', state: 'unverified', payload: { summary: '待验证事实' } },
    { id: 'fact-2', kind: 'fact', state: 'verified', payload: { summary: '有来源事实' } },
    { id: 'finding-node', kind: 'finding', state: 'unverified', finding_id: 'finding-id', payload: { summary: '旧摘要' }, finding: { id: 'finding-id', title: '真实标题', status: 'pending_review', source_kind: 'speculative', verification_state: 'unverified' } },
  ],
  edges: [
    { id: 'e-1', src: 'intent-node', dst: 'fact-1', rel: 'yields' },
    { id: 'e-2', src: 'fact-2', dst: 'intent-node', rel: 'derived_from' },
    { id: 'e-3', src: 'fact-2', dst: 'goal-2', rel: 'proves', reason: '实测证据' },
  ],
};

test('preserves canonical IDs, isolated goals, exact lineage and only recorded proofs', () => {
  const { graphNodes, graphEdges } = buildExplorationGraph(parseExplorationGraph(graph));
  assert.equal(graphNodes.length, 6);
  assert.deepEqual(graphEdges, graph.edges);
  assert.equal(graphEdges.filter(e => e.rel === 'proves').length, 1);
  assert.ok(!graphEdges.some(e => e.src === 'goal-1' || e.dst === 'goal-1'));
  const direction = graphNodes.find(n => n.id === 'intent-node')!;
  assert.equal(direction.work_item_id, 'work-id');
  assert.equal(direction.state, 'completed');
  assert.equal(direction.node_state, 'open');
  assert.equal(graphNodes.find(n => n.id === 'fact-1')!.state, 'unverified');
  const finding = graphNodes.find(n => n.id === 'finding-node')!;
  assert.equal(finding.finding_id, 'finding-id');
  assert.equal(finding.summary, '真实标题');
  assert.equal(finding.finding?.verification_state, 'unverified');
  assert.equal(finding.finding?.status, 'pending_review');
});

test('includes the whole snapshot, terminal nodes and parallel relationships', () => {
  const many: ExplorationGraph = { nodes: [...graph.nodes], edges: [...graph.edges, { id: 'other-rel', src: 'fact-2', dst: 'intent-node', rel: 'spawns' }] };
  for (let i = 0; i < 110; i++) many.nodes.push({ id: `fact-extra-${i}`, kind: 'fact', state: 'unverified', payload: {} });
  const view = buildExplorationGraph(parseExplorationGraph(many));
  assert.equal(view.graphNodes.length, 116);
  assert.equal(view.graphEdges.length, 4);
});

test('malformed snapshots fail visibly instead of becoming an empty or partly invented graph', () => {
  assert.throws(() => parseExplorationGraph({ nodes: [], edges: [{ id: 'bad', src: 'missing', dst: 'other', rel: 'proves' }] }));
  assert.throws(() => parseExplorationGraph({ goals: [] }));
  assert.deepEqual(buildExplorationGraph(null), { graphNodes: [], graphEdges: [] });
});
