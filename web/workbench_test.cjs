const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const {Client, RequestScope} = require('./static/api.js');
const data = require('./static/data.js');
const {mapState} = require('./static/graph-data.js');
const html = fs.readFileSync(path.join(__dirname, 'static/index.html'), 'utf8');
const app = fs.readFileSync(path.join(__dirname, 'static/app.js'), 'utf8');
const now = '2026-09-24T00:00:00Z';
const project = (id, generation = 0) => ({id,title:id,status:'active',generation,created_at:now,scenario:'ctf'});
const snapshot = (id, generation = 0, revision = 1) => ({graph:{project:project(id,generation),facts:[],intents:[],hints:[]},goals:[],steps:[],fact_records:[],findings:[],revision});
const deferred = () => { let resolve; const promise = new Promise(done => { resolve = done; }); return {promise,resolve}; };
const settle = async () => { for (let i = 0; i < 4; i++) await new Promise(resolve => setImmediate(resolve)); };

// A small DOM adapter exercises the actual app orchestration without a browser
// dependency. Layout and pointer interaction are verified in browser tests.
class Element {
  constructor(tag = 'div') { this.tagName = tag; this.children = []; this.listeners = new Map(); this.dataset = {}; this.style = {}; this.attributes = {}; this.value = ''; this.text = ''; this.className = ''; this.scrollTop = 0; }
  get classList() {
    return {add: name => { this.className += ' ' + name; }, remove: name => { this.className = this.className.split(' ').filter(value => value !== name).join(' '); }, toggle: name => { const found = this.className.split(' ').includes(name); found ? this.classList.remove(name) : this.classList.add(name); return !found; }};
  }
  set textContent(value) { this.text = String(value); this.children = []; }
  get textContent() { return this.text + this.children.map(child => typeof child === 'string' ? child : child.textContent).join(''); }
  append(...items) { for (const item of items) { if (typeof item === 'object') item.parentElement = this; this.children.push(item); } }
  replaceChildren(...items) { this.children = []; this.text = ''; this.append(...items); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  removeAttribute(name) { delete this.attributes[name]; }
  addEventListener(name, listener) { if (!this.listeners.has(name)) this.listeners.set(name, []); this.listeners.get(name).push(listener); }
  async emit(name, extra = {}) { for (const listener of this.listeners.get(name) || []) await listener({target:this,currentTarget:this,preventDefault(){},...extra}); }
  click() { return this.disabled ? Promise.resolve() : this.emit('click'); }
  showModal() { this.open = true; }
  close() { this.open = false; this.emit('close'); }
  focus() {}
  reset() {}
  closest() { return null; }
}

function harness(handler, selected = 'A') {
  const elements = new Map([...html.matchAll(/id="([^"]+)"/g)].map(match => [match[1], new Element()]));
  const tabs = ['board','system','result'].map(name => { const element = elements.get('tab-' + name); element.dataset.tab = name; return element; });
  const dialogs = ['create','hint','confirm','about'].map(name => elements.get(name + '-dialog'));
  const timers = new Map(), calls = [], projections = [], displayed = []; let timerID = 0, graph;
  const document = {
    hidden:false, getElementById:id => elements.get(id), createElement:tag => new Element(tag), createElementNS:(_,tag) => new Element(tag), createTextNode:text => new Element('#text'),
    querySelectorAll:selector => selector === '[data-tab]' ? tabs : selector === 'dialog' ? dialogs : [], querySelector:() => null, addEventListener() {}
  };
  document.createTextNode = text => { const node = new Element('#text'); node.textContent = text; return node; };
  class FakeClient extends Client {
    async request(url, options = {}) { calls.push({url,options}); return handler(url,options); }
  }
  class FakeGraph {
    constructor(_,options) { this.state = null; this.options = options; this.selected = null; graph = this; }
    setState(value) { this.state = value; displayed.push(value); if (this.selected) { const current = this.getNodes().find(node => node.key === this.selected); this.options.onSelect(current || null); } }
    getNodes() { return mapState(this.state).nodes; }
    getVisibleNodeCount() { return this.getNodes().length; }
    selectNode(ref) { const node = ref && this.getNodes().find(node => node.key === (ref.type + ':' + ref.id)); this.selected = node?.key; this.options.onSelect(node || null); }
    getEdgeDetails() { return null; }
    destroy() {}
  }
  const window = {XLoomAPI:{Client:FakeClient,RequestScope},XLoomGraph:FakeGraph,XLoomData:{...data,buildLogs(state,events,runs) { projections.push({state,events,runs}); return data.buildLogs(state,events,runs); }},addEventListener() {}};
  const context = {window,document,location:{search:'?project=' + selected},localStorage:{getItem(){},setItem(){},removeItem(){}},URLSearchParams,AbortController,innerWidth:1400,innerHeight:900,
    setTimeout(fn,ms) { timers.set(++timerID,{fn,ms}); return timerID; }, clearTimeout(id) { timers.delete(id); }, FormData:class { get() { return 'ctf'; } }, console};
  vm.runInNewContext(app,context,{filename:'app.js'});
  return {elements,calls,projections,displayed,timers,graph,fireTimer:async ms => { const entry = [...timers].find(([,value]) => value.ms === ms); assert.ok(entry, 'scheduled timer ' + ms); timers.delete(entry[0]); await entry[1].fn(); await settle(); },
    async select(id) { const row = elements.get('project-list').children.find(item => item.dataset?.projectId === id); assert.ok(row, 'project row ' + id); const pending = row.children[0].click(); await settle(); return pending; }};
}

function standard(url, states, extra = () => undefined) {
  const custom = extra(url); if (custom !== undefined) return custom;
  if (url === '/projects') return Object.values(states).map(state => state.graph.project);
  if (url === '/ui/overview') return {active_workers:99,observed_at:now};
  const match = url.match(/^\/projects\/([^/?]+)(.*)$/); if (!match) throw new Error(url);
  const state = states[match[1]], suffix = match[2];
  if (suffix === '/state') return state;
  if (suffix.startsWith('/executions')) return {items:[],through:0};
  if (suffix.startsWith('/state/events')) return [];
  if (!suffix) return {project:state.graph.project};
  throw new Error('unhandled ' + url);
}

test('workbench and graph icons resolve to embedded symbols', () => {
  const symbols = new Set([...html.matchAll(/<symbol id="i-([^"]+)"/g)].map(match => match[1]));
  const graphSource = fs.readFileSync(path.join(__dirname, 'static/graph.js'), 'utf8');
  const explicit = [...(app + graphSource).matchAll(/(?:this\.)?icon\('([^']+)'\)/g)].map(match => match[1]);
  const markup = [...html.matchAll(/<use href="#i-([^"]+)"/g)].map(match => match[1]);
  for (const icon of [...explicit,...markup,'shield','code','flag','graph','pause','play','restart','stop','trash','node','file','check']) assert.ok(symbols.has(icon), 'missing icon ' + icon);
});

test('late A state cannot replace a completed B selection even if transport ignores abort', async () => {
  const pending = deferred(), states = {A:snapshot('A'),B:snapshot('B')}; let aSignal;
  const h = harness((url,options) => standard(url, states, value => { if (value === '/projects/A/state') { aSignal = options.signal; return pending.promise; } }));
  await settle(); await h.select('B'); assert.equal(aSignal.aborted,true);
  assert.equal(h.elements.get('project-title').textContent,'B'); pending.resolve(states.A); await settle();
  assert.equal(h.elements.get('project-title').textContent,'B'); assert.equal(h.displayed.filter(Boolean).at(-1).graph.project.id,'B');
});

test('generation conflict discards mixed state and immediately reloads the current round', async () => {
  const states = {A:snapshot('A')}; let stateReads = 0;
  const h = harness(url => standard(url,states,value => {
    if (value === '/projects/A/state') return snapshot('A', stateReads++ ? 1 : 0);
    if (value === '/projects/A') return {project:project('A',1)};
  }));
  await settle(); assert.equal(h.displayed.filter(Boolean).length,0); await h.fireTimer(0);
  assert.equal(h.displayed.filter(Boolean).length,1); assert.equal(h.displayed.filter(Boolean)[0].graph.project.generation,1);
  assert.match(h.elements.get('round-label').textContent,/2/);
});

test('event snapshot ceiling preserves future events and rollback resets only accepted state revisions', async () => {
  const states = {A:snapshot('A',0,1)}; let delivered = false;
  const h = harness(url => standard(url,states,value => {
    if (value.startsWith('/projects/A/state/events')) {
      if (delivered) return []; delivered = true;
      return [1,2].map(revision => ({revision,type:'project.updated',created_at:now}));
    }
  }));
  await settle(); assert.deepEqual(Array.from(h.projections.at(-1).events, row => row.revision),[1]);
  await h.fireTimer(2500);
  assert.equal(h.calls.filter(call => call.url === '/projects/A/state/events?after=2').length,1,'future events stay cached while state lags');
  states.A = snapshot('A',0,2); await h.fireTimer(2500);
  assert.deepEqual(Array.from(h.projections.at(-1).events, row => row.revision),[1,2]);
  states.A = snapshot('A',0,1); await h.fireTimer(2500);
  assert.equal(h.calls.filter(call => call.url === '/projects/A/state/events?after=0').length,2,'actual rollback resets cursor');
});

test('current round execution filtering and list counts do not use worker capacity or search matches', async () => {
  const states = {A:snapshot('A',2),B:snapshot('B')}; states.B.graph.project.status = 'stopped';
  const h = harness(url => standard(url,states,value => value.startsWith('/projects/A/executions') ? {through:2,items:[{id:'old',generation:1},{id:'new',generation:2}]} : undefined));
  await settle(); assert.deepEqual(Array.from(h.projections.at(-1).runs,run => run.id),['new']);
  h.elements.get('project-search').value = 'no match'; await h.elements.get('project-search').emit('input');
  assert.equal(h.elements.get('project-count').textContent,'2'); assert.equal(h.elements.get('running-project-count').textContent,'1');
});

test('hint draft survives a poll, failed write, closing and reopening the dialog', async () => {
  const states = {A:snapshot('A')}; let submissions = 0; const gate = deferred();
  const h = harness((url,options) => {
    if (url === '/projects/A/hints' && options.method === 'POST') { submissions++; return gate.promise; }
    return standard(url,states);
  });
  await settle(); await h.elements.get('add-hint').click(); const input = h.elements.get('hint-input'); input.value = '保留我的提示草稿'; await input.emit('input');
  await h.fireTimer(2500); assert.equal(input.value,'保留我的提示草稿');
  // Resolve as a rejected API call after a duplicate submit has been attempted.
  const first = h.elements.get('hint-form').emit('submit'); await settle(); await h.elements.get('hint-form').emit('submit'); assert.equal(submissions,1);
  gate.resolve(Promise.reject(Object.assign(new Error('temporarily unavailable'),{status:503}))); await first; await settle();
  assert.equal(input.value,'保留我的提示草稿'); assert.match(h.elements.get('hint-error').textContent,/temporarily unavailable/);
  h.elements.get('hint-dialog').close(); await h.elements.get('add-hint').click(); assert.equal(input.value,'保留我的提示草稿');
});

test('network failure preserves the last valid graph and recovers on the next poll', async () => {
  const states = {A:snapshot('A')}; let unavailable = false;
  const h = harness(url => { if (unavailable && url === '/projects/A/state') throw new Error('offline'); return standard(url,states); });
  await settle(); const old = h.displayed.filter(Boolean).at(-1); unavailable = true; await h.fireTimer(2500);
  assert.equal(h.displayed.filter(Boolean).at(-1),old); assert.match(h.elements.get('workspace-error').textContent,/offline/); assert.equal(h.elements.get('connection-state').dataset.status,'error');
  unavailable = false; await h.fireTimer(2500); assert.equal(h.elements.get('connection-state').dataset.status,'connected');
});

test('refreshing the selected node never switches the user away from the result panel', async () => {
  const states = {A:snapshot('A')}; states.A.fact_records = [{id:'f',description:'original',status:'valid'}];
  const h = harness(url => standard(url,states)); await settle(); h.graph.selectNode({type:'fact',id:'f'});
  await h.elements.get('tab-result').click(); assert.equal(h.elements.get('tab-result').attributes['aria-selected'],'true');
  states.A.fact_records[0].description = 'updated'; await h.fireTimer(2500);
  assert.equal(h.elements.get('tab-result').attributes['aria-selected'],'true'); assert.match(h.elements.get('node-inspector').textContent,/updated/);
});

test('restart conflict refreshes the round and requires a new confirmation before any retry', async () => {
  const states = {A:snapshot('A')}; let restarts = 0;
  const h = harness((url,options) => {
    if (url === '/projects/A/restart') {
      restarts++; states.A = snapshot('A',restarts);
      if (restarts === 1) throw Object.assign(new Error('generation conflict'),{status:409});
      return states.A.graph;
    }
    return standard(url,states);
  });
  const openRestart = async () => {
    const row = h.elements.get('project-list').children.find(item => item.dataset?.projectId === 'A');
    const item = row.children[1].children[1].children.find(button => button.dataset.action === 'restart'); await item.click();
  };
  await settle(); await openRestart(); await h.elements.get('confirm-form').emit('submit');
  assert.equal(restarts,1); assert.equal(h.calls.find(call => call.url.endsWith('/restart')).options.body.expected_generation,0);
  assert.equal(h.elements.get('confirm-action').disabled,true); assert.match(h.elements.get('round-label').textContent,/2/);
  await h.elements.get('confirm-form').emit('submit'); assert.equal(restarts,1,'stale confirmation cannot replay the write');
  h.elements.get('confirm-dialog').close(); await openRestart(); await h.elements.get('confirm-form').emit('submit');
  assert.equal(restarts,2); assert.equal(h.calls.filter(call => call.url.endsWith('/restart'))[1].options.body.expected_generation,1);
});
