const test = require('node:test');
const assert = require('node:assert/strict');

// Run against the embedded assets in a disposable Linux xloom serve instance.
// Browser dependencies stay outside the product's zero-build static bundle.
test('embedded canvas handles live graph changes and unrestricted pointer movement', {
  skip: !process.env.XLOOM_WEB_URL, timeout:90000
}, async t => {
  const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
  const browser = await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE});
  t.after(() => browser.close());
  const page = await browser.newPage({viewport:{width:1440,height:960}});
  const errors = [];
  page.on('pageerror',error => errors.push(error.message));
  await page.goto(process.env.XLOOM_WEB_URL);
  await page.waitForFunction(() => typeof window.XLoomGraph === 'function');
  await page.evaluate(() => {
    const host = document.createElement('div'); host.id = 'canvas-probe';
    host.style.cssText = 'position:fixed;inset:0;z-index:10000;background:#faf9f5';
    document.body.append(host);
    window.probe = new XLoomGraph(host);
    window.probeState = {graph:{project:{id:'browser-graph',generation:0,status:'active'}},
      goals:[{id:'goal',condition:'真实目标',status:'open'}],
      steps:Array.from({length:60},(_,i) => ({id:'s'+i,description:'任务 '+i,goal_id:'goal',from:['origin'],result:'f'+i,status:'completed'})),
      fact_records:[{id:'origin',description:'项目输入',status:'input'},...Array.from({length:60},(_,i) => ({id:'f'+i,description:'证据 '+i,status:'valid'}))],
      findings:[],fact_relations:[{kind:'refutes',source:'f0',target:'f1',reason:'第一条说明'},
        {kind:'narrows',source:'f0',target:'f1',reason:'另一种关系'},
        {kind:'supersedes',source:'f1',target:'f0',reason:'有环'},
        {kind:'refutes',source:'missing',target:'f1',reason:'缺失引用'}]};
    const started = performance.now(); probe.setState(probeState);
    window.initialDuration = performance.now() - started;
  });
  await page.waitForFunction(() => document.querySelectorAll('#canvas-probe .graph-node').length === 122);
  await page.evaluate(() => new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
  const initial = await page.evaluate(() => ({duration:initialDuration,positions:[...probe.positions],camera:[probe.scale,probe.tx,probe.ty]}));
  assert.ok(initial.duration < 3000, `122-node initial update blocked for ${initial.duration}ms`);
  const update = await page.evaluate(async () => {
    const element = document.querySelector('#canvas-probe [data-node-key="fact:f0"]');
    probeState.fact_records.reverse(); probeState.steps.reverse();
    probeState.fact_records.find(f=>f.id==='f0').description = '变更标题，不重排';
    probeState.fact_relations[0].reason = '更新后的关系说明';
    probe.setState(probeState);
    await new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));
    return {positions:[...probe.positions],camera:[probe.scale,probe.tx,probe.ty],
      sameDOM:element===document.querySelector('#canvas-probe [data-node-key="fact:f0"]'),
      text:document.querySelector('#canvas-probe [data-node-key="fact:f0"]').textContent,
      details:probe.getEdgeDetails('edge:'+JSON.stringify(['refutes','fact:f0','fact:f1']))};
  });
  assert.deepEqual(new Map(update.positions),new Map(initial.positions));
  assert.deepEqual(update.camera,initial.camera);
  assert.equal(update.sameDOM,true);
  assert.match(update.text,/变更标题/);
  assert.match(JSON.stringify(update.details),/更新后的关系说明/);
  assert.equal(await page.locator('#canvas-probe .graph-edge-hit').count(),183);
  const filtered = await page.evaluate(async () => {
    probe.setStatusFilter('done');
    await new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));
    return {positions:[...probe.positions],camera:[probe.scale,probe.tx,probe.ty],count:probe.getVisibleNodeCount()};
  });
  assert.deepEqual(new Map(filtered.positions),new Map(initial.positions)); assert.deepEqual(filtered.camera,initial.camera);
  assert.equal(filtered.count,121); assert.equal(await page.locator('#canvas-probe .graph-node:visible').count(),121);
  assert.equal(await page.locator('#canvas-probe .graph-edge-hit:visible').count(),123);
  await page.locator('#canvas-probe .graph-viewport').focus(); await page.keyboard.press('Tab');
  assert.ok(await page.evaluate(()=>document.activeElement.dataset.edgeKey),'visible edge hit targets remain keyboard reachable');
  await page.evaluate(()=>probe.setStatusFilter('pending'));
  await page.waitForFunction(()=>document.querySelector('#canvas-probe [data-node-key="fact:f0"]').hidden);
  assert.equal(await page.locator('#canvas-probe .graph-node:visible').count(),1);
  assert.equal(await page.locator('#canvas-probe .graph-edge-hit:visible').count(),0);
  await page.locator('#canvas-probe .graph-viewport').focus(); await page.keyboard.press('0');
  const pendingFit = await page.evaluate(() => {
    const bounds = XLoomCanvas.worldBounds(new Map([['goal:goal',probe.positions.get('goal:goal')]]), {width:XLoomLayout.NODE_WIDTH,height:XLoomLayout.NODE_HEIGHT,baseWidth:0,baseHeight:0,padding:96});
    const expected = XLoomCanvas.fitTransform(bounds,1440,960);
    return {camera:[probe.scale,probe.tx,probe.ty],expected:[expected.scale,expected.tx,expected.ty]};
  });
  assert.deepEqual(pendingFit.camera,pendingFit.expected,'keyboard fit excludes all hidden cards');
  await page.keyboard.press('Tab');
  assert.equal(await page.evaluate(()=>document.activeElement.dataset.nodeKey),'goal:goal','keyboard navigation skips hidden nodes and edge hit targets');
  await page.evaluate(()=>probe.setStatusFilter('running'));
  await page.waitForFunction(()=>!document.querySelector('#canvas-probe .graph-data-empty').hidden);
  assert.equal(await page.locator('#canvas-probe .graph-node:visible').count(),0);
  await page.evaluate(()=>{probeState.steps.find(step=>step.id==='s0').status='running'; probe.setState(probeState);});
  await page.waitForFunction(()=>!document.querySelector('#canvas-probe [data-node-key="step:s0"]').hidden);
  assert.equal(await page.locator('#canvas-probe .graph-node:visible').count(),1);
  assert.equal(await page.locator('#canvas-probe .graph-edge-hit:visible').count(),0);
  await page.evaluate(()=>{probeState.steps.find(step=>step.id==='s0').status='completed'; probe.setState(probeState); probe.setStatusFilter('all');});
  await page.waitForFunction(()=>!document.querySelector('#canvas-probe [data-node-key="fact:f0"]').hidden);
  assert.equal(await page.locator('#canvas-probe .graph-node:visible').count(),122);
  assert.equal(await page.locator('#canvas-probe .graph-edge-hit:visible').count(),183);
  await page.evaluate(async () => {
    probe.positions.set('fact:f0',{x:-15000,y:-9000});
    probe.setState(probeState); probe.fit();
    await new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));
  });
  const far = await page.locator('#canvas-probe [data-node-key="fact:f0"]').boundingBox();
  assert.ok(far.x >= 0 && far.y >= 0 && far.x+far.width <= 1441 && far.y+far.height <= 961);
  await page.evaluate(() => {probe.zoomBy(12); probe.focusNode('fact:f0');});
  const node = page.locator('#canvas-probe [data-node-key="fact:f0"]');
  const rect = await node.boundingBox();
  await page.mouse.move(rect.x+rect.width/2,rect.y+rect.height/2);
  await page.mouse.down(); await page.mouse.move(8,8,{steps:8});
  const before = await page.evaluate(() => ({point:{...probe.positions.get('fact:f0')},tx:probe.tx,ty:probe.ty}));
  await page.waitForTimeout(220);
  const moving = await page.evaluate(() => ({point:{...probe.positions.get('fact:f0')},tx:probe.tx,ty:probe.ty}));
  assert.ok(moving.tx > before.tx && moving.ty > before.ty,'top/left edge pan must continue without pointer events');
  assert.ok(moving.point.x < before.point.x && moving.point.y < before.point.y,'drag permits negative world coordinates');
  await page.mouse.wheel(0,-100);
  await page.waitForTimeout(50);
  const grabbed = await node.boundingBox();
  assert.ok(grabbed.x <= 9 && grabbed.x+grabbed.width >= 7 && grabbed.y <= 9 && grabbed.y+grabbed.height >= 7,'zoom retains the pointer grab');
  await page.mouse.up();
  const stopped = await page.evaluate(()=>[probe.tx,probe.ty]);
  await page.waitForTimeout(130);
  assert.deepEqual(await page.evaluate(()=>[probe.tx,probe.ty]),stopped);
  await page.evaluate(()=>probe.focusNode('fact:f0'));
  const secondGrab = await node.boundingBox();
  await page.mouse.move(secondGrab.x+secondGrab.width/2,secondGrab.y+secondGrab.height/2);
  await page.mouse.down(); await page.mouse.move(1432,952,{steps:8});
  const rightBefore = await page.evaluate(()=>[probe.tx,probe.ty]);
  await page.waitForTimeout(220);
  const rightAfter = await page.evaluate(()=>[probe.tx,probe.ty]);
  assert.ok(rightAfter[0]<rightBefore[0] && rightAfter[1]<rightBefore[1],'right/bottom edge pan continues');
  await page.evaluate(()=>probe.viewport.dispatchEvent(new PointerEvent('pointercancel',{pointerId:probe.drag.pointerId})));
  await page.mouse.up();
  const cancelled = await page.evaluate(()=>[probe.tx,probe.ty]);
  await page.waitForTimeout(130);
  assert.deepEqual(await page.evaluate(()=>[probe.tx,probe.ty]),cancelled);
  const center = await page.evaluate(()=>({scale:probe.scale,x:(720-probe.tx)/probe.scale,y:(480-probe.ty)/probe.scale}));
  await page.setViewportSize({width:1280,height:800});
  await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
  const resized = await page.evaluate(()=>({scale:probe.scale,x:(640-probe.tx)/probe.scale,y:(400-probe.ty)/probe.scale}));
  assert.equal(resized.scale,center.scale);
  assert.ok(Math.abs(resized.x-center.x)<0.001 && Math.abs(resized.y-center.y)<0.001,'resize keeps world center');
  await page.setViewportSize({width:1440,height:960});
  await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
  const saved = await page.evaluate(()=>({point:probe.positions.get('fact:f0'),camera:[probe.scale,probe.tx,probe.ty]}));
  await page.evaluate(() => {probe.setState({graph:{project:{id:'other',generation:0}}}); probe.setState(probeState);});
  assert.deepEqual(await page.evaluate(()=>({point:probe.positions.get('fact:f0'),camera:[probe.scale,probe.tx,probe.ty]})),saved);
  await page.evaluate(()=>{probe.selectNode('fact:f0'); probeState.fact_records=probeState.fact_records.filter(f=>f.id!=='f0'); probe.setState(probeState);});
  assert.equal(await page.locator('#canvas-probe [data-node-key="fact:f0"]').count(),0);
  await page.evaluate(()=>{probeState.graph.project.generation=1; probe.setState(probeState);});
  assert.notDeepEqual(await page.evaluate(()=>[probe.scale,probe.tx,probe.ty]),saved.camera);
  await page.evaluate(()=>{probe.destroy(); document.getElementById('canvas-probe').remove();});
  assert.deepEqual(errors,[]);
  t.diagnostic(`122 nodes / 183 edges; initial synchronous update ${initial.duration.toFixed(1)} ms`);
});
