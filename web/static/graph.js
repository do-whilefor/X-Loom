(function (root, factory) {
  'use strict';
  const api = factory(root);
  if (typeof module === 'object' && module.exports) module.exports = api;
  else {
    root.XLoomGraph = api.XLoomGraph;
    root.XLoomGraphData = {
      mapState: api.mapState, resolveNodeKey: api.resolveNodeKey,
      formatNodeLabel: api.formatNodeLabel, createTextMeasurer: api.createTextMeasurer, LABEL_LAYOUT: api.LABEL_LAYOUT,
      buildFlowLayout: api.buildFlowLayout, nodePresentation: api.nodePresentation
    };
  }
}(typeof window === 'object' ? window : globalThis, function (root) {
  'use strict';

  const TYPES = ['goal', 'step', 'fact', 'finding'];
  const TYPE_LABELS = {goal: '目标', step: '任务', fact: '事实', finding: '发现'};
  const STATUS_LABELS = {
    open: '待执行', running: '运行中', completed: '已完成', failed: '失败', abandoned: '已放弃',
    achieved: '已达成', withdrawn: '已撤回', valid: '有效', input: '输入', superseded: '被取代',
    refuted: '被反驳', narrowed: '已收窄', candidate: '待验证', verified: '已验证', paused: '已暂停', unknown: '未标明状态'
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
    const header = (TYPE_LABELS[node.type] || '节点') + ' · ' + (STATUS_LABELS[node.status] || node.status) + (supportWarning(node) ? ' · 支持失效' : '');
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
        if (text(raw.goal_id)) addEdge('goal_step', keyOf('goal', raw.goal_id), node.key, '任务归属', null);
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
    const generation = Number(state.graph && state.graph.project && state.graph.project.generation) || 0;
    return {projectId, generation, nodes, edges, diagnostics};
  }

  // Presentation roles never replace the typed IDs or records sent to the log.
  // The stored origin fact is the sole start; the actual root goal is the end.
  function nodePresentation(node, endKey) {
    const role = node.key === 'fact:origin' ? 'start' : node.key === endKey ? 'goal' : node.type === 'step' ? 'task' : node.type === 'goal' ? 'subgoal' : node.type;
    return {
      role,
      label: role === 'start' ? '起点' : role === 'goal' ? '终点目标' : role === 'subgoal' ? '子目标' : node.key === 'fact:goal' ? '目标输入' : TYPE_LABELS[node.type] || '节点',
      width: role === 'start' ? 210 : role === 'goal' ? 290 : 220,
      height: role === 'start' ? 60 : 80
    };
  }

  const FLOW_KINDS = new Set(['step_input', 'step_result', 'finding_support', 'goal_support']);
  const FLOW_COLUMNS = 4;
  function buildFlowLayout(mapped) {
    const records = array(mapped && mapped.nodes);
    const recordKeys = new Set(records.map(node => node.key));
    const roots = records.filter(node => node.type === 'goal' && !text(node.raw && node.raw.parent_id)).sort((a, b) => a.key.localeCompare(b.key));
    const endKey = recordKeys.has('goal:goal') ? 'goal:goal' : roots.length ? roots[0].key : recordKeys.has('fact:goal') ? 'fact:goal' : null;
    // The goal input fact and the root Goal describe the same project endpoint.
    // Merge only their visual card, retaining both complete typed records in
    // mapState/getNodes and retaining each edge's original typed endpoints.
    const aliases = {};
    if (endKey && endKey !== 'fact:goal' && recordKeys.has('fact:goal')) aliases['fact:goal'] = endKey;
    const canonicalKey = key => aliases[key] || key;
    const nodes = records.filter(node => !aliases[node.key]);
    const edges = array(mapped && mapped.edges).map(edge => ({
      ...edge, source: canonicalKey(edge.source), target: canonicalKey(edge.target), recordSource: edge.source, recordTarget: edge.target
    })).sort((a, b) => a.id.localeCompare(b.id));
    const byKey = new Map(nodes.map(node => [node.key, node]));
    const startKey = byKey.has('fact:origin') ? 'fact:origin' : null;
    if (!nodes.length) return {nodes: [], edges, aliases, endKey, startKey};
    const keys = nodes.map(node => node.key).sort();
    const forward = new Map(keys.map(key => [key, []]));
    const backward = new Map(keys.map(key => [key, []]));
    // Corrective relationships and goal ownership are not execution order.
    // Parent goals sit below their children, but the displayed edge retains its
    // original parent-to-child direction and is styled as structural metadata.
    for (const edge of edges) {
      let source = edge.source, target = edge.target;
      if (edge.kind === 'parent') [source, target] = [target, source];
      else if (!FLOW_KINDS.has(edge.kind)) continue;
      if (!byKey.has(source) || !byKey.has(target) || source === endKey || target === startKey) continue;
      forward.get(source).push(target);
      backward.get(target).push(source);
    }
    for (const neighbors of [...forward.values(), ...backward.values()]) neighbors.sort();

    // Iterative strongly connected components avoid recursion overflow on long
    // sessions. Cycles remain together; we never invent an order inside them.
    const visited = new Set(), order = [];
    for (const key of keys) {
      if (visited.has(key)) continue;
      visited.add(key);
      const stack = [[key, 0]];
      while (stack.length) {
        const frame = stack[stack.length - 1];
        const neighbors = forward.get(frame[0]);
        if (frame[1] < neighbors.length) {
          const next = neighbors[frame[1]++];
          if (!visited.has(next)) { visited.add(next); stack.push([next, 0]); }
        } else { order.push(frame[0]); stack.pop(); }
      }
    }
    const componentOf = new Map(), components = [];
    for (const key of order.reverse()) {
      if (componentOf.has(key)) continue;
      const index = components.length, members = [], stack = [key];
      componentOf.set(key, index);
      while (stack.length) {
        const current = stack.pop();
        members.push(current);
        for (const previous of backward.get(current)) {
          if (!componentOf.has(previous)) { componentOf.set(previous, index); stack.push(previous); }
        }
      }
      components.push(members.sort());
    }
    const successors = components.map(() => new Set());
    const indegree = components.map(() => 0);
    for (const [source, targets] of forward) for (const target of targets) {
      const a = componentOf.get(source), b = componentOf.get(target);
      if (a !== b && !successors[a].has(b)) { successors[a].add(b); indegree[b]++; }
    }
    const rank = components.map(() => startKey ? 1 : 0);
    if (startKey) rank[componentOf.get(startKey)] = 0;
    const queue = indegree.map((count, index) => count === 0 ? index : -1).filter(index => index >= 0);
    for (let cursor = 0; cursor < queue.length; cursor++) {
      const index = queue[cursor];
      for (const next of successors[index]) {
        rank[next] = Math.max(rank[next], rank[index] + 1);
        if (--indegree[next] === 0) queue.push(next);
      }
    }
    if (endKey) rank[componentOf.get(endKey)] = rank.reduce((highest, value) => Math.max(highest, value), 0) + 1;
    const rows = new Map();
    for (const key of keys) {
      const level = rank[componentOf.get(key)];
      if (!rows.has(level)) rows.set(level, []);
      rows.get(level).push(key);
    }
    const positions = new Map();
    const entries = [];
    let row = 0;
    for (const [layer, rowKeys] of [...rows].sort(([a], [b]) => a - b)) {
      // Average predecessor position keeps branches aligned without using
      // corrective edges to falsely suggest a chronology.
      const center = key => {
        const known = backward.get(key).filter(previous => positions.has(previous));
        return known.length ? known.reduce((sum, previous) => sum + positions.get(previous).x, 0) / known.length : 0;
      };
      rowKeys.sort((a, b) => center(a) - center(b) || a.localeCompare(b));
      // A dependency layer may contain hundreds of independent branches. Wrap
      // its cards before moving to the next layer so the canvas width stays
      // usable. Rows are presentation only: no edges or dependency ranks are
      // added between wrapped peers, including members of the same cycle.
      for (let start = 0; start < rowKeys.length; start += FLOW_COLUMNS) {
        const visibleRow = rowKeys.slice(start, start + FLOW_COLUMNS);
        const widths = visibleRow.map(key => nodePresentation(byKey.get(key), endKey).width);
        const totalWidth = widths.reduce((sum, width) => sum + width, 0) + (visibleRow.length - 1) * 42;
        let x = -totalWidth / 2;
        for (let index = 0; index < visibleRow.length; index++) {
          const key = visibleRow[index], node = byKey.get(key), presentation = nodePresentation(node, endKey);
          const position = {x: x + widths[index] / 2, y: row * 138};
          positions.set(key, position);
          entries.push({key, node, ...presentation, position, layer});
          x += widths[index] + 42;
        }
        row++;
      }
    }
    return {nodes: entries, edges, aliases, endKey, startKey};
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

  function visualData(node, measure, presentation) {
    return {
      id: node.key, key: node.key, type: node.type, status: node.status,
      text: formatNodeLabel(node, measure).text,
      width: presentation ? presentation.width : LABEL_LAYOUT.width, height: presentation ? presentation.height : LABEL_LAYOUT.height,
      borderColor: COLORS[node.type], backgroundColor: BACKGROUNDS[node.type],
      supportWarning: supportWarning(node), invalid: ['failed', 'refuted', 'superseded', 'narrowed', 'withdrawn', 'abandoned'].includes(node.status)
    };
  }

  const GRAPH_STYLE = [
    {selector: 'node', style: {
      shape: 'roundrectangle', width: 'data(width)', height: 'data(height)', 'background-color': 'data(backgroundColor)',
      'border-width': 1.4, 'border-color': 'data(borderColor)', label: 'data(text)', color: '#34473b',
      'font-family': LABEL_LAYOUT.fontFamily, 'font-size': LABEL_LAYOUT.fontSize, 'font-style': 'normal', 'font-weight': 'normal',
      'text-wrap': 'wrap', 'text-max-width': LABEL_LAYOUT.width - LABEL_LAYOUT.insetX * 2, 'text-valign': 'center', 'text-halign': 'center',
      'line-height': LABEL_LAYOUT.lineHeight, 'overlay-opacity': 0, 'text-events': 'no',
      'background-opacity': 0, 'border-opacity': 0, 'text-opacity': 0
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
    {selector: 'edge[kind = "goal_step"],edge[kind = "parent"]', style: {
      'line-color': '#cbd2c6', 'target-arrow-color': '#bcc8b4', color: '#94a08b', 'line-style': 'dotted',
      'curve-style': 'unbundled-bezier', 'control-point-distances': 95, 'control-point-weights': .5
    }},
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
      .xl-flow-cards { position:absolute; inset:0; pointer-events:none; overflow:hidden; }
      .xl-flow-world { position:absolute; inset:0; transform-origin:0 0; }
      .xl-flow-world .flow-card { box-sizing:border-box; pointer-events:auto; margin:0; font-family:inherit; }
      .xl-flow-world .flow-card-title { -webkit-line-clamp:2; max-height:36px; }
      .xl-flow-world .type-start .flow-card-title { -webkit-line-clamp:1; max-height:18px; }
      .xl-flow-world .flow-status { max-width:115px; overflow:hidden; text-overflow:ellipsis; }
      .xl-flow-world .flow-card.is-dimmed { opacity:.16; }
      .xl-flow-world .flow-card.is-invalid { border-style:dashed; background:#f7f6f4; }
      .xl-flow-world .flow-card.has-support-warning { border-color:#c68b5b; border-style:dashed; }
      .xl-flow-world .flow-card.is-subgoal { background:#f7faf3; border-color:#d9e3d0; }
      .xl-flow-world .flow-card:focus-visible { outline:2px solid #7da184; outline-offset:3px; }
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
      this.generation = null;
      this.topology = null;
      this.cards = new Map();
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
      this.cardLayer = this.document.createElement('div');
      this.cardLayer.className = 'xl-flow-cards';
      this.cardWorld = this.document.createElement('div');
      this.cardWorld.className = 'xl-flow-world';
      this.cardLayer.appendChild(this.cardWorld);
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
      this.host.append(this.canvas, this.cardLayer, this.empty, this.warning, this.picker, this.live);
      this.cy = root.cytoscape({
        container: this.canvas, elements: [], style: GRAPH_STYLE, layout: {name: 'preset'},
        minZoom: .04, maxZoom: 2.6, boxSelectionEnabled: false,
        autounselectify: true, selectionType: 'single'
      });
      this.cy.on('tap', 'node', event => this.selectNode(event.target.id()));
      this.cy.on('tap', event => { if (event.target === this.cy) this.selectNode(null); });
      this.cy.on('zoom', () => this.emitZoom());
      this.cy.on('pan zoom render position', () => this.updateCardPositions());
      this.picker.addEventListener('change', () => this.selectNode(this.picker.value || null));
      this.canvas.addEventListener('keydown', event => this.onKeyDown(event));
      this.cardLayer.addEventListener('keydown', event => {
        if (event.key !== 'Enter' && event.key !== ' ') this.onKeyDown(event);
      });
      this.cardLayer.addEventListener('wheel', event => {
        event.preventDefault();
        const bounds = this.host.getBoundingClientRect();
        const factor = Math.exp(-Math.max(-150, Math.min(150, event.deltaY)) * .003);
        this.cy.zoom({level: Math.max(.04, Math.min(2.6, this.cy.zoom() * factor)), renderedPosition: {x: event.clientX - bounds.left, y: event.clientY - bounds.top}});
      }, {passive: false});
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
      const projectChanged = this.projectId !== mapped.projectId || this.generation !== mapped.generation;
      if (projectChanged) this.pendingFit = false;
      const previousSelected = this.nodes.find(node => node.key === this.selectedKey);
      const hadNodes = this.nodes.length > 0;
      const topology = JSON.stringify([mapped.nodes.map(node => node.key), mapped.edges.map(edge => [edge.kind, edge.source, edge.target])]);
      const topologyChanged = projectChanged || this.topology !== topology;
      const oldZoom = this.cy.zoom();
      const oldPan = {...this.cy.pan()};
      this.projectId = mapped.projectId;
      this.generation = mapped.generation;
      this.topology = topology;
      this.snapshot = snapshot;
      this.nodes = mapped.nodes;
      this.edges = mapped.edges;
      this.diagnostics = mapped.diagnostics;
      this.flowLayout = buildFlowLayout(mapped);
      const nextKeys = new Set([...this.flowLayout.nodes.map(entry => entry.key), ...this.flowLayout.edges.map(edge => edge.id)]);
      this.cy.startBatch();
      try {
        if (projectChanged) this.cy.elements().remove();
        else this.cy.elements().filter(element => !nextKeys.has(element.id())).remove();
        this.flowLayout.nodes.forEach(entry => {
          const node = entry.node;
          const existing = this.cy.getElementById(node.key);
          const data = visualData(node, this.measureLabel, entry);
          if (existing.length) existing.data(data);
          else this.cy.add({group: 'nodes', data});
        });
        this.flowLayout.edges.forEach(edge => {
          const data = {id: edge.id, source: edge.source, target: edge.target, recordSource: edge.recordSource, recordTarget: edge.recordTarget, kind: edge.kind, label: edge.label, invalidSupport: edge.supportValid === false};
          const existing = this.cy.getElementById(edge.id);
          if (existing.length) {
            if (existing.data('source') !== data.source || existing.data('target') !== data.target) existing.move({source: data.source, target: data.target});
            existing.data(data);
          }
          else this.cy.add({group: 'edges', data});
        });
      } finally {
        this.cy.endBatch();
      }
      this.renderCards();
      if (topologyChanged) this.layoutNewNodes();
      if (projectChanged || !hadNodes) this.fit();
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

    layoutNewNodes() {
      // A changed dependency can require an existing node to move. Relayout the
      // topology to keep origin first and the real goal last, while preserving
      // the user's pan and zoom. Status/log updates never enter this method.
      const layout = this.flowLayout || buildFlowLayout({nodes: this.nodes, edges: this.edges});
      this.cy.batch(() => {
        layout.nodes.forEach(entry => this.cy.getElementById(entry.key).position(entry.position));
      });
      this.updateCardPositions();
    }

    renderCards() {
      const entries = this.flowLayout.nodes;
      const nextKeys = new Set(entries.map(entry => entry.key));
      for (const [key, card] of this.cards) if (!nextKeys.has(key)) { card.remove(); this.cards.delete(key); }
      for (const entry of entries) {
        const node = entry.node;
        let card = this.cards.get(node.key);
        if (!card) {
          card = this.document.createElement('button');
          card.type = 'button';
          card.dataset.nodeId = node.id;
          card.dataset.nodeKey = node.key;
          card.addEventListener('click', () => this.selectNode(node.key));
          this.cards.set(node.key, card);
          this.cardWorld.appendChild(card);
        }
        const status = entry.role === 'goal' && node.type === 'fact' ? 'pending' : ['completed', 'achieved', 'valid', 'input'].includes(node.status) ? 'done' : ['running', 'paused', 'verified'].includes(node.status) ? node.status : 'pending';
        card.className = 'flow-card type-' + (entry.role === 'subgoal' ? 'goal is-subgoal' : entry.role) + ' status-' + status;
        card.classList.toggle('is-invalid', ['failed', 'refuted', 'superseded', 'narrowed', 'withdrawn', 'abandoned'].includes(node.status));
        card.classList.toggle('has-support-warning', supportWarning(node));
        const statusText = (STATUS_LABELS[node.status] || node.status) + (supportWarning(node) ? ' · 支持失效' : '');
        card.setAttribute('aria-label', entry.label + '：' + (node.label || node.id) + '；' + statusText);
        card.title = entry.label + ' · ' + statusText + '\n' + (node.label || node.id);
        Object.assign(card.style, {position: 'absolute', width: entry.width + 'px', height: entry.height + 'px'});
        const head = this.document.createElement('span');
        head.className = 'flow-card-head';
        const kind = this.document.createElement('span');
        kind.className = 'flow-kind';
        const mark = this.document.createElement('span');
        mark.className = 'flow-kind-mark';
        mark.setAttribute('aria-hidden', 'true');
        const label = this.document.createElement('span');
        label.textContent = entry.label;
        kind.append(mark, label);
        const state = this.document.createElement('span');
        state.className = 'flow-status';
        state.textContent = statusText;
        head.append(kind, state);
        const title = this.document.createElement('span');
        title.className = 'flow-card-title';
        title.textContent = node.label || node.id;
        card.replaceChildren(head, title);
      }
      this.updateCardPositions();
    }

    updateCardPositions() {
      if (!this.cardWorld || !this.cy) return;
      const pan = this.cy.pan(), zoom = this.cy.zoom();
      this.cardWorld.style.transform = 'translate(' + pan.x + 'px,' + pan.y + 'px) scale(' + zoom + ')';
      for (const [key, card] of this.cards) {
        const element = this.cy.getElementById(key);
        if (!element.length) continue;
        const position = element.position();
        card.style.left = (position.x - element.data('width') / 2) + 'px';
        card.style.top = (position.y - element.data('height') / 2) + 'px';
      }
    }

    updatePicker() {
      const active = this.document.activeElement === this.picker;
      this.picker.replaceChildren();
      const empty = this.document.createElement('option');
      empty.value = '';
      empty.textContent = this.nodes.length ? '选择节点 · ' + this.nodes.length + ' 个' : '暂无图节点';
      this.picker.appendChild(empty);
      this.nodes.filter(node => this.recordMatchesFilter(node)).forEach(node => {
        const option = this.document.createElement('option');
        option.value = node.key;
        option.textContent = (TYPE_LABELS[node.type] || '节点') + ' · ' + node.id + ' · ' + shortText(node.label, 80);
        this.picker.appendChild(option);
      });
      this.picker.value = this.selectedKey || '';
      if (active) this.picker.focus({preventScroll: true});
    }

    updateAppearance() {
      const selectedVisualKey = this.visualNodeKey(this.selectedKey);
      if (this.cards) for (const [key, card] of this.cards) {
        card.classList.toggle('is-selected', key === selectedVisualKey);
        card.classList.toggle('is-dimmed', !this.visualMatchesFilter(key));
        card.setAttribute('aria-pressed', String(key === selectedVisualKey));
      }
      this.cy.batch(() => {
        this.cy.nodes().forEach(element => {
          element.toggleClass('is-selected', element.id() === selectedVisualKey);
          element.toggleClass('is-dimmed', !this.visualMatchesFilter(element.id()));
        });
        this.cy.edges().forEach(edge => {
          // Highlight only relationships belonging to the selected typed
          // record, even when two records share their visible endpoint card.
          edge.toggleClass('is-related', (edge.data('recordSource') || edge.source().id()) === this.selectedKey || (edge.data('recordTarget') || edge.target().id()) === this.selectedKey);
          edge.toggleClass('is-dimmed', !this.visualMatchesFilter(edge.source().id()) && !this.visualMatchesFilter(edge.target().id()));
        });
      });
    }

    visualNodeKey(key) { return this.flowLayout && this.flowLayout.aliases[key] || key; }

    recordMatchesFilter(node) {
      if (this.filter === 'all') return true;
      if (this.filter === 'start') return node.key === 'fact:origin';
      if (this.filter === 'goal' && this.flowLayout && this.visualNodeKey(node.key) === this.flowLayout.endKey) return true;
      return node.type === this.filter;
    }

    visualMatchesFilter(key) {
      return this.filter === 'all' || this.nodes.some(node => this.recordMatchesFilter(node) && this.visualNodeKey(node.key) === key);
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
        const element = this.cy.getElementById(this.visualNodeKey(key));
        // A Finding evidence button can unhide the graph and select in the same
        // event, before ResizeObserver has refreshed Cytoscape's cached size.
        this.refreshViewport();
        if (this.hasViewport()) {
          const bounds = element.renderedBoundingBox();
          if (bounds.x1 < 12 || bounds.y1 < 12 || bounds.x2 > this.host.clientWidth - 12 || bounds.y2 > this.host.clientHeight - 12) this.cy.center(element);
        }
        this.live.textContent = (TYPE_LABELS[node.type] || '节点') + '，' + node.label + '，' + (STATUS_LABELS[node.status] || node.status);
      } else this.live.textContent = '已清除节点选择';
      this.onSelect(node ? clone(node) : null);
    }

    onKeyDown(event) {
      if (['ArrowDown', 'ArrowRight', 'ArrowUp', 'ArrowLeft'].includes(event.key)) {
        event.preventDefault();
        const visible = this.flowLayout ? this.flowLayout.nodes.filter(entry => this.visualMatchesFilter(entry.key)).map(entry => entry.node) : this.nodes.filter(node => this.recordMatchesFilter(node));
        if (!visible.length) return;
        const index = visible.findIndex(node => node.key === this.visualNodeKey(this.selectedKey));
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
      this.filter = type === 'start' || TYPES.includes(type) ? type : 'all';
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

    getVisibleNodeCount() { return this.flowLayout ? this.flowLayout.nodes.length : 0; }

    destroy() {
      if (this.destroyed) return;
      this.destroyed = true;
      if (this.resizeObserver) this.resizeObserver.disconnect();
      if (this.resizeHandler) root.removeEventListener('resize', this.resizeHandler);
      this.cy.destroy();
      [this.canvas, this.cardLayer, this.empty, this.warning, this.picker, this.live].forEach(element => element.remove());
    }
  }

  return {XLoomGraph, mapState, resolveNodeKey, LABEL_LAYOUT, createTextMeasurer, wrapMeasuredText, formatNodeLabel, buildFlowLayout, nodePresentation};
}));
