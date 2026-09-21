const test = require('node:test');
const assert = require('node:assert/strict');
const data = require('../static/data.js');

function fixture() {
  return {
    revision:3,
    graph:{project:{id:'project-a',title:'真实项目',status:'active',created_at:'2026-09-21T01:00:00Z'},
      facts:[{id:'origin',description:'本机测试服务'},{id:'goal',description:'检查访问约束'},{id:'f1',description:'响应状态'}],
      intents:[{id:'s1',from:['origin'],to:'f1',description:'请求服务',creator:'planner@r1',worker:'worker@r2',created_at:'2026-09-21T01:01:00Z',concluded_at:'2026-09-21T01:02:00Z'}],hints:[]},
    goals:[{id:'goal',condition:'检查访问约束',status:'open',sources:[],created_at:'2026-09-21T01:00:00Z',support_valid:false}],
    steps:[{id:'s1',from:['origin'],description:'请求服务',status:'completed',created_at:'2026-09-21T01:01:00Z'}],
    fact_records:[{id:'f1',description:'响应状态',status:'valid',scope:'本机',observed_at:'2026-09-21T01:02:00Z'}],
    findings:[{id:'finding1',claim:'请求被正确拒绝',scope:'匿名请求',status:'verified',sources:['f1'],evidence:[],created_at:'2026-09-21T01:03:00Z',updated_at:'2026-09-21T01:03:00Z',support_valid:true}]
  };
}

test('three explicit scenarios and legacy projects remain unclassified', () => {
  assert.deepEqual(data.SCENARIOS.map(item => item.id),['ctf','pentest','audit']);
  assert.equal(data.scenarioName(''),'未分类');
  assert.equal(data.scenarioName('unknown'),'未分类');
  assert.equal(data.scenarioName('audit'),'代码审计');
});

test('project validation preserves plain input and bootstrap choice', () => {
  const input = {title:'  <img src=x onerror=alert(1)> ',origin:'  http://localhost:8080\n范围仅本机  ',goal:'  检查权限  ',scenario:'audit',bootstrap_enabled:false};
  const saved = JSON.stringify(input);
  assert.deepEqual(data.validateProject(input),{title:input.title.trim(),origin:input.origin.trim(),goal:'检查权限',scenario:'audit',bootstrap_enabled:false});
  assert.equal(JSON.stringify(input),saved);
  assert.equal(data.validateProject({...input,bootstrap_enabled:undefined}).bootstrap_enabled,true);
});

test('project validation rejects missing, invalid types and excessive field lengths', () => {
  const valid = {title:'项目',origin:'源代码',goal:'审计',scenario:'audit'};
  for (const key of ['title','origin','goal']) {
    assert.throws(() => data.validateProject({...valid,[key]:' '}));
    assert.throws(() => data.validateProject({...valid,[key]:{value:'x'}}));
    assert.throws(() => data.validateProject({...valid,[key]:'x'.repeat(key === 'title' ? 201 : 32769)}));
  }
  assert.throws(() => data.validateProject({...valid,scenario:'reason'}));
  assert.throws(() => data.validateProject({...valid,bootstrap_enabled:'false'}));
});

test('empty backend state never produces sample projects, nodes or logs', () => {
  assert.deepEqual(data.buildLogs({},[],[]),[]);
  assert.deepEqual(data.buildLogs(null),[]);
});

test('real state contributes graph facts, steps, hints and active decision without invented timestamps', () => {
  const state = fixture();
  state.graph.hints.push({id:'h1',content:'只检查已授权目录',creator:'human',created_at:'2026-09-21T01:04:00Z'});
  state.graph.project.reason = {worker:'general@r3',trigger:'新事实',started_at:'2026-09-21T01:05:00Z'};
  state.graph.facts.push({id:'legacy',description:'旧事实没有时间'});
  const logs = data.buildLogs(state);
  assert(logs.some(log => log.title === '项目提示' && log.body === '只检查已授权目录'));
  assert(logs.some(log => log.phase === 'Decide' && log.worker === 'general@r3' && log.body === '新事实'));
  assert.equal(logs.find(log => log.node?.id === 'legacy').time,'');
  assert.equal(logs.find(log => log.node?.type === 'step').time,'2026-09-21T01:02:00Z');
  assert.equal(data.formatTime(''),'未记录时间');
  assert.equal(data.formatTime('invalid'),'未记录时间');
});

