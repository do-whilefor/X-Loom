const {test} = require('node:test');
const assert = require('node:assert/strict');
const {XLoomGraph, mapState, resolveNodeKey, LABEL_LAYOUT, createTextMeasurer, wrapMeasuredText, formatNodeLabel, buildFlowLayout, nodePresentation} = require('../static/graph.js');

// Deliberately proportional metrics: equal-length strings can have very
// different rendered widths. Browser acceptance uses the real canvas metrics.
function proportionalWidth(value) {
  const segmenter = new Intl.Segmenter(undefined, {granularity:'grapheme'});
  return [...segmenter.segment(value)].reduce((width, {segment}) => {
    if (/\p{Extended_Pictographic}|\p{Regional_Indicator}/u.test(segment)) return width + 16;
    if (/\p{Script=Han}/u.test(segment)) return width + 12;
    if (segment === ' ') return width + 3.5;
    if (segment === 'i' || segment === 'l') return width + 3;
    if (segment === 'W' || segment === 'M') return width + 11;
    return width + 7;
  }, 0);
}

function assertFitsNode(formatted, measure = proportionalWidth) {
  const maxWidth = LABEL_LAYOUT.width - 2 * LABEL_LAYOUT.insetX - LABEL_LAYOUT.measureSlack;
  assert.ok(formatted.lines.length <= LABEL_LAYOUT.maxLines);
  for (const line of formatted.lines) assert.ok(measure(line) <= maxWidth, 'pixel width overflow: ' + line);
  assert.ok(formatted.lines.length * LABEL_LAYOUT.fontSize * LABEL_LAYOUT.lineHeight <= LABEL_LAYOUT.height - 2 * LABEL_LAYOUT.insetY);
}

test('node labels bound Chinese, continuous UUIDs and paths by measured width and three-line height', () => {
  for (const value of [
    '真实模型接入检查成功已读取本地文件并确认随机标记' + '没有空格的中文事实'.repeat(18),
    'f72b5d67-3d69-4ff8-9cab-68dc9ca4b04b'.repeat(5),
    '/workspace/.xloom/runs/24dab863-75b4-4b37-b09c-a46c371b459a/model-acceptance-evidence.txt'.repeat(3),
    '模型验收 UUID=f72b5d67-3d69-4ff8-9cab-68dc9ca4b04b 读取 /workspace/proof.txt 后确认结果。'.repeat(4)
  ]) {
    const node = {type:'fact', status:'valid', id:'f001', label:value, description:value};
    const before = structuredClone(node);
    const formatted = formatNodeLabel(node, proportionalWidth);
    assertFitsNode(formatted);
    assert.equal(formatted.lines.length, 3);
    assert.equal(formatted.lines[0], '事实 · 有效');
    assert.ok(formatted.lines.at(-1).endsWith('…'));
    assert.equal(formatted.truncated, true);
    assert.deepEqual(node, before, 'layout must never shorten content exposed to the log or selection callback');
  }
});

test('pixel layout distinguishes narrow and wide text instead of using a character limit', () => {
  const base = {type:'step', status:'open', id:'i001'};
  const narrow = formatNodeLabel({...base, label:'i'.repeat(60)}, proportionalWidth);
  const wide = formatNodeLabel({...base, label:'W'.repeat(60)}, proportionalWidth);
  assertFitsNode(narrow); assertFitsNode(wide);
  assert.equal(narrow.truncated, false);
  assert.equal(narrow.lines.length, 2);
  assert.equal(narrow.lines[1], 'i'.repeat(60));
  assert.equal(wide.truncated, true);
  assert.equal(wide.lines.length, 3);
});

test('status header and empty-label ID fallback obey the same bounds', () => {
  const warning = formatNodeLabel({type:'finding', status:'verified', supportValid:false, id:'finding_1', label:'有界说明'}, proportionalWidth);
  assertFitsNode(warning);
  assert.ok(warning.lines[0].includes('支持失效'));
  const unknown = formatNodeLabel({type:'fact', status:'上游自定义超长状态'.repeat(12), id:'f001', label:'原文'}, proportionalWidth);
  assertFitsNode(unknown);
  assert.ok(unknown.lines[0].endsWith('…'));
  const fallback = formatNodeLabel({type:'goal', status:'open', id:'goal-' + 'a'.repeat(100), label:' \n\t'}, proportionalWidth);
  assertFitsNode(fallback);
  assert.ok(fallback.lines[1].startsWith('goal-'));
  assert.ok(fallback.lines.at(-1).endsWith('…'));
});

