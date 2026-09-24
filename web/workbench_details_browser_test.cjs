'use strict';

// Loads the service's embedded assets while intercepting every project API call.
// Safe to run against a populated development service: no project data is written.
// XLOOM_WEB_URL=http://127.0.0.1:8000 node --test web/workbench_details_browser_test.cjs
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');

const created = '2026-09-24T02:00:00Z';
const boardHead = '浏览器折叠验收的黑板记录开头。';
const boardTail = '黑板记录末尾：展开后应完整可见。';
const systemHead = '浏览器折叠验收的系统记录开头。';
const systemTail = '系统记录末尾：展开后应完整可见。';
const boardBody = boardHead + '\n' + '这是应折叠显示的详细证据内容。'.repeat(120) + '\n' + boardTail;
const systemBody = systemHead + '\n' + '这是应折叠显示的执行错误详情。'.repeat(120) + '\n' + systemTail;

function fixture() {
  const project = {id:'ui-details-fixture',title:'日志与任务筛选浏览器验收',scenario:'ctf',status:'active',generation:0,created_at:created};
  const facts = [
    {id:'origin',description:'受控的浏览器测试输入',status:'input'},
    {id:'long',description:boardBody,status:'valid'},
    {id:'short',description:'简短的证据记录',status:'valid'},
  ];
  const steps = [
    {id:'done',description:'已经完成的任务',status:'completed',from:['origin'],result:'long'},
    {id:'running',description:'正在运行的任务',status:'running',from:['long']},
    {id:'pending',description:'等待执行的任务',status:'open',from:['short']},
    {id:'failed',description:'失败的任务',status:'failed',from:['origin']},
  ].map(step => ({...step,goal_id:'goal',created_at:created}));
  const state = {
    graph:{project,facts,intents:[],hints:[]},revision:1,
    goals:[{id:'goal',condition:'验证日志折叠、布局和状态筛选',status:'open',created_at:created}],
    steps,fact_records:facts,findings:[],fact_relations:[],
  };
  const runs = [{
    id:'long-system-run',project_id:project.id,generation:0,kind:'explore',intent:'failed',
    status:'failed',created_at:created,updated_at:created,result:{error:systemBody},
  }];
  return {state,runs};
}

