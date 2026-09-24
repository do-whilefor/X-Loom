(function (root, factory) {
  'use strict';
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.XLoomLayout = api;
})(typeof globalThis === 'object' ? globalThis : this, function () {
  'use strict';
  const NODE_WIDTH = 164, NODE_HEIGHT = 90;
  function randomFrom(seed) {
    let state = 2166136261;
    for (const character of String(seed)) state = Math.imul(state ^ character.charCodeAt(0), 16777619);
    return () => {
      state = (state + 0x6d2b79f5) | 0;
      let value = Math.imul(state ^ (state >>> 15), state | 1);
      value ^= value + Math.imul(value ^ (value >>> 7), value | 61);
      return ((value ^ (value >>> 14)) >>> 0) / 4294967296;
    };
  }
  const nodeKey = node => node.key || node.id;
  function anchors(nodes) {
    const startKey = nodes.some(node => nodeKey(node) === 'fact:origin') ? 'fact:origin' : null;
    const roots = nodes.filter(node => node.type === 'goal' && !node.raw?.parent_id).sort((a, b) => nodeKey(a).localeCompare(nodeKey(b)));
    // fact:goal is an input, not an inferred terminal answer.
    const endKey = roots.find(node => node.id === 'goal')?.key || (roots.length ? nodeKey(roots[0]) : null);
    return {startKey, endKey};
  }
  // Placement is independent of relationships: cycles, corrective edges and
  // missing references cannot turn layout into a failed topological sort.
  // Existing coordinates are copied verbatim; only new IDs get a free slot.
  function mergePositions(nodes, previous = new Map(), options = {}) {
    const gapX = options.gapX ?? 24, gapY = options.gapY ?? 24, padding = options.padding ?? 48;
    if (![gapX, gapY, padding].every(value => Number.isFinite(value) && value >= 0)) throw new RangeError('Layout spacing must be non-negative');
    const ordered = [...nodes].sort((a, b) => nodeKey(a).localeCompare(nodeKey(b)));
    const keys = ordered.map(nodeKey);
    if (keys.some(key => typeof key !== 'string' || !key) || new Set(keys).size !== keys.length) throw new TypeError('Node keys must be unique non-empty strings');
    const scale = Math.sqrt(Math.max(1, nodes.length) / 34);
    const width = Math.ceil(Math.max(940, 1780 * scale)), height = Math.ceil(Math.max(640, 1080 * scale));
    const positions = new Map();
    for (const key of keys) {
      const point = previous.get(key);
      if (point && Number.isFinite(point.x) && Number.isFinite(point.y)) positions.set(key, {...point});
    }
    const random = randomFrom(options.seed ?? keys.join('|'));
    const {startKey, endKey} = anchors(nodes);
    const occupied = candidate => [...positions.values()].some(point => candidate.x < point.x + NODE_WIDTH + gapX && candidate.x + NODE_WIDTH + gapX > point.x && candidate.y < point.y + NODE_HEIGHT + gapY && candidate.y + NODE_HEIGHT + gapY > point.y);
    const anchorPoints = new Map([[startKey, {x: padding, y: (height - NODE_HEIGHT) / 2}], [endKey, {x: width - padding - NODE_WIDTH, y: (height - NODE_HEIGHT) / 2}]]);
    for (const key of [startKey, endKey]) {
      if (key && !positions.has(key) && !occupied(anchorPoints.get(key))) positions.set(key, anchorPoints.get(key));
    }
    for (const key of keys) {
      if (positions.has(key)) continue;
      let candidate;
      for (let attempt = 0; attempt < 500; attempt++) {
        candidate = {x: Math.round((padding + random() * Math.max(1, width - padding * 2 - NODE_WIDTH)) * 10) / 10, y: Math.round((padding + random() * Math.max(1, height - padding * 2 - NODE_HEIGHT)) * 10) / 10};
        if (!occupied(candidate)) break;
        candidate = null;
      }
      // A bounded random search always has a finite fallback outside the cloud.
      // It neither moves existing/manual positions nor clamps world coordinates.
      if (!candidate) {
        const right = Math.max(width, ...[...positions.values()].map(point => point.x + NODE_WIDTH + gapX));
        candidate = {x: right + gapX, y: padding + random() * height};
      }
      positions.set(key, candidate);
    }
    return {positions, width, height, startKey, endKey};
  }
  function layout(nodes, edges = [], options = {}) { return mergePositions(nodes, new Map(), options); }
  return {layout, mergePositions, anchors, NODE_WIDTH, NODE_HEIGHT};
});