test('wrapping retains emoji, flags and combining marks as whole graphemes', () => {
  for (const glyph of ['👩🏽‍💻', '👨‍👩‍👧‍👦', '🇨🇳', 'e\u0301', '1️⃣']) {
    const glyphMeasure = value => [...new Intl.Segmenter(undefined, {granularity:'grapheme'}).segment(value)]
      .reduce((sum, {segment}) => sum + (segment === '…' ? 5 : 16), 0);
    const result = wrapMeasuredText(glyph.repeat(8), glyphMeasure, 23, 2);
    assert.deepEqual(result.lines, [glyph, glyph + '…']);
    assert.equal(result.truncated, true);
    assert.ok(result.lines.every(line => glyphMeasure(line) <= 23));
  }
});

test('grapheme fallback also avoids splitting composed emoji and flag pairs', () => {
  const fs = require('node:fs'), path = require('node:path'), vm = require('node:vm');
  const context = {module:{exports:{}}, Intl:{}};
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../static/graph.js'), 'utf8'), context);
  for (const glyph of ['👩🏽‍💻', '👨‍👩‍👧‍👦', '🇨🇳', 'e\u0301', '1️⃣']) {
    const measure = value => [...new Intl.Segmenter(undefined, {granularity:'grapheme'}).segment(value)]
      .reduce((sum, {segment}) => sum + (segment === '…' ? 5 : 16), 0);
    const wrapped = context.module.exports.wrapMeasuredText(glyph.repeat(8), measure, 23, 2);
    assert.equal(wrapped.lines.join('\n'), glyph + '\n' + glyph + '…');
  }
});

test('ellipsis reserves actual pixel width and oversized graphemes cannot escape the card', () => {
  const measure = value => proportionalWidth(value) + (value.endsWith('…') ? 14 : 0);
  const result = wrapMeasuredText('W'.repeat(100), measure, 34, 2);
  assert.ok(result.lines.every(line => measure(line) <= 34));
  assert.equal(result.lines.at(-1), 'W…');
  assert.deepEqual(wrapMeasuredText('👩🏽‍💻', value => value === '…' ? 8 : 400, 194, 2), {lines:['…'], truncated:true});
  assert.deepEqual(wrapMeasuredText('大', () => 400, 194, 2), {lines:[], truncated:true});
  assert.deepEqual(wrapMeasuredText('', proportionalWidth, 194, 2), {lines:[], truncated:false});
});

test('canvas measurer uses the rendered font, ink width and a bounded cache', () => {
  let calls = 0;
  const context = {font:'', measureText(value) {
    calls++;
    assert.equal(this.font, 'normal normal 12px ' + LABEL_LAYOUT.fontFamily);
    return {width:value.length * 4, actualBoundingBoxLeft:2, actualBoundingBoxRight:value.length * 4 + 1};
  }};
  const measure = createTextMeasurer(context);
  assert.equal(measure('ink'), 15);
  assert.equal(measure('ink'), 15);
  assert.equal(calls, 1);
  for (let i = 0; i < 4100; i++) measure('unique' + i);
  const before = calls;
  measure('ink');
  assert.equal(calls, before + 1, 'old cache entries must not grow without bound');
  assert.throws(() => createTextMeasurer(null), TypeError);
});

function stateFixture() {
  return {
    graph: {project: {id: 'proj-real'}},
    goals: [
      {id: 'goal', condition: '真实根目标', status: 'open', sources: [], support_valid: false},
      {id: 'child', parent_id: 'goal', condition: '子目标', status: 'achieved', sources: ['result'], support_valid: true}
    ],
    steps: [{id: 'run', goal_id: 'child', description: '检查本地记录', from: ['origin'], result: 'result', status: 'completed'}],
    fact_records: [
      {id: 'origin', description: '输入范围', status: 'input', legacy: true, evidence: []},
      {id: 'goal', description: '同名的用户目标输入', status: 'input', evidence: []},
      {id: 'result', description: '原始记录内容', status: 'valid', evidence: [{run_id: 'run-real', path: '/workspace/result', excerpt: '原始证据', start_line: 3, end_line: 4}]}
    ],
    findings: [{id: 'found', claim: '范围内的结论', scope: '样本', status: 'verified', sources: ['result'], evidence: [], support_valid: true}],
    fact_relations: []
  };
}

