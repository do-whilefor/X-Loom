(function (root, factory) {
  'use strict';
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.XLoomGraphView = api;
})(typeof globalThis === 'object' ? globalThis : this, function () {
  'use strict';
  const STATES = {open: '待执行', pending: '待执行', running: '运行中', completed: '已完成', failed: '失败', abandoned: '已放弃', achieved: '已达成', withdrawn: '已撤回', valid: '有效', input: '输入', superseded: '被取代', refuted: '被反驳', narrowed: '已收窄', candidate: '待验证', verified: '已验证', paused: '已暂停', needs_review: '待复核', unknown: '未标明状态'};
  const KINDS = {start: '起点', task: '任务', fact: '事实', goal: '终点', subgoal: '子目标', finding: '发现'};
  const edgeKey = edge => typeof edge === 'string' ? edge : edge?.id || 'edge:' + JSON.stringify([edge.kind, edge.source, edge.target]);
  function supportProblem(node) {
    if (node.raw?.invalid_sources?.length) return '支持失效';
    if (node.type === 'goal' && node.status === 'achieved') {
      if (!node.sources?.length) return '缺少有效证据';
      if (node.supportValid !== true) return node.supportValid === false ? '支持失效' : '证据待核对';
    }
    if (node.type === 'finding') {
      if (node.status === 'verified') {
        if (!node.sources?.length) return '缺少有效证据';
        if (node.supportValid !== true) return node.supportValid === false ? '支持失效' : '证据待核对';
      }
      if (node.sources?.length && node.supportValid === false) return '支持失效';
    }
    return '';
  }
  function visualStatus(node) {
    if (supportProblem(node) || ['failed', 'refuted', 'superseded', 'narrowed', 'withdrawn', 'abandoned', 'needs_review'].includes(node.status)) return 'invalid';
    if (['completed', 'achieved', 'valid', 'input', 'verified'].includes(node.status)) return 'done';
    if (node.status === 'running') return 'running';
    if (node.status === 'paused') return 'paused';
    return 'pending';
  }
  function nodePresentation(node, endKey) {
    const kind = node.key === 'fact:origin' ? 'start' : node.key === endKey ? 'goal' : node.type === 'step' ? 'task' : node.type === 'goal' ? 'subgoal' : node.type;
    const invalid = supportProblem(node);
    const statusLabel = (STATES[node.status] || node.status) + (invalid ? ' · ' + invalid : '');
    return {kind, label: node.key === 'fact:goal' ? '目标输入' : KINDS[kind] || node.type, status: visualStatus(node), statusLabel, subtitle: node.key === 'fact:origin' ? '项目原始输入' : node.key === 'fact:goal' ? '用户定义的目标输入' : invalid ? '证据支持需要复核' : node.type === 'goal' ? node.raw?.parent_id ? '下级目标条件' : '项目根目标条件' : node.type === 'finding' ? '发现与证据' : node.description || node.id};
  }
  function projectGeometry(nodes, positions, width = 164, height = 90) {
    return new Map(nodes.filter(node => positions.has(node.key)).map(node => [node.key, {...positions.get(node.key), width, height}]));
  }
  function describeEdge(project, edge) {
    if (!project || !edge) return null;
    const record = project.edgeIndex?.get(edgeKey(edge)) || project.edges.find(item => edgeKey(item) === edgeKey(edge));
    if (!record) return null;
    const sourceNode = project.nodeIndex?.get(record.source) || project.nodes.find(node => node.key === record.source);
    const targetNode = project.nodeIndex?.get(record.target) || project.nodes.find(node => node.key === record.target);
    if (!sourceNode || !targetNode) return null;
    const correction = ['refutes', 'supersedes', 'narrows'].includes(record.kind);
    const invalid = record.supportValid === false || (['goal_support', 'finding_support', 'step_input'].includes(record.kind) && ['refuted', 'superseded', 'narrowed'].includes(sourceNode.status));
    const step = record.kind === 'step_input' ? targetNode : record.kind === 'step_result' ? sourceNode : null;
    const supported = ['goal_support', 'finding_support'].includes(record.kind) ? targetNode : null;
    const status = invalid || correction ? 'invalid' : step ? visualStatus(step) : supported ? visualStatus(supported) : 'recorded';
    const statusLabel = invalid ? '支持已失效' : correction ? record.label : step ? nodePresentation(step, null).statusLabel : supported ? nodePresentation(supported, null).statusLabel : '已记录关系';
    return {...record, sourceNode, targetNode, status, statusLabel, description: record.raw?.description || record.raw?.reason || `${record.label}：${sourceNode.label} → ${targetNode.label}`};
  }
  function collectSelection(project, nodeId, selectedEdge, direction = 'upstream') {
    const nodeIds = new Set(), edgeKeys = new Set();
    if (!project) return {active: false, nodeIds, edgeKeys};
    if (selectedEdge) {
      const edge = project.edgeIndex?.get(edgeKey(selectedEdge)) || project.edges.find(candidate => edgeKey(candidate) === edgeKey(selectedEdge));
      if (edge) { nodeIds.add(edge.source); nodeIds.add(edge.target); edgeKeys.add(edgeKey(edge)); }
    } else if (project.nodeIndex?.has(nodeId) || project.nodes.some(node => node.key === nodeId)) {
      const adjacency = direction === 'downstream' ? project.outgoing : project.incoming;
      const indexed = adjacency || new Map(project.nodes.map(node => [node.key, []]));
      if (!adjacency) for (const edge of project.edges) indexed.get(direction === 'downstream' ? edge.source : edge.target)?.push(edge);
      const pending = [nodeId];
      while (pending.length) {
        const id = pending.pop();
        if (nodeIds.has(id)) continue;
        nodeIds.add(id);
        for (const edge of indexed.get(id) || []) { edgeKeys.add(edgeKey(edge)); pending.push(direction === 'downstream' ? edge.target : edge.source); }
      }
    }
    return {active: nodeIds.size > 0, nodeIds, edgeKeys};
  }
  function placeEdgeLabel(points, label) {
    if (!points?.length) return null;
    const middle = Math.floor(points.length / 2), point = points[middle];
    const before = points[Math.max(0, middle - 1)], after = points[Math.min(points.length - 1, middle + 1)];
    const angle = Math.atan2(after.y - before.y, after.x - before.x);
    const rotation = angle > Math.PI / 2 ? angle - Math.PI : angle < -Math.PI / 2 ? angle + Math.PI : angle;
    return {text: label, x: point.x + Math.sin(rotation) * 12, y: point.y - Math.cos(rotation) * 12, angle: rotation * 180 / Math.PI};
  }
  return {STATES, KINDS, edgeKey, visualStatus, nodePresentation, projectGeometry, describeEdge, collectSelection, placeEdgeLabel};
});
