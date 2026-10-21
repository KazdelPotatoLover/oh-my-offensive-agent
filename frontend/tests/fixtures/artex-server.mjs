import http from 'node:http';
import { randomUUID } from 'node:crypto';

// Local-only model/target substitutes. They never forward to an external API.
const state = { patched: false, holdWorker: true, releases: new Set(), fixtureRequests: 0, modelRequests: 0, workerWaiting: false, sawSteer: false, runId: null };
const json = (res, value, status = 200) => { res.writeHead(status, { 'content-type': 'application/json' }); res.end(JSON.stringify(value)); };
const readBody = async req => { let raw = ''; for await (const chunk of req) raw += chunk; return raw ? JSON.parse(raw) : {}; };
const api = async route => { const response = await fetch(`http://127.0.0.1:28181/api${route}`); if (!response.ok) throw new Error(`local API ${route}: ${response.status}`); return response.json(); };
const tool = (name, args) => ({ id: randomUUID(), type: 'function', function: { name, arguments: JSON.stringify(args) } });
const modelResponse = (res, calls = [], content = 'Recorded local evidence.') => json(res, {
  id: randomUUID(), model: 'local-browser-fixture', choices: [{ index: 0, finish_reason: calls.length ? 'tool_calls' : 'stop', message: { role: 'assistant', content, ...(calls.length ? { tool_calls: calls } : {}) } }],
  usage: { prompt_tokens: 100, completion_tokens: 40, total_tokens: 140 },
});
async function currentRun() {
  if (state.runId) return state.runId;
  const { tasks } = await api('/tasks');
  const { runs } = await api(`/tasks/${tasks[0].id}/runs`);
  state.runId = runs[0].id;
  return state.runId;
}

const target = http.createServer((req, res) => {
  state.fixtureRequests++;
  if (state.patched) json(res, { error: 'SECRET_ACCESS_DENIED' }, 403);
  else json(res, { secret: 'SECRET_MARKER', scope: 'local browser acceptance fixture' });
});
target.listen(29191, '127.0.0.1');

const model = http.createServer(async (req, res) => {
  try {
    if (req.url === '/health') return json(res, { status: 'ok' });
    if (req.url === '/state') return json(res, { ...state, releases: state.releases.size });
    if (req.url === '/control') {
      const control = await readBody(req);
      if (control.reset) { state.runId = null; state.patched = false; state.holdWorker = true; state.sawSteer = false; }
      if (control.patch !== undefined) state.patched = control.patch;
      if (control.release) { state.holdWorker = false; for (const release of state.releases) release(); state.releases.clear(); }
      return json(res, { status: 'ok' });
    }
    if (req.url !== '/v1/chat/completions') return json(res, { error: 'not found' }, 404);
    state.modelRequests++;
    const request = await readBody(req);
    const messages = request.messages || [];
    const names = new Set((request.tools || []).map(t => t.function.name));
    const full = messages.map(m => m.content || '').join('\n');
    const last = messages.at(-1);
    const callNames = new Map(messages.flatMap(m => m.tool_calls || []).map(t => [t.id, t.function.name]));
    const lastTool = last?.role === 'tool' ? callNames.get(last.tool_call_id) : '';
    state.sawSteer ||= full.includes('[Operator Steer Guidance]');
    const runId = await currentRun();
    const { assets } = await api(`/runs/${runId}/assets`);
    const endpoint = assets.find(a => a.kind === 'http_endpoint');

    if (full.includes('Independent Security Verifier')) {
      if (lastTool === 'get_endpoint') return modelResponse(res, [], 'Current response collected. The server evaluates the stored verification conditions.');
      return modelResponse(res, [tool('get_endpoint', { asset_id: endpoint.id })]);
    }
    if (names.has('submit_plan')) {
      if (lastTool === 'submit_plan' || lastTool === 'retry_work_item' || lastTool === 'continue_work_item') return modelResponse(res, [], 'Plan accepted.');
      const { work_items: items } = await api(`/runs/${runId}/work_items`);
      if (!items.length) return modelResponse(res, [tool('submit_plan', { items: [{ key: 'browser_direction', title: '保存本地目标证据', objective: 'Read the authorized marker and preserve a reproducible finding', work_type: 'intent', asset_ids: [endpoint.id], input_data: { profile: 'browser_evidence' } }] })]);
      if (items.some(i => i.status === 'exhausted')) return modelResponse(res, [tool('continue_work_item', { work_item_key: 'browser_direction', reason: 'Operator explicitly resumed the saved direction in a new epoch' })]);
      if (items.some(i => i.status === 'cancelled' || i.status === 'failed')) return modelResponse(res, [tool('retry_work_item', { work_item_key: 'browser_direction', reason: 'Operator explicitly resumed the same saved direction' })]);
      if (items.some(i => i.status === 'pending' || i.status === 'running')) return modelResponse(res, [], 'Wait for the active direction; no extra work is needed.');
      const { goals } = await api(`/runs/${runId}/goals`);
      const { findings } = await api(`/runs/${runId}/findings`);
      if (goals.some(g => g.state !== 'met')) {
        if (!findings.length) return modelResponse(res, [tool('suggest_completion', { final_summary: 'No empirical evidence was collected.', conclusion: 'partial' })]);
        return modelResponse(res, [tool('prove_goal', { goal_id: goals[0].id, evidence_id: findings[0].id, reason: 'The current scoped response contains the marker; the finding links its real tool evidence.' })]);
      }
      if (lastTool === 'suggest_completion') return modelResponse(res, [], 'Deliverable proved and preserved.');
      return modelResponse(res, [tool('suggest_completion', { final_summary: 'Authorized fixture response, original evidence and proof are preserved.', conclusion: 'complete' })]);
    }
    if (lastTool === 'get_endpoint') {
      const output = JSON.parse(last.content);
      const source = output._execution.source_tool_call_id;
      return modelResponse(res, [
        tool('record_fact', { summary: 'The authorized fixture returned its marker', detail: 'The persisted HTTP response includes SECRET_MARKER.', confidence: 'observed', evidence: source, asset_ids: [endpoint.id] }),
        tool('report_finding', { asset_id: endpoint.id, title: 'Browser fixture exposure', category: 'information_disclosure', severity: 'medium', description: 'The local fixture exposes a reproducible test marker.', source_kind: 'empirical', source_tool_call_id: source, trigger_condition: 'GET the authorized fixture endpoint', reproduce_steps: 'Read the authorized endpoint and observe SECRET_MARKER.', verification: { vulnerable: { status_code: 200, body_contains: 'SECRET_MARKER' }, fixed: { status_code: 403, body_contains: 'SECRET_ACCESS_DENIED' } } }),
        tool('register_asset', { parent_asset_id: endpoint.id, kind: 'host' }),
      ]);
    }
    if (lastTool === 'register_asset' || lastTool === 'report_finding') return modelResponse(res, [], 'Real response, fact, finding and host relation saved.');
    if (state.holdWorker) {
      state.workerWaiting = true;
      await new Promise(resolve => {
        const release = () => { state.releases.delete(release); resolve(); };
        state.releases.add(release); res.once('close', release);
      });
      state.workerWaiting = false;
      if (res.destroyed) return;
    }
    modelResponse(res, [tool('get_endpoint', { asset_id: endpoint.id })]);
  } catch (error) { console.error('Local model fixture failed:', error.message); if (!res.headersSent) json(res, { error: error.message }, 500); }
});
model.listen(28282, '127.0.0.1');
for (const signal of ['SIGTERM', 'SIGINT']) process.on(signal, () => { for (const release of state.releases) release(); model.close(); target.close(); });