test('complete State maps goals, steps, facts and findings with the correct typed directions', () => {
  const mapped = mapState(stateFixture());
  assert.equal(mapped.projectId, 'proj-real');
  assert.equal(mapped.nodes.length, 7);
  const relations = new Set(mapped.edges.map(edge => edge.kind + '|' + edge.source + '|' + edge.target));
  for (const expected of [
    'parent|goal:goal|goal:child', 'goal_step|goal:child|step:run', 'step_input|fact:origin|step:run',
    'step_result|step:run|fact:result', 'goal_support|fact:result|goal:child', 'finding_support|fact:result|finding:found'
  ]) assert.ok(relations.has(expected), expected);
  assert.deepEqual(mapped.diagnostics, []);
});

test('same raw IDs in different node types remain distinct and ambiguous bare IDs are rejected', () => {
  const state = stateFixture();
  state.steps.push({id: 'goal', description: '同名步骤', status: 'open', from: ['goal'], goal_id: 'goal'});
  state.findings.push({id: 'goal', claim: '同名发现', status: 'candidate', sources: ['goal'], evidence: []});
  const {nodes, edges} = mapState(state);
  assert.equal(new Set(nodes.map(node => node.key)).size, nodes.length);
  for (const type of ['goal', 'fact', 'step', 'finding']) {
    assert.equal(resolveNodeKey(nodes, {type, id: 'goal'}), type + ':goal');
    assert.equal(resolveNodeKey(nodes, type + ':goal'), type + ':goal');
  }
  assert.equal(resolveNodeKey(nodes, 'goal'), null);
  assert.equal(resolveNodeKey(nodes, 'origin'), 'fact:origin');
  assert.ok(edges.some(edge => edge.source === 'fact:goal' && edge.target === 'step:goal'));
  assert.ok(edges.some(edge => edge.source === 'goal:goal' && edge.target === 'step:goal'));
});

test('missing references produce diagnostics without fabricating nodes or dangling edges', () => {
  const state = stateFixture();
  state.goals[1].parent_id = 'missing-parent';
  state.steps[0].from.push('missing-input');
  state.steps[0].result = 'missing-result';
  state.findings[0].sources.push('missing-source');
  state.fact_relations.push({kind: 'refutes', source: 'result', target: 'missing-old', reason: '不存在'});
  const {nodes, edges, diagnostics} = mapState(state);
  assert.equal(nodes.length, 7);
  assert.equal(diagnostics.filter(item => item.kind === 'missing_reference').length, 5);
  const keys = new Set(nodes.map(node => node.key));
  assert.ok(edges.every(edge => keys.has(edge.source) && keys.has(edge.target)));
  assert.equal(nodes.some(node => node.id.startsWith('missing-')), false);
});

test('evidence, statuses and support validity are retained independently without inventing verification', () => {
  const state = stateFixture();
  state.fact_records[2].status = 'refuted';
  state.findings[0].support_valid = false;
  state.findings.push({id: 'empty', claim: '尚未验证', status: 'candidate', sources: [], evidence: []});
  const {nodes, edges} = mapState(state);
  const source = nodes.find(node => node.key === 'fact:result');
  const finding = nodes.find(node => node.key === 'finding:found');
  const empty = nodes.find(node => node.key === 'finding:empty');
  assert.equal(source.status, 'refuted');
  assert.deepEqual(source.evidence, state.fact_records[2].evidence);
  assert.equal(finding.status, 'verified');
  assert.equal(finding.supportValid, false);
  assert.deepEqual(finding.evidence, []);
  assert.deepEqual(finding.sources, ['result']);
  assert.equal(empty.status, 'candidate');
  assert.equal(empty.supportValid, null);
  assert.deepEqual(empty.evidence, []);
  assert.equal(edges.find(edge => edge.target === finding.key).supportValid, false);
});

test('corrective fact relations point from the correcting source to the old target with labels', () => {
  const state = stateFixture();
  state.fact_relations = ['supersedes', 'refutes', 'narrows'].map(kind => ({kind, source: 'result', target: 'goal', reason: '原始原因', run_id: 'run-real'}));
  const {edges} = mapState(state);
  for (const [kind, label] of Object.entries({supersedes: '取代', refutes: '反驳', narrows: '收窄'})) {
    const edge = edges.find(item => item.kind === kind);
    assert.equal(edge.source, 'fact:result');
    assert.equal(edge.target, 'fact:goal');
    assert.equal(edge.label, label);
    assert.equal(edge.raw.reason, '原始原因');
  }
});

