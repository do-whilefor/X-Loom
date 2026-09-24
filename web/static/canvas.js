(function (root, factory) {
  'use strict';
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.XLoomCanvas = api;
})(typeof globalThis === 'object' ? globalThis : this, function () {
  'use strict';

  function worldBounds(positions, options = {}) {
    const { width = 164, height = 90, baseWidth = 1780, baseHeight = 1080, padding = 32 } = options;
    if (![width, height, baseWidth, baseHeight, padding].every(value => Number.isFinite(value) && value >= 0)) throw new RangeError('Canvas dimensions must be finite and non-negative');
    let left = 0, top = 0, right = baseWidth, bottom = baseHeight;
    for (const point of positions.values()) {
      if (!point || !Number.isFinite(point.x) || !Number.isFinite(point.y)) throw new RangeError('Node coordinates must be finite');
      left = Math.min(left, point.x - padding);
      top = Math.min(top, point.y - padding);
      right = Math.max(right, point.x + width + padding);
      bottom = Math.max(bottom, point.y + height + padding);
    }
    return { left, top, right, bottom, width: right - left, height: bottom - top };
  }

  function fitTransform(bounds, viewportWidth, viewportHeight) {
    if (!Number.isFinite(viewportWidth) || !Number.isFinite(viewportHeight) || viewportWidth <= 0 || viewportHeight <= 0) return null;
    if (![bounds.left, bounds.top, bounds.width, bounds.height].every(Number.isFinite) || bounds.width < 0 || bounds.height < 0) throw new RangeError('Canvas bounds must be finite with non-negative dimensions');
    const scale = Math.min(1.15, Math.max(1, viewportWidth - 48) / Math.max(1, bounds.width), Math.max(1, viewportHeight - 90) / Math.max(1, bounds.height));
    return {
      scale,
      tx: (viewportWidth - bounds.width * scale) / 2 - bounds.left * scale,
      ty: 40 + (viewportHeight - 90 - bounds.height * scale) / 2 - bounds.top * scale,
    };
  }

  function edgePanVelocity(x, y, width, height) {
    function axis(coordinate, extent) {
      if (!Number.isFinite(coordinate) || !Number.isFinite(extent) || extent <= 0) return 0;
      const zone = Math.min(56, extent / 2);
      if (coordinate < zone) return 480 * Math.min(1, (zone - coordinate) / zone);
      if (coordinate > extent - zone) return -480 * Math.min(1, (coordinate - extent + zone) / zone);
      return 0;
    }
    return { x: axis(x, width), y: axis(y, height) };
  }

  return { worldBounds, fitTransform, edgePanVelocity };
});
