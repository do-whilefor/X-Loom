'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {XLoomGraph} = require('./static/graph.js');

// Minimal deterministic DOM/event clock: browser geometry and rendering are
// covered by browser_test.cjs; this exercises lifecycle and update contracts.
class Target {
  constructor() { this.events = new Map(); }
  addEventListener(type, fn) { if (!this.events.has(type)) this.events.set(type, new Set()); this.events.get(type).add(fn); }
  removeEventListener(type, fn) { this.events.get(type)?.delete(fn); }
  dispatchEvent(event) { for (const fn of this.events.get(event.type) || []) fn(event); }
  listenerCount() { return [...this.events.values()].reduce((total, entries) => total + entries.size, 0); }
}
class Element extends Target {
  constructor(document) {
    super(); this.ownerDocument = document; this.children = []; this.dataset = {}; this.style = {}; this.attrs = {}; this.className = ''; this.clientWidth = 800; this.clientHeight = 600; this.capture = new Set();
    this.classList = {contains: name => this.className.split(' ').includes(name), toggle: (name, active) => { const names = new Set(this.className.split(' ').filter(Boolean)); if (active) names.add(name); else names.delete(name); this.className = [...names].join(' '); }, add: name => this.classList.toggle(name, true), remove: name => this.classList.toggle(name, false)};
  }
  append(...nodes) { for (const node of nodes) { node.remove(); node.parent = this; this.children.push(node); } }
  replaceChildren(...nodes) { for (const node of [...this.children]) node.remove(); this.append(...nodes); }
  remove() { if (this.parent) { this.parent.children = this.parent.children.filter(child => child !== this); this.parent = null; } }
  setAttribute(key, value) { this.attrs[key] = String(value); if (key === 'class') this.className = String(value); }
  getAttribute(key) { return this.attrs[key]; }
  getBoundingClientRect() { return {left: 0, top: 0, width: this.clientWidth, height: this.clientHeight}; }
  closest(selector) { if (selector.split(',').some(part => this.classList.contains(part.trim().slice(1)))) return this; return this.parent?.closest(selector) || null; }
  setPointerCapture(id) { this.capture.add(id); }
  hasPointerCapture(id) { return this.capture.has(id); }
  releasePointerCapture(id) { this.capture.delete(id); }
}
function harness(options = {}) {
  const document = new Target(), window = new Target(), frames = new Map(), timers = new Map(); let serial = 0;
  document.createElement = () => new Element(document); document.createElementNS = () => new Element(document); document.defaultView = window;
  window.requestAnimationFrame = fn => { frames.set(++serial, fn); return serial; }; window.cancelAnimationFrame = id => frames.delete(id);
  window.setTimeout = fn => { timers.set(++serial, fn); return serial; }; window.clearTimeout = id => timers.delete(id);
  window.CustomEvent = class { constructor(type, config) { this.type = type; Object.assign(this, config); } };
  window.ResizeObserver = class { observe() {} disconnect() { this.disconnected = true; } };
  const host = new Element(document), graph = new XLoomGraph(host, options);
  return {graph, host, document, window, frames, timers, flush(time = 16) { const pending = [...frames]; frames.clear(); for (const [, fn] of pending) fn(time); }};
}
function state(id = 'A', generation = 1) {
  return {graph: {project: {id, generation}}, fact_records: [{id: 'origin', description: '原始输入', status: 'input'}, {id: 'old', description: '旧事实', status: 'valid'}], steps: [{id: 'work', description: '任务', status: 'running', from: ['origin'], result: 'old'}], goals: [], findings: [], fact_relations: [{kind: 'refutes', source: 'origin', target: 'old', reason: 'original'}]};
}
test('repeated updates preserve DOM identity and manual positions, and merge one animation frame', () => {
  const h = harness(), graph = h.graph, value = state(); graph.setState(value); h.flush();
  const card = graph.cards.get('fact:origin'); graph.positions.set('fact:origin', {x: -9900, y: 4200}); graph.zoomBy(.7);
  const camera = [graph.scale, graph.tx, graph.ty];
  value.fact_records.reverse(); value.fact_records.push({id: 'new', description: '新增', status: 'valid'});
  graph.setState(value); graph.setState(value); graph.setState(value);
  assert.equal(h.frames.size, 1); h.flush();
  assert.equal(graph.cards.get('fact:origin'), card); assert.deepEqual(graph.positions.get('fact:origin'), {x: -9900, y: 4200});
  assert.deepEqual([graph.scale, graph.tx, graph.ty], camera); assert.ok(graph.positions.has('fact:new'));
  graph.destroy();
});
test('switching projects restores exact camera and positions; new generation invalidates the old round', () => {
  const h = harness(), graph = h.graph; graph.setState(state('A')); h.flush();
  graph.positions.set('fact:origin', {x: -444, y: 777}); graph.scale = .36; graph.tx = -555; graph.ty = 888; graph.transform();
  graph.setState(null); h.flush(); graph.setState(state('B')); h.flush(); graph.zoomBy(1.4);
  graph.setState(state('A')); h.flush();
  assert.deepEqual(graph.positions.get('fact:origin'), {x: -444, y: 777}); assert.deepEqual([graph.scale, graph.tx, graph.ty], [.36, -555, 888]);
  graph.setState(state('A', 2)); h.flush(); assert.notDeepEqual(graph.positions.get('fact:origin'), {x: -444, y: 777});
  assert.ok(!graph.projectLayouts.has(JSON.stringify(['A', 1]))); graph.forgetProject('B'); assert.ok(!graph.projectLayouts.has(JSON.stringify(['B', 1])));
  graph.destroy();
});
test('updated text invalidates no geometry but refreshes cached-edge labels, details and selection callbacks', () => {
  const selected = [], h = harness({onSelect: node => selected.push(node)}), graph = h.graph, value = state(); graph.setState(value); h.flush();
  const edge = graph.edges.find(item => item.kind === 'refutes'), hit = graph.edgeElements.get(edge.id).hit, cache = graph.edgeRouteCache;
  graph.selectNode('fact:origin'); value.fact_records[0].description = '<img src=x onerror=alert(1)>'; value.fact_relations[0].reason = 'new explanation';
  graph.setState(value); h.flush();
  assert.equal(graph.edgeRouteCache, cache); assert.equal(graph.edgeElements.get(edge.id).hit, hit);
  assert.match(hit.getAttribute('aria-label'), /<img/); assert.match(graph.edgeElements.get(edge.id).title.textContent, /new explanation/);
  assert.equal(selected.at(-1).description, '<img src=x onerror=alert(1)>');
  graph.project.edgeIndex.get(edge.id).label = 'updated relation'; graph.scheduleDraw(); h.flush(); assert.equal(graph.edgeElements.get(edge.id).label.textContent, 'updated relation');
  graph.destroy();
});
test('deleted selection and active drag release capture, stop edge-pan and notify application', () => {
  const selection = [], edges = [], h = harness({onSelect: node => selection.push(node), onSelectEdge: edge => edges.push(edge)}), graph = h.graph, value = state(); graph.setState(value); h.flush();
  graph.selectEdge(graph.edges.find(edge => edge.kind === 'refutes')); h.flush();
  value.fact_relations = []; graph.setState(value); h.flush(); assert.equal(edges.at(-1), null); assert.equal(graph.selectedEdge, null);
  graph.selectNode('fact:old'); const card = graph.cards.get('fact:old');
  graph.pointerDown({button: 0, target: card, pointerId: 7, clientX: 600, clientY: 300}); graph.pointerMove({pointerId: 7, clientX: 799, clientY: 300});
  assert.ok(graph.viewport.hasPointerCapture(7)); assert.notEqual(graph.panFrame, null);
  value.fact_records = value.fact_records.filter(fact => fact.id !== 'old'); value.steps = [];
  graph.setState(value); h.flush(); assert.equal(selection.at(-1), null); assert.equal(graph.drag, null); assert.equal(graph.panFrame, null); assert.ok(!graph.viewport.hasPointerCapture(7));
  graph.destroy();
});
test('zoom during drag preserves world grab offset; resizing preserves camera center', () => {
  const h = harness(), graph = h.graph; graph.setState(state()); h.flush(); const card = graph.cards.get('fact:origin');
  graph.pointerDown({button: 0, target: card, pointerId: 1, clientX: 230, clientY: 200}); graph.pointerMove({pointerId: 1, clientX: 400, clientY: 240});
  const grab = [graph.drag.grabX, graph.drag.grabY]; graph.zoom(.72, 40, 60);
  const point = graph.positions.get('fact:origin');
  assert.ok(Math.abs((point.x + grab[0]) * graph.scale + graph.tx - 400) < 1e-7); assert.ok(Math.abs((point.y + grab[1]) * graph.scale + graph.ty - 240) < 1e-7);
  graph.cancelDrag(); const center = [(400 - graph.tx) / graph.scale, (300 - graph.ty) / graph.scale], scale = graph.scale;
  graph.viewport.clientWidth = 600; graph.viewport.clientHeight = 420; graph.resize();
  assert.equal(graph.scale, scale); assert.deepEqual([(300 - graph.tx) / graph.scale, (210 - graph.ty) / graph.scale], center); graph.destroy();
});
test('blur, hidden document and destruction stop animation and remove every listener', () => {
  const h = harness(), graph = h.graph; graph.setState(state()); h.flush();
  const begin = () => { graph.pointerDown({button: 0, target: graph.cards.get('fact:origin'), pointerId: 1, clientX: 200, clientY: 200}); graph.pointerMove({pointerId: 1, clientX: 799, clientY: 300}); };
  begin(); h.window.dispatchEvent({type: 'blur'}); assert.equal(graph.drag, null); assert.equal(graph.panFrame, null);
  begin(); h.document.hidden = true; h.document.dispatchEvent({type: 'visibilitychange'}); assert.equal(graph.drag, null);
  begin(); const viewport = graph.viewport, svg = graph.svg, nodeHost = graph.nodeHost;
  graph.destroy(); graph.destroy(); h.flush();
  assert.equal(h.frames.size, 0); assert.equal(h.timers.size, 0); assert.equal(h.host.children.length, 0); assert.equal(graph.resizeObserver.disconnected, true);
  for (const target of [h.window, h.document, viewport, svg, nodeHost]) assert.equal(target.listenerCount(), 0);
});
test('separate graph instances have unique SVG marker identities', () => {
  const first = harness(), second = harness(); assert.notEqual(first.graph.instanceId, second.graph.instanceId);
  assert.notEqual(first.graph.defs.children[0].getAttribute('id'), second.graph.defs.children[0].getAttribute('id')); first.graph.destroy(); second.graph.destroy();
});
test('the first real nodes arriving after an empty snapshot receive initial fit', () => {
  const h = harness(), graph = h.graph; graph.setState({graph: {project: {id: 'A', generation: 1}}}); h.flush();
  graph.setState(state()); assert.notEqual(graph.fitFrame, null); h.flush();
  assert.ok(graph.scale < 1); graph.destroy();
});