test('arbitrary graph size, branches, cycles and disconnected facts are preserved', () => {
  const state = stateFixture();
  for (let i = 0; i < 175; i++) {
    state.fact_records.push({id: 'f' + i, description: '记录 ' + i, status: 'valid', evidence: []});
    state.steps.push({id: 's' + i, description: '步骤 ' + i, goal_id: 'goal', status: 'completed', from: [i ? 'f' + (i - 1) : 'origin'], result: 'f' + i});
  }
  state.fact_records.push({id: 'unlinked', description: '独立记录', status: 'valid', evidence: []});
  state.fact_relations.push({kind: 'refutes', source: 'f0', target: 'f174', reason: '服务器关系'});
  const {nodes, edges, diagnostics} = mapState(state);
  assert.equal(nodes.length, 358);
  assert.ok(nodes.some(node => node.id === 'unlinked'));
  assert.ok(edges.some(edge => edge.source === 'fact:f0' && edge.target === 'fact:f174'));
  assert.equal(diagnostics.length, 0);
});

test('null and empty states are empty, and malformed or duplicate records are diagnosed', () => {
  for (const state of [null, undefined, {}, {goals: null, steps: null, fact_records: null, findings: null}]) {
    assert.deepEqual(mapState(state).nodes, []);
    assert.deepEqual(mapState(state).edges, []);
  }
  const mapped = mapState({goals: [{id: 'one', condition: '保留', status: 'open'}, {id: 'one', condition: '重复'}, null], fact_relations: [null]});
  assert.equal(mapped.nodes.length, 1);
  assert.equal(mapped.nodes[0].label, '保留');
  assert.deepEqual(mapped.diagnostics.map(item => item.kind), ['duplicate_node', 'invalid_node', 'invalid_relation']);
});

test('labels and raw evidence remain text and returned data cannot mutate the input State', () => {
  const state = stateFixture();
  const source = state.fact_records[2];
  source.description = '<img src=x onerror=alert(1)>\n真实记录 ' + '长文本'.repeat(100);
  const before = structuredClone(state);
  const mapped = mapState(state);
  const node = mapped.nodes.find(item => item.key === 'fact:result');
  assert.equal(node.label, source.description);
  assert.equal(node.description, source.description);
  node.evidence[0].excerpt = '回调修改';
  node.raw.evidence[0].path = '回调修改';
  mapped.nodes.find(item => item.type === 'finding').sources.push('outside');
  assert.deepEqual(state, before);
});

test('polling order and revision-only changes produce the same deterministic mapped graph', () => {
  const state = stateFixture();
  const initial = mapState(state);
  state.revision = 500;
  state.decision_revision = 21;
  state.goals.reverse();
  state.fact_records.reverse();
  state.steps.reverse();
  assert.deepEqual(mapState(state), initial);
  assert.equal(resolveNodeKey(initial.nodes, null), null);
  assert.equal(resolveNodeKey(initial.nodes, {type: 'unknown', id: 'goal'}), null);
  assert.equal(resolveNodeKey(initial.nodes, 'missing'), null);
});

test('flow uses real origin once, preserves every typed record and places the actual root goal last', () => {
  const mapped = mapState(stateFixture());
  const before = structuredClone(mapped);
  const layout = buildFlowLayout(mapped);
  assert.equal(layout.startKey, 'fact:origin');
  assert.equal(layout.endKey, 'goal:goal');
  assert.equal(layout.nodes.length, mapped.nodes.length - 1);
  assert.equal(layout.nodes.filter(node => node.role === 'start').length, 1);
  assert.equal(layout.nodes.filter(node => node.role === 'goal').length, 1);
  assert.equal(layout.nodes.find(node => node.key === 'goal:child').label, '子目标');
  assert.equal(layout.nodes.some(node => node.key === 'fact:goal'), false);
  assert.deepEqual(layout.aliases, {'fact:goal':'goal:goal'});
  assert.equal(mapped.nodes.find(node => node.key === 'fact:goal').label, '同名的用户目标输入');
  const y = Object.fromEntries(layout.nodes.map(node => [node.key, node.position.y]));
  for (const node of layout.nodes) {
    if (node.key !== layout.startKey) assert.ok(node.position.y > y[layout.startKey]);
    if (node.key !== layout.endKey) assert.ok(node.position.y < y[layout.endKey]);
  }
  assert.ok(y['step:run'] < y['fact:result']);
  assert.ok(y['fact:result'] < y['finding:found']);
  assert.ok(y['fact:result'] < y['goal:child']);
  assert.deepEqual(mapped, before, 'layout must not rewrite FGS facts, status or relationships');
});