test('paginated duplicate events collapse and equivalent state is suppressed', () => {
  const state = fixture();
  const fact = state.fact_records[0];
  const event = {revision:1,op:'fact',id:'f1',run_id:'worker@r2',created_at:fact.observed_at,result:fact};
  const logs = data.buildLogs(state,[event,{...event}]);
  assert.equal(logs.filter(log => log.node?.type === 'fact' && log.node.id === 'f1').length,1);
  assert.equal(logs.filter(log => log.id === 'event:1').length,1);
});

test('fact logs retain full node content, scope and structured evidence after the graph popup is removed', () => {
  const state = fixture();
  const evidence = {run_id:'run-2',path:'/workspace/proof.txt',start_line:12,end_line:14,excerpt:'<script>plain evidence</script>\nHTTP 403'};
  Object.assign(state.fact_records[0], {
    description:'完整事实内容'.repeat(100),scope:'只验证匿名请求',evidence:[evidence]
  });
  const before = structuredClone(state);
  const log = data.buildLogs(state).find(log => log.node?.type === 'fact' && log.node.id === 'f1');
  assert.equal(log.body,state.fact_records[0].description);
  assert.equal(log.scope,'只验证匿名请求');
  assert.deepEqual(log.artifacts,[evidence]);
  assert.notEqual(log.kind,'model');
  log.artifacts[0].excerpt = 'changed';
  assert.deepEqual(state,before,'log evidence must be detached from the source state');
});

test('a fact snapshot with evidence missing from its event is still available in the log', () => {
  const state = fixture();
  const fact = state.fact_records[0];
  fact.evidence = [{run_id:'run-2',path:'/workspace/proof.txt',start_line:1,end_line:1,excerpt:'HTTP 403'}];
  const event = {revision:1,op:'fact',id:fact.id,created_at:fact.observed_at,result:{...fact,evidence:[]}};
  const logs = data.buildLogs(state,[event]).filter(log => log.node?.type === 'fact' && log.node.id === fact.id);
  assert.equal(logs.length,2);
  assert.deepEqual(logs.find(log => log.source === 'state').artifacts,fact.evidence);
  assert.equal(data.buildLogs(state,[{...event,result:fact}]).filter(log => log.node?.type === 'fact' && log.node.id === fact.id).length,1);
});

test('goal logs retain reasons, source links and authoritative support warnings', () => {
  const state = fixture();
  const goal = state.goals[0];
  Object.assign(goal,{status:'achieved',sources:['f1'],reason:'由响应证据确认',support_valid:true});
  const event = {revision:3,op:'goal',id:goal.id,created_at:goal.created_at,result:{...goal,support_valid:false}};
  const logs = data.buildLogs(state,[event]).filter(log => log.node?.type === 'goal');
  assert.equal(logs.length,1,'equivalent state must not duplicate the goal event');
  assert.equal(logs[0].supportValid,true,'event defaults must not hide current support');
  assert.doesNotMatch(logs[0].title,/证据失效/);
  assert.match(logs[0].body,/由响应证据确认/);
  assert.deepEqual(logs[0].evidence,[{type:'fact',id:'f1'}]);
  goal.support_valid = false;
  const invalid = data.buildLogs(state,[event]).find(log => log.node?.type === 'goal');
  assert.equal(invalid.supportValid,false);
  assert.equal(invalid.level,'warning');
  assert.match(invalid.title,/证据失效/);
  goal.status = 'open';
  const open = data.buildLogs(state).find(log => log.node?.type === 'goal');
  assert.doesNotMatch(open.title,/证据失效/,'open goals do not claim completed support');
});

test('same node transitions stay in timeline without repeated final snapshot', () => {
  const state = fixture();
  const finding = state.findings[0];
  const events = [
    {revision:2,op:'finding',id:finding.id,created_at:'2026-09-21T01:02:20Z',result:{...finding,status:'candidate',sources:[],support_valid:false}},
    {revision:3,op:'finding',id:finding.id,created_at:finding.updated_at,result:{...finding,support_valid:false}}
  ];
  const logs = data.buildLogs(state,[events[1],events[0],events[1]]).filter(log => log.node?.type === 'finding');
  assert.equal(logs.length,2);
  assert.match(logs[0].title,/待验证/);
  assert.match(logs[1].title,/已验证/);
  assert.equal(logs[1].supportValid,true);
  assert.deepEqual(logs[1].evidence,[{type:'fact',id:'f1'}]);
});

