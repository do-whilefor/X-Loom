(function (root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.XLoomAPI = api;
}(typeof window === 'object' ? window : globalThis, function () {
  'use strict';
  class APIError extends Error {
    constructor(message, status = 0) { super(message); this.name = 'APIError'; this.status = status; }
  }
  function errorMessage(detail, status) {
    if (typeof detail === 'string') return detail;
    if (Array.isArray(detail)) return detail.map(item => [item.loc?.at(-1), item.msg].filter(Boolean).join('：')).join('；');
    return '请求失败（HTTP ' + status + '）';
  }
  class Client {
    constructor(fetcher = (...args) => fetch(...args), timeout = 15000) { this.fetcher = fetcher; this.timeout = timeout; }
    async projectExecutions(projectPath, options = {}) {
      const items = [];
      let cursor = 0, through = 0;
      do {
        const page = await this.request(projectPath + '/executions?limit=20&cursor=' + cursor + '&through=' + through, options);
        if (!Array.isArray(page.items) || !Number.isSafeInteger(page.through) || page.through < 0 ||
            (page.next_cursor !== undefined && (!Number.isSafeInteger(page.next_cursor) || page.next_cursor <= cursor || page.next_cursor > page.through)) ||
            (through !== 0 && page.through !== through)) throw new APIError('执行记录分页边界无效');
        items.push(...page.items);
        through = page.through;
        cursor = page.next_cursor || 0;
      } while (cursor);
      return items;
    }
    async request(path, {method = 'GET', body, signal} = {}) {
      if (!path.startsWith('/') || path.startsWith('//') || path.includes('\\')) throw new APIError('仅支持本站接口');
      const controller = new AbortController();
      const abort = () => controller.abort(signal.reason);
      if (signal?.aborted) abort();
      else signal?.addEventListener('abort', abort, {once:true});
      const timer = setTimeout(() => controller.abort(new Error('请求超时')), this.timeout);
      try {
        const response = await this.fetcher(path, {method, credentials:'same-origin', cache:'no-store',
          headers:body === undefined ? {Accept:'application/json'} : {Accept:'application/json','Content-Type':'application/json'},
          body:body === undefined ? undefined : JSON.stringify(body), signal:controller.signal});
        if (response.status === 204) return null;
        const text = await response.text();
        let result;
        try { result = JSON.parse(text); }
        catch { throw new APIError(response.ok ? '服务返回了无法读取的数据' : '请求失败（HTTP ' + response.status + '）', response.status); }
        if (!response.ok) throw new APIError(errorMessage(result?.detail, response.status), response.status);
        return result;
      } catch (error) {
        if (signal?.aborted) throw error;
        if (controller.signal.aborted) throw new APIError('连接超时，请稍后重试');
        if (error instanceof APIError) throw error;
        throw new APIError('无法连接 X-Loom，请检查服务状态');
      } finally { clearTimeout(timer); signal?.removeEventListener('abort', abort); }
    }
  }
  // Project selection and refresh share a scope: old responses cannot replace a newer selection.
  class RequestScope {
    constructor() { this.version = 0; this.controller = null; }
    begin() { this.controller?.abort(); this.controller = new AbortController(); return {version:++this.version, signal:this.controller.signal}; }
    current(version) { return version === this.version && !this.controller?.signal.aborted; }
    cancel() { this.controller?.abort(); this.version++; }
  }
  return {Client, APIError, RequestScope};
}));
