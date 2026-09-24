'use strict';

// Start xloom serve with a dedicated empty database, then run:
// XLOOM_WEB_URL=http://127.0.0.1:18767 node --test web/browser_test.cjs
// PLAYWRIGHT_MODULE and PLAYWRIGHT_CHROMIUM_EXECUTABLE optionally select local dependencies.
// This test creates real projects and removes only the projects it created.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');

test('workbench persists project operations through the real HTTP service', {
  timeout: 150000,
  skip: process.env.XLOOM_WEB_URL ? false : 'Set XLOOM_WEB_URL to a dedicated empty xloom serve instance',
}, async t => {
  const base = new URL(process.env.XLOOM_WEB_URL);
  assert.ok(['http:', 'https:'].includes(base.protocol), 'XLOOM_WEB_URL must be an HTTP service');
  const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
  const projectPath = id => '/projects/' + encodeURIComponent(id);
  const created = new Map();
  const screenshots = path.resolve(__dirname, '../tmp/web-workbench-integration');
  const request = async (endpoint, options = {}) => {
    const response = await fetch(new URL(endpoint, base), {signal: AbortSignal.timeout(10000), ...options});
    const body = await response.text();
    assert.ok(response.ok, `${options.method || 'GET'} ${endpoint}: HTTP ${response.status} ${body}`);
    return body ? JSON.parse(body) : null;
  };
  assert.deepEqual(await request('/projects'), [], 'use a dedicated empty database; existing projects are never removed');

  const browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined});
  const context = await browser.newContext({viewport: {width: 1440, height: 960}, deviceScaleFactor: 1});
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  const browserErrors = [];
  const resourceErrors = [];
  let expectedHintFailure = '';
  page.on('pageerror', error => browserErrors.push(error.message));
  page.on('console', message => {
    if (message.type() !== 'error') return;
    if (message.location().url === expectedHintFailure && /status of 503/.test(message.text())) return;
    browserErrors.push(message.text());
  });
  page.on('response', response => {
    if (new URL(response.url()).pathname.startsWith('/static/') && response.status() >= 400) {
      resourceErrors.push(`${response.status()} ${response.url()}`);
    }
  });
  const waitText = (id, text) => page.waitForFunction(({id, text}) => document.getElementById(id)?.textContent.trim() === text, {id, text: String(text)});
  const waitCounts = async (total, active) => { await waitText('project-count', total); await waitText('running-project-count', active); };
  const row = title => page.locator('#project-list .project-row').filter({has: page.getByText(title, {exact: true})});
  const select = async project => { await row(project.title).locator('.project-item').click(); await waitText('project-title', project.title); };
  const action = async (project, name) => {
    const target = row(project.title);
    await target.locator('summary').click();
    await target.getByRole('button', {name}).click();
  };
  const waitWrite = (endpoint, method) => page.waitForResponse(response => new URL(response.url()).pathname === endpoint && response.request().method() === method);
  const expectState = async (project, status) => {
    const graph = await request(projectPath(project.id));
    assert.equal(graph.project.status, status);
    return graph;
  };
  const noOverflow = async () => {
    const sizes = await page.evaluate(() => ({viewport: innerWidth, document: document.documentElement.scrollWidth, body: document.body.scrollWidth}));
    assert.ok(sizes.document <= sizes.viewport + 1 && sizes.body <= sizes.viewport + 1, `horizontal overflow: ${JSON.stringify(sizes)}`);
  };
  const confirmAction = async (project, name, endpoint) => {
    await action(project, name);
    await page.locator('#confirm-dialog').waitFor({state: 'visible'});
    const response = waitWrite(endpoint, name.test('删除') ? 'DELETE' : 'POST');
    await page.locator('#confirm-action').click();
    const result = await response;
    assert.ok(result.ok(), `${name}: HTTP ${result.status()}`);
    await page.locator('#confirm-dialog').waitFor({state: 'hidden'});
    return result;
  };

  try {
    await t.test('empty service shows an empty graph with no demo projects', async () => {
      await page.goto(base.href, {waitUntil: 'networkidle'});
      await waitCounts(0, 0);
      await page.locator('#graph-empty').waitFor({state: 'visible'});
      assert.equal(await page.locator('#project-list .project-row').count(), 0);
      assert.equal(await page.locator('#graph-host .graph-node').count(), 0);
      assert.match(await page.title(), /X-Loom/);
      const scripts = await page.locator('script[src]').evaluateAll(elements => elements.map(element => element.getAttribute('src')));
      assert.ok(scripts.every(source => source.startsWith('/static/')));
      assert.ok(!scripts.some(source => /model\.js|cytoscape|dagre/.test(source)));
    });

    await t.test('three scenarios persist after reload and retain complete creation timestamps', async () => {
      for (const scenario of ['ctf', 'pentest', 'audit']) {
        const title = `Browser ${scenario} ${process.pid}`;
        await page.locator('#new-project').click();
        await page.locator(`#scenario-options input[value="${scenario}"]`).check();
        await page.locator('#create-name').fill(title);
        await page.locator('#create-origin').fill('Controlled local browser acceptance input.');
        await page.locator('#create-goal').fill('Verify persisted project management without starting a worker.');
        const response = waitWrite('/projects', 'POST');
        await page.locator('#submit-create').click();
        const result = await response;
        assert.equal(result.status(), 201);
        const graph = await result.json();
        created.set(graph.project.id, graph.project);
        assert.equal(graph.project.scenario, scenario);
        assert.equal(graph.project.bootstrap_enabled, false);
        await page.locator('#create-dialog').waitFor({state: 'hidden'});
        await waitText('project-title', title);
      }
      await waitCounts(3, 3);
      await page.reload({waitUntil: 'networkidle'});
      await waitCounts(3, 3);
      const stored = await request('/projects');
      assert.deepEqual(stored.map(project => project.scenario).sort(), ['audit', 'ctf', 'pentest']);
      for (const project of created.values()) {
        const stamp = new Date(Date.parse(project.created_at) + 8 * 3600000).toISOString().slice(0, 19).replace('T', ' ');
        assert.ok((await row(project.title).innerText()).includes(stamp), `missing Shanghai creation time ${stamp}`);
      }
      const current = [...created.values()].at(-1);
      await waitText('project-title', current.title);
      const state = await request(projectPath(current.id) + '/state');
      const expectedKeys = [['goal', state.goals], ['step', state.steps], ['fact', state.fact_records], ['finding', state.findings]]
        .flatMap(([type, entities]) => (entities || []).map(entity => `${type}:${entity.id}`)).sort();
      await page.waitForFunction(count => document.querySelectorAll('#graph-host .graph-node').length === count, expectedKeys.length);
      const keys = await page.locator('#graph-host .graph-node').evaluateAll(nodes => nodes.map(node => node.dataset.nodeKey).sort());
      assert.deepEqual(keys, expectedKeys, 'canvas must show exactly the entities returned by the real state endpoint');
    });

    await t.test('search does not alter global project and running counts', async () => {
      await page.locator('#project-search').fill('unmatched-browser-search');
      assert.equal(await page.locator('#project-list .project-row').count(), 0);
      await waitCounts(3, 3);
      await page.locator('#project-search').fill('');
      assert.equal(await page.locator('#project-list .project-row').count(), 3);
    });

    const project = [...created.values()].at(-1);
    assert.ok(project, 'project creation did not complete');
    await t.test('pause and continue use the backend lifecycle states', async () => {
      await select(project);
      let response = waitWrite(projectPath(project.id) + '/status', 'PUT');
      await page.locator('#toggle-running').click();
      assert.equal((await response).request().postDataJSON().status, 'stopped');
      await waitCounts(3, 2);
      await expectState(project, 'stopped');
      response = waitWrite(projectPath(project.id) + '/status', 'PUT');
      await page.locator('#toggle-running').click();
      assert.equal((await response).request().postDataJSON().status, 'active');
      await waitCounts(3, 3);
      await expectState(project, 'active');
    });

    await t.test('failed hints retain drafts and repeated submits cause only one persisted hint', async () => {
      await page.locator('#add-hint').click();
      const draft = 'Preserve this draft after a controlled service error.';
      await page.locator('#hint-input').fill(draft);
      const hintPath = projectPath(project.id) + '/hints';
      expectedHintFailure = new URL(hintPath, base).href;
      let releaseFailure;
      const failureGate = new Promise(resolve => { releaseFailure = resolve; });
      let failedRequests = 0;
      const matcher = url => url.pathname === hintPath;
      const failOnce = async route => {
        if (route.request().method() !== 'POST') return route.continue();
        failedRequests++;
        await failureGate;
        await route.fulfill({status: 503, contentType: 'application/json', body: JSON.stringify({detail: 'Controlled hint failure'})});
      };
      await page.route(matcher, failOnce);
      try {
        const requested = page.waitForRequest(request => new URL(request.url()).pathname === hintPath && request.method() === 'POST');
        await page.locator('#send-hint').click();
        await requested;
        assert.equal(await page.locator('#send-hint').isDisabled(), true);
        await page.locator('#hint-form').evaluate(form => { form.requestSubmit(); form.requestSubmit(); });
        releaseFailure();
        await waitText('hint-error', 'Controlled hint failure');
        assert.equal(failedRequests, 1, 'a pending hint must reject duplicate submits');
        assert.equal(await page.locator('#hint-input').inputValue(), draft);
        assert.equal(await page.locator('#hint-dialog').isVisible(), true);
        assert.equal((await request(projectPath(project.id))).hints.length, 0);
      } finally {
        releaseFailure();
        await page.unroute(matcher, failOnce);
      }

      let releaseSuccess;
      const successGate = new Promise(resolve => { releaseSuccess = resolve; });
      let arrived;
      const arrivedAtServer = new Promise(resolve => { arrived = resolve; });
      let successfulRequests = 0;
      const delayResponse = async route => {
        if (route.request().method() !== 'POST') return route.continue();
        successfulRequests++;
        const response = await route.fetch();
        arrived();
        await successGate;
        await route.fulfill({response});
      };
      await page.route(matcher, delayResponse);
      try {
        await page.locator('#send-hint').click();
        await arrivedAtServer;
        assert.equal(await page.locator('#send-hint').isDisabled(), true);
        await page.locator('#hint-form').evaluate(form => { form.requestSubmit(); form.requestSubmit(); });
        releaseSuccess();
        await page.locator('#hint-dialog').waitFor({state: 'hidden'});
        const hints = (await request(projectPath(project.id))).hints;
        assert.equal(successfulRequests, 1);
        assert.equal(hints.length, 1);
        assert.equal(hints[0].content, draft);
        assert.equal(hints[0].creator, 'user');
      } finally {
        releaseSuccess();
        await page.unroute(matcher, delayResponse);
      }
    });

    await t.test('restart archives the prior generation and preserves project inputs', async () => {
      const before = await request(projectPath(project.id));
      const generation = before.project.generation || 0;
      const response = await confirmAction(project, /重启/, projectPath(project.id) + '/restart');
      assert.deepEqual(response.request().postDataJSON(), {expected_generation: generation});
      const after = await expectState(project, 'active');
      assert.equal(after.project.generation, generation + 1);
      assert.equal(after.project.created_at, before.project.created_at);
      assert.deepEqual(after.hints, before.hints);
      const rounds = await request(projectPath(project.id) + '/rounds');
      assert.ok(rounds.items.some(round => round.generation === generation));
      await page.reload({waitUntil: 'networkidle'});
      await waitText('project-title', project.title);
      assert.ok((await page.locator('#round-label').innerText()).includes(String(after.project.generation + 1)));
    });

    await t.test('termination preserves evidence and never displays a completed result', async () => {
      const before = await request(projectPath(project.id));
      const response = await confirmAction(project, /终止/, projectPath(project.id) + '/terminate');
      assert.deepEqual(response.request().postDataJSON(), {expected_generation: before.project.generation || 0});
      const after = await expectState(project, 'terminated');
      assert.deepEqual(after.facts, before.facts);
      await waitCounts(3, 2);
      await waitText('project-status', '已终止');
      assert.equal(await page.locator('#add-hint').isDisabled(), true);
      await page.locator('#tab-result').click();
      const result = await page.locator('#activity-content').innerText();
      assert.match(result, /终止/);
      assert.doesNotMatch(result, /项目已完成|目标已完成|已成功完成/);
      await page.locator('#tab-system').click();
      assert.match(await page.locator('#activity-content').innerText(), /暂未接入/);
    });

    await t.test('result exports use same-origin real YAML and timeline text', async () => {
      await page.locator('#tab-result').click();
      for (const format of ['yaml', 'timeline']) {
        const link = page.locator(`.export-links a[href$="format=${format}"]`);
        const target = new URL(await link.getAttribute('href'), base);
        assert.equal(target.origin, base.origin);
        assert.equal(target.pathname, projectPath(project.id) + '/export');
        assert.equal(target.searchParams.get('format'), format);
        assert.ok(await link.getAttribute('download'));
        const response = await fetch(target, {signal: AbortSignal.timeout(10000)});
        assert.equal(response.status, 200);
        assert.match(response.headers.get('content-type'), /^text\/plain/);
        const body = await response.text();
        assert.ok(body.includes('Controlled local browser acceptance input.'));
        if (format === 'yaml') assert.ok(body.includes(project.title));
        else {
          assert.ok(body.includes(project.id));
          assert.match(body, /CURRENT FGS generation=1 .*status=terminated/);
        }
      }
    });

    await t.test('desktop layouts retain all three columns without optional topbar controls', async () => {
      await fs.mkdir(screenshots, {recursive: true});
      assert.equal(await page.locator('#mobile-menu, #sidebar-scrim, #connection-state, #refresh-project, #about-button, #about-dialog').count(), 0);
      for (const width of [1920, 1440, 1280, 1024]) {
        await page.setViewportSize({width, height: 960});
        await noOverflow();
        assert.equal(await page.locator('#sidebar').isVisible(), true);
        assert.equal(await page.locator('.main-pane').isVisible(), true);
        assert.equal(await page.locator('.activity-pane').isVisible(), true);
      }
      await page.setViewportSize({width: 1440, height: 960});
      await page.screenshot({path: path.join(screenshots, 'desktop.png'), fullPage: true, animations: 'disabled'});
    });

    await t.test('more than ten projects and unclassified legacy metadata remain real and searchable', async () => {
      let legacy;
      for (let index = 0; index < 10; index++) {
        const payload = {title: `Browser extra ${index} ${process.pid}`, origin: 'Controlled project-count acceptance input.', goal: 'Check real list metadata.'};
        if (index < 9) payload.scenario = ['ctf', 'pentest', 'audit'][index % 3];
        const graph = await request('/projects', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(payload)});
        created.set(graph.project.id, graph.project);
        if (index === 9) legacy = graph.project;
      }
      assert.equal(legacy.scenario, undefined, 'the server must not invent a scenario for omitted metadata');
      await waitCounts(13, 12);
      assert.equal(await page.locator('#project-list .project-row').count(), 13);
      await select(legacy);
      await waitText('project-type', '未分类');
      assert.match(await row(legacy.title).locator('.project-item').getAttribute('aria-label'), /未分类/);
      await page.locator('#project-search').fill('未分类');
      assert.equal(await page.locator('#project-list .project-row').count(), 1);
      await waitCounts(13, 12);
      await page.locator('#project-search').fill('unmatched-browser-search');
      assert.equal(await page.locator('#project-list .project-row').count(), 0);
      await waitCounts(13, 12);
      await page.locator('#project-search').fill('');
      assert.equal(await page.locator('#project-list .project-row').count(), 13);
    });

    await t.test('delete removes each real project and the final project returns to empty state', async () => {
      for (const target of [...created.values()]) {
        const response = await confirmAction(target, /删除/, projectPath(target.id));
        assert.equal(response.status(), 204);
        created.delete(target.id);
        const missing = await fetch(new URL(projectPath(target.id), base));
        assert.equal(missing.status, 404);
      }
      await waitCounts(0, 0);
      await page.locator('#graph-empty').waitFor({state: 'visible'});
      assert.equal(await page.locator('#graph-host .graph-node').count(), 0);
      assert.deepEqual(await request('/projects'), []);
      await page.reload({waitUntil: 'networkidle'});
      await waitCounts(0, 0);
    });

    await t.test('embedded resources and scripts produce no browser errors', () => {
      assert.deepEqual(resourceErrors, []);
      assert.deepEqual(browserErrors, []);
    });
  } finally {
    await context.close();
    await browser.close();
    for (const id of created.keys()) await request(projectPath(id), {method: 'DELETE'});
  }
});
