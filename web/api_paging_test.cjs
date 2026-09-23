const test = require('node:test');
const assert = require('node:assert/strict');
const {Client} = require('./static/api.js');

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

test('broken continuation fails instead of silently presenting partial history', async () => {
  for (const tail of [{items:[],next_cursor:1,through:2},{items:[],through:3}]) {
    const client = new Client();
    let calls = 0;
    client.request = async () => ++calls === 1 ? {items:[{id:'first'}],next_cursor:1,through:2} : tail;
    await assert.rejects(client.projectExecutions('/projects/example'), /分页边界无效/);
  }
});
