(function (root, factory) {
  'use strict';
  const api = factory(root);
  if (typeof module === 'object' && module.exports) module.exports = api;
  else {
    root.XLoomGraph = api.XLoomGraph;
    root.XLoomGraphData = {
      mapState: api.mapState, resolveNodeKey: api.resolveNodeKey,
      formatNodeLabel: api.formatNodeLabel, createTextMeasurer: api.createTextMeasurer, LABEL_LAYOUT: api.LABEL_LAYOUT
    };
  }
}(typeof window === 'object' ? window : globalThis, function (root) {
  'use strict';

  const TYPES = ['goal', 'step', 'fact', 'finding'];
  const STATUS_LABELS = {
    open: '待执行', running: '运行中', completed: '已完成', failed: '失败', abandoned: '已放弃',
    achieved: '已达成', withdrawn: '已撤回', valid: '有效', input: '输入', superseded: '被取代',
    refuted: '被反驳', narrowed: '已收窄', candidate: '候选', verified: '已验证', unknown: '未标明状态'
  };
  const RELATION_LABELS = {supersedes: '取代', refutes: '反驳', narrows: '收窄'};
  const COLORS = {goal: '#668b73', step: '#ba9558', fact: '#6a90ac', finding: '#a080b0'};
  const BACKGROUNDS = {goal: '#f4f9f5', step: '#fffaf1', fact: '#f5f9fc', finding: '#faf6fc'};
  // Three measured lines fit inside the unchanged 220 x 80 card: one status
  // line and two content lines, 55.8px high with at least 10px vertical inset.
  const LABEL_LAYOUT = Object.freeze({
    width: 220, height: 80, fontSize: 12, lineHeight: 1.55,
    fontFamily: 'Inter, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif',
    insetX: 12, insetY: 10, maxLines: 3, measureSlack: 2
  });
  const LABEL_FONT = 'normal normal ' + LABEL_LAYOUT.fontSize + 'px ' + LABEL_LAYOUT.fontFamily;
  const array = value => Array.isArray(value) ? value : [];
  const strings = value => array(value).filter(item => typeof item === 'string' && item.length > 0);
  const clone = value => value === undefined ? undefined : JSON.parse(JSON.stringify(value));
  const text = value => typeof value === 'string' ? value : '';
  const keyOf = (type, id) => type + ':' + id;
  const shortText = (value, limit = 58) => {
    const chars = Array.from(text(value).replace(/\s+/g, ' ').trim());
    return chars.length > limit ? chars.slice(0, limit - 1).join('') + '…' : chars.join('');
  };

  const segmenter = typeof Intl === 'object' && typeof Intl.Segmenter === 'function'
    ? new Intl.Segmenter(undefined, {granularity: 'grapheme'}) : null;
  function graphemes(value) {
    if (segmenter) return Array.from(segmenter.segment(value), part => part.segment);
    // Keep common composed emoji and combining sequences intact on older
    // browsers too; Array.from alone would split ZWJ emoji and flag pairs.
    const parts = [];
    for (const char of Array.from(value)) {
      const last = parts[parts.length - 1];
      const continuation = /[\p{Mark}\p{Emoji_Modifier}\u200d\uFE0E\uFE0F\u{E0020}-\u{E007F}]/u.test(char);
      const flagPair = /^\p{Regional_Indicator}$/u.test(char) && /^\p{Regional_Indicator}$/u.test(last || '');
      if (last && (continuation || last.endsWith('\u200d') || flagPair)) parts[parts.length - 1] += char;
      else parts.push(char);
    }
    return parts;
  }

  function createTextMeasurer(context) {
    if (!context || typeof context.measureText !== 'function') throw new TypeError('A canvas text measurement context is required');
    const cache = new Map();
    return value => {
      if (cache.has(value)) return cache.get(value);
      context.font = LABEL_FONT;
      const metrics = context.measureText(value);
      // Include ink overhang, while retaining advance width for spaces/kerning.
      const ink = Number(metrics.actualBoundingBoxLeft) + Number(metrics.actualBoundingBoxRight);
      const width = Math.max(metrics.width, Number.isFinite(ink) ? ink : 0);
      if (!Number.isFinite(width) || width < 0) throw new TypeError('Canvas returned invalid text metrics');
      if (cache.size >= 4096) cache.clear();
      cache.set(value, width);
      return width;
    };
  }

  function wrapMeasuredText(value, measure, maxWidth, maxLines) {
    if (typeof measure !== 'function' || !(maxWidth > 0) || !Number.isInteger(maxLines) || maxLines < 1) {
      throw new TypeError('Text wrapping requires a pixel measurer and positive bounds');
    }
    const parts = graphemes(text(value).replace(/\s+/g, ' ').trim());
    const lines = [];
    const fits = value => measure(value) <= maxWidth;
    let start = 0;
    while (start < parts.length && lines.length < maxLines) {
      let end = start;
      let line = '';
      while (end < parts.length && fits(line + parts[end])) line += parts[end++];
      if (end === parts.length) { lines.push(line.trimEnd()); start = end; break; }
      // A single unusually wide grapheme cannot be cut in half. Represent the
      // omitted content with an ellipsis, provided that itself fits the card.
      if (lines.length === maxLines - 1 || end === start) {
        while (end > start && !fits(parts.slice(start, end).join('').trimEnd() + '…')) end--;
        if (fits('…')) lines.push(parts.slice(start, end).join('').trimEnd() + '…');
        return {lines, truncated: true};
      }
      // Prefer word boundaries when available; unspaced Chinese, UUIDs and
      // paths use measured grapheme boundaries instead of overflowing.
      let boundary = end;
      for (let index = end - 1; index > start; index--) {
        if (parts[index] === ' ') { boundary = index; break; }
      }
      lines.push(parts.slice(start, boundary).join('').trimEnd());
      start = boundary;
      while (parts[start] === ' ') start++;
    }
    return {lines, truncated: start < parts.length};
  }

  function supportWarning(node) {
    return (node.type === 'finding' || node.type === 'goal') && node.supportValid === false && (node.status === 'verified' || node.status === 'achieved');
  }

  function formatNodeLabel(node, measure) {
    const maxWidth = LABEL_LAYOUT.width - LABEL_LAYOUT.insetX * 2 - LABEL_LAYOUT.measureSlack;
    const header = node.type.toUpperCase() + ' · ' + (STATUS_LABELS[node.status] || node.status) + (supportWarning(node) ? ' · 支持失效' : '');
    const heading = wrapMeasuredText(header, measure, maxWidth, 1);
    const body = wrapMeasuredText(text(node.label).trim() || node.id, measure, maxWidth, LABEL_LAYOUT.maxLines - 1);
    const lines = [...heading.lines, ...body.lines];
    return {text: lines.join('\n'), lines, truncated: heading.truncated || body.truncated};
  }

  // State is the server's complete /projects/{id}/state response. No inferred facts
  // or evidence are added here: a missing endpoint is reported and its edge omitted.
  function mapState(input) {
    const state = input && typeof input === 'object' ? input : {};
    const nodes = [];
    const edges = [];
    const diagnostics = [];
    const byKey = new Map();
    const addNode = (type, raw, labelField) => {
      if (!raw || typeof raw !== 'object' || typeof raw.id !== 'string' || !raw.id) {
        diagnostics.push({kind: 'invalid_node', type});
        return;
      }
      const key = keyOf(type, raw.id);
      if (byKey.has(key)) {
        diagnostics.push({kind: 'duplicate_node', key});
        return;
      }
      const label = text(raw[labelField]);
      const node = {
        key, id: raw.id, type, label, description: label, status: text(raw.status) || 'unknown',
        evidence: clone(array(raw.evidence)), sources: [...strings(raw.sources)],
        supportValid: typeof raw.support_valid === 'boolean' ? raw.support_valid : null,
        raw: clone(raw)
      };
      nodes.push(node);
      byKey.set(key, node);
    };
    array(state.goals).forEach(raw => addNode('goal', raw, 'condition'));
    array(state.steps).forEach(raw => addNode('step', raw, 'description'));
    array(state.fact_records).forEach(raw => addNode('fact', raw, 'description'));
    array(state.findings).forEach(raw => addNode('finding', raw, 'claim'));
    const edgeKeys = new Set();
    const addEdge = (kind, source, target, label, raw, supportValid = null) => {
      const missing = [source, target].filter(key => !byKey.has(key));
      if (missing.length) {
        diagnostics.push({kind: 'missing_reference', relation: kind, source, target, missing});
        return;
      }
      const id = 'edge:' + JSON.stringify([kind, source, target]);
      if (edgeKeys.has(id)) return;
      edgeKeys.add(id);
      edges.push({id, kind, source, target, label, supportValid, raw: clone(raw)});
    };
    nodes.forEach(node => {
      const raw = node.raw;
      if (node.type === 'goal') {
        if (text(raw.parent_id)) addEdge('parent', keyOf('goal', raw.parent_id), node.key, '子目标', null);
        if (node.status === 'achieved') node.sources.forEach(id => addEdge('goal_support', keyOf('fact', id), node.key, '支持目标', null, node.supportValid));
      } else if (node.type === 'step') {
        if (text(raw.goal_id)) addEdge('goal_step', keyOf('goal', raw.goal_id), node.key, '执行步骤', null);
        strings(raw.from).forEach(id => addEdge('step_input', keyOf('fact', id), node.key, '依据', null));
        if (text(raw.result)) addEdge('step_result', node.key, keyOf('fact', raw.result), '产生', null);
      } else if (node.type === 'finding') {
        node.sources.forEach(id => addEdge('finding_support', keyOf('fact', id), node.key, '支持发现', null, node.supportValid));
      }
    });
    array(state.fact_relations).forEach(relation => {
      if (!relation || !text(relation.source) || !text(relation.target) || !text(relation.kind)) {
        diagnostics.push({kind: 'invalid_relation'});
        return;
      }
      // A correcting source refutes/narrows/supersedes its old target.
      addEdge(relation.kind, keyOf('fact', relation.source), keyOf('fact', relation.target), RELATION_LABELS[relation.kind] || relation.kind, relation);
    });
    nodes.sort((a, b) => a.key < b.key ? -1 : a.key > b.key ? 1 : 0);
    edges.sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
    const projectId = text(state.graph && state.graph.project && state.graph.project.id);
    return {projectId, nodes, edges, diagnostics};
  }

  function resolveNodeKey(nodes, selection) {
    if (selection === null || selection === undefined) return null;
    if (typeof selection === 'object') {
      if (!TYPES.includes(selection.type) || typeof selection.id !== 'string') return null;
      const key = keyOf(selection.type, selection.id);
      return nodes.some(node => node.key === key) ? key : null;
    }
    if (typeof selection !== 'string') return null;
    if (nodes.some(node => node.key === selection)) return selection;
    const matches = nodes.filter(node => node.id === selection);
    return matches.length === 1 ? matches[0].key : null;
  }

  function visualData(node, measure) {
    return {
      id: node.key, key: node.key, type: node.type, status: node.status,
      text: formatNodeLabel(node, measure).text,
      borderColor: COLORS[node.type], backgroundColor: BACKGROUNDS[node.type],
      supportWarning: supportWarning(node), invalid: ['failed', 'refuted', 'superseded', 'narrowed', 'withdrawn', 'abandoned'].includes(node.status)
    };
  }

  const GRAPH_STYLE = [
    {selector: 'node', style: {
      shape: 'roundrectangle', width: LABEL_LAYOUT.width, height: LABEL_LAYOUT.height, 'background-color': 'data(backgroundColor)',
      'border-width': 1.4, 'border-color': 'data(borderColor)', label: 'data(text)', color: '#34473b',
      'font-family': LABEL_LAYOUT.fontFamily, 'font-size': LABEL_LAYOUT.fontSize, 'font-style': 'normal', 'font-weight': 'normal',
      'text-wrap': 'wrap', 'text-max-width': LABEL_LAYOUT.width - LABEL_LAYOUT.insetX * 2, 'text-valign': 'center', 'text-halign': 'center',
      'line-height': LABEL_LAYOUT.lineHeight, 'overlay-opacity': 0, 'text-events': 'no'
    }},
    {selector: 'node[?invalid]', style: {'background-color': '#f7f6f4', 'border-color': '#b6aaa2', color: '#7a7069', 'border-style': 'dashed'}},
    {selector: 'node[?supportWarning]', style: {'border-color': '#c68b5b', 'border-style': 'dashed'}},
    {selector: 'node[status = "running"]', style: {'border-width': 2.2}},
    {selector: 'node.is-selected', style: {'border-width': 3, 'overlay-color': '#729481', 'overlay-opacity': .09, 'overlay-padding': 5}},
    {selector: 'edge', style: {
      width: 1.25, 'line-color': '#c0cfc5', 'target-arrow-color': '#a8bcae', 'target-arrow-shape': 'triangle',
      'arrow-scale': .7, 'curve-style': 'bezier', 'control-point-step-size': 45,
      label: 'data(label)', 'font-size': 9, color: '#809185', 'text-background-color': '#f7faf7',
      'text-background-opacity': .95, 'text-background-padding': 3, 'text-rotation': 'autorotate', 'overlay-opacity': 0
    }},
    {selector: 'edge[kind = "refutes"]', style: {'line-color': '#bc8c84', 'target-arrow-color': '#bc8c84', color: '#a1716a', 'line-style': 'dashed'}},
    {selector: 'edge[kind = "supersedes"],edge[kind = "narrows"]', style: {'line-color': '#b69d79', 'target-arrow-color': '#b69d79', color: '#a68a62', 'line-style': 'dashed'}},
    {selector: 'edge[?invalidSupport]', style: {'line-color': '#c19876', 'target-arrow-color': '#c19876', 'line-style': 'dashed'}},
    {selector: 'edge.is-related', style: {width: 2.2, 'line-color': '#749080', 'target-arrow-color': '#749080'}},
    {selector: '.is-dimmed', style: {opacity: .13}}
  ];

  let instanceCounter = 0;
  function installStyles(document) {
    if (document.getElementById('xloom-live-graph-styles')) return;
    const style = document.createElement('style');
    style.id = 'xloom-live-graph-styles';
    style.textContent = `
      .xl-live-graph { position:absolute; inset:0; overflow:hidden; }
      .xl-live-graph:focus-visible { outline:2px solid #90ad9a; outline-offset:-4px; }
      .xl-graph-empty { position:absolute; inset:0; display:grid; place-items:center; pointer-events:none; color:#89998d; font-size:13px; }
      .xl-graph-empty[hidden],.xl-graph-warning[hidden] { display:none; }
      .xl-graph-warning { position:absolute; top:12px; left:50%; transform:translateX(-50%); padding:6px 10px; border:1px solid #e3cfaf; border-radius:6px; background:#fff9ee; color:#9a794b; font-size:11px; pointer-events:none; }
      .xl-graph-node-picker,.xl-graph-screen-reader { position:absolute; width:1px; height:1px; padding:0; border:0; margin:-1px; overflow:hidden; clip-path:inset(50%); white-space:nowrap; }
      .xl-graph-node-picker:focus { z-index:4; bottom:16px; left:16px; width:min(360px,calc(100% - 32px)); height:36px; margin:0; padding:4px 8px; clip-path:none; border:1px solid #789984; border-radius:6px; background:#fff; color:#34473b; outline:2px solid #b8cfc0; font-size:12px; }
    `;
    document.head.appendChild(style);
  }

  class XLoomGraph {
    constructor(host, options = {}) {
      if (!host || !host.ownerDocument) throw new TypeError('XLoomGraph requires a host element');
      if (typeof root.cytoscape !== 'function') throw new Error('Local Cytoscape dependency is unavailable');
      this.host = host;
      this.document = host.ownerDocument;
      this.measureLabel = createTextMeasurer(this.document.createElement('canvas').getContext('2d'));
      this.onSelect = typeof options.onSelect === 'function' ? options.onSelect : function () {};
      this.nodes = [];
      this.edges = [];
      this.diagnostics = [];
      this.selectedKey = null;
      this.filter = 'all';
      this.projectId = null;
      this.snapshot = null;
      this.destroyed = false;
      this.pendingFit = false;
      installStyles(this.document);
      this.canvas = this.document.createElement('div');
      this.canvas.className = 'xl-live-graph';
      this.canvas.tabIndex = 0;
      this.canvas.setAttribute('role', 'group');
      this.canvas.setAttribute('aria-label', 'FGS 任务图。方向键浏览节点，Enter 选择，Escape 清除选择，加减号缩放，0 显示全图。');
      this.empty = this.document.createElement('div');
      this.empty.className = 'xl-graph-empty';
      this.empty.textContent = '暂无图节点';
      this.warning = this.document.createElement('div');
      this.warning.className = 'xl-graph-warning';
      this.warning.hidden = true;
      this.picker = this.document.createElement('select');
      this.picker.className = 'xl-graph-node-picker';
      this.picker.setAttribute('aria-label', '按类型和名称选择图节点');
      this.live = this.document.createElement('div');
      this.live.className = 'xl-graph-screen-reader';
      this.live.id = 'xloom-graph-live-' + (++instanceCounter);
      this.live.setAttribute('aria-live', 'polite');
      this.host.append(this.canvas, this.empty, this.warning, this.picker, this.live);
      this.cy = root.cytoscape({
        container: this.canvas, elements: [], style: GRAPH_STYLE, layout: {name: 'preset'},
        minZoom: .04, maxZoom: 2.6, boxSelectionEnabled: false,
        autounselectify: true, selectionType: 'single'
      });
      this.cy.on('tap', 'node', event => this.selectNode(event.target.id()));
      this.cy.on('tap', event => { if (event.target === this.cy) this.selectNode(null); });
      this.cy.on('zoom', () => this.emitZoom());
      this.picker.addEventListener('change', () => this.selectNode(this.picker.value || null));
      this.canvas.addEventListener('keydown', event => this.onKeyDown(event));
      if (typeof root.ResizeObserver === 'function') {
        this.resizeObserver = new root.ResizeObserver(() => this.refreshViewport());
        this.resizeObserver.observe(host);
      } else {
        this.resizeHandler = () => this.refreshViewport();
        root.addEventListener('resize', this.resizeHandler);
      }
      this.updatePicker();
    }

    setState(state) {
      if (this.destroyed) return;
      const mapped = mapState(state);
      const snapshot = JSON.stringify(mapped);
      if (snapshot === this.snapshot) { this.refreshViewport(); return; }
      const projectChanged = this.projectId !== mapped.projectId;
      if (projectChanged) this.pendingFit = false;
      const previousSelected = this.nodes.find(node => node.key === this.selectedKey);
      const previousPositions = new Map();
      if (!projectChanged) this.cy.nodes().forEach(node => previousPositions.set(node.id(), {...node.position()}));
      const oldZoom = this.cy.zoom();
      const oldPan = {...this.cy.pan()};
      this.projectId = mapped.projectId;
      this.snapshot = snapshot;
      this.nodes = mapped.nodes;
      this.edges = mapped.edges;
      this.diagnostics = mapped.diagnostics;
      const nextKeys = new Set([...this.nodes.map(node => node.key), ...this.edges.map(edge => edge.id)]);
      const newNodeKeys = this.nodes.filter(node => !previousPositions.has(node.key)).map(node => node.key);
      this.cy.startBatch();
      try {
        if (projectChanged) this.cy.elements().remove();
        else this.cy.elements().filter(element => !nextKeys.has(element.id())).remove();
        this.nodes.forEach(node => {
          const existing = this.cy.getElementById(node.key);
          const data = visualData(node, this.measureLabel);
          if (existing.length) existing.data(data);
          else this.cy.add({group: 'nodes', data});
        });
        this.edges.forEach(edge => {
          const data = {id: edge.id, source: edge.source, target: edge.target, kind: edge.kind, label: edge.label, invalidSupport: edge.supportValid === false};
          const existing = this.cy.getElementById(edge.id);
          if (existing.length) existing.data(data);
          else this.cy.add({group: 'edges', data});
        });
      } finally {
        this.cy.endBatch();
      }
      if (newNodeKeys.length) this.layoutNewNodes(previousPositions, newNodeKeys);
      if (projectChanged || previousPositions.size === 0) this.fit();
      else { this.cy.zoom(oldZoom); this.cy.pan(oldPan); }
      if (projectChanged || !this.nodes.some(node => node.key === this.selectedKey)) this.selectedKey = null;
      this.empty.hidden = this.nodes.length > 0;
      this.warning.hidden = this.diagnostics.length === 0;
      this.warning.textContent = this.diagnostics.length ? this.diagnostics.length + ' 条图数据引用需要检查' : '';
      this.updatePicker();
      this.updateAppearance();
      const selected = this.nodes.find(node => node.key === this.selectedKey);
      if (previousSelected && (!selected || JSON.stringify(previousSelected) !== JSON.stringify(selected))) this.onSelect(selected ? clone(selected) : null);
    }

    layoutNewNodes(previousPositions, newKeys) {
      // Dagre suggests positions for additions. Existing positions and the user's
      // viewport are restored so background polling never moves familiar nodes.
      try {
        this.cy.layout({name: 'dagre', rankDir: 'TB', nodeSep: 42, rankSep: 68, edgeSep: 16, fit: false, animate: false, nodeDimensionsIncludeLabels: false}).run();
      } catch (error) {
        this.cy.layout({name: 'breadthfirst', directed: true, spacingFactor: 1.4, fit: false, animate: false}).run();
      }
      if (!previousPositions.size) return;
      const retained = [...previousPositions].filter(([key]) => this.cy.getElementById(key).length);
      const anchor = retained[0];
      const anchorPosition = anchor ? this.cy.getElementById(anchor[0]).position() : {x: 0, y: 0};
      const delta = anchor ? {x: anchor[1].x - anchorPosition.x, y: anchor[1].y - anchorPosition.y} : {x: 0, y: 0};
      const occupied = retained.map(([, position]) => position);
      const additions = newKeys.map(key => {
        const proposed = this.cy.getElementById(key).position();
        const position = {x: proposed.x + delta.x, y: proposed.y + delta.y};
        while (occupied.some(other => Math.abs(other.x - position.x) < 252 && Math.abs(other.y - position.y) < 108)) position.x += 262;
        occupied.push(position);
        return [key, position];
      });
      this.cy.batch(() => {
        retained.forEach(([key, position]) => this.cy.getElementById(key).position(position));
        additions.forEach(([key, position]) => this.cy.getElementById(key).position(position));
      });
    }

    updatePicker() {
      const active = this.document.activeElement === this.picker;
      this.picker.replaceChildren();
      const empty = this.document.createElement('option');
      empty.value = '';
      empty.textContent = this.nodes.length ? '选择节点 · ' + this.nodes.length + ' 个' : '暂无图节点';
      this.picker.appendChild(empty);
      this.nodes.filter(node => this.filter === 'all' || node.type === this.filter).forEach(node => {
        const option = this.document.createElement('option');
        option.value = node.key;
        option.textContent = node.type.toUpperCase() + ' · ' + node.id + ' · ' + shortText(node.label, 80);
        this.picker.appendChild(option);
      });
      this.picker.value = this.selectedKey || '';
      if (active) this.picker.focus({preventScroll: true});
    }

    updateAppearance() {
      this.cy.batch(() => {
        this.cy.nodes().forEach(element => {
          element.toggleClass('is-selected', element.id() === this.selectedKey);
          element.toggleClass('is-dimmed', this.filter !== 'all' && element.data('type') !== this.filter);
        });
        this.cy.edges().forEach(edge => {
          edge.toggleClass('is-related', edge.source().id() === this.selectedKey || edge.target().id() === this.selectedKey);
          edge.toggleClass('is-dimmed', this.filter !== 'all' && edge.source().data('type') !== this.filter && edge.target().data('type') !== this.filter);
        });
      });
    }

    selectNode(selection) {
      if (this.destroyed) return;
      const key = resolveNodeKey(this.nodes, selection);
      if (selection !== null && selection !== undefined && !key) return;
      this.selectedKey = key;
      this.updateAppearance();
      this.picker.value = key || '';
      const node = this.nodes.find(item => item.key === key);
      if (node) {
        const element = this.cy.getElementById(key);
        // A Finding evidence button can unhide the graph and select in the same
        // event, before ResizeObserver has refreshed Cytoscape's cached size.
        this.refreshViewport();
        if (this.hasViewport()) {
          const bounds = element.renderedBoundingBox();
          if (bounds.x1 < 12 || bounds.y1 < 12 || bounds.x2 > this.host.clientWidth - 12 || bounds.y2 > this.host.clientHeight - 12) this.cy.center(element);
        }
        this.live.textContent = node.type.toUpperCase() + '，' + node.label + '，' + (STATUS_LABELS[node.status] || node.status);
      } else this.live.textContent = '已清除节点选择';
      this.onSelect(node ? clone(node) : null);
    }

    onKeyDown(event) {
      if (['ArrowDown', 'ArrowRight', 'ArrowUp', 'ArrowLeft'].includes(event.key)) {
        event.preventDefault();
        const visible = this.nodes.filter(node => this.filter === 'all' || node.type === this.filter);
        if (!visible.length) return;
        const index = visible.findIndex(node => node.key === this.selectedKey);
        const direction = event.key === 'ArrowUp' || event.key === 'ArrowLeft' ? -1 : 1;
        const next = index < 0 ? (direction > 0 ? 0 : visible.length - 1) : (index + direction + visible.length) % visible.length;
        this.selectNode(visible[next].key);
      } else if (event.key === 'Escape') {
        event.preventDefault();
        this.selectNode(null);
      } else if (event.key === 'Enter' && this.selectedKey) {
        event.preventDefault();
        this.selectNode(this.selectedKey);
      } else if (event.key === '+' || event.key === '=') {
        event.preventDefault(); this.zoomBy(1.2);
      } else if (event.key === '-') {
        event.preventDefault(); this.zoomBy(1 / 1.2);
      } else if (event.key === '0') {
        event.preventDefault(); this.fit();
      }
    }

    setFilter(type) {
      this.filter = TYPES.includes(type) ? type : 'all';
      this.updatePicker();
      this.updateAppearance();
    }

    hasViewport() { return this.host.clientWidth > 0 && this.host.clientHeight > 0; }

    refreshViewport() {
      if (this.destroyed || !this.hasViewport()) return;
      this.cy.resize();
      if (this.pendingFit) this.fit();
    }

    fit() {
      if (this.destroyed || !this.cy.nodes().length) return;
      if (!this.hasViewport()) { this.pendingFit = true; return; }
      this.pendingFit = false;
      this.cy.resize();
      this.cy.fit(this.cy.elements(), 36);
      if (this.cy.zoom() > 1.1) { this.cy.zoom(1.1); this.cy.center(); }
      this.emitZoom();
    }

    zoomBy(factor) {
      if (this.destroyed || !Number.isFinite(factor) || factor <= 0) return;
      this.cy.zoom({level: Math.max(.04, Math.min(2.6, this.cy.zoom() * factor)), renderedPosition: {x: this.host.clientWidth / 2, y: this.host.clientHeight / 2}});
    }

    emitZoom() {
      this.host.dispatchEvent(new root.CustomEvent('graphzoom', {detail: {zoom: this.cy.zoom()}}));
    }

    getNodes() { return clone(this.nodes); }

    destroy() {
      if (this.destroyed) return;
      this.destroyed = true;
      if (this.resizeObserver) this.resizeObserver.disconnect();
      if (this.resizeHandler) root.removeEventListener('resize', this.resizeHandler);
      this.cy.destroy();
      [this.canvas, this.empty, this.warning, this.picker, this.live].forEach(element => element.remove());
    }
  }

  return {XLoomGraph, mapState, resolveNodeKey, LABEL_LAYOUT, createTextMeasurer, wrapMeasuredText, formatNodeLabel};
}));
