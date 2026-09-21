const {test} = require('node:test');
const assert = require('node:assert/strict');
const {Client,APIError,RequestScope} = require('../static/api.js');
const reply = (body,status=200) => ({status,ok:status>=200&&status<300,text:async()=>JSON.stringify(body)});

test('API serializes project input without interpreting markup and handles deletion',async()=>{
  let captured;
  const client=new Client(async(path,options)=>{captured={path,options};return reply({id:'p'});});
  assert.deepEqual(await client.request('/projects',{method:'POST',body:{title:'<script>literal</script>'}}),{id:'p'});
  assert.equal(captured.options.body,'{"title":"<script>literal</script>"}');
  assert.equal(captured.options.credentials,'same-origin');
  assert.equal(await new Client(async()=>reply(null,204)).request('/projects/p',{method:'DELETE'}),null);
});
test('API reports structured validation and invalid responses',async()=>{
  await assert.rejects(new Client(async()=>reply({detail:[{loc:['body','origin'],msg:'required'}]},422)).request('/projects'),e=>e.status===422&&e.message==='origin：required');
  await assert.rejects(new Client(async()=>({status:200,ok:true,text:async()=>'<html>wrong</html>'})).request('/projects'),APIError);
  for(const path of ['https://elsewhere.test/','//elsewhere.test/','/\\elsewhere.test/']) await assert.rejects(new Client(()=>assert.fail('must not send')).request(path));
});
test('changing selection aborts old requests and rejects stale commits',async()=>{
  const scope=new RequestScope();const first=scope.begin();
  const waiting=new Client((path,{signal})=>new Promise((resolve,reject)=>signal.addEventListener('abort',()=>reject(new Error('aborted')),{once:true})));
  const promise=waiting.request('/projects/old/state',{signal:first.signal});
  const second=scope.begin();
  await assert.rejects(promise,/aborted/);
  assert.equal(first.signal.aborted,true);assert.equal(scope.current(first.version),false);assert.equal(scope.current(second.version),true);
  scope.cancel();assert.equal(scope.current(second.version),false);
});
test('timeout terminates a hung connection with a recoverable error',async()=>{
  const client=new Client((path,{signal})=>new Promise((resolve,reject)=>signal.addEventListener('abort',()=>reject(new Error('timeout')))),5);
  await assert.rejects(client.request('/projects'),/连接超时/);
});
