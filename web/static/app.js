(function () {
  'use strict';
  const data = window.XLoomData;
  const api = new window.XLoomAPI.Client();
  const requests = new window.XLoomAPI.RequestScope();
  const $ = id => document.getElementById(id);
  let projects = [], state = null, executions = [], logs = [], selectedId = '', selectedNode = null;
  let phase = 'all', view = 'graph', follow = true, logLimit = 300, logSignature = '';
  let timer, toastTimer, management = null, mutating = false, connected = false;
  const eventCache = new Map();
  try { selectedId = new URLSearchParams(location.search).get('project') || localStorage.getItem('xloom.selected-project') || ''; } catch {}
  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }
  function icon(name) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', 'icon'); svg.setAttribute('aria-hidden', 'true');
    const use = document.createElementNS(svg.namespaceURI, 'use');
    use.setAttribute('href', '#i-' + name); svg.append(use); return svg;
  }
  function toast(message) {
    clearTimeout(toastTimer); $('toast').textContent = message; $('toast').hidden = false;
    toastTimer = setTimeout(() => { $('toast').hidden = true; }, 4000);
  }
  const graph = new window.XLoomGraph($('graph-host'), {onSelect: showNode});
  const current = () => state?.graph?.project || projects.find(project => project.id === selectedId);
  const pathFor = id => '/projects/' + encodeURIComponent(id);
  const nodeKey = node => node ? node.type + ':' + node.id : '';
  function setConnection(ok, message) {
    connected = ok;
    $('connection-state').textContent = ok ? '已连接' : '连接中断';
    $('connection-state').dataset.status = ok ? 'connected' : 'error';
    $('workspace-error').hidden = !message;
    $('workspace-error').textContent = message || '';
    renderHeader();
  }
  function renderProjects() {
    const query = $('project-search').value.trim().toLocaleLowerCase();
    const matches = projects.filter(project => (project.title + data.scenarioName(project.scenario)).toLocaleLowerCase().includes(query));
    $('project-count').textContent = projects.length;
    $('project-empty').hidden = matches.length > 0 || projects.length === 0;
    $('project-list').replaceChildren(...matches.map(project => {
      const button = el('button', 'project-item' + (project.id === selectedId ? ' selected' : ''));
      button.type = 'button'; button.title = project.title;
      button.setAttribute('aria-label', project.title + '，' + data.statusName(project.status));
      button.setAttribute('aria-current', String(project.id === selectedId));
      const copy = el('span', 'project-copy');
      const meta = el('small');
      meta.append(el('span', 'dot ' + project.status), el('span', '', data.statusName(project.status)), el('span', 'project-date', data.formatTime(project.created_at, {date:true})));
      copy.append(el('strong', '', project.title), meta);
      button.append(icon(data.SCENARIOS.find(item => item.id === project.scenario)?.icon || 'grid'), copy);
      button.addEventListener('click', () => selectProject(project.id));
      return button;
    }));
  }
  function projectWorkers(project) {
    if (!project || project.status !== 'active') return 0;
    if (state?.graph?.project?.id !== project.id) return (project.working_intent_count || 0) + (project.reason ? 1 : 0);
    const workers = new Set((state.steps || []).filter(step => step.status === 'running' && step.worker).map(step => step.worker));
    if (project.reason?.worker) workers.add(project.reason.worker);
    return workers.size;
  }
  function renderHeader() {
    const project = current();
    $('breadcrumb-title').textContent = project?.title || '工作台';
    $('project-title').textContent = project?.title || (connected ? '创建你的第一个项目' : '正在加载项目');
    $('project-status').replaceChildren();
    if (project) $('project-status').append(el('span', 'dot'), document.createTextNode(data.statusName(project.status)));
    $('project-status').dataset.status = project?.status || '';
    $('project-scenario').textContent = project ? data.scenarioName(project.scenario) : '';
    $('worker-count').textContent = project ? projectWorkers(project) + ' 个任务进行中' : '';
    $('project-time').textContent = project ? data.formatTime(project.created_at, {date:true}) + ' 创建' : '';
    const completed = project?.status === 'completed';
    const running = project?.status === 'active';
    $('toggle-running').replaceChildren(icon(completed ? 'check' : running ? 'pause' : 'play'), el('span', '', completed ? '已完成' : running ? '暂停' : '继续'));
    $('toggle-running').disabled = !project || completed || mutating || !connected;
    $('project-actions').hidden = !project;
    const reopen = document.querySelector('[data-action="reopen"]');
    reopen.disabled = !completed;
    $('node-count').textContent = graph.getNodes().length;
    $('finding-count').textContent = state?.findings?.length || 0;
    $('log-live').textContent = project ? (running ? 'LIVE' : completed ? 'DONE' : 'PAUSED') : '';
    if (project) {
      $('export-state').href = pathFor(project.id) + '/state';
      $('export-state').download = project.id + '-state.json';
      $('export-timeline').href = pathFor(project.id) + '/export?format=timeline';
      $('export-timeline').download = project.id + '-timeline.txt';
    }
  }
  function setPhase(value) {
    phase = value;
    document.querySelectorAll('[data-phase]').forEach(button => {
      const active = button.dataset.phase === value;
      button.classList.toggle('active', active); button.setAttribute('aria-pressed', String(active));
    });
  }
  function setGraphFilter(value) {
    graph.setFilter(value);
    document.querySelectorAll('[data-graph-filter]').forEach(button => {
      const active = button.dataset.graphFilter === value;
      button.classList.toggle('selected', active); button.setAttribute('aria-pressed', String(active));
    });
  }
  function revealNode(node) {
    setPhase('all'); setGraphFilter('all'); $('log-query').value = '';
    switchView('graph'); graph.selectNode(node);
  }
  function evidenceButtons(references) {
    const box = el('div', 'model-answer-evidence');
    for (const ref of references || []) {
      const button = el('button', '', ref.id);
      button.type = 'button'; button.setAttribute('aria-label', '查看证据 ' + ref.id);
      const exists = graph.getNodes().some(node => nodeKey(node) === nodeKey(ref));
      button.disabled = !exists;
      button.title = exists ? ref.type + ' · ' + ref.id : '当前任务图中没有此节点';
      button.addEventListener('click', () => revealNode(ref)); box.append(button);
    }
    return box;
  }
  function artifacts(references) {
    const box = el('div', 'evidence-artifacts');
    for (const ref of references || []) {
      const detail = el('details', 'evidence-artifact');
      const title = (ref.path || '证据摘录') + (ref.start_line ? ':' + ref.start_line : '');
      detail.append(el('summary', '', title));
      if (ref.run_id) detail.append(el('small', '', '执行 ' + ref.run_id));
      detail.append(el('pre', '', ref.excerpt || '未记录摘录')); box.append(detail);
    }
    return box;
  }
  function showNode(node) {
    selectedNode = node ? {type:node.type, id:node.id} : null;
    $('node-detail').hidden = !node;
    if (node) {
      $('node-detail-type').textContent = node.type.toUpperCase() + ' · ' + data.statusName(node.status);
      $('node-detail-id').textContent = node.id;
      $('node-detail-title').textContent = node.label;
      $('node-detail-body').textContent = node.description;
      $('node-detail-evidence').replaceChildren();
      const raw = node.raw || {};
      if (raw.scope) $('node-detail-evidence').append(el('p', '', '范围：' + raw.scope));
      if (raw.reason) $('node-detail-evidence').append(el('p', '', raw.reason));
      if (node.supportValid === false && (node.status === 'verified' || node.status === 'achieved')) $('node-detail-evidence').append(el('p', 'danger-text', '当前支持证据无效，请结合事实状态复核。'));
      if (node.sources?.length) $('node-detail-evidence').append(evidenceButtons(node.sources.map(id => ({type:'fact', id}))));
      if (node.evidence?.length) $('node-detail-evidence').append(artifacts(node.evidence));
    }
    renderLogs({reset:true});
  }
  function renderModelAnswer(log) {
    const article = el('article', 'model-answer ' + log.level);
    article.setAttribute('aria-label', '模型关键结论');
    const meta = el('div', 'model-answer-meta');
    const author = el('span', 'model-answer-author');
    author.append(icon('spark'), document.createTextNode(log.worker ? '关键结论 · ' + log.worker : '关键结论'));
    meta.append(author, el('time', '', data.formatTime(log.time)));
    article.append(meta, el('h3', '', log.title), el('p', 'model-answer-body', log.body));
    if (log.scope) article.append(el('p', 'model-answer-next', '范围：' + log.scope));
    if (log.evidence?.length) article.append(evidenceButtons(log.evidence));
    if (log.artifacts?.length) article.append(artifacts(log.artifacts));
    if (log.node) {
      const button = el('button', 'plain-button log-node-link', '定位 ' + log.node.id);
      button.addEventListener('click', () => revealNode(log.node)); article.append(button);
    }
    return article;
  }
  function renderLog(log) {
    if (log.kind === 'model') return renderModelAnswer(log);
    const article = el('article', 'log-entry ' + log.level);
    const meta = el('div', 'log-meta', data.formatTime(log.time));
    meta.append(el('span', 'phase-label ' + log.phase, log.phase));
    article.append(meta, el('h3', '', log.title), el('p', '', log.body));
    if (log.code) article.append(el('code', '', log.code));
    if (log.worker) article.append(el('div', 'log-worker', log.worker));
    if (log.node && graph.getNodes().some(node => nodeKey(node) === nodeKey(log.node))) {
      const link = el('button', 'plain-button log-node-link', log.node.type + ' · ' + log.node.id);
      link.addEventListener('click', () => revealNode(log.node)); article.append(link);
    }
    return article;
  }
  function renderLogs({reset = false, force = false} = {}) {
    const filtered = data.filterLogs(logs, {phase, query:$('log-query').value, node:selectedNode});
    const signature = JSON.stringify([filtered, logLimit, selectedNode, phase]);
    $('log-node-filter').hidden = !selectedNode;
    $('log-node-label').textContent = selectedNode ? '关联节点 · ' + selectedNode.type + ' / ' + selectedNode.id : '';
    $('log-count').textContent = filtered.length === logs.length ? logs.length : filtered.length + ' / ' + logs.length;
    if (!force && !reset && signature === logSignature) return;
    logSignature = signature;
    const stream = $('log-stream'), previousTop = stream.scrollTop;
    const shown = filtered.slice(-logLimit);
    stream.replaceChildren();
    if (filtered.length > shown.length) {
      const more = el('button', 'button subtle earlier-logs', '显示更早记录（' + (filtered.length - shown.length) + '）');
      more.addEventListener('click', () => { const before = stream.scrollHeight; logLimit += 300; const oldFollow = follow; follow = false; renderLogs({force:true}); stream.scrollTop += stream.scrollHeight - before; follow = oldFollow; });
      stream.append(more);
    }
    stream.append(...shown.map(renderLog));
    if (!shown.length) stream.append(el('div', 'logs-empty', logs.length ? '没有匹配的记录。可切换筛选或清除节点选择。' : selectedId ? '等待执行记录。' : '选择项目后查看执行记录。'));
    stream.scrollTo({top:reset ? 0 : follow ? stream.scrollHeight : previousTop, behavior:'instant'});
  }
  function renderFindings() {
    $('findings-list').replaceChildren(...(state?.findings || []).map(finding => {
      const card = el('article', 'finding-card');
      const heading = el('div', '', 'FINDING · ' + finding.id);
      const validity = finding.status === 'verified' && finding.support_valid === false ? ' · 证据失效' : '';
      const badge = el('span', '', data.statusName(finding.status) + validity);
      badge.dataset.status = validity ? 'invalid' : finding.status;
      heading.append(badge);
      card.append(heading, el('h3', '', finding.claim));
      if (finding.scope) card.append(el('p', '', '范围：' + finding.scope));
      if (finding.reason) card.append(el('p', '', finding.reason));
      if (finding.sources?.length) card.append(evidenceButtons(finding.sources.map(id => ({type:'fact', id}))));
      if (finding.evidence?.length) card.append(artifacts(finding.evidence));
      const footer = el('footer', '', data.formatTime(finding.updated_at || finding.created_at, {date:true}));
      const button = el('button', '', '查看关联节点');
      button.addEventListener('click', () => revealNode({type:'finding', id:finding.id}));
      footer.append(button); card.append(footer); return card;
    }));
    if (!state?.findings?.length) $('findings-list').append(el('p', 'empty-message', '暂时没有 Finding。已提交的发现会自动出现在这里。'));
  }
  function switchView(name) {
    view = name;
    $('graph-stage').hidden = name !== 'graph'; $('findings-view').hidden = name !== 'findings';
    for (const item of ['graph', 'findings']) {
      $(item + '-tab').classList.toggle('active', name === item);
      $(item + '-tab').setAttribute('aria-selected', String(name === item));
    }
    if (name === 'findings') renderFindings();
  }
  function resetSelection(id) {
    selectedId = id; state = null; executions = []; logs = []; selectedNode = null; logSignature = ''; logLimit = 300;
    $('node-detail').hidden = true; $('log-query').value = ''; setPhase('all'); setGraphFilter('all');
    graph.setState(null); switchView('graph'); renderHeader(); renderLogs({reset:true});
    try { if (id) localStorage.setItem('xloom.selected-project', id); else localStorage.removeItem('xloom.selected-project'); } catch {}
  }
  async function selectProject(id) {
    if (id !== selectedId || !state) resetSelection(id);
    renderProjects(); await loadWorkspace(id);
  }
  async function loadWorkspace(preferred = selectedId) {
    clearTimeout(timer);
    const request = requests.begin();
    const options = {signal:request.signal};
    try {
      const [list, overview] = await Promise.all([api.request('/projects', options), api.request('/ui/overview', options)]);
      if (!requests.current(request.version)) return;
      projects = list.slice().sort((a,b) => b.created_at.localeCompare(a.created_at) || b.id.localeCompare(a.id));
      const target = projects.some(project => project.id === preferred) ? preferred : projects[0]?.id || '';
      if (target !== selectedId) resetSelection(target);
      renderProjects();
      $('capacity-count').textContent = String(overview.active_workers);
      $('capacity-detail').textContent = '全实例 · ' + projects.filter(project => project.status === 'active').length + ' 个运行项目';
      if (!target) {
        state = null; graph.setState(null); logs = [];
        $('graph-empty').hidden = false; renderFindings(); renderLogs({reset:true});
        setConnection(true); $('last-update').textContent = data.formatTime(overview.observed_at); return;
      }
      const firstLoad = !state;
      $('graph-empty').hidden = true;
      const [nextState, runs] = await Promise.all([api.request(pathFor(target) + '/state', options), api.request(pathFor(target) + '/executions', options)]);
      if (!requests.current(request.version)) return;
      let cache = eventCache.get(target) || {after:0, events:[]};
      if (nextState.revision < cache.after) cache = {after:0, events:[]};
      const batch = [];
      let after = cache.after;
      for (let page = 0; page < 5; page++) {
        const events = await api.request(pathFor(target) + '/state/events?after=' + after, options);
        if (!requests.current(request.version)) return;
        if (!events.length) break;
        const next = Math.max(...events.map(event => event.revision));
        if (next <= after) break;
        batch.push(...events); after = next;
        if (events.length < 1000) break;
      }
      if (!requests.current(request.version)) return;
      cache = {after, events:cache.events.concat(batch)}; eventCache.set(target, cache);
      state = nextState; executions = runs;
      // Events may advance while the state request is in flight. Keep a coherent
      // graph/log snapshot; cached newer events become visible on the next poll.
      logs = data.buildLogs(state, cache.events.filter(event => event.revision <= state.revision), executions);
      graph.setState(state);
      renderHeader();
      if (selectedNode) {
        const node = graph.getNodes().find(item => nodeKey(item) === nodeKey(selectedNode));
        if (!node) showNode(null);
      }
      if (view === 'findings') renderFindings();
      renderLogs({force:firstLoad});
      setConnection(true); $('last-update').textContent = data.formatTime(new Date().toISOString());
    } catch (error) {
      if (!requests.current(request.version)) return;
      setConnection(false, error.message + '。已显示的数据会保留，稍后自动重试。');
    } finally {
      if (requests.current(request.version)) timer = setTimeout(() => loadWorkspace(selectedId), document.hidden ? 10000 : 2500);
    }
  }
  function openCreate() {
    $('create-form').reset(); $('form-error').textContent = ''; $('create-dialog').showModal(); $('create-name').focus();
  }
  data.SCENARIOS.forEach((item,index) => {
    const choice = el('label', 'scenario-choice'); const radio = el('input');
    radio.type = 'radio'; radio.name = 'scenario'; radio.value = item.id; radio.defaultChecked = index === 0;
    radio.setAttribute('aria-label', item.name);
    choice.append(radio, icon(item.icon), el('strong', '', item.name), el('small', '', item.description)); $('scenario-options').append(choice);
  });
  $('create-form').addEventListener('submit', async event => {
    event.preventDefault(); if ($('submit-create').disabled) return;
    try {
      const payload = data.validateProject({title:$('create-name').value, origin:$('create-origin').value, goal:$('create-goal').value,
        scenario:new FormData($('create-form')).get('scenario'), bootstrap_enabled:$('create-bootstrap').checked});
      $('submit-create').disabled = true; $('form-error').textContent = '';
      const created = await api.request('/projects', {method:'POST', body:payload});
      $('create-dialog').close(); $('project-search').value = ''; resetSelection(created.project.id);
      await loadWorkspace(created.project.id); toast('项目已创建');
    } catch (error) { $('form-error').textContent = error.message + (error.status === 0 ? '；提交结果未确认，请先刷新项目列表再决定是否重试。' : ''); }
    finally { $('submit-create').disabled = false; }
  });
  $('create-form').addEventListener('input', () => { $('form-error').textContent = ''; });
  ['new-project', 'empty-create'].forEach(id => $(id).addEventListener('click', openCreate));
  ['close-create', 'cancel-create'].forEach(id => $(id).addEventListener('click', () => $('create-dialog').close()));
  $('help-button').addEventListener('click', () => $('about-dialog').showModal());
  ['close-about', 'acknowledge-help'].forEach(id => $(id).addEventListener('click', () => $('about-dialog').close()));
  $('project-search').addEventListener('input', renderProjects);
  $('log-query').addEventListener('input', () => { logLimit = 300; renderLogs({reset:true}); });
  document.querySelectorAll('[data-phase]').forEach(button => button.addEventListener('click', () => { setPhase(button.dataset.phase); logLimit = 300; renderLogs({reset:true}); }));
  document.querySelectorAll('[data-graph-filter]').forEach(button => button.addEventListener('click', () => setGraphFilter(button.dataset.graphFilter)));
  ['close-node', 'clear-log-node'].forEach(id => $(id).addEventListener('click', () => graph.selectNode(null)));
  $('zoom-in').addEventListener('click', () => graph.zoomBy(1.2)); $('zoom-out').addEventListener('click', () => graph.zoomBy(1 / 1.2));
  $('fit-graph').addEventListener('click', () => graph.fit());
  $('graph-host').addEventListener('graphzoom', event => { $('zoom-label').textContent = Math.round(event.detail.zoom * 100) + '%'; });
  ['graph', 'findings'].forEach(name => $(name + '-tab').addEventListener('click', () => switchView(name)));
  $('refresh-project').addEventListener('click', () => loadWorkspace(selectedId));
  $('scroll-logs').addEventListener('click', () => { $('log-stream').scrollTop = $('log-stream').scrollHeight; });
  $('autoscroll').addEventListener('click', () => {
    follow = !follow; $('autoscroll').classList.toggle('active', follow); $('autoscroll').setAttribute('aria-pressed', String(follow));
    if (follow) $('log-stream').scrollTop = $('log-stream').scrollHeight;
  });
  $('toggle-running').addEventListener('click', async () => {
    const project = current(); if (!project || mutating || project.status === 'completed') return;
    const next = project.status === 'active' ? 'stopped' : 'active';
    mutating = true; renderHeader();
    try { await api.request(pathFor(project.id) + '/status', {method:'PUT', body:{status:next}}); await loadWorkspace(selectedId); toast(next === 'stopped' ? '项目已暂停' : '项目已继续'); }
    catch (error) { toast(error.message); }
    finally { mutating = false; renderHeader(); }
  });
  document.querySelectorAll('[data-action]').forEach(button => button.addEventListener('click', () => {
    const project = current(); if (!project) return;
    management = {action:button.dataset.action, id:project.id};
    $('project-actions').open = false; $('manage-error').textContent = '';
    const labels = {rename:['重命名项目','新名称','保存'],hint:['补充信息','新的信息或执行建议','提交'],reopen:['重新打开项目','说明还需要完成的内容','重新打开'],delete:['删除项目','','确认删除']}[management.action];
    $('manage-title').textContent = labels[0]; $('manage-label').textContent = labels[1]; $('submit-manage').textContent = labels[2];
    $('manage-project').textContent = project.title;
    $('manage-input').value = management.action === 'rename' ? project.title : '';
    $('manage-input').maxLength = management.action === 'rename' ? 200 : 32768;
    $('manage-input').hidden = management.action === 'delete'; $('manage-label').hidden = management.action === 'delete';
    $('manage-warning').textContent = management.action === 'delete' ? '项目、任务图和全部记录将被永久删除。' : '';
    $('submit-manage').classList.toggle('danger', management.action === 'delete');
    $('manage-dialog').showModal();
  }));
  $('cancel-manage').addEventListener('click', () => $('manage-dialog').close());
  $('manage-form').addEventListener('submit', async event => {
    event.preventDefault(); if (!management || $('submit-manage').disabled) return;
    const command = {...management}, value = $('manage-input').value.trim();
    if (command.action !== 'delete' && !value) { $('manage-error').textContent = '请填写内容。'; return; }
    $('submit-manage').disabled = true;
    try {
      const base = pathFor(command.id);
      if (command.action === 'delete') await api.request(base, {method:'DELETE'});
      if (command.action === 'rename') await api.request(base + '/title', {method:'PUT', body:{title:value}});
      if (command.action === 'hint') await api.request(base + '/hints', {method:'POST', body:{content:value, creator:'user'}});
      if (command.action === 'reopen') await api.request(base + '/reopen', {method:'POST', body:{description:value, creator:'user'}});
      $('manage-dialog').close();
      if (command.action === 'delete') { eventCache.delete(command.id); if (selectedId === command.id) resetSelection(''); }
      await loadWorkspace(selectedId); toast('操作已保存');
    } catch (error) { $('manage-error').textContent = error.message; }
    finally { $('submit-manage').disabled = false; }
  });
  document.addEventListener('keydown', event => {
    if (event.ctrlKey || event.metaKey || event.altKey || event.repeat || document.querySelector('dialog[open]') || event.target.closest('input,textarea,select,[contenteditable="true"]')) return;
    if (event.key.toLowerCase() === 'n') { event.preventDefault(); openCreate(); }
    if (event.key === '/') { event.preventDefault(); $('project-search').focus(); }
    if (event.key === 'Escape') { graph.selectNode(null); $('project-actions').open = false; }
  });
  document.addEventListener('visibilitychange', () => { if (!document.hidden) loadWorkspace(selectedId); });
  window.addEventListener('pagehide', () => { clearTimeout(timer); requests.cancel(); });
  window.addEventListener('pageshow', event => { if (event.persisted) loadWorkspace(selectedId); });
  setPhase('all'); setGraphFilter('all'); loadWorkspace(selectedId);
}());
