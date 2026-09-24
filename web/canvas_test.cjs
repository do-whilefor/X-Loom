'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {worldBounds, fitTransform, edgePanVelocity} = require('./static/canvas.js');
test('fit includes far and negative positions without limiting world bounds', () => {
  const positions = new Map([['left', {x: -100000, y: -300000}], ['right', {x: 800000, y: 600000}]]);
  const bounds = worldBounds(positions, {baseWidth: 0, baseHeight: 0});
  assert.ok(bounds.left < -100000); assert.ok(bounds.top < -300000);
  const camera = fitTransform(bounds, 750, 600);
  assert.ok(camera.scale > 0 && camera.scale < .001);
  assert.ok(bounds.left * camera.scale + camera.tx >= 0);
  assert.ok(bounds.right * camera.scale + camera.tx <= 750);
  assert.ok(bounds.top * camera.scale + camera.ty >= 0);
  assert.ok(bounds.bottom * camera.scale + camera.ty <= 600);
});
test('four-edge auto pan stops in center and continues outside viewport', () => {
  assert.ok(edgePanVelocity(0, 300, 700, 600).x > 0);
  assert.ok(edgePanVelocity(700, 300, 700, 600).x < 0);
  assert.ok(edgePanVelocity(350, 0, 700, 600).y > 0);
  assert.ok(edgePanVelocity(350, 600, 700, 600).y < 0);
  assert.deepEqual(edgePanVelocity(350, 300, 700, 600), {x: 0, y: 0});
  assert.equal(edgePanVelocity(-500, 300, 700, 600).x, 480);
});
test('zero-sized and empty viewports do not yield infinite camera transforms', () => {
  const bounds = worldBounds(new Map(), {baseWidth: 0, baseHeight: 0});
  assert.equal(fitTransform(bounds, 0, 200), null);
  assert.ok(Object.values(fitTransform(bounds, 100, 100)).every(Number.isFinite));
});
