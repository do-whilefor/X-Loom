(function () {
  'use strict';
  const data = window.XLoomData, api = new window.XLoomAPI.Client(), requests = new window.XLoomAPI.RequestScope();
  const $ = id => document.getElementById(id), NS = 'http://www.w3.org/2000/svg';
  const eventCache = new Map(), drafts = new Map(), expandedLogs = new Set();
  let logBodyId = 0;
  let projects = [], state = null, executions = [], events = [], logs = [], selectedId = '';
  let selectedNode = null, selectedEdge = null, tab = 'board', systemFilter = 'all', logLimit = 300;
  let timer, toastTimer, management = null, hintProjectId = '', mutating = false, connected = false;
  let projectsSignature = '', activitySignature = '', createDraft = false, updatingGraph = false;
  try { selectedId = new URLSearchParams(location.search).get('project') || localStorage.getItem('xloom.selected-project') || ''; } catch {}
  const pathFor = id => '/projects/' + encodeURIComponent(id);
  const current = () => state?.graph?.project || projects.find(project => project.id === selectedId);
  const nodeKey = node => node ? node.key || node.type + ':' + node.id : '';
  const cssStatus = status => status === 'stopped' ? 'paused' : status;
  const scenarioIcon = project => ({pentest:'shield',audit:'code',ctf:'flag'})[project?.scenario] || 'graph';
  const graph = new window.XLoomGraph($('graph-host'), {onSelect:selectNode, onSelectEdge:selectEdge});

  function el(tag, cls, text) {
    const node = document.createElement(tag); if (cls) node.className = cls; if (text !== undefined) node.textContent = text; return node;
  }
  function icon(name) {
    const svg = document.createElementNS(NS, 'svg'), use = document.createElementNS(NS, 'use');
    svg.setAttribute('class', 'icon'); svg.setAttribute('aria-hidden', 'true'); use.setAttribute('href', '#i-' + name); svg.append(use); return svg;
  }
  function timestamp(value) {
    const time = el('time', '', data.formatTime(value)); if (value && Number.isFinite(Date.parse(value))) time.dateTime = value; return time;
  }
  function toast(message) {
    clearTimeout(toastTimer); $('toast').textContent = message; $('toast').hidden = false; toastTimer = setTimeout(() => { $('toast').hidden = true; }, 4500);
  }
  function closeMenus() { document.querySelectorAll('.project-menu[open]').forEach(menu => { menu.open = false; }); }
  function hideSidebar() { $('sidebar').classList.remove('open'); $('sidebar-scrim').hidden = true; $('mobile-menu').setAttribute('aria-expanded', 'false'); }
  function setConnection(ok, message = '') {
    connected = ok; $('connection-state').textContent = ok ? '已连接' : '连接中断'; $('connection-state').dataset.status = ok ? 'connected' : 'error';
    $('workspace-error').hidden = !message; $('workspace-error').textContent = message; renderHeader(); renderProjects();
  }
  function renderProjects() {
    const query = $('project-search').value.trim().toLocaleLowerCase();
    $('project-count').textContent = projects.length; $('running-project-count').textContent = projects.filter(project => project.status === 'active').length;
    const matches = projects.filter(project => (project.title + data.scenarioName(project.scenario)).toLocaleLowerCase().includes(query));
    const signature = JSON.stringify([matches, selectedId, mutating, connected]); if (signature === projectsSignature) return; projectsSignature = signature;
    $('project-list').replaceChildren(...matches.map(project => {
      const row = el('div', 'project-row' + (project.id === selectedId ? ' selected' : '')); row.dataset.projectId = project.id;
      const button = el('button', 'project-item'), mark = el('span', 'project-symbol'), copy = el('span', 'project-copy'), meta = el('small');
      button.type = 'button'; button.dataset.projectId = project.id; button.title = project.title; button.disabled = mutating;
      button.setAttribute('aria-current', String(project.id === selectedId)); button.setAttribute('aria-label', project.title + '，' + data.scenarioName(project.scenario)); mark.append(icon(scenarioIcon(project)));
      meta.append(el('i', 'status-dot ' + cssStatus(project.status)), el('span', '', data.statusName(project.status)));
      const created = timestamp(project.created_at); created.className = 'project-created'; created.title = '项目创建时间（上海时区）';
      copy.append(el('strong', '', project.title), meta, created); button.append(mark, copy); button.addEventListener('click', () => selectProject(project.id));
      const menu = el('details', 'project-menu'), summary = el('summary'), panel = el('div', 'project-menu-panel');
      summary.setAttribute('aria-label', '项目操作：' + project.title); summary.append(icon('more'));
      const actions = project.status === 'active' ? ['pause','restart','terminate','delete'] : project.status === 'stopped' ? ['resume','restart','terminate','delete'] : ['restart','delete'];
      const labels = {pause:'暂停项目',resume:'继续项目',restart:'重启项目',terminate:'终止项目',delete:'删除项目'}, icons = {pause:'pause',resume:'play',restart:'restart',terminate:'stop',delete:'trash'};
      for (const action of actions) {
        const item = el('button', action === 'delete' ? 'danger' : ''); item.type = 'button'; item.dataset.action = action; item.disabled = mutating || !connected;
        item.append(icon(icons[action]), document.createTextNode(labels[action]));
        item.addEventListener('click', () => { menu.open = false; if (action === 'pause' || action === 'resume') changeStatus(project.id, action); else confirmOperation(project.id, action); }); panel.append(item);
      }
      menu.append(summary, panel); menu.addEventListener('toggle', () => {
        if (!menu.open) return; document.querySelectorAll('.project-menu[open]').forEach(other => { if (other !== menu) other.open = false; });
        const rect = summary.getBoundingClientRect(); panel.style.left = Math.max(8, Math.min(innerWidth - 164, rect.right - 156)) + 'px'; panel.style.top = Math.max(8, Math.min(innerHeight - panel.offsetHeight - 8, rect.bottom + 5)) + 'px';
      });
      menu.addEventListener('keydown', event => { if (event.key === 'Escape') { menu.open = false; summary.focus(); } }); row.append(button, menu); return row;
    }));
    if (!matches.length) $('project-list').append(el('p', 'empty-projects', projects.length ? '没有找到匹配的项目。' : '还没有项目，创建一个开始探索。'));
  }
  function renderHeader() {
    const project = current(), ended = project && ['completed','terminated'].includes(project.status);
    $('breadcrumb-title').textContent = project?.title || '我的项目'; $('project-title').textContent = project?.title || (connected ? '准备好开始探索了吗？' : '正在加载项目');
    $('project-type').replaceChildren(); if (project) $('project-type').append(icon(scenarioIcon(project)), document.createTextNode(data.scenarioName(project.scenario)));
    $('project-status').textContent = project ? data.statusName(project.status) : ''; $('project-status').className = 'status-badge ' + cssStatus(project?.status || ''); $('project-status').hidden = !project;
    $('round-label').textContent = project ? '第 ' + ((project.generation || 0) + 1) + ' 轮探索' : '新的探索';
    const timing = data.projectTiming(state, executions), progress = data.taskProgress(state);
    $('project-start').textContent = project ? '开始 ' + (timing.startedAt ? data.formatTime(timing.startedAt) : '尚未开始') : '等待创建项目';
    if (timing.startedAt) $('project-start').dateTime = timing.startedAt; else $('project-start').removeAttribute('datetime');
    $('project-end').textContent = ended ? '结束 ' + (timing.endedAt ? data.formatTime(timing.endedAt) : '未记录时间') : '';
    const action = project?.status === 'stopped' ? 'resume' : ended ? 'restart' : 'pause', toggle = $('toggle-running');
    toggle.dataset.action = action; toggle.replaceChildren(icon({pause:'pause',resume:'play',restart:'restart'}[action]), document.createTextNode({pause:'暂停',resume:'继续',restart:'重启'}[action])); toggle.disabled = !project || mutating || !connected;
    $('add-hint').disabled = !project || ended || mutating || !connected; $('new-project').disabled = $('empty-create').disabled = mutating; $('refresh-project').disabled = mutating;
    $('node-count').textContent = graph.getVisibleNodeCount() + ' 个节点'; $('task-progress').textContent = progress.completed + ' / ' + progress.total + ' 个任务已完成'; $('progress-fill').style.width = progress.total ? (progress.completed / progress.total * 100) + '%' : '0%';
    const statusFilter = graph.getStatusFilter(), filterLabel = {done:'已完成',running:'运行中',pending:'待执行'}[statusFilter];
    document.querySelectorAll('[data-status-filter]').forEach(button => { button.setAttribute('aria-pressed', String(button.dataset.statusFilter === statusFilter)); button.disabled = !state; });
    $('canvas-caption').textContent = !project ? '从一个清晰的目标开始' : !state ? '正在读取任务图' : filterLabel ? '正在显示：' + filterLabel + ' · 再次点击恢复全部' : graph.getVisibleNodeCount() === 0 ? '当前轮尚未产生节点' : ({active:'探索正在展开',stopped:'探索已暂停，已有线索完整保留',completed:'项目已完成，查看结论与证据',terminated:'探索已终止，已有记录仍可查看'})[project.status] || data.statusName(project.status);
    $('graph-empty').hidden = !!project; $('activity-live').textContent = project ? data.statusName(project.status) : '等待项目';
    for (const id of ['zoom-in','zoom-out','fit-graph','arrange-graph']) $(id).disabled = !state || !graph.getVisibleNodeCount();
    if (hintProjectId) {
      const hinted = projects.find(item => item.id === hintProjectId), allowed = hinted && ['active','stopped'].includes(hinted.status); $('send-hint').disabled = !allowed || mutating || !connected;
      $('hint-caption').textContent = !allowed ? '本轮已结束，重启后可继续补充提示' : hinted.status === 'stopped' ? '提示将保存，在继续运行后读取' : '提示将写入当前项目的黑板';
    }
  }
  function emptyPanel(title, body, name = 'file') { const panel = el('div', 'empty-panel'); panel.append(icon(name), el('h3', '', title), el('p', '', body)); return panel; }
  function revealNode(ref) {
    const node = graph.getNodes().find(item => nodeKey(item) === nodeKey(ref)); if (!node) return; tab = 'board'; graph.setStatusFilter('all'); renderHeader(); graph.selectNode(ref); graph.focusNode(node.key); renderActivity();
  }
  function evidenceButtons(references) {
    const box = el('div', 'evidence-links'), keys = new Set(graph.getNodes().map(nodeKey));
    for (const ref of references || []) {
      const button = el('button', 'entry-node', data.nodeTypeName(ref.type) + ' · ' + ref.id); button.type = 'button'; button.disabled = !keys.has(nodeKey(ref));
      button.title = button.disabled ? '当前图中没有此节点' : '查看关联证据'; button.addEventListener('click', () => revealNode(ref)); box.append(button);
    }
    return box;
  }
  function artifacts(references) {
    const box = el('div', 'evidence-artifacts');
    for (const ref of references || []) {
      const detail = el('details', 'evidence-artifact'); detail.append(el('summary', '', (ref.path || '证据摘录') + (ref.start_line ? ':' + ref.start_line : '')));
      if (ref.run_id) detail.append(el('small', '', '执行 ' + ref.run_id)); detail.append(el('pre', '', ref.excerpt || '未记录摘录')); box.append(detail);
    }
    return box;
  }
  function renderInspector() {
    const node = selectedNode && graph.getNodes().find(item => nodeKey(item) === nodeKey(selectedNode)), edge = selectedEdge && graph.getEdgeDetails(selectedEdge.id), host = $('node-inspector');
    host.replaceChildren(); host.hidden = !node && !edge; host.dataset.selection = edge ? 'edge' : node ? 'node' : ''; if (!node && !edge) return;
    const close = el('button', 'icon-button'); close.setAttribute('aria-label', '清除节点或连线筛选'); close.append(icon('close')); close.addEventListener('click', () => { selectedNode = null; selectedEdge = null; graph.selectNode(null); renderActivity(); }); host.append(close);
    if (edge) {
      host.append(el('h3', '', edge.label || edge.kind), el('small', 'inspector-status', edge.kind + (edge.statusLabel ? ' / ' + edge.statusLabel : ''))); const endpoints = el('div', 'edge-endpoints');
      for (const [label, endpoint] of [['来源', edge.sourceNode], ['去向', edge.targetNode]]) {
        if (!endpoint) continue; const row = el('div', 'edge-endpoint'), button = el('button', 'inspector-node-link', endpoint.title || endpoint.label || endpoint.id);
        button.type = 'button'; button.addEventListener('click', () => revealNode(endpoint)); row.append(el('span', '', label), button); endpoints.append(row);
      }
      host.append(endpoints); if (edge.description) host.append(el('p', '', edge.description)); if (edge.invalid || edge.supportValid === false) host.append(el('p', 'evidence-warning', '此关系的证据支持已失效，请核对来源。'));
    } else {
      host.append(el('h3', '', node.title || node.label || node.id), el('p', '', node.description || ''), el('small', '', data.nodeTypeName(node.type) + ' / ' + data.statusName(node.status)));
      if (node.raw?.invalid_sources?.length) host.append(el('p', 'evidence-warning', '无效证据：' + node.raw.invalid_sources.join('、')));
    }
  }
  function logContent(source, id, parts) {
    const content = el('div', 'log-content'), full = el('div', 'log-full'); full.append(...parts);
    const text = parts.map(part => part.textContent).filter(Boolean).join('\n'), lines = text.split('\n');
    const previewText = Array.from(lines.slice(0, 4).join('\n')).slice(0, 140).join('');
    if (previewText === text) { content.append(full); return content; }
    const key = JSON.stringify([selectedId, state?.graph?.project?.generation || 0, source, id]);
    const preview = el('p', 'log-preview', previewText.trimEnd() + '…'), toggle = el('button', 'log-toggle');
    full.id = 'log-body-' + (++logBodyId); toggle.type = 'button'; toggle.setAttribute('aria-controls', full.id);
    const update = () => { const expanded = expandedLogs.has(key); full.hidden = !expanded; preview.hidden = expanded; toggle.setAttribute('aria-expanded', String(expanded)); toggle.textContent = expanded ? '收起' : '展开全文'; };
    toggle.addEventListener('click', () => { if (expandedLogs.has(key)) expandedLogs.delete(key); else expandedLogs.add(key); update(); });
    update(); content.append(preview, full, toggle); return content;
  }
  function renderLog(log) {
    const article = el('article', 'timeline-entry ' + log.level + (log.kind === 'model' ? ' conclusion' : '')); article.dataset.logId = log.id;
    const meta = el('div', 'entry-meta'); meta.append(el('span', 'entry-tag', log.kind === 'model' ? '关键结论' : data.phaseName(log.phase)), timestamp(log.time)); article.append(meta, el('h3', '', log.title));
    const parts = [el('p', '', log.body || '')]; if (log.scope) parts.push(el('p', '', '范围：' + log.scope)); if (log.code) parts.push(el('pre', 'log-code', log.code));
    article.append(logContent('board', log.id, parts)); if (log.worker) article.append(el('small', 'log-worker', log.worker));
    if (log.truncated && !(log.body || '').includes('输出已截断')) article.append(el('p', 'evidence-warning', '输出已截断，内容不完整。'));
    if (log.evidence?.length) article.append(evidenceButtons(log.evidence)); if (log.artifacts?.length) article.append(artifacts(log.artifacts)); if (log.node) article.append(evidenceButtons([log.node])); return article;
  }
  function renderBoard() {
    $('activity-tools').append(el('span', '', selectedEdge ? '当前连线的关联记录' : selectedNode ? '当前节点的关联记录' : '探索过程与共享线索'), el('span', '', '最新在前'));
    let entries = selectedNode ? data.filterLogs(logs, {node:selectedNode}) : logs;
    if (selectedEdge) {
      const detail = graph.getEdgeDetails(selectedEdge.id), ids = new Set(); for (const node of [detail?.sourceNode, detail?.targetNode]) if (node) for (const log of data.filterLogs(logs, {node})) ids.add(log.id); entries = logs.filter(log => ids.has(log.id));
    }
    $('activity-count').textContent = entries.length + ' 条记录'; if (!entries.length) { $('activity-content').append(emptyPanel('还没有关联记录', '项目产生的决策、执行和证据会显示在这里。', 'message')); return; }
    const shown = entries.slice(-logLimit).reverse(); $('activity-content').append(...shown.map(renderLog));
    if (entries.length > shown.length) { const more = el('button', 'button secondary more-logs', '显示更早记录（' + (entries.length - shown.length) + '）'); more.addEventListener('click', () => { logLimit += 300; renderActivity(); }); $('activity-content').append(more); }
  }
  function renderSystem(system) {
    const filter = el('select'); filter.setAttribute('aria-label', '筛选系统日志');
    for (const [value, label] of [['all','全部日志'],['http','LLM 错误'],['errors','全部错误']]) { const option = el('option', '', label); option.value = value; filter.append(option); }
    filter.value = systemFilter; filter.addEventListener('change', () => { systemFilter = filter.value; renderActivity(); }); $('activity-tools').append(el('span', '', '系统运行记录'), filter);
    $('activity-content').append(el('p', 'source-notice', system.unavailable.join('、') + '：暂未接入。'));
    const entries = system.logs.filter(log => systemFilter === 'http' ? log.component === 'LLM' && log.level === 'error' : systemFilter !== 'errors' || log.level === 'error'); $('activity-count').textContent = entries.length + ' 条记录';
    for (const log of entries.slice(-logLimit).reverse()) {
      const article = el('article', 'system-entry ' + log.level), meta = el('div', 'system-meta'); article.dataset.logId = log.id; meta.append(icon('terminal'), el('span', '', log.component), timestamp(log.time)); article.append(meta, el('h3', '', log.title || ''), logContent('system', log.id, [el('p', '', log.body || '')]));
      if (log.truncated) article.append(el('p', 'evidence-warning', '输出已截断，内容不完整。')); if (log.node) article.append(evidenceButtons([log.node])); $('activity-content').append(article);
    }
    if (entries.length > logLimit) { const more = el('button', 'button secondary more-logs', '显示更早记录（' + (entries.length - logLimit) + '）'); more.addEventListener('click', () => { logLimit += 300; renderActivity(); }); $('activity-content').append(more); }
    if (!entries.length) $('activity-content').append(emptyPanel(systemFilter === 'http' ? 'LLM 请求日志暂未接入' : '暂无匹配的公开记录', '当前接口提供执行结果和黑板事件，无法据此确认全部组件或模型请求的状态。', 'terminal'));
  }
  function exportLinks() {
    const box = el('div', 'export-links');
    for (const [format, label, suffix] of [['yaml','导出项目','yaml'], ['timeline','导出时间线','txt']]) { const link = el('a', 'button secondary export-result'); link.href = pathFor(selectedId) + '/export?format=' + format; link.download = selectedId + '-' + format + '.' + suffix; link.append(icon('download'), document.createTextNode(label)); box.append(link); }
    return box;
  }
  function renderResult() {
    const result = data.buildResult(state, executions); $('activity-tools').append(el('span', '', '结论与证据'), el('span', '', result.status === 'completed' ? '本轮已完成' : '当前轮')); $('activity-count').textContent = result.findings.length + ' 项发现';
    const titles = {completed:'探索已完成',terminated:'本轮已终止',pending:'答案正在探索中',unverified:'完成证据待核对'};
    if (result.status === 'completed') { const header = el('div', 'result-header'), check = el('span', 'result-check'); check.append(icon('check')); header.append(check, el('h3', '', titles[result.status])); $('activity-content').append(header, el('p', 'result-summary', result.summary)); }
    else $('activity-content').append(emptyPanel(titles[result.status] || titles.pending, result.notice || '当前轮还没有可确认的项目完成结论。'));
    if (result.status === 'completed' && result.notice) $('activity-content').append(el('p', 'source-notice', result.notice)); if (result.truncated) $('activity-content').append(el('p', 'evidence-warning', '包含已截断输出，结论内容不完整。'));
    if (result.findings.length) $('activity-content').append(el('h3', 'result-subheading', '发现与支持证据'));
    for (const finding of result.findings) { const article = el('article', 'finding' + (finding.supportValid === false ? ' invalid' : '')); article.append(el('h3', '', finding.claim), el('p', '', finding.statusLabel || data.statusName(finding.status))); if (finding.sources?.length) article.append(evidenceButtons(finding.sources)); $('activity-content').append(article); }
    if (result.conclusions.length) { $('activity-content').append(el('h3', 'result-subheading', '单次执行结论'), el('p', 'source-notice', '执行结论不等于项目已完成，请结合目标和有效证据核对。')); $('activity-content').append(...result.conclusions.map(renderLog)); }
    $('activity-content').append(exportLinks());
  }
  function renderActivity({reset = false} = {}) {
    if (selectedNode && !graph.getNodes().some(node => nodeKey(node) === nodeKey(selectedNode))) selectedNode = null; if (selectedEdge && !graph.getEdgeDetails(selectedEdge.id)) selectedEdge = null;
    const system = data.buildSystemLogs(state, events, executions), signature = JSON.stringify([tab, systemFilter, logs, system, state, nodeKey(selectedNode), selectedEdge?.id, logLimit]);
    if (!reset && signature === activitySignature) return; activitySignature = signature; const content = $('activity-content'), previousTop = content.scrollTop;
    content.replaceChildren(); $('activity-tools').replaceChildren(); renderInspector();
    document.querySelectorAll('[data-tab]').forEach(button => { const active = button.dataset.tab === tab; button.setAttribute('aria-selected', String(active)); button.tabIndex = active ? 0 : -1; }); content.setAttribute('aria-labelledby', 'tab-' + tab);
    const errors = system.logs.filter(log => log.level === 'error').length; $('system-alert').textContent = errors ? String(errors) : ''; $('system-alert').title = '本轮已记录的执行与系统错误';
    if (!current()) { content.append(emptyPanel('等待新的探索', '创建项目后，这里会记录每一步决策与执行。', 'message')); $('activity-count').textContent = '0 条记录'; }
    else if (tab === 'board') renderBoard(); else if (tab === 'system') renderSystem(system); else renderResult(); content.scrollTop = reset ? 0 : previousTop;
  }
  function selectNode(node) { selectedNode = node; if (node) { selectedEdge = null; if (!updatingGraph) tab = 'board'; } renderActivity({reset:!updatingGraph}); }
  function selectEdge(edge) { selectedEdge = edge; if (edge) { selectedNode = null; if (!updatingGraph) tab = 'board'; } renderActivity({reset:!updatingGraph}); }
  function updateGraph(value) { updatingGraph = true; try { graph.setState(value); } finally { updatingGraph = false; } }
  function resetSelection(id) {
    selectedId = id; state = null; executions = []; events = []; logs = []; selectedNode = null; selectedEdge = null; logLimit = 300; activitySignature = '';
    updateGraph(null); renderHeader(); renderActivity({reset:true}); try { if (id) localStorage.setItem('xloom.selected-project', id); else localStorage.removeItem('xloom.selected-project'); } catch {}
  }
  async function selectProject(id) { if (mutating) return; if (id !== selectedId || !state) resetSelection(id); closeMenus(); hideSidebar(); renderProjects(); await loadWorkspace(id); }
  async function loadWorkspace(preferred = selectedId) {
    if (mutating) return; clearTimeout(timer); const request = requests.begin(), options = {signal:request.signal}; let retryGeneration = false;
    try {
      const [list, overview] = await Promise.all([api.request('/projects', options), api.request('/ui/overview', options)]); if (!requests.current(request.version)) return;
      projects = list.slice().sort((a,b) => String(b.created_at || '').localeCompare(String(a.created_at || '')) || b.id.localeCompare(a.id));
      const target = projects.some(project => project.id === preferred) ? preferred : projects[0]?.id || ''; if (target !== selectedId) resetSelection(target); renderProjects();
      if (!target) { state = null; executions = []; events = []; logs = []; updateGraph(null); renderActivity({reset:true}); setConnection(true); $('last-update').textContent = '刷新 ' + data.formatTime(overview.observed_at); return; }
      const [nextState, runs] = await Promise.all([api.request(pathFor(target) + '/state', options), api.projectExecutions(pathFor(target), options)]); if (!requests.current(request.version)) return;
      const generation = nextState.graph.project.generation || 0;
      let cache = eventCache.get(target) || {after:0,events:[],generation};
      // after may include events newer than this state snapshot. Only a state
      // revision rollback (or a new generation) invalidates the saved history.
      if (cache.generation !== generation || nextState.revision < (cache.stateRevision || 0)) cache = {after:0,events:[],generation};
      const batch = []; let after = cache.after;
      for (let page = 0; page < 5; page++) {
        const nextEvents = await api.request(pathFor(target) + '/state/events?after=' + after, options); if (!requests.current(request.version)) return; if (!nextEvents.length) break;
        const next = Math.max(...nextEvents.map(event => event.revision)); if (next <= after) break; batch.push(...nextEvents); after = next; if (nextEvents.length < 1000) break;
      }
      const latest = await api.request(pathFor(target), options); if (!requests.current(request.version)) return;
      if ((latest.project.generation || 0) !== generation) { eventCache.delete(target); retryGeneration = true; return; }
      cache = {after,generation,stateRevision:nextState.revision,events:cache.events.concat(batch)}; eventCache.set(target, cache);
      if (state && (state.graph.project.generation || 0) !== generation) { selectedNode = null; selectedEdge = null; logLimit = 300; }
      state = nextState; executions = runs.filter(run => (run.generation || 0) === generation); events = cache.events.filter(event => event.revision <= state.revision);
      logs = data.buildLogs(state, events, executions); updateGraph(state); renderActivity(); setConnection(true); $('last-update').textContent = '刷新 ' + data.formatTime(new Date().toISOString());
    } catch (error) { if (!requests.current(request.version)) return; setConnection(false, error.message + '。已显示的数据会保留，稍后自动重试。'); }
    finally { if (requests.current(request.version)) timer = setTimeout(() => loadWorkspace(selectedId), retryGeneration ? 0 : document.hidden ? 10000 : 2500); }
  }
  function startMutation() {
    mutating = true; clearTimeout(timer); requests.cancel(); closeMenus(); document.querySelectorAll('dialog [data-close]').forEach(button => { button.disabled = true; }); renderHeader(); renderProjects();
  }
  async function finishMutation() {
    mutating = false; document.querySelectorAll('dialog [data-close]').forEach(button => { button.disabled = false; }); renderHeader(); renderProjects(); await loadWorkspace(selectedId);
  }
  const mutationError = error => error.message + (error.status === 0 ? '；结果未确认，请先刷新核对，避免重复操作。' : error.status === 409 ? '；项目状态或轮次已变化，正在刷新数据。请关闭此窗口并重新确认操作。' : '');
  async function changeStatus(id, action) {
    const project = projects.find(item => item.id === id); if (mutating || !connected || !project || !['active','stopped'].includes(project.status)) return; startMutation();
    try { await api.request(pathFor(id) + '/status', {method:'PUT',body:{status:action === 'pause' ? 'stopped' : 'active'}}); toast(action === 'pause' ? '项目已暂停，已有进度保留' : '项目已继续调度'); }
    catch (error) { toast(mutationError(error)); } finally { await finishMutation(); }
  }
  function confirmOperation(id, action) {
    const project = projects.find(item => item.id === id); if (!project || mutating || !connected) return; if (action === 'terminate' && !['active','stopped'].includes(project.status)) return;
    management = {id,action,generation:project.generation || 0}; const title = {restart:'重启项目',terminate:'终止项目',delete:'删除项目'}[action]; $('confirm-title').textContent = title + '？'; $('confirm-error').textContent = ''; $('confirm-action').disabled = false;
    $('confirm-message').textContent = action === 'restart' ? '将归档「' + project.title + '」的当前轮，并清空当前轮任务图、执行记录和结论重新开始。项目输入、创建时间、补充提示及历史轮次会保留。'
      : action === 'terminate' ? '将终止「' + project.title + '」的未完成任务，保留现有记录。终止不会将目标标记为完成，后续可以重启。'
      : '将删除「' + project.title + '」的项目记录，并取消未结束的执行。容器清理由后台处理，工作目录文件不会因此删除。';
    $('confirm-action').textContent = title; $('confirm-action').className = 'button primary' + (action === 'restart' ? '' : ' danger-button'); $('confirm-dialog').showModal();
  }
  function openCreate() { if (mutating) return; if (!createDraft) $('create-form').reset(); $('create-error').textContent = ''; $('create-dialog').showModal(); $('create-name').focus(); }
  function openHint() {
    const project = current(); if (!project || !['active','stopped'].includes(project.status) || mutating || !connected) return;
    hintProjectId = project.id; $('hint-project').textContent = project.title; $('hint-input').value = drafts.get(project.id) || ''; $('hint-error').textContent = ''; $('hint-input').removeAttribute('aria-invalid'); renderHeader(); $('hint-dialog').showModal(); $('hint-input').focus();
  }
  data.SCENARIOS.forEach((item,index) => {
    const choice = el('label', 'scenario-choice'), radio = el('input'), card = el('span', 'scenario-card'); radio.type = 'radio'; radio.name = 'scenario'; radio.value = item.id; radio.defaultChecked = index === 0; radio.checked = index === 0;
    card.append(icon(scenarioIcon({scenario:item.id})), el('strong', '', item.name), el('small', '', item.description)); choice.append(radio, card); $('scenario-options').append(choice);
  });
  $('create-form').addEventListener('input', () => { createDraft = true; $('create-error').textContent = ''; });
  $('create-form').addEventListener('submit', async event => {
    event.preventDefault(); if (mutating || $('submit-create').disabled) return; let started = false;
    try {
      const payload = data.validateProject({title:$('create-name').value,origin:$('create-origin').value,goal:$('create-goal').value,scenario:new FormData($('create-form')).get('scenario')});
      createDraft = true; $('submit-create').disabled = true; $('create-error').textContent = ''; startMutation(); started = true;
      const created = await api.request('/projects', {method:'POST',body:payload}); createDraft = false; $('create-dialog').close(); $('create-form').reset(); $('project-search').value = ''; tab = 'board'; resetSelection(created.project.id); hideSidebar(); toast('项目已创建');
    } catch (error) { $('create-error').textContent = mutationError(error); } finally { $('submit-create').disabled = false; if (started) await finishMutation(); }
  });
  $('confirm-form').addEventListener('submit', async event => {
    event.preventDefault(); if (!management || mutating || $('confirm-action').disabled) return; const command = {...management}; let requireConfirm = false; $('confirm-action').disabled = true; startMutation();
    try {
      if (command.action === 'delete') await api.request(pathFor(command.id), {method:'DELETE'}); else await api.request(pathFor(command.id) + '/' + command.action, {method:'POST',body:{expected_generation:command.generation}});
      $('confirm-dialog').close(); eventCache.delete(command.id);
      if (command.action === 'delete') { drafts.delete(command.id); graph.forgetProject?.(command.id); if (selectedId === command.id) resetSelection(''); }
      else if (command.id === selectedId) { selectedNode = null; selectedEdge = null; graph.selectNode(null); if (command.action === 'restart') resetSelection(command.id); }
      toast({delete:'项目记录已删除',restart:'项目已重启，旧轮已归档',terminate:'项目已终止，记录已保留'}[command.action]);
    } catch (error) { requireConfirm = [0,409].includes(error.status); $('confirm-error').textContent = mutationError(error); }
    finally { $('confirm-action').disabled = requireConfirm; await finishMutation(); }
  });
  $('confirm-dialog').addEventListener('close', () => { management = null; });
  $('hint-input').addEventListener('input', () => { $('hint-error').textContent = ''; $('hint-input').removeAttribute('aria-invalid'); if (hintProjectId) drafts.set(hintProjectId, $('hint-input').value); });
  $('hint-dialog').addEventListener('close', () => { if (hintProjectId) drafts.set(hintProjectId, $('hint-input').value); hintProjectId = ''; });
  $('hint-form').addEventListener('submit', async event => {
    event.preventDefault(); if (!hintProjectId || mutating || $('send-hint').disabled) return; const id = hintProjectId, content = $('hint-input').value.trim();
    if (!content || content.length > 32768) { $('hint-error').textContent = content ? '提示不能超过 32768 个字符。' : '请输入补充提示。'; $('hint-input').setAttribute('aria-invalid', 'true'); return; }
    drafts.set(id, $('hint-input').value); startMutation();
    try { await api.request(pathFor(id) + '/hints', {method:'POST',body:{content,creator:'user'}}); $('hint-input').value = ''; drafts.delete(id); $('hint-dialog').close(); selectedNode = null; selectedEdge = null; graph.selectNode(null); tab = 'board'; toast('补充提示已保存'); }
    catch (error) { $('hint-error').textContent = mutationError(error); } finally { await finishMutation(); }
  });
  $('hint-input').addEventListener('keydown', event => { if ((event.ctrlKey || event.metaKey) && event.key === 'Enter') { event.preventDefault(); $('hint-form').requestSubmit(); } });
  $('new-project').addEventListener('click', openCreate); $('empty-create').addEventListener('click', openCreate); $('add-hint').addEventListener('click', openHint); $('about-button').addEventListener('click', () => $('about-dialog').showModal());
  document.querySelectorAll('[data-close]').forEach(button => button.addEventListener('click', () => { if (!mutating) $(button.dataset.close).close(); })); document.querySelectorAll('dialog').forEach(dialog => dialog.addEventListener('cancel', event => { if (mutating) event.preventDefault(); }));
  $('project-search').addEventListener('input', renderProjects); $('refresh-project').addEventListener('click', () => loadWorkspace(selectedId));
  $('toggle-running').addEventListener('click', () => { const project = current(); if (!project) return; const action = $('toggle-running').dataset.action; if (action === 'restart') confirmOperation(project.id, action); else changeStatus(project.id, action); });
  document.querySelectorAll('[data-tab]').forEach(button => {
    button.addEventListener('click', () => { tab = button.dataset.tab; renderActivity({reset:true}); }); button.addEventListener('keydown', event => {
      const tabs = [...document.querySelectorAll('[data-tab]')]; let index = tabs.indexOf(button);
      if (event.key === 'ArrowRight') index = (index + 1) % tabs.length; else if (event.key === 'ArrowLeft') index = (index + tabs.length - 1) % tabs.length; else if (event.key === 'Home') index = 0; else if (event.key === 'End') index = tabs.length - 1; else return;
      event.preventDefault(); tabs[index].click(); tabs[index].focus();
    });
  });
  $('zoom-out').addEventListener('click', () => graph.zoomBy(1 / 1.15)); $('zoom-in').addEventListener('click', () => graph.zoomBy(1.15)); $('fit-graph').addEventListener('click', () => graph.fit()); $('arrange-graph').addEventListener('click', () => { graph.arrange(); toast('已重新随机分布节点'); });
  document.querySelectorAll('[data-status-filter]').forEach(button => button.addEventListener('click', () => {
    graph.setStatusFilter(graph.getStatusFilter() === button.dataset.statusFilter ? 'all' : button.dataset.statusFilter); renderHeader();
  }));
  $('graph-host').addEventListener('graphzoom', event => { $('zoom-label').textContent = Math.round(event.detail.zoom * 100) + '%'; });
  $('mobile-menu').addEventListener('click', () => { const open = $('sidebar').classList.toggle('open'); $('sidebar-scrim').hidden = !open; $('mobile-menu').setAttribute('aria-expanded', String(open)); }); $('sidebar-scrim').addEventListener('click', hideSidebar);
  document.addEventListener('pointerdown', event => { if (!event.target.closest('.project-menu')) closeMenus(); }); window.addEventListener('resize', closeMenus);
  document.addEventListener('keydown', event => {
    if (event.target.closest('input,textarea,select,[contenteditable="true"]') || document.querySelector('dialog[open]') || event.ctrlKey || event.metaKey || event.altKey || event.repeat) return;
    if (event.key === 'Escape') { graph.selectNode(null); closeMenus(); hideSidebar(); } else if (event.key.toLowerCase() === 'n') { event.preventDefault(); openCreate(); }
    else if (event.key === '/') { event.preventDefault(); if (innerWidth <= 880) { $('sidebar').classList.add('open'); $('sidebar-scrim').hidden = false; $('mobile-menu').setAttribute('aria-expanded', 'true'); } $('project-search').focus(); }
  });
  document.addEventListener('visibilitychange', () => { if (!document.hidden) loadWorkspace(selectedId); });
  window.addEventListener('pagehide', event => { clearTimeout(timer); clearTimeout(toastTimer); requests.cancel(); if (!event.persisted) graph.destroy(); }); window.addEventListener('pageshow', event => { if (event.persisted) loadWorkspace(selectedId); });
  renderHeader(); renderActivity(); loadWorkspace(selectedId);
}());
