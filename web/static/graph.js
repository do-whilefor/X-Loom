(function (root, factory) {
  'use strict';
  const common = typeof module === 'object' && module.exports;
  const api = factory(root, common ? require('./graph-data.js') : root.XLoomGraphData, common ? require('./canvas.js') : root.XLoomCanvas, common ? require('./layout.js') : root.XLoomLayout, common ? require('./routing.js') : root.XLoomRouting, common ? require('./graph-view.js') : root.XLoomGraphView);
  if (common) module.exports = api;
  else root.XLoomGraph = api.XLoomGraph;
})(typeof window === 'object' ? window : globalThis, function (root, data, canvas, layout, routing, view) {
  'use strict';
  const {mapState, resolveNodeKey} = data;
  const {worldBounds, fitTransform, edgePanVelocity} = canvas;
  const {mergePositions, NODE_WIDTH, NODE_HEIGHT} = layout;
  const {projectGeometry, describeEdge, collectSelection, placeEdgeLabel, nodePresentation, edgeKey} = view;
  const NS = 'http://www.w3.org/2000/svg';
  const COLORS = {done: '#8fa17a', running: '#d97757', pending: '#b0aea5', recorded: '#b0aea5', paused: '#b38548', invalid: '#b18748', selected: '#bc6043'};
  let nextInstance = 0;
  const clone = value => value == null ? null : JSON.parse(JSON.stringify(value));

  class XLoomGraph {
    constructor(host, options = {}) {
      if (!host?.ownerDocument) throw new TypeError('XLoomGraph requires a host element');
      this.host = host; this.document = host.ownerDocument; this.window = this.document.defaultView || root;
      this.onSelect = options.onSelect || (() => {}); this.onSelectEdge = options.onSelectEdge || (() => {});
      this.instanceId = 'xloom-graph-' + (++nextInstance);
      this.positions = new Map(); this.nodeGeometry = new Map(); this.projectLayouts = new Map();
      this.cards = new Map(); this.edgeElements = new Map(); this.listeners = [];
      this.selected = null; this.selectedEdge = null; this.scale = 1; this.tx = 0; this.ty = 0;
      this.project = null; this.projectId = null; this.generation = null; this.nodes = []; this.edges = []; this.diagnostics = [];
      this.drag = null; this.filter = 'all'; this.statusFilter = 'all'; this.direction = 'upstream'; this.destroyed = false;
      this.panFrame = null; this.panTime = null; this.fitFrame = null; this.drawFrame = null; this.clickTimer = null;
      this.viewport = this.el('div', 'graph-viewport'); this.viewport.tabIndex = 0;
      this.viewport.setAttribute('role', 'group'); this.viewport.setAttribute('aria-label', '任务图。点击节点追踪上游，方向键移动节点，加减号缩放，0 适应画布，Escape 清除选择。');
      this.world = this.el('div', 'graph-world'); this.nodeHost = this.el('div', 'graph-nodes'); this.svg = this.svgEl('svg', {class: 'graph-edges', tabindex: -1});
      this.defs = this.svgEl('defs');
      for (const [name, color] of Object.entries(COLORS)) {
        const marker = this.svgEl('marker', {id: this.instanceId + '-arrow-' + name, viewBox: '0 0 10 10', refX: 9, refY: 5, markerWidth: 5, markerHeight: 5, orient: 'auto-start-reverse'});
        marker.append(this.svgEl('path', {d: 'M 1 1 L 9 5 L 1 9 Z', fill: color})); this.defs.append(marker);
      }
      this.edgeLayer = this.svgEl('g'); this.labelLayer = this.svgEl('g', {class: 'graph-edge-labels'}); this.svg.append(this.defs, this.edgeLayer, this.labelLayer);
      this.world.append(this.svg, this.nodeHost); this.viewport.append(this.world);
      this.empty = this.el('div', 'graph-data-empty', '暂无图节点'); this.empty.hidden = true;
      this.warning = this.el('div', 'graph-data-warning'); this.warning.hidden = true;
      this.live = this.el('div', 'graph-screen-reader'); this.live.setAttribute('aria-live', 'polite');
      host.append(this.viewport, this.empty, this.warning, this.live);
      this.viewportSize = this.size();
      this.listen(this.viewport, 'pointerdown', event => this.pointerDown(event));
      this.listen(this.viewport, 'pointermove', event => this.pointerMove(event));
      this.listen(this.viewport, 'pointerup', event => this.pointerUp(event));
      for (const type of ['pointercancel', 'lostpointercapture']) this.listen(this.viewport, type, event => { if (event.pointerId === this.drag?.pointerId) this.cancelDrag(); });
      this.listen(this.window, 'blur', () => this.cancelDrag());
      this.listen(this.document, 'visibilitychange', () => { if (this.document.hidden) this.cancelDrag(); });
      this.listen(this.viewport, 'wheel', event => { event.preventDefault(); const rect = this.viewport.getBoundingClientRect(); this.zoom(Math.exp(-Math.max(-150, Math.min(150, event.deltaY)) * .003), event.clientX - rect.left, event.clientY - rect.top); }, {passive: false});
      this.listen(this.nodeHost, 'click', event => { const button = event.target.closest('.graph-node'); if (button && !this.suppressClick) this.selectNode(button.dataset.nodeKey); });
      this.listen(this.viewport, 'keydown', event => this.keyDown(event));
      const hitTarget = target => target?.closest?.('.graph-edge-hit, .graph-edge-label');
      const activateEdge = event => {
        const hit = hitTarget(event.target);
        if (!hit || (event.type === 'keydown' && !['Enter', ' '].includes(event.key))) return;
        event.preventDefault(); event.stopPropagation(); this.selectEdge(hit.dataset.edgeKey);
      };
      this.listen(this.svg, 'click', activateEdge); this.listen(this.svg, 'keydown', activateEdge);
      this.listen(this.svg, 'pointerover', event => { this.hoveredEdgeKey = hitTarget(event.target)?.dataset.edgeKey || null; this.updateEdgeLabelVisibility(); });
      this.listen(this.svg, 'pointerout', event => { this.hoveredEdgeKey = hitTarget(event.relatedTarget)?.dataset.edgeKey || null; this.updateEdgeLabelVisibility(); });
      this.listen(this.svg, 'focusin', event => { this.focusedEdgeKey = hitTarget(event.target)?.dataset.edgeKey || null; this.updateEdgeLabelVisibility(); });
      this.listen(this.svg, 'focusout', event => { this.focusedEdgeKey = hitTarget(event.relatedTarget)?.dataset.edgeKey || null; this.updateEdgeLabelVisibility(); });
      if (typeof this.window.ResizeObserver === 'function') { this.resizeObserver = new this.window.ResizeObserver(() => this.resize()); this.resizeObserver.observe(this.viewport); }
      else this.listen(this.window, 'resize', () => this.resize());
    }
    el(tag, cls, text) { const node = this.document.createElement(tag); node.className = cls; if (text !== undefined) node.textContent = text; return node; }
    svgEl(tag, attrs = {}) { const node = this.document.createElementNS(NS, tag); for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, value); return node; }
    icon(name) { const icon = this.svgEl('svg', {class: 'icon', 'aria-hidden': 'true'}); icon.append(this.svgEl('use', {href: '#i-' + name})); return icon; }
    listen(target, name, fn, options) { target.addEventListener(name, fn, options); this.listeners.push(() => target.removeEventListener(name, fn, options)); }
    size() { return {width: this.viewport.clientWidth, height: this.viewport.clientHeight}; }
    requestFrame(fn) { return this.window.requestAnimationFrame(fn); }
    cancelFrame(id) { if (id !== null) this.window.cancelAnimationFrame(id); }
    cacheKey(project = this.project) { return project ? JSON.stringify([project.projectId, project.generation]) : null; }
    saveView() {
      const saved = this.projectLayouts.get(this.cacheKey());
      if (saved) { saved.positions = this.positions; saved.camera = {scale: this.scale, tx: this.tx, ty: this.ty, ...this.size()}; }
    }
    setState(state) {
      if (this.destroyed) return;
      const mapped = mapState(state), key = this.cacheKey(mapped), changed = key !== this.cacheKey();
      const previousNode = this.project?.nodeIndex.get(this.selected), previousEdge = this.project?.edgeIndex.get(this.selectedEdge);
      this.saveView();
      if (changed) { this.cancelDrag(); this.cancelPendingFit(); this.statusFilter = 'all'; }
      // New generations invalidate previous-round coordinates and camera.
      for (const [savedKey, saved] of this.projectLayouts) if (saved.projectId === mapped.projectId && saved.generation !== mapped.generation) this.projectLayouts.delete(savedKey);
      let saved = this.projectLayouts.get(key);
      const fresh = !saved;
      if (!saved) saved = {projectId: mapped.projectId, generation: mapped.generation, positions: new Map(), seed: 0};
      const firstNodes = !saved.positions.size && mapped.nodes.length > 0;
      Object.assign(saved, mergePositions(mapped.nodes, saved.positions, {seed: key + ':' + saved.seed}));
      this.projectLayouts.set(key, saved); this.positions = saved.positions;
      this.project = mapped; this.projectId = mapped.projectId; this.generation = mapped.generation;
      this.nodes = mapped.nodes; this.edges = mapped.edges; this.diagnostics = mapped.diagnostics;
      mapped.nodeIndex = new Map(mapped.nodes.map(node => [node.key, node])); mapped.edgeIndex = new Map(mapped.edges.map(edge => [edge.id, edge]));
      mapped.incoming = new Map(mapped.nodes.map(node => [node.key, []])); mapped.outgoing = new Map(mapped.nodes.map(node => [node.key, []]));
      for (const edge of mapped.edges) { mapped.incoming.get(edge.target)?.push(edge); mapped.outgoing.get(edge.source)?.push(edge); }
      if (this.drag?.id && !mapped.nodeIndex.has(this.drag.id)) this.cancelDrag();
      if (changed || !mapped.nodeIndex.has(this.selected)) this.selected = null;
      if (changed || !mapped.edgeIndex.has(this.selectedEdge)) this.selectedEdge = null;
      if (!mapped.edgeIndex.has(this.hoveredEdgeKey)) this.hoveredEdgeKey = null;
      if (!mapped.edgeIndex.has(this.focusedEdgeKey)) this.focusedEdgeKey = null;
      this.clearHiddenSelection(false); this.warning.hidden = !this.diagnostics.length;
      this.warning.textContent = this.diagnostics.length ? this.diagnostics.length + ' 条图数据引用需要检查' : '';
      this.renderCards(saved.endKey); this.scheduleDraw();
      if (changed && saved.camera) {
        const size = this.size(); this.scale = saved.camera.scale;
        this.tx = saved.camera.tx + (size.width - saved.camera.width) / 2; this.ty = saved.camera.ty + (size.height - saved.camera.height) / 2;
        this.viewportSize = size; this.transform();
      } else if (fresh) { this.scale = 1; this.tx = 0; this.ty = 0; this.transform(); this.scheduleFit(); }
      else if (firstNodes) this.scheduleFit();
      const selected = mapped.nodeIndex.get(this.selected), selectedEdge = mapped.edgeIndex.get(this.selectedEdge);
      if (previousNode || selected) { if (changed || JSON.stringify(previousNode) !== JSON.stringify(selected)) this.onSelect(clone(selected)); }
      if (previousEdge || selectedEdge) { if (changed || JSON.stringify(previousEdge) !== JSON.stringify(selectedEdge)) this.onSelectEdge(clone(selectedEdge)); }
    }
    renderCards(endKey) {
      for (const [key, card] of this.cards) if (!this.project.nodeIndex.has(key)) { card.remove(); this.cards.delete(key); }
      for (const node of this.nodes) {
        let card = this.cards.get(node.key);
        if (!card) {
          card = this.el('button', 'graph-node'); card.type = 'button'; card.dataset.nodeKey = node.key; card.dataset.nodeId = node.id;
          const top = this.el('span', 'node-top'), kind = this.el('span', 'node-kind'), status = this.el('span', 'node-status');
          const kindText = this.el('span', ''), statusText = this.el('span', ''); kind.append(kindText); status.append(statusText); top.append(kind, status);
          const title = this.el('strong', 'node-title'), subtitle = this.el('span', 'node-subtitle'); card.append(top, title, subtitle);
          card.parts = {kind, kindText, status, statusText, title, subtitle}; this.cards.set(node.key, card); this.nodeHost.append(card);
        }
        const shape = nodePresentation(node, endKey), stamp = JSON.stringify([node.label, shape]);
        if (stamp === card.stamp) continue;
        card.stamp = stamp; card.className = 'graph-node ' + shape.kind + ' ' + shape.status;
        card.dataset.status = node.status; card.dataset.viewType = shape.kind;
        card.setAttribute('aria-label', shape.label + '：' + (node.label || node.id) + '，' + shape.statusLabel);
        card.title = shape.label + ' · ' + shape.statusLabel + '\n' + (node.description || node.id);
        card.parts.kind.replaceChildren(this.icon(({start: 'flag', task: 'node', fact: 'file', goal: 'check', subgoal: 'node', finding: 'file'})[shape.kind] || 'node'), card.parts.kindText);
        card.parts.kindText.textContent = shape.label; card.parts.statusText.textContent = shape.statusLabel;
        card.parts.title.textContent = node.label || node.id; card.parts.subtitle.textContent = shape.subtitle;
        card.parts.status.replaceChildren();
        if (shape.status === 'done') card.parts.status.append(this.icon('check'));
        if (shape.status === 'running') card.parts.status.append(this.el('i', 'node-running-dot'));
        card.parts.status.append(card.parts.statusText);
      }
    }
    scheduleFit() { this.cancelPendingFit(); this.fitFrame = this.requestFrame(() => { this.fitFrame = null; if (!this.destroyed && !this.drag) this.fit(); }); }
    cancelPendingFit() { this.cancelFrame(this.fitFrame); this.fitFrame = null; }
    scheduleDraw() { if (this.drawFrame === null && !this.destroyed) this.drawFrame = this.requestFrame(() => { this.drawFrame = null; this.drawEdges(); }); }
    syncGeometry() {
      this.nodeGeometry = projectGeometry(this.nodes, this.positions, NODE_WIDTH, NODE_HEIGHT);
      for (const [key, card] of this.cards) {
        const shape = this.nodeGeometry.get(key); if (!shape) continue;
        card.style.left = shape.x + 'px'; card.style.top = shape.y + 'px'; card.style.width = shape.width + 'px'; card.style.height = shape.height + 'px';
      }
    }
    updateBounds() {
      const bounds = worldBounds(this.positions, {width: NODE_WIDTH, height: NODE_HEIGHT, baseWidth: 0, baseHeight: 0, padding: 96});
      this.world.style.width = Math.max(1, bounds.right) + 'px'; this.world.style.height = Math.max(1, bounds.bottom) + 'px';
      this.svg.style.left = bounds.left + 'px'; this.svg.style.top = bounds.top + 'px';
      this.svg.setAttribute('width', Math.max(1, bounds.width)); this.svg.setAttribute('height', Math.max(1, bounds.height));
      this.svg.setAttribute('viewBox', `${bounds.left} ${bounds.top} ${Math.max(1, bounds.width)} ${Math.max(1, bounds.height)}`);
      return bounds;
    }
    drawEdges() {
      if (this.destroyed || !this.project) return;
      this.syncGeometry(); this.updateBounds();
      this.empty.hidden = this.getVisibleNodeCount() > 0;
      this.empty.textContent = this.statusFilter === 'all' ? '暂无图节点' : '暂无' + ({done: '已完成', running: '运行中', pending: '待执行'})[this.statusFilter] + '节点';
      const selection = collectSelection(this.project, this.selected, this.selectedEdge, this.direction);
      for (const [key, card] of this.cards) {
        const node = this.project.nodeIndex.get(key), related = selection.nodeIds.has(key);
        const matches = this.filter === 'all' || node.type === this.filter || (this.filter === 'start' && key === 'fact:origin');
        card.hidden = !this.isNodeVisible(node);
        card.classList.toggle('selected', this.selected === key); card.classList.toggle('lineage', selection.active && related); card.classList.toggle('dim', (selection.active && !related) || !matches);
        card.setAttribute('aria-pressed', String(this.selected === key));
      }
      const signature = JSON.stringify([this.edges.map(edge => [edge.id, edge.source, edge.target]), [...this.nodeGeometry]]);
      if (this.edgeRouteCache?.signature !== signature) {
        this.edgeRouteCache = {signature, routes: new Map([...routing.routeEdges(this.edges, this.nodeGeometry)].map(([edge, route]) => [edge.id, route]))};
      }
      for (const [key, entry] of this.edgeElements) if (!this.project.edgeIndex.has(key)) { entry.group.remove(); entry.label.remove(); this.edgeElements.delete(key); }
      for (const edge of this.edges) {
        const route = this.edgeRouteCache.routes.get(edge.id), detail = describeEdge(this.project, edge); if (!route || !detail) continue;
        let entry = this.edgeElements.get(edge.id);
        if (!entry) {
          const group = this.svgEl('g'), path = this.svgEl('path', {'aria-hidden': 'true'}), port = this.svgEl('circle', {r: 2.2, class: 'graph-source-port'});
          const hit = this.svgEl('path', {role: 'button', tabindex: 0}), title = this.svgEl('title'), label = this.svgEl('text', {'text-anchor': 'middle', 'dominant-baseline': 'central', 'aria-hidden': 'true'});
          hit.append(title); group.append(path, port, hit); this.edgeLayer.append(group); this.labelLayer.append(label);
          hit.dataset.edgeKey = edge.id; label.dataset.edgeKey = edge.id;
          entry = {group, path, port, hit, title, label}; this.edgeElements.set(edge.id, entry);
        }
        const visible = this.isEdgeVisible(edge);
        entry.group.style.display = visible ? '' : 'none'; entry.label.style.display = visible ? '' : 'none';
        const selected = this.selectedEdge === edge.id, related = selection.edgeKeys.has(edge.id), dim = selection.active && !related;
        const color = selected ? 'selected' : detail.status;
        const classes = detail.status + (selected ? ' selected' : '') + (related ? ' lineage' : '') + (dim ? ' dim' : '');
        entry.path.setAttribute('d', route.d); entry.path.setAttribute('class', 'graph-edge ' + classes); entry.path.setAttribute('marker-end', 'url(#' + this.instanceId + '-arrow-' + color + ')');
        entry.port.setAttribute('cx', route.start.x); entry.port.setAttribute('cy', route.start.y); entry.port.setAttribute('fill', COLORS[color]); entry.port.setAttribute('opacity', dim ? .2 : 1);
        entry.hit.setAttribute('d', route.d); entry.hit.setAttribute('class', 'graph-edge-hit ' + classes); entry.hit.setAttribute('aria-pressed', String(selected));
        entry.hit.setAttribute('aria-label', detail.label + '：' + detail.sourceNode.label + ' → ' + detail.targetNode.label + '，' + detail.statusLabel);
        entry.title.textContent = detail.label + ' · ' + detail.statusLabel + '\n' + detail.description;
        // Text is deliberately projected on every update, outside geometry cache.
        const label = placeEdgeLabel(route.points, detail.label);
        entry.label.setAttribute('class', 'graph-edge-label ' + classes); entry.label.setAttribute('transform', `translate(${label.x} ${label.y}) rotate(${label.angle})`); entry.label.textContent = label.text;
      }
      this.updateEdgeLabelVisibility(); this.saveView();
    }
    updateEdgeLabelVisibility() { for (const [key, entry] of this.edgeElements) entry.label.classList.toggle('is-revealed', key === this.hoveredEdgeKey || key === this.focusedEdgeKey); }
    getEdgeDetails(edge) { return clone(describeEdge(this.project, edge)); }
    selectNode(selection) {
      if (this.destroyed) return;
      const key = resolveNodeKey(this.nodes, selection);
      if (selection != null && (!key || !this.isNodeVisible(this.project?.nodeIndex.get(key)))) return;
      const hadEdge = this.selectedEdge !== null; this.selected = key; this.selectedEdge = null; this.scheduleDraw();
      if (hadEdge) this.onSelectEdge(null);
      const node = this.project?.nodeIndex.get(key); this.onSelect(clone(node));
      this.live.textContent = node ? node.label : '已清除选择';
    }
    selectEdge(edge) {
      if (this.destroyed) return;
      const key = edge == null ? null : edgeKey(edge);
      if (key && !this.isEdgeVisible(this.project?.edgeIndex.get(key))) return;
      const hadNode = this.selected !== null; this.selected = null; this.selectedEdge = key; this.scheduleDraw();
      if (hadNode) this.onSelect(null); this.onSelectEdge(clone(this.project?.edgeIndex.get(key)));
    }
    clearSelection() { this.selected = null; this.selectedEdge = null; this.scheduleDraw(); this.onSelect(null); this.onSelectEdge(null); }
    setTraceDirection(direction) { this.direction = direction === 'downstream' ? 'downstream' : 'upstream'; this.scheduleDraw(); }
    setFilter(type) { this.filter = ['goal', 'step', 'fact', 'finding', 'start'].includes(type) ? type : 'all'; this.scheduleDraw(); }
    isNodeVisible(node) { return !!node && (this.statusFilter === 'all' || nodePresentation(node, null).status === this.statusFilter); }
    isEdgeVisible(edge) { return !!edge && this.isNodeVisible(this.project?.nodeIndex.get(edge.source)) && this.isNodeVisible(this.project?.nodeIndex.get(edge.target)); }
    clearHiddenSelection(notify = true) {
      if (this.drag?.id && !this.isNodeVisible(this.project?.nodeIndex.get(this.drag.id))) this.cancelDrag();
      if (this.selected && !this.isNodeVisible(this.project?.nodeIndex.get(this.selected))) { this.selected = null; if (notify) this.onSelect(null); this.live.textContent = '已清除选择'; }
      if (this.selectedEdge && !this.isEdgeVisible(this.project?.edgeIndex.get(this.selectedEdge))) { this.selectedEdge = null; if (notify) this.onSelectEdge(null); }
      if (!this.isEdgeVisible(this.project?.edgeIndex.get(this.hoveredEdgeKey))) this.hoveredEdgeKey = null;
      if (!this.isEdgeVisible(this.project?.edgeIndex.get(this.focusedEdgeKey))) this.focusedEdgeKey = null;
    }
    setStatusFilter(status) {
      if (this.destroyed) return;
      const next = ['done', 'running', 'pending'].includes(status) ? status : 'all';
      if (next === this.statusFilter) return;
      this.statusFilter = next; this.cancelPendingFit(); this.clearHiddenSelection(); this.scheduleDraw();
    }
    getStatusFilter() { return this.statusFilter; }
    arrange() {
      if (this.destroyed || !this.project) return;
      this.cancelDrag(); this.cancelPendingFit(); const saved = this.projectLayouts.get(this.cacheKey()); saved.seed++;
      Object.assign(saved, mergePositions(this.nodes, new Map(), {seed: this.cacheKey() + ':' + saved.seed})); this.positions = saved.positions;
      this.scheduleDraw(); this.fit();
    }
    transform() {
      this.world.style.transform = `translate(${this.tx}px,${this.ty}px) scale(${this.scale})`; this.saveView();
      this.host.dispatchEvent(new this.window.CustomEvent('graphzoom', {detail: {zoom: this.scale}}));
    }
    fit() {
      if (this.destroyed) return;
      this.cancelDrag(); this.cancelPendingFit(); const size = this.size();
      const visible = this.nodes.filter(node => this.isNodeVisible(node));
      if (!visible.length || !size.width || !size.height) { this.pendingFit = !!visible.length; return; }
      const bounds = worldBounds(new Map(visible.map(node => [node.key, this.positions.get(node.key)])), {width: NODE_WIDTH, height: NODE_HEIGHT, baseWidth: 0, baseHeight: 0, padding: 96});
      const camera = fitTransform(bounds, size.width, size.height); if (!camera) return;
      this.pendingFit = false; this.viewportSize = size; Object.assign(this, camera); this.transform();
    }
    zoom(factor, x = this.viewport.clientWidth / 2, y = this.viewport.clientHeight / 2) {
      if (this.destroyed || !Number.isFinite(factor) || factor <= 0) return;
      this.cancelPendingFit(); const next = Math.min(2.5, this.scale * factor); if (!Number.isFinite(next) || next <= 0) return;
      const ratio = next / this.scale; this.tx = x - (x - this.tx) * ratio; this.ty = y - (y - this.ty) * ratio; this.scale = next;
      this.transform(); this.syncDragToView();
    }
    zoomBy(factor) { this.zoom(factor); }
    resize() {
      if (this.destroyed) return;
      const size = this.size(), previous = this.viewportSize; this.viewportSize = size;
      if (!this.project || !size.width || !size.height) return;
      if (this.pendingFit || !previous.width || !previous.height) { this.fit(); return; }
      this.tx += (size.width - previous.width) / 2; this.ty += (size.height - previous.height) / 2; this.transform(); this.syncDragToView();
    }
    refreshViewport() { this.resize(); }
    syncDragToView() {
      if (this.drag?.id) this.updateDraggedNode();
      else if (this.drag) { this.drag.ox = this.tx; this.drag.oy = this.ty; this.drag.x = this.drag.lastX; this.drag.y = this.drag.lastY; }
    }
    focusNode(selection) {
      const key = resolveNodeKey(this.nodes, selection), point = this.positions.get(key); if (this.destroyed || !point || !this.isNodeVisible(this.project?.nodeIndex.get(key))) return;
      this.cancelPendingFit(); this.tx = this.viewport.clientWidth / 2 - (point.x + NODE_WIDTH / 2) * this.scale;
      this.ty = this.viewport.clientHeight / 2 - (point.y + NODE_HEIGHT / 2) * this.scale; this.transform();
    }
    pointerDown(event) {
      if (this.destroyed || event.button !== 0 || this.drag || event.target.closest('.graph-edge-hit, .graph-edge-label')) return;
      this.cancelPendingFit(); const button = event.target.closest('.graph-node'), key = button?.dataset.nodeKey, position = this.positions.get(key), rect = this.viewport.getBoundingClientRect();
      this.drag = {id: key, pointerId: event.pointerId, projectKey: this.cacheKey(), x: event.clientX, y: event.clientY, lastX: event.clientX, lastY: event.clientY, ox: this.tx, oy: this.ty, moved: false,
        grabX: position ? (event.clientX - rect.left - this.tx) / this.scale - position.x : 0, grabY: position ? (event.clientY - rect.top - this.ty) / this.scale - position.y : 0};
      this.viewport.setPointerCapture(event.pointerId); this.viewport.classList.add('is-panning');
    }
    pointerMove(event) {
      if (!this.drag || event.pointerId !== this.drag.pointerId) return;
      const drag = this.drag, dx = event.clientX - drag.x, dy = event.clientY - drag.y;
      if (Math.abs(dx) + Math.abs(dy) > 5) drag.moved = true; drag.lastX = event.clientX; drag.lastY = event.clientY;
      if (drag.id) { if (drag.moved) { this.updateDraggedNode(); this.startEdgePan(); } }
      else { this.tx = drag.ox + dx; this.ty = drag.oy + dy; this.transform(); }
    }
    updateDraggedNode() {
      const drag = this.drag; if (!drag?.id || drag.projectKey !== this.cacheKey()) return;
      const point = this.positions.get(drag.id); if (!point) { this.cancelDrag(); return; }
      const rect = this.viewport.getBoundingClientRect();
      point.x = (drag.lastX - rect.left - this.tx) / this.scale - drag.grabX; point.y = (drag.lastY - rect.top - this.ty) / this.scale - drag.grabY;
      this.scheduleDraw(); this.saveView();
    }
    startEdgePan() {
      if (this.panFrame !== null || !this.drag?.id || !this.drag.moved) return;
      this.panTime = null;
      const step = time => {
        this.panFrame = null; const drag = this.drag;
        if (this.destroyed || !drag?.id || !drag.moved || drag.projectKey !== this.cacheKey()) { this.panTime = null; return; }
        const rect = this.viewport.getBoundingClientRect(), velocity = edgePanVelocity(drag.lastX - rect.left, drag.lastY - rect.top, rect.width, rect.height);
        if (!velocity.x && !velocity.y) { this.panTime = null; return; }
        const seconds = this.panTime === null ? 0 : Math.min(.04, (time - this.panTime) / 1000); this.panTime = time;
        this.tx += velocity.x * seconds; this.ty += velocity.y * seconds; this.transform(); this.updateDraggedNode(); this.panFrame = this.requestFrame(step);
      };
      this.panFrame = this.requestFrame(step);
    }
    cancelDrag() {
      this.cancelFrame(this.panFrame); this.panFrame = null; this.panTime = null;
      const drag = this.drag; this.drag = null; this.viewport.classList.remove('is-panning');
      if (drag && this.viewport.hasPointerCapture(drag.pointerId)) this.viewport.releasePointerCapture(drag.pointerId);
      this.saveView();
    }
    pointerUp(event) {
      if (!this.drag || event.pointerId !== this.drag.pointerId) return;
      const drag = this.drag; this.suppressClick = true; this.cancelDrag();
      if (!drag.moved) { if (drag.id) this.selectNode(drag.id); else this.clearSelection(); }
      if (this.clickTimer !== null) this.window.clearTimeout(this.clickTimer);
      this.clickTimer = this.window.setTimeout(() => { this.suppressClick = false; this.clickTimer = null; }, 0);
    }
    keyDown(event) {
      const button = event.target.closest('.graph-node'), direction = {ArrowLeft: [-12, 0], ArrowRight: [12, 0], ArrowUp: [0, -12], ArrowDown: [0, 12]}[event.key];
      if (button && direction) { event.preventDefault(); const point = this.positions.get(button.dataset.nodeKey); if (point) { point.x += direction[0]; point.y += direction[1]; this.scheduleDraw(); this.saveView(); } }
      else if (event.key === 'Escape') { event.preventDefault(); this.clearSelection(); }
      else if (event.key === '+' || event.key === '=') { event.preventDefault(); this.zoomBy(1.2); }
      else if (event.key === '-') { event.preventDefault(); this.zoomBy(1 / 1.2); }
      else if (event.key === '0') { event.preventDefault(); this.fit(); }
    }
    getNodes() { return clone(this.nodes); }
    getVisibleNodeCount() { return this.nodes.filter(node => this.isNodeVisible(node)).length; }
    forgetProject(projectId) {
      for (const [key, value] of this.projectLayouts) if (value.projectId === projectId) this.projectLayouts.delete(key);
      if (this.projectId === projectId) this.setState(null);
    }
    destroy() {
      if (this.destroyed) return;
      this.cancelDrag(); this.cancelPendingFit(); this.cancelFrame(this.drawFrame); this.drawFrame = null;
      if (this.clickTimer !== null) this.window.clearTimeout(this.clickTimer); this.clickTimer = null;
      this.destroyed = true; this.resizeObserver?.disconnect(); for (const remove of this.listeners) remove(); this.listeners = [];
      this.viewport.remove(); this.empty.remove(); this.warning.remove(); this.live.remove(); this.cards.clear(); this.edgeElements.clear(); this.projectLayouts.clear();
    }
  }
  return {XLoomGraph, mapState, resolveNodeKey};
});
