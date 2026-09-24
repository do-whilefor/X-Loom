'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {mapState, resolveNodeKey} = require('./static/graph-data.js');
const {layout, mergePositions, anchors} = require('./static/layout.js');
const {routeEdges} = require('./static/routing.js');
const {visualStatus, nodePresentation, collectSelection, describeEdge} = require('./static/graph-view.js');

function fixture() {
  return {graph: {project: {id: 'real-project', generation: 4}}, goals: [{id: 'goal', condition: '证明目标', status: 'achieved', sources: ['result'], support_valid: false}],
    steps: [{id: 'same', goal_id: 'goal', description: '执行真实步骤', status: 'completed', from: ['origin'], result: 'result', invalid_sources: ['origin']}],
    fact_records: [{id: 'origin', description: '原始输入', status: 'input'}, {id: 'goal', description: '目标输入', status: 'input'}, {id: 'same', description: '不同实体同 ID', status: 'valid'}, {id: 'result', description: '真实结果', status: 'refuted'}],
    findings: [{id: 'candidate', claim: '未验证的发现', status: 'candidate', sources: ['result']}], fact_relations: [{kind: 'refutes', source: 'same', target: 'result', reason: '证据矛盾'}, {kind: 'narrows', source: 'same', target: 'result'}]};
}
test('mapping preserves typed identities, exact relationships, evidence validity and raw records', () => {
  const input = fixture(), mapped = mapState(input);
  assert.equal(mapped.projectId, 'real-project'); assert.equal(mapped.generation, 4); assert.equal(mapped.nodes.length, 7);
  assert.equal(resolveNodeKey(mapped.nodes, 'same'), null); assert.equal(resolveNodeKey(mapped.nodes, {type: 'step', id: 'same'}), 'step:same');
  const parallels = mapped.edges.filter(edge => edge.source === 'fact:same' && edge.target === 'fact:result');
  assert.equal(parallels.length, 2); assert.notEqual(parallels[0].id, parallels[1].id);
  assert.equal(mapped.edges.find(edge => edge.kind === 'step_input').supportValid, false);
  const relation = mapped.edges.find(edge => edge.kind === 'refutes'); assert.deepEqual(relation.raw, input.fact_relations[0]);
  assert.equal(relation.id, 'edge:' + JSON.stringify(['refutes', 'fact:same', 'fact:result']));
  mapped.nodes[0].raw.description = 'changed'; assert.notEqual(input.fact_records[0].description, 'changed');
});
test('missing references are diagnostic only and never synthesize graph nodes', () => {
  const state = fixture(); state.fact_relations.push({kind: 'refutes', source: 'missing', target: 'result'}); state.steps.push({id: 'bad', from: ['unknown']});
  const mapped = mapState(state); assert.equal(mapped.diagnostics.filter(item => item.kind === 'missing_reference').length, 2);
  assert.ok(!mapped.nodes.some(node => node.id === 'missing')); assert.ok(!mapped.edges.some(edge => edge.source === 'fact:missing'));
  assert.deepEqual(mapState(null).nodes, []);
});
test('root goal is terminal anchor; goal input never becomes an inferred answer', () => {
  const mapped = mapState(fixture()); assert.equal(anchors(mapped.nodes).endKey, 'goal:goal');
  assert.equal(anchors(mapped.nodes.filter(node => node.type !== 'goal')).endKey, null);
  assert.equal(nodePresentation(mapped.nodes.find(node => node.key === 'fact:goal'), 'goal:goal').label, '目标输入');
  assert.equal(nodePresentation(mapped.nodes.find(node => node.type === 'finding'), 'goal:goal').status, 'pending');
  assert.equal(visualStatus(mapped.nodes.find(node => node.type === 'step')), 'invalid');
  assert.equal(visualStatus(mapped.nodes.find(node => node.type === 'goal')), 'invalid');
});
test('incremental layout preserves moved points through reorder, add/remove and cyclic relationships', () => {
  const mapped = mapState(fixture()); const first = layout(mapped.nodes, mapped.edges, {seed: 'stable'});
  first.positions.set('fact:origin', {x: -50000, y: 88000}); first.positions.set('step:same', {x: 121, y: -8500});
  const nodes = [...mapped.nodes.filter(node => node.key !== 'fact:same').reverse(), {key: 'fact:new', id: 'new', type: 'fact'}];
  const second = mergePositions(nodes, first.positions, {seed: 'stable'});
  assert.deepEqual(second.positions.get('fact:origin'), {x: -50000, y: 88000}); assert.deepEqual(second.positions.get('step:same'), {x: 121, y: -8500});
  assert.ok(!second.positions.has('fact:same')); assert.ok(second.positions.has('fact:new'));
  assert.doesNotThrow(() => layout(nodes, [{source: 'fact:origin', target: 'step:same'}, {source: 'step:same', target: 'fact:origin'}, {source: 'missing', target: 'missing'}]));
  assert.equal(layout([], []).positions.size, 0);
});
test('open goals and source-free candidate findings are pending, not failed evidence', () => {
  const goal = {key: 'goal:goal', type: 'goal', status: 'open', supportValid: false, sources: [], raw: {}};
  const candidate = {key: 'finding:new', type: 'finding', status: 'candidate', supportValid: false, sources: [], raw: {}};
  for (const node of [goal, candidate]) {
    assert.equal(visualStatus(node), 'pending');
    assert.doesNotMatch(nodePresentation(node, 'goal:goal').statusLabel, /失效/);
  }
  assert.match(nodePresentation({...candidate, status: 'verified'}, null).statusLabel, /缺少有效证据/);
  assert.equal(visualStatus({...goal, status: 'achieved'}), 'invalid');
  assert.equal(visualStatus({...candidate, status: 'verified', supportValid: null}), 'invalid');
  assert.match(nodePresentation({...goal, status: 'achieved', sources: ['fact'], supportValid: null}, 'goal:goal').statusLabel, /待核对/);
  assert.match(nodePresentation({...candidate, status: 'verified', sources: ['fact'], supportValid: null}, null).statusLabel, /待核对/);
});
test('dense random placement completes with finite nonoverlapping cards and fixed initial anchors', () => {
  const nodes = mapState(fixture()).nodes.concat(Array.from({length: 180}, (_, id) => ({key: 'fact:dense-' + id, id: String(id), type: 'fact'})));
  const first = layout(nodes, [], {seed: 'one'}), second = layout(nodes, [], {seed: 'two'});
  assert.deepEqual(first.positions.get('fact:origin'), second.positions.get('fact:origin')); assert.deepEqual(first.positions.get('goal:goal'), second.positions.get('goal:goal'));
  const points = [...first.positions.values()];
  for (let a = 0; a < points.length; a++) for (let b = a + 1; b < points.length; b++) {
    assert.ok(Math.abs(points[a].x - points[b].x) >= 188 || Math.abs(points[a].y - points[b].y) >= 114);
  }
  assert.ok(points.every(point => Number.isFinite(point.x) && Number.isFinite(point.y)));
});
test('parallel and reverse relations keep independently selectable cubic routes; self edges do not crash', () => {
  const points = new Map([['a', {x: -250, y: -90}], ['b', {x: 520, y: 200}]]);
  const edges = [{id: 'one', source: 'a', target: 'b'}, {id: 'two', source: 'a', target: 'b'}, {id: 'reverse', source: 'b', target: 'a'}, {id: 'self', source: 'a', target: 'a'}];
  const routes = routeEdges(edges, points); assert.equal(routes.size, 4); assert.equal(new Set([...routes.values()].map(route => route.d)).size, 4);
  for (const route of routes.values()) { assert.match(route.d, /^M .* C /); assert.ok(route.points.every(point => Number.isFinite(point.x) && Number.isFinite(point.y))); }
});
test('selection tracks upstream/downstream cycles and distinguishes same-endpoint edge IDs', () => {
  const project = {nodes: ['a', 'b', 'c'].map(key => ({key})), edges: [{id: 'one', source: 'a', target: 'b'}, {id: 'two', source: 'a', target: 'b'}, {id: 'back', source: 'b', target: 'a'}, {id: 'out', source: 'b', target: 'c'}]};
  assert.deepEqual([...collectSelection(project, null, 'one').edgeKeys], ['one']);
  assert.deepEqual(new Set(collectSelection(project, 'b', null).nodeIds), new Set(['a', 'b']));
  assert.deepEqual(new Set(collectSelection(project, 'a', null, 'downstream').nodeIds), new Set(['a', 'b', 'c']));
});
test('edge details use exact relationship, source record and latest text, not a successful target inference', () => {
  const mapped = mapState(fixture()), edge = mapped.edges.find(item => item.kind === 'refutes');
  const detail = describeEdge(mapped, edge.id); assert.equal(detail.label, '反驳'); assert.equal(detail.description, '证据矛盾'); assert.equal(detail.status, 'invalid');
  mapped.nodes.find(node => node.key === edge.source).label = '<script>new title</script>';
  assert.equal(describeEdge(mapped, edge.id).sourceNode.label, '<script>new title</script>');
  assert.equal(describeEdge(mapped, mapped.edges.find(item => item.kind === 'finding_support')).status, 'invalid');
});
module.exports = {fixture};