test('corrections and goal ownership preserve directions without affecting execution order', () => {
  const state = stateFixture();
  const before = buildFlowLayout(mapState(state));
  state.fact_relations = ['refutes','supersedes','narrows'].map(kind => ({kind, source:'result', target:'origin'}));
  const mapped = mapState(state);
  const after = buildFlowLayout(mapped);
  assert.deepEqual(after.nodes.map(node => [node.key,node.position]), before.nodes.map(node => [node.key,node.position]));
  for (const edge of mapped.edges.filter(edge => ['refutes','supersedes','narrows'].includes(edge.kind))) {
    assert.equal(edge.source, 'fact:result');
    assert.equal(edge.target, 'fact:origin');
  }
  const ownership = mapped.edges.find(edge => edge.kind === 'goal_step');
  assert.equal(ownership.source, 'goal:child');
  assert.equal(ownership.target, 'step:run');
  assert.equal(ownership.label, '任务归属');
});

test('empty or incomplete State does not manufacture start or goal evidence', () => {
  assert.deepEqual(buildFlowLayout(mapState(null)), {nodes:[], edges:[], aliases:{}, endKey:null, startKey:null});
  const mapped = mapState({fact_records:[{id:'unlinked',description:'未连接的事实',status:'valid'}]});
  const layout = buildFlowLayout(mapped);
  assert.equal(layout.nodes.length, 1);
  assert.equal(layout.startKey, null);
  assert.equal(layout.endKey, null);
  assert.equal(layout.nodes[0].role, 'fact');
  const onlyGoal = buildFlowLayout(mapState({goals:[{id:'real-root',condition:'实际根目标',status:'open'}]}));
  assert.equal(onlyGoal.endKey, 'goal:real-root');
  assert.equal(onlyGoal.nodes.length, 1);
  const legacy = buildFlowLayout(mapState({fact_records:[
    {id:'origin',description:'真实输入',status:'input'},
    {id:'goal',description:'历史项目目标',status:'input'}
  ]}));
  assert.equal(legacy.nodes.length, 2);
  assert.equal(legacy.startKey, 'fact:origin');
  assert.equal(legacy.endKey, 'fact:goal');
  assert.deepEqual(legacy.aliases, {});
  assert.equal(legacy.nodes.find(node => node.key === 'fact:goal').role, 'goal');
});

test('cycles remain together and arbitrary large branches retain bounded non-overlapping cards', () => {
  const state = stateFixture();
  state.steps.push({id:'cycle', description:'再次检查', from:['result'], result:'origin-cycle', status:'open'});
  state.fact_records.push({id:'origin-cycle',description:'循环依据',status:'valid'});
  state.steps[0].from.push('origin-cycle');
  for (let i = 0; i < 120; i++) {
    state.steps.push({id:'branch-' + i,description:'并行检查',from:['origin'],result:'branch-result-' + i,status:'open'});
    state.fact_records.push({id:'branch-result-' + i,description:'检查事实',status:'valid'});
  }
  const layout = buildFlowLayout(mapState(state));
  const byKey = new Map(layout.nodes.map(node => [node.key,node]));
  assert.equal(byKey.get('step:run').layer, byKey.get('step:cycle').layer);
  assert.equal(byKey.get('step:run').layer, byKey.get('fact:result').layer);
  for (let i = 0; i < layout.nodes.length; i++) for (let j = i + 1; j < layout.nodes.length; j++) {
    const a = layout.nodes[i], b = layout.nodes[j];
    assert.ok(Math.abs(a.position.x - b.position.x) >= (a.width + b.width) / 2 + 40 || Math.abs(a.position.y - b.position.y) >= (a.height + b.height) / 2 + 40);
  }
  const reversed = mapState(state);
  reversed.nodes.reverse(); reversed.edges.reverse();
  assert.deepEqual(buildFlowLayout(reversed), layout);
  assert.equal(nodePresentation(byKey.get('fact:origin').node, layout.endKey).label, '起点');
});

