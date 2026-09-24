const test = require('node:test');
const assert = require('node:assert/strict');
const {Client, APIError, RequestScope} = require('./static/api.js');

test('execution history follows every page without changing its upper boundary', async () => {
  const client = new Client();
  const signal = new AbortController().signal;
  const paths = [];
  client.request = async (path, options) => {
    paths.push(path);
    assert.equal(options.signal, signal);
    return paths.length === 1
      ? {items:[{id:'first'},{id:'second'}],next_cursor:2,through:3}
      : {items:[{id:'third'}],through:3};
  };
  assert.deepEqual(await client.projectExecutions('/projects/example', {signal}), [{id:'first'},{id:'second'},{id:'third'}]);
  assert.deepEqual(paths, ['/projects/example/executions?limit=20&cursor=0&through=0','/projects/example/executions?limit=20&cursor=2&through=3']);
});

test('deleting a project accepts an empty 204 and keeps requests same-origin', async () => {
  let received;
  const client = new Client(async (path,options) => {
    received = {path,options};
    return {status:204,text:async () => { throw new Error('must not parse an empty response'); }};
  });
  assert.equal(await client.request('/projects/p',{method:'DELETE'}),null);
  assert.equal(received.options.credentials,'same-origin');
  assert.equal(received.options.body,undefined);
  for (const path of ['https://example.com','//example.com','/\\example.com']) {
    await assert.rejects(client.request(path),APIError);
  }
});

test('restart conflicts surface once without retrying non-idempotent writes', async () => {
  let calls = 0;
  const client = new Client(async () => {
    calls++;
    return {status:409,ok:false,text:async () => JSON.stringify({detail:'generation changed'})};
  });
  await assert.rejects(client.request('/projects/p/restart',{method:'POST',body:{expected_generation:2}}),
    error => error.status === 409 && error.message === 'generation changed');
  assert.equal(calls,1);
});

test('selection scopes reject a late old response even when the transport ignored cancellation', async () => {
  const scope = new RequestScope();
  const first = scope.begin();
  const second = scope.begin();
  assert.equal(first.signal.aborted,true);
  assert.equal(scope.current(first.version),false);
  assert.equal(scope.current(second.version),true);
  scope.cancel();
  assert.equal(second.signal.aborted,true);
  assert.equal(scope.current(second.version),false);
});

test('timeout aborts the request without replaying it', async () => {
  let calls = 0;
  const client = new Client((path,{signal}) => new Promise((resolve,reject) => {
    calls++;
    signal.addEventListener('abort',() => reject(signal.reason),{once:true});
  }),5);
  await assert.rejects(client.request('/projects',{method:'POST',body:{title:'draft'}}),/超时/);
  assert.equal(calls,1);
});

test('broken continuation fails instead of silently presenting partial history', async () => {
  for (const tail of [{items:[],next_cursor:1,through:2},{items:[],through:3}]) {
    const client = new Client();
    let calls = 0;
    client.request = async () => ++calls === 1 ? {items:[{id:'first'}],next_cursor:1,through:2} : tail;
    await assert.rejects(client.projectExecutions('/projects/example'), /分页边界无效/);
  }
});