test('candidate, refuted and invalid supporting facts cannot be rendered as verified', () => {
  for (const status of ['candidate','refuted','verified']) {
    const state = fixture();
    state.findings[0].status = status;
    state.findings[0].support_valid = false;
    const log = data.buildLogs(state).find(log => log.kind === 'model');
    assert.equal(log.status,status);
    assert.equal(log.supportValid,false);
    assert.notEqual(log.level,'success');
    assert.match(log.title,/证据失效/);
    if (status === 'candidate') assert.match(log.title,/待验证/);
    if (status === 'refuted') assert.match(log.title,/已反驳/);
  }
});

test('events newer than the state snapshot retain their actual finding status', () => {
  const state = fixture();
  const newer = {...state.findings[0],status:'refuted',reason:'后续复核发现反例',updated_at:'2026-09-21T01:04:00Z',support_valid:false};
  const event = {revision:4,op:'finding',id:newer.id,created_at:newer.updated_at,result:newer};
  const log = data.buildLogs(state,[event]).find(item => item.id === 'event:4');
  assert.equal(log.status,'refuted');
  assert.match(log.title,/已反驳/);
  assert.match(log.body,/后续复核发现反例/);
});

test('typed node filtering isolates repeated ids while finding sources link to facts', () => {
  const logs = [
    {id:'f',phase:'Execute',body:'fact',node:{type:'fact',id:'same'}},
    {id:'s',phase:'Execute',body:'step',node:{type:'step',id:'same'}},
    {id:'m',phase:'Model',body:'Supported answer',node:{type:'finding',id:'f1'},evidence:[{type:'fact',id:'same'}]}
  ];
  assert.deepEqual(data.filterLogs(logs,{node:{type:'fact',id:'same'}}).map(log => log.id),['f','m']);
  assert.deepEqual(data.filterLogs(logs,{node:{type:'step',id:'same'}}).map(log => log.id),['s']);
  assert.deepEqual(data.filterLogs(logs,{node:{type:'fact',id:'same'},phase:'Model',query:'SUPPORTED'}).map(log => log.id),['m']);
  assert.deepEqual(data.filterLogs(logs,{query:'missing'}),[]);
});

test('logs sort by real ISO times, retain stable ties and do not mutate inputs', () => {
  const state = fixture();
  const events = [
    {revision:2,op:'custom',id:'2',created_at:'2026-09-21T03:30:00+02:00'},
    {revision:1,op:'custom',id:'1',created_at:'2026-09-21T01:30:00Z'},
    {revision:3,op:'custom',id:'3',created_at:'2026-09-21T01:20:00Z'}
  ];
  const before = JSON.stringify({state,events});
  assert.deepEqual(data.buildLogs(state,events).filter(log => log.source === 'event').map(log => log.id),['event:3','event:1','event:2']);
  assert.equal(JSON.stringify({state,events}),before);
});

test('successful execution only exposes explicit final descriptions and fact references', () => {
  const state = fixture();
  const executions = [{id:'run1',project_id:'project-a',kind:'reason',backend:'general',intent:'s1',status:'succeeded',updated_at:'2026-09-21T01:10:00Z',
    result:{text:JSON.stringify({accepted:true,data:{complete:{description:'访问边界检查完成',from:['f1']},thinking:'DO NOT SHOW'}})}}];
  const model = data.buildLogs(state,[],executions).find(log => log.id === 'execution:run1:conclusion:0');
  assert.equal(model.body,'访问边界检查完成');
  assert.deepEqual(model.evidence,[{type:'fact',id:'f1'}]);
  assert.equal(model.worker,'general');
  assert.equal(model.statusLabel,'执行结果');
  assert.equal(model.supportValid,undefined);
  assert(!JSON.stringify(model).includes('DO NOT SHOW'));
});