test('large peer layers wrap at four cards without growing canvas width or inventing dependencies', () => {
  const state = stateFixture();
  for (let index = 0; index < 97; index++) {
    state.steps.push({id:'parallel-' + index,description:'独立分支',from:['origin'],result:'result-' + index,status:'open'});
    state.fact_records.push({id:'result-' + index,description:'分支证据',status:'valid'});
    state.fact_records.push({id:'isolated-' + index,description:'孤立事实',status:'valid'});
  }
  const mapped = mapState(state);
  const original = structuredClone(mapped);
  const layout = buildFlowLayout(mapped);
  const physicalRows = new Map(), layerRows = new Map();
  for (const entry of layout.nodes) {
    if (!physicalRows.has(entry.position.y)) physicalRows.set(entry.position.y, []);
    physicalRows.get(entry.position.y).push(entry);
    if (!layerRows.has(entry.layer)) layerRows.set(entry.layer, []);
    layerRows.get(entry.layer).push(entry.position.y);
    assert.ok(entry.position.x - entry.width / 2 >= -503);
    assert.ok(entry.position.x + entry.width / 2 <= 503);
  }
  for (const entries of physicalRows.values()) {
    assert.ok(entries.length <= 4);
    assert.equal(new Set(entries.map(entry => entry.layer)).size, 1, 'physical rows never mix dependency layers');
    const sorted = entries.toSorted((a,b) => a.position.x - b.position.x);
    for (let i = 1; i < sorted.length; i++) {
      const left = sorted[i - 1], right = sorted[i];
      assert.ok(right.position.x - right.width / 2 - left.position.x - left.width / 2 >= 42);
    }
  }
  const layers = [...layerRows].sort(([a],[b]) => a - b);
  for (let i = 1; i < layers.length; i++) {
    assert.ok(Math.max(...layers[i - 1][1]) < Math.min(...layers[i][1]), 'all wrapped peers precede the next dependency layer');
  }
  assert.ok(new Set(layerRows.get(1)).size > 1, 'wide first layer actually wraps');
  assert.equal(layout.nodes.find(entry => entry.key === layout.startKey).position.x, 0);
  assert.equal(layout.nodes.find(entry => entry.key === layout.endKey).position.x, 0);
  assert.equal(layout.edges.length, mapped.edges.length);
  for (const edge of layout.edges) {
    const real = mapped.edges.find(item => item.id === edge.id);
    assert.equal(edge.recordSource, real.source);
    assert.equal(edge.recordTarget, real.target);
    assert.equal(edge.kind, real.kind);
  }
  assert.deepEqual(mapped, original);
});

function headlessGraph() {
  const cytoscape = require('../static/vendor/cytoscape.min.js');
  const graph = Object.create(XLoomGraph.prototype);
  Object.assign(graph, {
    cy:cytoscape({headless:true}), nodes:[], edges:[], diagnostics:[], selectedKey:null,
    projectId:null, generation:null, topology:null, snapshot:null, pendingFit:false, destroyed:false,
    filter:'all', measureLabel:proportionalWidth, empty:{hidden:false}, warning:{hidden:true,textContent:''},
    picker:{value:''}, live:{textContent:''}, host:{clientWidth:800,clientHeight:500},
    fits:0, selections:[], fit() { this.fits++; }, refreshViewport() {}, renderCards() {}, updatePicker() {},
    onSelect(node) { this.selections.push(node); }
  });
  return graph;
}

test('merged endpoint projects input edges while selection and logs keep their original typed records', () => {
  const graph = headlessGraph();
  try {
    const state = stateFixture();
    state.steps[0].from.push('goal');
    state.goals[0].status = 'achieved';
    state.goals[0].sources = ['result'];
    graph.setState(state);
    assert.equal(graph.getNodes().length, 7, 'raw graph keeps both records');
    assert.equal(graph.getVisibleNodeCount(), 6, 'only one root endpoint is rendered');
    assert.equal(graph.cy.getElementById('fact:goal').length, 0);
    assert.equal(graph.cy.getElementById('goal:goal').length, 1);
    const input = graph.cy.edges().filter(edge => edge.data('recordSource') === 'fact:goal');
    assert.equal(input.length, 1);
    assert.equal(input.source().id(), 'goal:goal');
    assert.equal(input.target().id(), 'step:run');
    const support = graph.cy.edges().filter(edge => edge.data('kind') === 'goal_support' && edge.data('recordTarget') === 'goal:goal');
    graph.selectNode({type:'fact',id:'goal'});
    assert.equal(graph.selectedKey, 'fact:goal');
    assert.equal(graph.cy.getElementById('goal:goal').hasClass('is-selected'), true);
    assert.equal(graph.selections.at(-1).type, 'fact');
    assert.equal(graph.selections.at(-1).label, '同名的用户目标输入');
    assert.equal(input.hasClass('is-related'), true);
    assert.equal(support.hasClass('is-related'), false);
    graph.selectNode({type:'goal',id:'goal'});
    assert.equal(graph.selections.at(-1).type, 'goal');
    assert.equal(graph.selections.at(-1).label, '真实根目标');
    assert.equal(input.hasClass('is-related'), false);
    assert.equal(support.hasClass('is-related'), true);
  } finally { graph.cy.destroy(); }
});