test('workbench collapses long logs and filters cards without losing the canvas view', {
  timeout:90000,
  skip:process.env.XLOOM_WEB_URL ? false : 'Set XLOOM_WEB_URL to load the embedded workbench assets',
}, async t => {
  const base = new URL(process.env.XLOOM_WEB_URL);
  assert.ok(['http:','https:'].includes(base.protocol));
  const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
  const browser = await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined});
  const page = await browser.newPage({viewport:{width:1440,height:960},deviceScaleFactor:1});
  page.setDefaultTimeout(10000);
  const screenshots = path.resolve(__dirname,'../tmp/web-workbench-details');
  await fs.mkdir(screenshots,{recursive:true});
  const {state,runs} = fixture(), errors = [], writes = [], assetErrors = [];
  let stateUnavailable = false;
  const projectPath = '/projects/' + state.graph.project.id;
  page.on('pageerror', error => errors.push(error.message));
  page.on('response', response => {
    if (new URL(response.url()).pathname.startsWith('/static/') && response.status() >= 400) assetErrors.push(response.url());
  });
  await page.route('**/*', async route => {
    const request = route.request(), url = new URL(request.url()), pathname = url.pathname;
    if (url.origin !== base.origin || (!pathname.startsWith('/projects') && pathname !== '/ui/overview')) return route.continue();
    if (request.method() !== 'GET') {
      writes.push(request.method() + ' ' + pathname);
      return route.fulfill({status:405,contentType:'application/json',body:JSON.stringify({detail:'Browser regression fixture is read-only'})});
    }
    let body;
    if (pathname === '/projects') body = [state.graph.project];
    else if (pathname === '/ui/overview') body = {active_workers:1,observed_at:created};
    else if (pathname === projectPath + '/state') {
      if (stateUnavailable) return route.abort('failed');
      body = state;
    }
    else if (pathname === projectPath + '/state/events') body = [];
    else if (pathname === projectPath + '/executions') body = {items:runs,through:runs.length};
    else if (pathname === projectPath) body = state.graph;
    else {
      errors.push('Unexpected API read: ' + pathname);
      return route.fulfill({status:404,contentType:'application/json',body:'{"detail":"Unknown fixture endpoint"}'});
    }
    await route.fulfill({status:200,contentType:'application/json',body:JSON.stringify(body)});
  });
  const waitPaint = () => page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  const waitLayout = async () => {
    await waitPaint();
    await page.evaluate(() => Promise.allSettled(document.getAnimations()
      .filter(animation => Number.isFinite(animation.effect?.getComputedTiming().endTime))
      .map(animation => animation.finished)));
    await waitPaint();
  };
  const boardLog = () => page.locator('.timeline-entry[data-log-id="state:fact:long"]');
  const systemLog = () => page.locator('.system-entry').filter({hasText:systemHead});
  const visibleCards = () => page.locator('#graph-host .graph-node:visible');
  const view = () => page.evaluate(() => ({
    camera:document.querySelector('#graph-host .graph-world').style.transform,
    positions:[...document.querySelectorAll('#graph-host .graph-node')].map(node => [node.dataset.nodeKey,node.style.left,node.style.top]),
  }));

  try {
    await page.goto(new URL('/?project=' + state.graph.project.id,base).href,{waitUntil:'networkidle'});
    await page.waitForFunction(() => document.querySelector('#toggle-running')?.disabled === false);
    await page.waitForFunction(() => document.querySelectorAll('#graph-host .graph-node').length === 8);
    await waitPaint();

    await t.test('compact desktop navigation and aligned separators persist at narrower window widths', async () => {
      assert.equal(await page.locator('#project-goal').count(),0);
      assert.equal(await page.locator('.canvas-help').count(),0);
      assert.equal(await page.locator('#mobile-menu, #sidebar-scrim, #connection-state, #refresh-project, #about-button, #about-dialog').count(),0);
      assert.ok(!(await page.locator('.main-pane').innerText()).includes('自由延展'));
      for (const width of [1920,1440,1280,1024,800]) {
        await page.setViewportSize({width,height:960}); await waitLayout();
        const rectangles = await page.evaluate(() => ({
          topbar:document.querySelector('.topbar').getBoundingClientRect().toJSON(),
          breadcrumbs:document.querySelector('.breadcrumbs').getBoundingClientRect().toJSON(),
          projectHeading:document.querySelector('.project-heading').getBoundingClientRect().toJSON(),
          activityHeading:document.querySelector('.activity-heading').getBoundingClientRect().toJSON(),
          graphTop:document.querySelector('.graph-stage').getBoundingClientRect().top,
          tabsBottom:document.querySelector('.activity-tabs').getBoundingClientRect().bottom,
          sidebar:document.querySelector('.sidebar').getBoundingClientRect().toJSON(),
          main:document.querySelector('.main-pane').getBoundingClientRect().toJSON(),
          activity:document.querySelector('.activity-pane').getBoundingClientRect().toJSON(),
          documentWidth:document.documentElement.scrollWidth,
          bodyWidth:document.body.getBoundingClientRect().width,
        }));
        assert.ok(rectangles.topbar.height <= 40,`navigation leaves excessive top whitespace at ${width}px`);
        assert.ok(rectangles.breadcrumbs.height > 0 && rectangles.breadcrumbs.top >= rectangles.topbar.top + 1
          && rectangles.breadcrumbs.bottom <= rectangles.topbar.bottom - 1,'breadcrumb text fits inside the compact navigation');
        assert.ok(Math.abs(rectangles.projectHeading.top - rectangles.topbar.bottom) <= 1,
          'project heading follows navigation without an empty gap');
        assert.ok(Math.abs(rectangles.activityHeading.top - rectangles.topbar.bottom) <= 1,
          'activity heading follows navigation without an empty gap');
        assert.ok(Math.abs(rectangles.graphTop - rectangles.tabsBottom) <= 1,
          `separators differ at ${width}px: ${JSON.stringify(rectangles)}`);
        assert.equal(await page.locator('#sidebar').isVisible(),true,'project navigation remains visible');
        assert.ok(rectangles.sidebar.left >= 0 && rectangles.sidebar.width > 0,'sidebar stays in the desktop layout');
        assert.ok(Math.abs(rectangles.main.left - rectangles.sidebar.right) <= 1,'main pane follows the sidebar');
        assert.ok(Math.abs(rectangles.activity.left - rectangles.main.right) <= 1,'logs remain beside the canvas');
        assert.ok(Math.abs(rectangles.activity.top - rectangles.main.top) <= 1,'logs never stack below the canvas');
        if (width >= 1024) assert.ok(rectangles.documentWidth <= width + 1,`desktop overflow at ${width}px`);
        else assert.ok(rectangles.bodyWidth >= 1024,'narrow windows retain the desktop minimum width');
        await fs.writeFile(path.join(screenshots,`layout-${width}.json`),JSON.stringify(rectangles,null,2));
        await page.screenshot({path:path.join(screenshots,`workbench-${width}.png`),fullPage:true});
      }
      await page.setViewportSize({width:1440,height:960}); await waitPaint();
    });

    await t.test('automatic polling reports network errors and restores actions after recovery', async () => {
      const original = await view();
      stateUnavailable = true;
      try {
        await page.locator('#workspace-error').waitFor({state:'visible'});
        assert.ok((await page.locator('#workspace-error').innerText()).trim());
        assert.equal(await page.locator('#toggle-running').isDisabled(),true);
        assert.equal(await page.locator('#add-hint').isDisabled(),true);
        // The error banner resizes the canvas and recenters its camera; graph coordinates stay intact.
        assert.deepEqual((await view()).positions,original.positions,'network failures preserve the last rendered graph');
      } finally {
        stateUnavailable = false;
      }
      await page.locator('#workspace-error').waitFor({state:'hidden'});
      await waitLayout();
      assert.equal(await page.locator('#toggle-running').isDisabled(),false);
      assert.equal(await page.locator('#add-hint').isDisabled(),false);
      assert.deepEqual(await view(),original,'polling recovery preserves the canvas');
    });

    await t.test('long blackboard logs start collapsed, preserve expanded text through polling and tab switches', async () => {
      const article = boardLog(), toggle = article.locator('.log-toggle');
      assert.equal(await toggle.getAttribute('aria-expanded'),'false');
      assert.ok((await article.innerText()).includes(boardHead));
      assert.ok(!(await article.innerText()).includes(boardTail),'collapsed preview must conceal the tail');
      assert.equal(await page.locator('.timeline-entry[data-log-id="state:fact:short"] .log-toggle').count(),0);
      await toggle.click();
      assert.equal(await toggle.getAttribute('aria-expanded'),'true');
      assert.ok((await article.innerText()).includes(boardTail));
      await article.evaluate(node => node.scrollIntoView({block:'start'}));
      await page.screenshot({path:path.join(screenshots,'workbench-board-expanded.png'),fullPage:true});
      const oldHeight = await article.evaluate(node => node.getBoundingClientRect().height);
      state.revision++;
      state.graph.hints.push({id:'poll-marker',content:'轮询新增记录',created_at:'2026-09-24T02:01:00Z',creator:'user'});
      await page.locator('.timeline-entry[data-log-id="hint:poll-marker"]').waitFor();
      assert.equal(await boardLog().locator('.log-toggle').getAttribute('aria-expanded'),'true');
      await page.locator('#tab-result').click(); await page.locator('#tab-board').click();
      assert.equal(await boardLog().locator('.log-toggle').getAttribute('aria-expanded'),'true');
      assert.ok((await boardLog().innerText()).includes(boardTail));
      await boardLog().locator('.log-toggle').click();
      assert.equal(await boardLog().locator('.log-toggle').getAttribute('aria-expanded'),'false');
      const previewHeight = await boardLog().evaluate(node => node.getBoundingClientRect().height);
      assert.ok(previewHeight < oldHeight / 2,'collapse should meaningfully reduce a long entry height');
    });

    await t.test('system logs have independent accessible previews that survive refreshes', async () => {
      await page.locator('#tab-system').click();
      const toggle = systemLog().locator('.log-toggle');
      assert.equal(await toggle.getAttribute('aria-expanded'),'false');
      assert.ok((await systemLog().innerText()).includes(systemHead));
      assert.ok(!(await systemLog().innerText()).includes(systemTail));
      assert.equal(await page.locator('.system-entry').filter({hasText:'项目已创建'}).locator('.log-toggle').count(),0);
      await toggle.click();
      assert.equal(await systemLog().locator('.log-toggle').getAttribute('aria-expanded'),'true');
      assert.ok((await systemLog().innerText()).includes(systemTail));
      await systemLog().evaluate(node => node.scrollIntoView({block:'start'}));
      await page.screenshot({path:path.join(screenshots,'workbench-system-expanded.png'),fullPage:true});
      state.revision++;
      runs.push({...runs[0],id:'poll-system-marker',result:{error:'轮询新增的简短错误'},updated_at:'2026-09-24T02:02:00Z'});
      await page.locator('.system-entry').filter({hasText:'轮询新增的简短错误'}).waitFor();
      assert.equal(await systemLog().locator('.log-toggle').getAttribute('aria-expanded'),'true');
      await page.locator('#tab-board').click(); await page.locator('#tab-system').click();
      assert.equal(await systemLog().locator('.log-toggle').getAttribute('aria-expanded'),'true');
      await systemLog().locator('.log-toggle').click();
      assert.ok(!(await systemLog().innerText()).includes(systemTail));
    });

    await t.test('legend filters show matching cards and restore all cards without changing positions or camera', async () => {
      await page.locator('#tab-board').click(); await waitPaint();
      const original = await view();
      assert.equal(await visibleCards().count(),8);
      for (const [status,count] of [['done',4],['running',1],['pending',2]]) {
        const button = page.locator(`[data-status-filter="${status}"]`);
        await button.click(); await waitPaint();
        assert.equal(await button.getAttribute('aria-pressed'),'true');
        assert.equal(await visibleCards().count(),count,`${status} visible card count`);
        assert.ok(await visibleCards().evaluateAll((nodes,status) => nodes.every(node => node.classList.contains(status)),status));
        const edgeVisibility = await page.evaluate(() => {
          const keys = new Set([...document.querySelectorAll('#graph-host .graph-node')].filter(node => !node.hidden).map(node => node.dataset.nodeKey));
          return [...document.querySelectorAll('#graph-host .graph-edge-hit')].map(hit => {
            const [,source,target] = JSON.parse(hit.dataset.edgeKey.slice('edge:'.length));
            return {shown:getComputedStyle(hit.parentElement).display !== 'none',expected:keys.has(source) && keys.has(target)};
          });
        });
        assert.ok(edgeVisibility.length > 0,'fixture must exercise graph relations');
        assert.ok(edgeVisibility.every(edge => edge.shown === edge.expected),'edges must follow visibility of both endpoints');
        assert.deepEqual(await view(),original,'filtering must preserve coordinates and camera');
        await button.click(); await waitPaint();
        assert.equal(await button.getAttribute('aria-pressed'),'false');
        assert.equal(await visibleCards().count(),8);
        assert.deepEqual(await view(),original);
      }
    });

    await t.test('following evidence reveals its card even when the selected status filter hides it', async () => {
      const pending = page.locator('[data-status-filter="pending"]');
      await pending.click(); await waitPaint();
      assert.equal(await page.locator('[data-node-key="fact:long"]').isVisible(),false);
      await boardLog().locator('.entry-node').last().click(); await waitPaint();
      assert.equal(await pending.getAttribute('aria-pressed'),'false');
      assert.equal(await visibleCards().count(),8);
      assert.equal(await page.locator('[data-node-key="fact:long"]').isVisible(),true);
      assert.equal(await page.locator('#node-inspector').isVisible(),true);
    });

    assert.deepEqual(writes,[],'regression must never mutate the backing service');
    assert.deepEqual(assetErrors,[]);
    assert.deepEqual(errors,[]);
  } finally {
    await browser.close();
  }
});