test('prefixed JSON is tolerated while arbitrary prose is never a confirmed model conclusion', () => {
  const state = fixture();
  const executions = [
    {id:'json',kind:'explore',status:'succeeded',result:{text:'最终结果：\n```json\n{"accepted":true,"data":{"description":"响应已记录"}}\n```'}},
    {id:'prose',kind:'explore',status:'succeeded',result:{text:'看起来完成了'}},
    {id:'reason',kind:'reason',status:'succeeded',result:{text:'{"accepted":true,"data":{"decided":true}}'}}
  ];
  const logs = data.buildLogs(state,[],executions);
  assert.equal(logs.find(log => log.id === 'execution:json:conclusion:0').body,'响应已记录');
  assert(!logs.some(log => log.id.startsWith('execution:prose:conclusion')));
  assert(!logs.some(log => log.id.startsWith('execution:reason:conclusion')));
  assert.equal(logs.find(log => log.id === 'execution:prose').body,'看起来完成了');
});

test('failed and rejected execution output stays diagnostic, errors and truncation visible', () => {
  const state = fixture();
  const executions = ['failed','rejected','cancelled','running'].map((status,index) => ({id:'run' + index,kind:'explore',status,
    result:{error:status === 'failed' ? 'provider disconnected' : '',text:'{"accepted":true,"data":{"description":"unconfirmed"}}',truncated:true}}));
  const logs = data.buildLogs(state,[],executions).filter(log => log.source === 'execution');
  assert.equal(logs.length,4);
  assert(logs.every(log => log.kind !== 'model' && log.truncated && /输出已截断/.test(log.body)));
  assert.match(logs[0].body,/provider disconnected/);
  assert.equal(logs[0].level,'error');
});

test('unparsed output hides tagged reasoning and cuts oversized final output', () => {
  const logs = data.buildLogs({},[],[{id:'r',kind:'explore',status:'failed',result:{text:'<think>private reasoning</think>final ' + 'x'.repeat(12000)}}]);
  assert(!logs[0].body.includes('private reasoning'));
  assert(logs[0].body.startsWith('final '));
  assert(logs[0].truncated);
  assert(logs[0].body.length < 10100);
});

test('diagnostic JSON also removes reasoning fields without stripping public final fields', () => {
  const logs = data.buildLogs({},[],[{id:'r',kind:'reason',status:'succeeded',result:{text:JSON.stringify({
    accepted:true,data:{decided:true},thinking:'private',nested:{analysis:'hidden',description:'public'}
  })}}]);
  assert.equal(logs.length,1);
  assert(!logs[0].body.includes('private'));
  assert(!logs[0].body.includes('hidden'));
  assert(logs[0].body.includes('public'));
});

test('incomplete and explicitly truncated output cannot become a key conclusion', () => {
  const logs = data.buildLogs({},[],[
    {id:'partial',kind:'explore',status:'succeeded',result:{text:'{"accepted":true,"data":{"description":"partial"}'}},
    {id:'truncated',kind:'explore',status:'succeeded',result:{text:'{"accepted":true,"data":{"description":"truncated"}}',truncated:true}}
  ]);
  assert.equal(logs.length,2);
  assert(logs.every(log => log.kind !== 'model'));
  assert(logs.find(log => log.id === 'execution:truncated').truncated);
});

test('execution pages deduplicate and reject another project execution', () => {
  const state = fixture();
  const older = {id:'r',project_id:'project-a',kind:'explore',status:'running',updated_at:'2026-09-21T01:20:00Z'};
  const newer = {...older,status:'failed',updated_at:'2026-09-21T01:21:00Z',result:{error:'超时'}};
  const logs = data.buildLogs(state,[],[newer,older,newer,{...newer,id:'outside',project_id:'project-b'}]).filter(log => log.source === 'execution');
  assert.equal(logs.length,1);
  assert.equal(logs[0].body,'超时');
});

test('legacy graph alone still yields input, intents and fact logs', () => {
  const state = fixture();
  const logs = data.buildLogs({graph:state.graph});
  assert(logs.some(log => log.node?.type === 'step' && log.node.id === 's1'));
  assert(logs.some(log => log.node?.type === 'fact' && log.node.id === 'f1'));
  assert(!logs.some(log => log.kind === 'model'));
});