test('a legacy target gains a root Goal without duplicate cards or losing typed fact selection', () => {
  const graph = headlessGraph();
  try {
    const state = stateFixture();
    state.steps[0].from.push('goal');
    const goals = state.goals;
    state.goals = [];
    graph.setState(state);
    graph.selectNode({type:'fact',id:'goal'});
    assert.equal(graph.flowLayout.endKey, 'fact:goal');
    assert.equal(graph.cy.getElementById('fact:goal').length, 1);
    state.goals = goals;
    graph.setState(state);
    assert.equal(graph.selectedKey, 'fact:goal');
    assert.equal(graph.flowLayout.endKey, 'goal:goal');
    assert.equal(graph.cy.getElementById('fact:goal').length, 0);
    assert.equal(graph.cy.getElementById('goal:goal').hasClass('is-selected'), true);
    assert.equal(graph.cy.edges().filter(edge => edge.data('recordSource') === 'fact:goal').source().id(), 'goal:goal');
    assert.equal(graph.getVisibleNodeCount(), graph.getNodes().length - 1);
  } finally { graph.cy.destroy(); }
});

test('start filter isolates the real origin and aliases participate in typed fact and goal filters', () => {
  const graph = headlessGraph();
  try {
    graph.setState(stateFixture());
    graph.setFilter('start');
    assert.equal(graph.filter, 'start');
    for (const node of graph.cy.nodes()) assert.equal(node.hasClass('is-dimmed'), node.id() !== 'fact:origin');
    graph.setFilter('fact');
    assert.equal(graph.cy.getElementById('goal:goal').hasClass('is-dimmed'), false);
    graph.setFilter('goal');
    assert.equal(graph.cy.getElementById('goal:goal').hasClass('is-dimmed'), false);
    assert.equal(graph.cy.getElementById('goal:child').hasClass('is-dimmed'), false);
    assert.equal(graph.cy.getElementById('fact:result').hasClass('is-dimmed'), true);
    const legacy = stateFixture();
    legacy.goals = [];
    graph.setState(legacy);
    assert.equal(graph.cy.getElementById('fact:goal').hasClass('is-dimmed'), false);
  } finally { graph.cy.destroy(); }
});

test('status polling retains positions and viewport while a new generation clears selection and refits', () => {
  const graph = headlessGraph();
  try {
    const state = stateFixture();
    state.graph.project.generation = 1;
    graph.setState(state);
    assert.equal(graph.fits, 1);
    graph.selectedKey = 'fact:result';
    graph.cy.getElementById('fact:result').position({x:777,y:333});
    graph.cy.zoom(.65); graph.cy.pan({x:31,y:49});
    state.steps[0].status = 'running';
    graph.setState(state);
    assert.equal(graph.fits, 1);
    assert.equal(graph.selectedKey, 'fact:result');
    assert.deepEqual(graph.cy.getElementById('fact:result').position(), {x:777,y:333});
    assert.equal(graph.cy.zoom(), .65);
    assert.deepEqual(graph.cy.pan(), {x:31,y:49});
    state.graph.project.generation = 2;
    graph.setState(state);
    assert.equal(graph.fits, 2);
    assert.equal(graph.selectedKey, null);
    assert.deepEqual(graph.selections, [null]);
    assert.notDeepEqual(graph.cy.getElementById('fact:result').position(), {x:777,y:333});
  } finally { graph.cy.destroy(); }
});

test('first real nodes arriving after an empty State fit once', () => {
  const graph = headlessGraph();
  try {
    graph.setState({graph:{project:{id:'proj-real'}},goals:[],steps:[],fact_records:[],findings:[]});
    const fits = graph.fits;
    graph.setState(stateFixture());
    assert.equal(graph.fits, fits + 1);
    graph.setState(stateFixture());
    assert.equal(graph.fits, fits + 1);
  } finally { graph.cy.destroy(); }
});

