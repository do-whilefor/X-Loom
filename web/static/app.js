(function () {
  'use strict';
  const data = window.XLoomData;
  const api = new window.XLoomAPI.Client();
  const requests = new window.XLoomAPI.RequestScope();
  const $ = id => document.getElementById(id);
  let projects = [], state = null, executions = [], logs = [], selectedId = '', selectedNode = null;
  let phase = 'all', view = 'graph', follow = true, logLimit = 300, logSignature = '';
  let timer, toastTimer, management = null, hintProjectId = "", mutating = false, connected = false, projectsSignature = "";
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
  const graph = new window.XLoomGraph($('graph-host'), {onSelect: selectLogNode});
  const current = () => state?.graph?.project || projects.find(project => project.id === selectedId);
  const pathFor = id => '/projects/' + encodeURIComponent(id);
  const nodeKey = node => node ? node.type + ':' + node.id : '';
  function setConnection(ok, message) {
    connected = ok;
    $('connection-state').textContent = ok ? '已连接' : '连接中断';
    $('connection-state').dataset.status = ok ? 'connected' : 'error';
    $('workspace-error').hidden = !message;
    $('workspace-error').textContent = message || '';
    renderHeader(); renderProjects();
  }
  function closeProjectMenus() {
    document.querySelectorAll('.project-actions[open]').forEach(menu => { menu.open = false; });
  }
  function renderProjects() {
    const query = $('project-search').value.trim().toLocaleLowerCase();
    const matches = projects.filter(project => (project.title + data.scenarioName(project.scenario)).toLocaleLowerCase().includes(query));
    $('project-count').textContent = projects.length;
    $('project-empty').hidden = matches.length > 0 || projects.length === 0;
    const signature = JSON.stringify([matches,selectedId,mutating,connected]);
    if (signature === projectsSignature) return;
    projectsSignature = signature;
    $('project-list').replaceChildren(...matches.map(project => {
      const row = el('div','project-row');
      const button = el('button','project-item' + (project.id === selectedId ? ' selected' : ''));
      button.type = 'button'; button.title = project.title; button.dataset.projectId = project.id;
      button.disabled = mutating;
      button.setAttribute('aria-label',project.title + '，' + data.scenarioName(project.scenario));
      button.setAttribute('aria-current',String(project.id === selectedId));
      const copy = el('span','project-copy'), meta = el('small');
      meta.append(el('span','small-dot ' + project.status),el('span','',data.statusName(project.status)),el('i'),el('span','',data.scenarioName(project.scenario)));
      const created = el('time','project-created',data.formatTime(project.created_at) + ' 创建');
      if (project.created_at) created.dateTime = project.created_at;
      copy.append(el('strong','',project.title),meta,created);
      button.append(icon(data.SCENARIOS.find(item => item.id === project.scenario)?.icon || 'graph'),copy);
      button.addEventListener('click',() => selectProject(project.id));
      const menu = el('details','project-actions'), trigger = el('summary','icon-button'), panel = el('div','project-action-menu');
      trigger.setAttribute('aria-label','项目操作：' + project.title); trigger.title = '项目操作'; trigger.append(icon('more'));
      for (const kind of ['restart','delete']) {
        const action = el('button','project-action' + (kind === 'delete' ? ' delete-action' : ''));
        action.type = 'button'; action.disabled = mutating || !connected;
        const label = kind === 'restart' ? '重启项目' : '删除项目';
        action.setAttribute('aria-label',label + '：' + project.title);
        action.append(icon(kind === 'restart' ? 'restart' : 'trash'),el('span','',label));
        action.addEventListener('click',() => { menu.open = false; openManagement(kind,project.id); }); panel.append(action);
      }
      menu.addEventListener('toggle',() => {
        if (!menu.open) return;
        document.querySelectorAll('.project-actions[open]').forEach(other => { if (other !== menu) other.open = false; });
        const bounds = trigger.getBoundingClientRect();
        panel.style.left = Math.max(8,Math.min(window.innerWidth - panel.offsetWidth - 8,bounds.right - panel.offsetWidth)) + 'px';
        panel.style.top = Math.max(8,Math.min(window.innerHeight - panel.offsetHeight - 8,bounds.bottom + 5)) + 'px';
      });
      menu.addEventListener('keydown',event => { if (event.key === 'Escape') { event.preventDefault(); menu.open = false; trigger.focus(); } });
      menu.append(trigger,panel); row.append(button,menu); return row;
    }));
  }
  function renderHeader() {
    const project = current(), running = project?.status === 'active', completed = project?.status === 'completed', terminated = project?.status === 'terminated';
    const ended = completed || terminated;
    $('breadcrumb-title').textContent = project?.title || '工作台';
    $('project-title').textContent = project?.title || (connected ? '创建你的第一个项目' : '正在加载项目');
    $('project-status').textContent = project ? data.statusName(project.status) : '';
    $('project-status').className = 'project-status ' + (project?.status || '');
    $('project-scenario').textContent = project ? data.scenarioName(project.scenario) : '';
    $('project-meta').hidden = !project;
    const progress = data.taskProgress(state), timing = data.projectTiming(state,executions);
    $('project-progress').textContent = progress.completed + ' / ' + progress.total + ' 个任务已完成';
    for (const [id,time,empty] of [['project-start',timing.startedAt,ended ? '未记录' : '尚未开始'],['project-end',timing.endedAt,ended ? '未记录' : '尚未结束']]) {
      $(id).textContent = time ? data.formatTime(time) : empty;
      if (time) $(id).dateTime = time; else $(id).removeAttribute('datetime');
    }
    $('toggle-running').replaceChildren(icon(terminated ? 'stop' : completed ? 'check' : running ? 'pause' : 'play'),el('span','',terminated ? '已终止' : completed ? '已完成' : running ? '暂停' : '继续'));
    $('toggle-running').disabled = !project || ended || mutating || !connected;
    $('toggle-running').title = running ? '暂停并取消当前执行，保留已有图和记录' : terminated ? '项目已终止，重新执行需在左侧重启' : completed ? '项目已完成，可在左侧重启' : '重新调度未完成任务';
    $('terminate-project').hidden = !project || ended;
    $('terminate-project').disabled = mutating || !connected;
    $('add-hint').disabled = !project || ended || mutating || !connected;
    $('add-hint').title = ended ? '项目已结束，需先重启再补充提示' : '';
    $('new-project').disabled = $('empty-create').disabled = mutating;
    $('refresh-project').disabled = mutating;
    $('node-count').textContent = graph.getVisibleNodeCount();
    $('finding-count').textContent = state?.findings?.length || 0;
    $('log-live').textContent = project ? data.statusName(project.status) : '';
    $('graph-footer').hidden = view !== 'graph' || !project;
    for (const [id,suffix,file] of [['export-state','/state','-state.json'],['export-timeline','/export?format=timeline','-timeline.txt']]) {
      if (project) { $(id).href = pathFor(project.id) + suffix; $(id).download = project.id + file; }
      else $(id).removeAttribute('href');
    }
    const rename = document.querySelector('[data-action="rename"]');
    if (rename) rename.disabled = !project || mutating;
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
      button.title = exists ? data.nodeTypeName(ref.type) + ' · ' + ref.id : '当前任务图中没有此节点';
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
  function selectLogNode(node) {
    selectedNode = node ? {type:node.type, id:node.id} : null;
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
    meta.append(el('span', 'phase-label ' + log.phase, data.phaseName(log.phase)));
    article.append(meta, el('h3', '', log.title), el('p', '', log.body));
    if (log.scope) article.append(el('p', '', '范围：' + log.scope));
    if (log.evidence?.length) article.append(evidenceButtons(log.evidence));
    if (log.artifacts?.length) article.append(artifacts(log.artifacts));
    if (log.code) article.append(el('code', '', log.code));
    if (log.worker) article.append(el('div', 'log-worker', log.worker));
    if (log.node && graph.getNodes().some(node => nodeKey(node) === nodeKey(log.node))) {
      const link = el('button', 'plain-button log-node-link', data.nodeTypeName(log.node.type) + ' · ' + log.node.id);
      link.addEventListener('click', () => revealNode(log.node)); article.append(link);
    }
    return article;
  }
  function renderLogs({reset = false, force = false} = {}) {
    const filtered = data.filterLogs(logs, {phase, query:$('log-query').value, node:selectedNode});
    const signature = JSON.stringify([filtered, logLimit, selectedNode, phase]);
    $('log-node-filter').hidden = !selectedNode;
    $('log-node-label').textContent = selectedNode ? '关联节点 · ' + data.nodeTypeName(selectedNode.type) + ' / ' + selectedNode.id : '';
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
    const findings = state?.findings || [];
    $('findings-summary-count').textContent = findings.length + ' 项发现 · ' + findings.filter(item => item.status === 'verified' && item.support_valid === true).length + ' 项已确认';
    $('findings-list').replaceChildren(...findings.map(finding => {
      const card = el('article', 'finding-card');
      const heading = el('div', '', '发现 · ' + finding.id);
      const validity = finding.status === 'verified' && finding.support_valid !== true ? ' · 证据失效' : '';
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
    if (!state?.findings?.length) $('findings-list').append(el('p', 'empty-message', '当前项目暂无发现'));
  }
  function switchView(name) {
    view = name;
    $('graph-stage').hidden = name !== 'graph'; $('findings-view').hidden = name !== 'findings';
    $('graph-footer').hidden = name !== 'graph' || !current();
    for (const item of ['graph', 'findings']) {
      $(item + '-tab').classList.toggle('active', name === item);
      $(item + '-tab').setAttribute('aria-selected', String(name === item));
      $(item + '-tab').tabIndex = name === item ? 0 : -1;
    }
    if (name === 'findings') renderFindings();
  }
  function resetSelection(id) {
    selectedId = id; state = null; executions = []; logs = []; selectedNode = null; logSignature = ''; logLimit = 300;
    $('log-query').value = ''; setPhase('all'); setGraphFilter('all');
    graph.setState(null); switchView('graph'); renderHeader(); renderLogs({reset:true});
    try { if (id) localStorage.setItem('xloom.selected-project', id); else localStorage.removeItem('xloom.selected-project'); } catch {}
  }
  async function selectProject(id) {
    if (mutating) return;
    if (id !== selectedId || !state) resetSelection(id);
    renderProjects(); await loadWorkspace(id);
  }
  async function loadWorkspace(preferred = selectedId) {
    if (mutating) return;
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
      $('capacity-count').title = '当前持有有效执行租约的任务数';
      if (!target) {
        state = null; graph.setState(null); logs = [];
        $('graph-empty').hidden = false; renderFindings(); renderLogs({reset:true});
        setConnection(true); $('last-update').textContent = data.formatTime(overview.observed_at); return;
      }
      const firstLoad = !state;
      $('graph-empty').hidden = true;
      const [nextState, runs] = await Promise.all([api.request(pathFor(target) + '/state', options), api.request(pathFor(target) + '/executions', options)]);
      if (!requests.current(request.version)) return;
      const generation = nextState.graph.project.generation || 0;
      let cache = eventCache.get(target) || {after:0,events:[],generation};
      if (cache.generation !== generation || nextState.revision < cache.after) cache = {after:0,events:[],generation};
      if (state && (state.graph.project.generation || 0) !== generation) { selectedNode = null; graph.selectNode(null); logSignature = ''; logLimit = 300; }
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
      const latestProject = await api.request(pathFor(target),options);
      if (!requests.current(request.version)) return;
      if ((latestProject.project.generation || 0) !== generation) { eventCache.delete(target); return; }
      cache = {after,generation,events:cache.events.concat(batch)}; eventCache.set(target, cache);
      state = nextState; executions = runs.filter(run => (run.generation || 0) === generation);
      // Events may advance while the state request is in flight. Keep a coherent
      // graph/log snapshot; cached newer events become visible on the next poll.
      logs = data.buildLogs(state, cache.events.filter(event => event.revision <= state.revision), executions);
      graph.setState(state);
      renderHeader();
      if (selectedNode) {
        const node = graph.getNodes().find(item => nodeKey(item) === nodeKey(selectedNode));
        if (!node) selectLogNode(null);
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
    if (mutating) return;
    $('create-form').reset(); $('form-error').textContent = ''; $('create-dialog').showModal(); $('create-name').focus();
  }
  data.SCENARIOS.forEach((item,index) => {
    const choice = el('label', 'scenario-choice'); const radio = el('input');
    radio.type = 'radio'; radio.name = 'scenario'; radio.value = item.id; radio.defaultChecked = index === 0;
    radio.setAttribute('aria-label', item.name);
    choice.append(radio, icon(item.icon), el('strong', '', item.name), el('small', '', item.description)); $('scenario-options').append(choice);
  });
  $('create-form').addEventListener('submit', async event => {
    event.preventDefault(); if ($('submit-create').disabled || mutating) return;
    let started = false;
    try {
      const payload = data.validateProject({title:$('create-name').value, origin:$('create-origin').value, goal:$('create-goal').value,
        scenario:new FormData($('create-form')).get('scenario'), bootstrap_enabled:$('create-bootstrap').checked});
      $('submit-create').disabled = true; $('form-error').textContent = '';
      started = true; startMutation();
      const created = await api.request('/projects', {method:'POST', body:payload});
      $('create-dialog').close(); $('project-search').value = ''; resetSelection(created.project.id);
      toast('项目已创建');
    } catch (error) { $('form-error').textContent = error.message + (error.status === 0 ? '；提交结果未确认，请先刷新项目列表再决定是否重试。' : ''); }
    finally { $('submit-create').disabled = false; if (started) await finishMutation(); }
  });
  $('create-form').addEventListener('input', () => { $('form-error').textContent = ''; });
  ['new-project', 'empty-create'].forEach(id => $(id).addEventListener('click', openCreate));
  ['close-create', 'cancel-create'].forEach(id => $(id).addEventListener('click', () => { if (!mutating) $('create-dialog').close(); }));
  $('create-dialog').addEventListener('cancel', event => { if (mutating) event.preventDefault(); });
  $('help-button').addEventListener('click', () => $('about-dialog').showModal());
  ['close-about', 'acknowledge-help'].forEach(id => $(id).addEventListener('click', () => $('about-dialog').close()));
  $('project-search').addEventListener('input', renderProjects);
  $('log-query').addEventListener('input', () => { logLimit = 300; renderLogs({reset:true}); });
  document.querySelectorAll('[data-phase]').forEach(button => button.addEventListener('click', () => { setPhase(button.dataset.phase); logLimit = 300; renderLogs({reset:true}); }));
  document.querySelectorAll('[data-graph-filter]').forEach(button => button.addEventListener('click', () => setGraphFilter(button.dataset.graphFilter)));
  $('clear-log-node').addEventListener('click', () => graph.selectNode(null));
  $('zoom-in').addEventListener('click', () => graph.zoomBy(1.2)); $('zoom-out').addEventListener('click', () => graph.zoomBy(1 / 1.2));
  $('fit-graph').addEventListener('click', () => graph.fit());
  $('graph-host').addEventListener('graphzoom', event => { $('zoom-label').textContent = Math.round(event.detail.zoom * 100) + '%'; });
  ['graph', 'findings'].forEach(name => $(name + '-tab').addEventListener('click', () => switchView(name)));
  document.querySelectorAll('[role=tab]').forEach(tab => tab.addEventListener('keydown',event => {
    if (!['ArrowLeft','ArrowRight','Home','End'].includes(event.key)) return;
    event.preventDefault(); const next = event.key === 'Home' ? 'graph' : event.key === 'End' ? 'findings' : view === 'graph' ? 'findings' : 'graph';
    switchView(next); $(next + '-tab').focus();
  }));
  $('refresh-project').addEventListener('click', () => loadWorkspace(selectedId));
  $('scroll-logs').addEventListener('click', () => { $('log-stream').scrollTop = $('log-stream').scrollHeight; });
  $('autoscroll').addEventListener('click', () => {
    follow = !follow; $('autoscroll').classList.toggle('active', follow); $('autoscroll').setAttribute('aria-pressed', String(follow));
    if (follow) $('log-stream').scrollTop = $('log-stream').scrollHeight;
  });
  function startMutation() {
    mutating = true; clearTimeout(timer); requests.cancel(); closeProjectMenus(); renderHeader(); renderProjects();
  }
  async function finishMutation(preferred = selectedId) {
    mutating = false; renderHeader(); renderProjects(); await loadWorkspace(preferred);
  }
  const mutationError = error => error.message + (error.status === 0 ? '；结果未确认，请先刷新核对，避免重复操作。' : '');
  $('toggle-running').addEventListener('click',async () => {
    const project = current(); if (!project || mutating || !['active','stopped'].includes(project.status) || !connected) return;
    const next = project.status === 'active' ? 'stopped' : 'active'; startMutation();
    try { await api.request(pathFor(project.id) + '/status',{method:'PUT',body:{status:next}}); toast(next === 'stopped' ? '项目已暂停，当前执行已取消' : '项目已继续调度'); }
    catch (error) { toast(mutationError(error)); }
    finally { await finishMutation(); }
  });
  function openManagement(action,id) {
    if (mutating || !connected) return;
    const project = projects.find(item => item.id === id); if (!project) return;
    if (action === 'terminate' && !['active','stopped'].includes(project.status)) return;
    $('about-dialog').close(); closeProjectMenus();
    management = {action,id,generation:project.generation || 0}; $('manage-error').textContent = '';
    const labels = {rename:['重命名项目','新名称','保存'],restart:['重启项目','','重启项目'],terminate:['终止项目','','终止项目'],delete:['删除项目','','删除项目']}[action];
    if (!labels) return;
    $('manage-title').textContent = labels[0]; $('manage-label').textContent = labels[1]; $('submit-manage').textContent = labels[2];
    $('manage-project').textContent = project.title; $('manage-input').value = action === 'rename' ? project.title : '';
    $('manage-input').maxLength = 200; $('manage-input').hidden = $('manage-label').hidden = action !== 'rename';
    $('manage-warning').textContent = action === 'delete' ? '将永久删除此项目及其任务图、发现、执行记录和提示，并取消尚未结束的执行。'
      : action === 'restart' ? '将取消当前执行，清空本轮任务图、发现、执行记录和日志，从原始目标重新开始。保留项目名称、创建时间、原始输入和补充提示。此操作无法撤销。'
      : action === 'terminate' ? '停止调度和当前执行，并停止项目容器。保留任务图、发现、日志和证据文件，目标不会被标记为完成。终止后不能直接继续；需要重新执行时，请在左侧选择重启项目。' : '';
    $('submit-manage').className = 'button ' + (['delete','terminate'].includes(action) ? 'danger-button' : 'primary');
    $('manage-dialog').showModal(); $('cancel-manage').focus();
  }
  document.querySelectorAll('[data-action]').forEach(button => button.addEventListener('click',() => openManagement(button.dataset.action,selectedId)));
  $('terminate-project').addEventListener('click',() => openManagement('terminate',selectedId));
  $('cancel-manage').addEventListener('click',() => $('manage-dialog').close());
  $('manage-dialog').addEventListener('close',() => { management = null; });
  $('manage-dialog').addEventListener('cancel',event => { if (mutating) event.preventDefault(); });
  $('manage-form').addEventListener('submit',async event => {
    event.preventDefault(); if (!management || mutating || $('submit-manage').disabled) return;
    const command = {...management}, value = $('manage-input').value.trim();
    if (command.action === 'rename' && !value) { $('manage-error').textContent = '请填写项目名称。'; return; }
    $('submit-manage').disabled = $('cancel-manage').disabled = true; startMutation();
    try {
      const base = pathFor(command.id);
      if (command.action === 'delete') await api.request(base,{method:'DELETE'});
      if (command.action === 'restart') await api.request(base + '/restart',{method:'POST',body:{expected_generation:command.generation}});
      if (command.action === 'terminate') await api.request(base + '/terminate',{method:'POST',body:{expected_generation:command.generation}});
      if (command.action === 'rename') await api.request(base + '/title',{method:'PUT',body:{title:value}});
      $('manage-dialog').close();
      if (['delete','restart'].includes(command.action)) eventCache.delete(command.id);
      if (command.action === 'delete' && selectedId === command.id) resetSelection('');
      if (command.action === 'restart') resetSelection(command.id);
      toast(command.action === 'delete' ? '项目已删除' : command.action === 'restart' ? '项目已重启，等待重新执行' : command.action === 'terminate' ? '项目已终止，后台正在停止执行与容器' : '项目名称已保存');
    } catch (error) { $('manage-error').textContent = mutationError(error); }
    finally { $('submit-manage').disabled = $('cancel-manage').disabled = false; await finishMutation(); }
  });
  function openHint() {
    const project = current(); if (!project || !['active','stopped'].includes(project.status) || mutating || !connected) return;
    hintProjectId = project.id;
    $('hint-form').reset(); $('hint-error').textContent = ''; $('hint-length').textContent = '0'; $('hint-project').textContent = project.title;
    const hints = state?.graph?.hints || [];
    $('hint-history').hidden = !hints.length; $('hint-history').open = false; $('hint-history-count').textContent = hints.length + ' 条';
    $('hint-history-list').replaceChildren(...hints.map(hint => {
      const item = el('article','hint-history-entry'), time = el('time','',data.formatTime(hint.created_at));
      if (hint.created_at) time.dateTime = hint.created_at;
      item.append(time,el('p','',hint.content)); return item;
    }));
    $('hint-state-note').textContent = project.status === 'stopped' ? '提交后仍保持暂停，继续执行时使用这些提示。' : '保存后会作为后续规划的补充信息。';
    $('hint-dialog').showModal(); $('hint-text').focus();
  }
  $('add-hint').addEventListener('click',openHint);
  ['close-hint','cancel-hint'].forEach(id => $(id).addEventListener('click',() => { if (!mutating) $('hint-dialog').close(); }));
  $('hint-dialog').addEventListener('close',() => { hintProjectId = ''; });
  $('hint-dialog').addEventListener('cancel',event => { if (mutating) event.preventDefault(); });
  $('hint-text').addEventListener('input',() => { $('hint-length').textContent = $('hint-text').value.length; $('hint-error').textContent = ''; });
  $('hint-form').addEventListener('submit',async event => {
    event.preventDefault(); if (!hintProjectId || mutating) return;
    const id = hintProjectId, content = $('hint-text').value.trim();
    if (!content) { $('hint-error').textContent = '请输入补充提示。'; return; }
    if (content.length > 32768) { $('hint-error').textContent = '提示不能超过 32768 个字符。'; return; }
    $('submit-hint').disabled = true; startMutation();
    try {
      await api.request(pathFor(id) + '/hints',{method:'POST',body:{content,creator:'user'}});
      $('hint-dialog').close(); selectedNode = null; graph.selectNode(null); setPhase('all'); $('log-query').value = ''; toast('补充提示已保存');
    } catch (error) { $('hint-error').textContent = mutationError(error); }
    finally { $('submit-hint').disabled = false; await finishMutation(); }
  });
  document.addEventListener('click',event => { if (!event.target.closest('.project-actions')) closeProjectMenus(); });
  window.addEventListener('resize',closeProjectMenus); document.addEventListener('scroll',closeProjectMenus,true);
  document.addEventListener('keydown', event => {
    if (event.ctrlKey || event.metaKey || event.altKey || event.repeat || document.querySelector('dialog[open]') || event.target.closest('input,textarea,select,[contenteditable="true"]')) return;
    if (event.key.toLowerCase() === 'n') { event.preventDefault(); openCreate(); }
    if (event.key === '/') { event.preventDefault(); $('project-search').focus(); }
    if (event.key === 'Escape') { graph.selectNode(null); closeProjectMenus(); }
  });
  document.addEventListener('visibilitychange', () => { if (!document.hidden) loadWorkspace(selectedId); });
  window.addEventListener('pagehide', () => { clearTimeout(timer); requests.cancel(); });
  window.addEventListener('pageshow', event => { if (event.persisted) loadWorkspace(selectedId); });
  setPhase('all'); setGraphFilter('all'); loadWorkspace(selectedId);
}());
