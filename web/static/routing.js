(function (root, factory) {
  'use strict';
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.XLoomRouting = api;
})(typeof globalThis === 'object' ? globalThis : this, function () {
  'use strict';

  function geometry(positions, options) {
    if (!(positions instanceof Map)) throw new TypeError('Routing positions must be a Map');
    const width = options.width ?? 164, height = options.height ?? 90;
    if (![width, height].every(value => Number.isFinite(value) && value > 0)) throw new RangeError('Routing dimensions must be positive');
    return new Map([...positions].map(([id, point]) => {
      if (![point.x, point.y].every(Number.isFinite)) throw new RangeError('Routing coordinates must be finite');
      const rect = { x: point.x, y: point.y, width: point.width ?? width, height: point.height ?? height };
      if (![rect.width, rect.height].every(value => Number.isFinite(value) && value > 0)) throw new RangeError('Routing node dimensions must be positive');
      return [id, rect];
    }));
  }

  function cubicPoint(start, first, second, end, t) {
    const u = 1 - t;
    return {
      x: u ** 3 * start.x + 3 * u * u * t * first.x + 3 * u * t * t * second.x + t ** 3 * end.x,
      y: u ** 3 * start.y + 3 * u * u * t * first.y + 3 * u * t * t * second.y + t ** 3 * end.y,
    };
  }

  function connect(sourceId, targetId, nodes, lane = 0) {
    const source = nodes.get(sourceId), target = nodes.get(targetId);
    if (!source || !target) throw new Error('Graph edge refers to an unknown node');
    if (sourceId === targetId) {
      const start = {x: source.x + source.width, y: source.y + source.height * .3};
      const end = {x: source.x + source.width * .7, y: source.y};
      const bend = 80 + Math.abs(lane) * 30;
      return curve(start, {x: start.x + bend, y: start.y - bend}, {x: end.x + bend, y: end.y - bend}, end, 'right', 'top');
    }
    const sourceCenter = { x: source.x + source.width / 2, y: source.y + source.height / 2 };
    const targetCenter = { x: target.x + target.width / 2, y: target.y + target.height / 2 };
    const dx = targetCenter.x - sourceCenter.x, dy = targetCenter.y - sourceCenter.y;
    const horizontal = Math.abs(dx) / ((source.width + target.width) / 2) >= Math.abs(dy) / ((source.height + target.height) / 2);
    const normal = horizontal ? { x: dx >= 0 ? 1 : -1, y: 0 } : { x: 0, y: dy >= 0 ? 1 : -1 };
    const sourcePort = horizontal ? normal.x > 0 ? 'right' : 'left' : normal.y > 0 ? 'bottom' : 'top';
    const targetPort = { right: 'left', left: 'right', bottom: 'top', top: 'bottom' }[sourcePort];
    const sourceBorder = { x: sourceCenter.x + normal.x * source.width / 2, y: sourceCenter.y + normal.y * source.height / 2 };
    const targetBorder = { x: targetCenter.x - normal.x * target.width / 2, y: targetCenter.y - normal.y * target.height / 2 };
    const gap = (targetBorder.x - sourceBorder.x) * normal.x + (targetBorder.y - sourceBorder.y) * normal.y;
    const sourceOffset = gap > 0 ? Math.min(3, gap * .2) : 3, targetOffset = gap > 0 ? Math.min(6, gap * .2) : 6;
    const start = { x: sourceBorder.x + normal.x * sourceOffset, y: sourceBorder.y + normal.y * sourceOffset };
    const end = { x: targetBorder.x - normal.x * targetOffset, y: targetBorder.y - normal.y * targetOffset };
    const span = (end.x - start.x) * normal.x + (end.y - start.y) * normal.y;
    // Facing ports and short handles give a single gentle curve. Other cards
    // intentionally do not affect its route; their opaque surfaces cover it.
    const bend = span > 0 ? Math.min(96, span * .4) : Math.min(96, Math.max(24, (Math.abs(dx) + Math.abs(dy)) * .2));
    const control1 = { x: start.x + normal.x * bend, y: start.y + normal.y * bend };
    const control2 = { x: end.x - normal.x * bend, y: end.y - normal.y * bend };
    // Keep distinct relation IDs independently clickable, even at identical
    // endpoints. Reverse edges receive their own lane as well.
    const offset = lane * 32;
    control1.x += -normal.y * offset; control1.y += normal.x * offset;
    control2.x += -normal.y * offset; control2.y += normal.x * offset;
    return curve(start, control1, control2, end, sourcePort, targetPort);
  }

  function curve(start, control1, control2, end, sourcePort, targetPort) {
    const pair = point => `${Number(point.x.toFixed(3))} ${Number(point.y.toFixed(3))}`;
    const d = `M ${pair(start)} C ${pair(control1)}, ${pair(control2)}, ${pair(end)}`;
    const points = Array.from({ length: 25 }, (_, index) => cubicPoint(start, control1, control2, end, index / 24));
    return { d, points, sourcePort, targetPort, start, end, control1, control2 };
  }

  function routeEdges(edges, positions, options = {}) {
    const nodes = geometry(positions, options);
    const groups = new Map(), routes = new Map();
    for (const edge of edges) {
      if (!nodes.has(edge.source) || !nodes.has(edge.target)) continue;
      const key = JSON.stringify([edge.source, edge.target].sort());
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(edge);
    }
    for (const group of groups.values()) {
      group.sort((a, b) => String(a.id || a.kind).localeCompare(String(b.id || b.kind)));
      group.forEach((edge, index) => {
        let lane = index - (group.length - 1) / 2;
        if (edge.source > edge.target) lane = -lane;
        routes.set(edge, connect(edge.source, edge.target, nodes, lane));
      });
    }
    return routes;
  }
  function routeEdge(sourceId, targetId, positions, options = {}) {
    return connect(sourceId, targetId, geometry(positions, options));
  }
  return { routeEdge, routeEdges };
});