test('topology changes keep real anchors at the extremes without changing the viewport', () => {
  const state = stateFixture();
  const graph = headlessGraph();
  const cy = graph.cy;
  try {
    graph.setState(state);
    cy.zoom(.7);
    cy.pan({x: 21, y: 43});
    const zoom = cy.zoom();
    const pan = {...cy.pan()};
    state.fact_records.push({id:'new', description:'新增事实', status:'valid'});
    state.fact_relations.push({kind:'refutes', source:'new', target:'origin'});
    graph.setState(state);
    const added = cy.getElementById('fact:new').position();
    assert.ok(Number.isFinite(added.x) && Number.isFinite(added.y));
    const origin = cy.getElementById('fact:origin').position().y;
    const goal = cy.getElementById('goal:goal').position().y;
    for (const node of cy.nodes()) {
      if (node.id() !== 'fact:origin') assert.ok(node.position().y > origin);
      if (node.id() !== 'goal:goal') assert.ok(node.position().y < goal);
    }
    assert.equal(cy.zoom(), zoom);
    assert.deepEqual(cy.pan(), pan);
  } finally {
    cy.destroy();
  }
});

test('a graph loaded in a hidden tab defers its first fit until visible and preserves later user zoom', () => {
  const cytoscape = require('../static/vendor/cytoscape.min.js');
  const cy = cytoscape({headless: true, elements: [{data: {id: 'goal:goal'}}]});
  try {
    const graph = Object.create(XLoomGraph.prototype);
    Object.assign(graph, {cy, host: {clientWidth: 0, clientHeight: 0}, destroyed: false, pendingFit: false, emitZoom() {}});
    let fits = 0;
    const originalFit = cy.fit.bind(cy);
    cy.fit = (...args) => { fits++; return originalFit(...args); };
    cy.zoom(.65); cy.pan({x: 120, y: 40});
    graph.fit();
    graph.refreshViewport();
    assert.equal(fits, 0);
    assert.equal(graph.pendingFit, true);
    assert.equal(cy.zoom(), .65);
    assert.deepEqual(cy.pan(), {x: 120, y: 40});
    graph.host.clientWidth = 800; graph.host.clientHeight = 500;
    graph.refreshViewport();
    assert.equal(fits, 1);
    assert.equal(graph.pendingFit, false);
    cy.zoom(.82); cy.pan({x: 31, y: 76});
    graph.host.clientWidth = 0; graph.host.clientHeight = 0;
    graph.refreshViewport();
    graph.host.clientWidth = 800; graph.host.clientHeight = 500;
    graph.refreshViewport();
    assert.equal(fits, 1);
    assert.equal(cy.zoom(), .82);
    assert.deepEqual(cy.pan(), {x: 31, y: 76});
  } finally { cy.destroy(); }
});

test('an evidence jump refreshes the just-revealed graph before centering its typed node', () => {
  const cytoscape = require('../static/vendor/cytoscape.min.js');
  const mapped = mapState(stateFixture());
  const cy = cytoscape({headless: true, elements: mapped.nodes.map(node => ({data: {id: node.key, type: node.type}}))});
  try {
    const graph = Object.create(XLoomGraph.prototype);
    let selected = null;
    Object.assign(graph, {
      cy, nodes: mapped.nodes, host: {clientWidth: 800, clientHeight: 500}, destroyed: false, pendingFit: false,
      filter: 'all', picker: {value: ''}, live: {textContent: ''}, onSelect(node) { selected = node; }
    });
    const operations = [];
    const originalResize = cy.resize.bind(cy);
    const originalCenter = cy.center.bind(cy);
    cy.resize = (...args) => { operations.push('resize'); return originalResize(...args); };
    cy.center = (...args) => { operations.push('center'); return originalCenter(...args); };
    cy.getElementById('fact:goal').position({x: 5000, y: 5000});
    graph.selectNode({type: 'fact', id: 'goal'});
    assert.equal(graph.selectedKey, 'fact:goal');
    assert.equal(selected.type, 'fact'); assert.equal(selected.id, 'goal');
    assert.ok(operations.indexOf('resize') >= 0);
    assert.ok(operations.indexOf('center') > operations.indexOf('resize'));
    assert.equal(graph.picker.value, 'fact:goal');
  } finally { cy.destroy(); }
});
