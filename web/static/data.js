(function (root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.XLoomData = api;
}(typeof window === 'object' ? window : globalThis, function () {
  'use strict';

  const SCENARIOS = Object.freeze([
    Object.freeze({id:'ctf', name:'CTF', description:'围绕明确目标验证结果', icon:'flag'}),
    Object.freeze({id:'pentest', name:'渗透测试', description:'记录过程中的观察与发现', icon:'shield'}),
    Object.freeze({id:'audit', name:'代码审计', description:'从代码到可追溯的结论', icon:'code'})
  ]);
  const NAMES = Object.freeze({
    active:'进行中', running:'进行中', paused:'已暂停', stopped:'已暂停', completed:'已完成', terminated:'已终止',
    open:'待执行', achieved:'已达成', withdrawn:'已撤回', abandoned:'已放弃',
    valid:'有效', input:'项目输入', superseded:'已被替代', refuted:'已反驳', narrowed:'范围已收窄',
    candidate:'待验证', verified:'已验证', failed:'执行失败', rejected:'结果被拒绝', cancelled:'已取消',
    prepared:'等待执行', retryable:'等待恢复', result_pending:'结果待写回', succeeded:'执行完成',
    retry_requested:'等待重试'
  });
  const PHASES = {bootstrap:'Bootstrap', reason:'Decide', explore:'Execute', intent:'Execute'};
  const PHASE_NAMES = Object.freeze({bootstrap:'启动引导',reason:'决策',decide:'决策',explore:'执行',intent:'执行',execute:'执行',system:'系统',model:'模型结论'});
  const NODE_TYPE_NAMES = Object.freeze({start:'起点',origin:'起点',step:'任务',intent:'任务',fact:'事实',finding:'发现',goal:'目标'});
  const TIME_FORMATTER = new Intl.DateTimeFormat('en-CA', {
    timeZone:'Asia/Shanghai',calendar:'gregory',numberingSystem:'latn',
    year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit',hourCycle:'h23'
  });
  const array = value => Array.isArray(value) ? value : [];
  const string = value => typeof value === 'string' ? value : '';
  const object = value => value && typeof value === 'object' && !Array.isArray(value) ? value : {};
  const nodeKey = node => node && string(node.type) && string(node.id) ? node.type + ':' + node.id : '';
  const ref = (type, id) => string(id) ? {type, id} : undefined;
  const refs = ids => [...new Set(array(ids).filter(id => typeof id === 'string' && id))].map(id => ({type:'fact', id}));
  const statusName = value => NAMES[value] || string(value) || '未知状态';
  const scenarioName = value => (SCENARIOS.find(scenario => scenario.id === value) || {name:'未分类'}).name;
  const phaseName = value => PHASE_NAMES[string(value).toLowerCase()] || string(value) || '系统';
  const nodeTypeName = value => NODE_TYPE_NAMES[string(value).toLowerCase()] || string(value) || '节点';

  function validateProject(input) {
    input = object(input);
    const fields = [['title',200,'项目名称'],['origin',32768,'项目输入'],['goal',32768,'项目目标']];
    const payload = {};
    for (const [key, limit, label] of fields) {
      const value = string(input[key]).trim();
      if (!value || value.length > limit) throw new Error(label + '需填写 1–' + limit + ' 个字符。');
      payload[key] = value;
    }
    if (!SCENARIOS.some(scenario => scenario.id === input.scenario)) throw new Error('请选择一个项目场景。');
    if (input.bootstrap_enabled !== undefined && typeof input.bootstrap_enabled !== 'boolean') throw new Error('启动引导必须为布尔值。');
    payload.scenario = input.scenario;
    payload.bootstrap_enabled = input.bootstrap_enabled === undefined ? true : input.bootstrap_enabled;
    return payload;
  }

  function formatTime(value) {
    if (!string(value)) return '未记录时间';
    const date = new Date(value);
    if (!Number.isFinite(date.getTime())) return '未记录时间';
    const parts = Object.fromEntries(TIME_FORMATTER.formatToParts(date).map(part => [part.type,part.value]));
    return parts.year.padStart(4,'0') + '-' + parts.month + '-' + parts.day + ' ' + parts.hour + ':' + parts.minute + ':' + parts.second;
  }

  function timestamp(value) {
    const time = string(value) ? Date.parse(value) : NaN;
    return Number.isFinite(time) ? time : null;
  }

  function belongsToCurrentRound(execution, project) {
    if (project.id && execution.project_id && execution.project_id !== project.id) return false;
    const generation = Number.isInteger(project.generation) ? project.generation : 0;
    if (Number.isInteger(execution.generation) && execution.generation !== generation) return false;
    const restarted = timestamp(project.restarted_at);
    if (restarted !== null) {
      const created = timestamp(execution.created_at);
      if (created !== null && created < restarted) return false;
      // Legacy execution summaries have no generation. After a restart they
      // need a current-round timestamp before their conclusions can be shown.
      if (!Number.isInteger(execution.generation) && created === null) return false;
    }
    return true;
  }

  function terminationTime(project) {
    if (project.status !== 'terminated') return null;
    const terminated = timestamp(project.terminated_at);
    const restarted = timestamp(project.restarted_at);
    return terminated !== null && (restarted === null || terminated >= restarted) ? terminated : null;
  }

  function projectTiming(state, executions = []) {
    state = object(state);
    const graph = object(state.graph), project = object(graph.project);
    const restarted = timestamp(project.restarted_at);
    const currentTime = value => {
      const time = timestamp(value);
      return time !== null && (restarted === null || time >= restarted) ? time : null;
    };
    const starts = [];
    const addStart = value => { const time = currentTime(value); if (time !== null) starts.push(time); };
    addStart(object(project.reason).started_at);
    for (const intent of array(graph.intents)) addStart(object(intent).started_at);
    for (const item of array(executions)) {
      const execution = object(item);
      if (!belongsToCurrentRound(execution,project)) continue;
      // A prepared record is still waiting for execution. Neither project/intent
      // creation nor a later heartbeat can supply a missing execution start.
      if (!['running','retryable','result_pending','succeeded','failed','rejected','cancelled','retry_requested'].includes(execution.status)) continue;
      addStart(execution.started_at || execution.created_at);
    }
    const started = starts.length ? Math.min(...starts) : null;
    const rootGoal = array(state.goals).find(goal => goal && goal.id === 'goal');
    const ends = [];
    if (project.status === 'terminated') {
      // Termination ends the round without claiming that the root goal was
      // achieved. Only the persisted termination time can close this round.
      const terminated = terminationTime(project);
      if (terminated !== null && (started === null || terminated >= started)) ends.push(terminated);
    } else if (project.status === 'completed' && (!rootGoal || rootGoal.status === 'achieved')) {
      for (const item of array(graph.intents)) {
        const intent = object(item);
        if (intent.to !== 'goal') continue;
        const time = currentTime(intent.concluded_at);
        if (time !== null && (started === null || time >= started)) ends.push(time);
      }
    }
    return {
      startedAt:started === null ? null : new Date(started).toISOString(),
      endedAt:ends.length ? new Date(Math.max(...ends)).toISOString() : null
    };
  }

  function taskProgress(state) {
    state = object(state);
    const graph = object(state.graph), project = object(graph.project);
    const records = array(state.steps).length ? array(state.steps) : array(graph.intents).map(item => {
      const intent = object(item);
      return {...intent,status:string(intent.to) ? 'completed' : intent.concluded_at ? 'abandoned' : string(intent.worker) ? 'running' : 'open'};
    });
    const tasks = new Map();
    for (const item of records) {
      const step = object(item);
      if (string(step.id)) tasks.set(step.id,step);
    }
    let completed = 0, running = 0;
    for (const step of tasks.values()) {
      if (step.status === 'completed') completed++;
      if (project.status === 'active' && step.status === 'running') running++;
    }
    return {completed,total:tasks.size,running};
  }

  function recordBody(record, type) {
    const primary = type === 'goal' ? record.condition : type === 'finding' ? record.claim : record.description;
    const description = string(primary);
    return string(record.reason) ? description + '\n\n' + record.reason : description;
  }

  function sourceSupport(record, facts) {
    // State computes support_valid; event results are stored before that computation.
    if (typeof record.support_valid === 'boolean') return record.support_valid;
    const sources = refs(record.sources);
    return sources.length > 0 && sources.every(source => facts.get(source.id)?.status === 'valid');
  }

  function findingLabel(finding, supportValid) {
    let label = statusName(finding.status);
    if (supportValid === false && array(finding.sources).length > 0) label += ' · 证据失效';
    else if (finding.status === 'verified' && supportValid !== true) label = '已标记验证 · 缺少有效证据';
    return label;
  }

  function recordLog(record, type, base, facts) {
    const status = string(record.status);
    const log = {...base, node:ref(type, record.id), body:recordBody(record, type), status,
      title:({goal:'目标',step:'步骤',fact:'事实',finding:'关键结论'}[type] || type) + ' · ' + statusName(status),
      level:['failed','refuted','superseded','abandoned','withdrawn'].includes(status) ? 'warning' : 'info',
      evidence:refs(type === 'step' ? record.from : record.sources)};
    if (type === 'fact' || type === 'finding') {
      log.scope = string(record.scope);
      log.artifacts = array(record.evidence).map(evidence => ({
        run_id:string(evidence.run_id),path:string(evidence.path),excerpt:string(evidence.excerpt),
        start_line:Number(evidence.start_line) || 0,end_line:Number(evidence.end_line) || 0
      }));
    }
    if (type === 'goal' && status === 'achieved') {
      log.supportValid = sourceSupport(record, facts);
      if (!log.supportValid) {
        log.title += ' · 证据失效';
        log.level = 'warning';
      }
    }
    if (type === 'finding') {
      log.kind = 'model';
      log.phase = 'Model';
      log.supportValid = sourceSupport(record, facts);
      log.statusLabel = findingLabel(record, log.supportValid);
      log.title = '关键结论 · ' + log.statusLabel;
      log.level = status === 'verified' && log.supportValid ? 'success' : status === 'candidate' ? 'info' : 'warning';
    }
    return log;
  }

  function fingerprint(log) {
    return JSON.stringify([nodeKey(log.node),log.body,log.status,log.scope,log.supportValid,log.evidence,log.artifacts]);
  }

  function parseFinal(text) {
    // Like the output contract, accept a JSON object or a JSON object wrapped in text.
    // Only explicitly selected fields are returned to the UI, never reasoning fields.
    text = string(text).trim();
    try {
      const parsed = JSON.parse(text);
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) return parsed;
    } catch (_) { /* Try a fenced or prefixed final object. */ }
    for (let start = text.indexOf('{'); start >= 0; start = text.indexOf('{', start + 1)) {
      let depth = 0, quoted = false, escaped = false;
      for (let index = start; index < text.length; index++) {
        const char = text[index];
        if (quoted) {
          if (escaped) escaped = false;
          else if (char === '\\') escaped = true;
          else if (char === '"') quoted = false;
        } else if (char === '"') quoted = true;
        else if (char === '{') depth++;
        else if (char === '}' && --depth === 0) {
          try { return object(JSON.parse(text.slice(start, index + 1))); } catch (_) { start = index; break; }
        }
      }
      // Never salvage an inner object from an incomplete outer response.
      if (depth !== 0) return null;
    }
    return null;
  }

  function visibleFinalText(value) {
    return string(value).replace(/<(think|thinking|analysis)\b[^>]*>[\s\S]*?(?:<\/\1\s*>|$)/gi, '').trim();
  }

  function redactReasoning(value) {
    if (Array.isArray(value)) return value.map(redactReasoning);
    if (!value || typeof value !== 'object') return value;
    const publicFields = {};
    for (const [key, item] of Object.entries(value)) {
      if (!['thinking','reasoning','analysis','reasoning_content','chain_of_thought'].includes(key.toLowerCase())) {
        Object.defineProperty(publicFields,key,{value:redactReasoning(item),enumerable:true,configurable:true,writable:true});
      }
    }
    return publicFields;
  }

  function buildLogs(state, events = [], executions = []) {
    state = object(state);
    const graph = object(state.graph), project = object(graph.project);
    const rows = [], identities = new Set(), represented = new Set();
    const facts = new Map(array(state.fact_records).map(fact => [fact.id,fact]));
    const currentSupportRecords = {
      finding:new Map(array(state.findings).map(finding => [finding.id,finding])),
      goal:new Map(array(state.goals).map(goal => [goal.id,goal]))
    };
    const intents = new Map(array(graph.intents).map(intent => [intent.id,intent]));
    const uniqueEvents = new Map();
    for (const event of array(events)) {
      if (!event || typeof event !== 'object') continue;
      const key = Number.isSafeInteger(event.revision) && event.revision > 0 ? String(event.revision)
        : JSON.stringify([event.op,event.id,event.run_id,event.created_at,event.payload,event.result]);
      if (!uniqueEvents.has(key)) uniqueEvents.set(key, event);
    }
    const orderedEvents = [...uniqueEvents.entries()].sort((a,b) => (Number(a[1].revision) || 0) - (Number(b[1].revision) || 0));
    const lastEvent = new Map();
    for (const [key,event] of orderedEvents) lastEvent.set(event.op + ':' + event.id,key);
    function add(log) {
      if (!log || identities.has(log.id)) return;
      identities.add(log.id);
      represented.add(fingerprint(log));
      rows.push({phase:'System',level:'info',worker:'',title:'',body:'',time:'',...log});
    }
    const base = (id,time,source,worker = '') => ({id,time:string(time),source,worker:string(worker)});

    if (string(project.id)) add({...base('project:' + project.id,project.created_at,'state'),title:'项目已创建',body:string(project.title)});
    if (Number.isInteger(project.generation) && project.generation > 0 && timestamp(project.restarted_at) !== null) {
      add({...base('project:' + string(project.id) + ':restart:' + project.generation,project.restarted_at,'state'),
        title:'项目已重启',body:'已清空本轮任务图、发现、执行记录和日志，保留原始输入、目标和补充提示，等待重新执行。'});
    }
    if (terminationTime(project) !== null) {
      const generation = Number.isInteger(project.generation) ? project.generation : 0;
      add({...base('project:' + string(project.id) + ':terminate:' + generation,project.terminated_at,'state'),
        title:'项目已终止',body:'本轮已终止，不再调度任务。已有任务图、发现、执行记录和日志已保留。'});
    }
    for (const [key,event] of orderedEvents) {
      const payload = object(event.payload), result = object(event.result);
      const common = {...base('event:' + key,event.created_at,'event',event.run_id),revision:event.revision};
      if (['goal','step','fact','finding'].includes(event.op)) {
        let record = {...payload,...result,id:string(event.id) || string(result.id)};
        if (event.op === 'finding' || event.op === 'goal') {
          // Stored event support_valid defaults false. Use current authoritative state
          // for the latest event, and current source effectiveness for older evidence.
          delete record.support_valid;
          const current = currentSupportRecords[event.op].get(record.id);
          const snapshotCoversEvent = Number.isSafeInteger(state.revision) && Number.isSafeInteger(event.revision)
            && state.revision >= event.revision;
          if (current && snapshotCoversEvent && lastEvent.get(event.op + ':' + record.id) === key) record = {...record,...current};
        }
        const log = recordLog(record,event.op,{...common,phase:['goal','step'].includes(event.op) ? 'Decide' : 'Execute'},facts);
        add(log);
      } else if (event.op === 'fact_relation') {
        const relation = {...payload,...result};
        add({...common,phase:'Execute',title:'事实关系 · ' + ({supersedes:'替代',refutes:'反驳',narrows:'收窄'}[relation.kind] || string(relation.kind)),
          body:string(relation.reason),node:ref('fact',relation.target),evidence:refs([relation.source,relation.target]),
          code:string(relation.source) + ' → ' + string(relation.target)});
      } else if (event.op === 'execution_failed') {
        const failure = {...payload,...result};
        add({...common,phase:'Execute',level:'error',title:statusName(failure.status || 'failed'),body:string(failure.reason),
          node:ref('step',event.id),status:string(failure.status),runId:string(failure.run_id)});
      } else {
        add({...common,title:'状态更新 · ' + string(event.op),body:string(result.description) || string(payload.reason)});
      }
    }

    function supplement(record,type,time,worker = '') {
      const log = recordLog(record,type,{...base('state:' + type + ':' + record.id,time,'state',worker),
        phase:type === 'step' || type === 'goal' ? 'Decide' : type === 'fact' ? 'Execute' : 'Model'},facts);
      if (!represented.has(fingerprint(log))) add(log);
    }
    for (const goal of array(state.goals)) supplement(goal,'goal',goal.created_at);
    for (const step of array(state.steps)) {
      const intent = intents.get(step.id) || {};
      supplement(step,'step',intent.concluded_at || step.created_at,step.worker || intent.creator);
    }
    if (!array(state.steps).length) for (const intent of array(graph.intents)) {
      supplement({...intent,status:intent.to ? 'completed' : intent.worker ? 'running' : 'open'},'step',intent.concluded_at || intent.created_at,intent.worker || intent.creator);
    }
    const representedFacts = new Set();
    for (const fact of [...array(state.fact_records),...array(graph.facts)]) {
      if (representedFacts.has(fact.id)) continue;
      representedFacts.add(fact.id);
      const intent = array(graph.intents).find(item => item.to === fact.id);
      const record = {...fact,status:fact.status || (fact.id === 'origin' || fact.id === 'goal' ? 'input' : 'valid')};
      const time = fact.observed_at || intent?.concluded_at || (record.status === 'input' ? project.created_at : '');
      supplement(record,'fact',time,fact.run_id || intent?.worker);
    }
    for (const finding of array(state.findings)) supplement(finding,'finding',finding.updated_at || finding.created_at);
    for (const hint of array(graph.hints)) add({...base('hint:' + hint.id,hint.created_at,'state',hint.creator),
      title:'项目提示',body:string(hint.content)});
    if (project.reason) {
      const reason = object(project.reason);
      add({...base('reason:' + string(reason.worker) + ':' + string(reason.started_at),reason.started_at,'state',reason.worker),
        phase:'Decide',title:'决策执行中',body:string(reason.trigger)});
    }

    const uniqueExecutions = new Map();
    for (const execution of array(executions)) {
      if (!execution || !string(execution.id) || !belongsToCurrentRound(execution,project)) continue;
      const old = uniqueExecutions.get(execution.id);
      if (!old || string(execution.updated_at) >= string(old.updated_at)) uniqueExecutions.set(execution.id,execution);
    }
    for (const execution of uniqueExecutions.values()) {
      const result = object(execution.result), phase = PHASES[execution.kind] || 'System';
      const failed = ['failed','rejected','cancelled'].includes(execution.status) || Boolean(string(result.error));
      const common = {...base('execution:' + execution.id,execution.updated_at || execution.created_at,'execution',execution.backend),
        runId:execution.id,phase,status:string(execution.status),node:ref('step',execution.intent)};
      const finalText = visibleFinalText(result.text);
      const parsedFinal = parseFinal(finalText.slice(0,10000));
      const publicFinal = parsedFinal && redactReasoning(parsedFinal);
      const output = parsedFinal && JSON.stringify(publicFinal) !== JSON.stringify(parsedFinal) ? JSON.stringify(publicFinal,null,2) : finalText;
      const truncated = result.truncated === true || finalText.length > 10000;
      const parsed = !failed && !truncated && execution.status === 'succeeded' ? publicFinal : null;
      const data = parsed && (parsed.accepted === true ? object(parsed.data) : parsed.accepted === undefined ? parsed : null);
      const conclusions = [];
      if (data) {
        if (execution.kind === 'explore' && string(data.description).trim()) conclusions.push({text:data.description});
        if (execution.kind === 'bootstrap' && string(object(data.fact).description).trim()) conclusions.push({text:data.fact.description});
        if (['bootstrap','reason'].includes(execution.kind) && string(object(data.complete).description).trim()) {
          conclusions.push({text:data.complete.description,evidence:refs(data.complete.from)});
        }
      }
      let body = string(result.error);
      if (output && !conclusions.length) body += (body ? '\n\n' : '') + output.slice(0,10000);
      if (!body) body = execution.resumes > 0 ? '已恢复 ' + execution.resumes + ' 次。' : '';
      if (truncated) body += (body ? '\n\n' : '') + '输出已截断';
      add({...common,level:failed ? 'error' : 'info',title:(phase === 'System' ? '执行' : phaseName(phase)) + ' · ' + statusName(execution.status),body,truncated});
      conclusions.forEach((conclusion,index) => add({...common,id:common.id + ':conclusion:' + index,kind:'model',phase:'Model',
        title:'关键结论 · 执行结果',body:conclusion.text,level:'info',evidence:conclusion.evidence || [],truncated,
        statusLabel:'执行结果',supportValid:undefined}));
    }
    return rows.map((row,index) => ({row,index})).sort((a,b) => {
      const at = Date.parse(a.row.time), bt = Date.parse(b.row.time);
      const diff = (Number.isFinite(at) ? at : -Infinity) - (Number.isFinite(bt) ? bt : -Infinity);
      return (Number.isNaN(diff) ? 0 : diff) || a.index - b.index;
    }).map(entry => entry.row);
  }

  function filterLogs(logs, options = {}) {
    const query = string(options.query).trim().toLocaleLowerCase();
    const selected = nodeKey(options.node);
    return array(logs).filter(log => (!options.phase || options.phase === 'all' || log.phase === options.phase)
      && (!selected || nodeKey(log.node) === selected || array(log.evidence).some(evidence => nodeKey(evidence) === selected))
      && (!query || [log.title,log.body,log.code,log.worker,log.scope,log.statusLabel,nodeKey(log.node),...array(log.evidence).map(nodeKey)]
        .filter(Boolean).join(' ').toLocaleLowerCase().includes(query)));
  }

  return {SCENARIOS,validateProject,scenarioName,statusName,phaseName,nodeTypeName,formatTime,projectTiming,taskProgress,buildLogs,filterLogs};
}));
