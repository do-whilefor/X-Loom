(function (root, factory) {
  'use strict';
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.XLoomGraphData = api;
})(typeof globalThis === 'object' ? globalThis : this, function () {
  'use strict';
  const TYPES = ['goal', 'step', 'fact', 'finding'];
  const RELATION_LABELS = {supersedes: '取代', refutes: '反驳', narrows: '收窄'};
  const array = value => Array.isArray(value) ? value : [];
  const strings = value => array(value).filter(item => typeof item === 'string' && item.length > 0);
  const clone = value => value === undefined ? undefined : JSON.parse(JSON.stringify(value));
  const text = value => typeof value === 'string' ? value : '';
  const keyOf = (type, id) => type + ':' + id;
  // State is the server's complete /projects/{id}/state response. No inferred facts
  // or evidence are added here: a missing endpoint is reported and its edge omitted.
  function mapState(input) {
    const state = input && typeof input === 'object' ? input : {};
    const nodes = [];
    const edges = [];
    const diagnostics = [];
    const byKey = new Map();
    const addNode = (type, raw, labelField) => {
      if (!raw || typeof raw !== 'object' || typeof raw.id !== 'string' || !raw.id) {
        diagnostics.push({kind: 'invalid_node', type});
        return;
      }
      const key = keyOf(type, raw.id);
      if (byKey.has(key)) {
        diagnostics.push({kind: 'duplicate_node', key});
        return;
      }
      const label = text(raw[labelField]);
      const node = {
        key, id: raw.id, type, label, title: label, description: label, status: text(raw.status) || 'unknown',
        evidence: clone(array(raw.evidence)), sources: [...strings(raw.sources)],
        supportValid: typeof raw.support_valid === 'boolean' ? raw.support_valid : null,
        raw: clone(raw)
      };
      nodes.push(node);
      byKey.set(key, node);
    };
    array(state.goals).forEach(raw => addNode('goal', raw, 'condition'));
    array(state.steps).forEach(raw => addNode('step', raw, 'description'));
    array(state.fact_records).forEach(raw => addNode('fact', raw, 'description'));
    array(state.findings).forEach(raw => addNode('finding', raw, 'claim'));
    const edgeKeys = new Set();
    const addEdge = (kind, source, target, label, raw, supportValid = null) => {
      const missing = [source, target].filter(key => !byKey.has(key));
      if (missing.length) {
        diagnostics.push({kind: 'missing_reference', relation: kind, source, target, missing});
        return;
      }
      const id = 'edge:' + JSON.stringify([kind, source, target]);
      if (edgeKeys.has(id)) return;
      edgeKeys.add(id);
      edges.push({id, kind, source, target, label, supportValid, raw: clone(raw)});
    };
    nodes.forEach(node => {
      const raw = node.raw;
      if (node.type === 'goal') {
        if (text(raw.parent_id)) addEdge('parent', keyOf('goal', raw.parent_id), node.key, '子目标', null);
        if (node.status === 'achieved') node.sources.forEach(id => addEdge('goal_support', keyOf('fact', id), node.key, '支持目标', null, node.supportValid));
      } else if (node.type === 'step') {
        if (text(raw.goal_id)) addEdge('goal_step', keyOf('goal', raw.goal_id), node.key, '任务归属', null);
        strings(raw.from).forEach(id => addEdge('step_input', keyOf('fact', id), node.key, '依据', null, strings(raw.invalid_sources).includes(id) ? false : null));
        if (text(raw.result)) addEdge('step_result', node.key, keyOf('fact', raw.result), '产生', null);
      } else if (node.type === 'finding') {
        node.sources.forEach(id => addEdge('finding_support', keyOf('fact', id), node.key, '支持发现', null, node.supportValid));
      }
    });
    array(state.fact_relations).forEach(relation => {
      if (!relation || !text(relation.source) || !text(relation.target) || !text(relation.kind)) {
        diagnostics.push({kind: 'invalid_relation'});
        return;
      }
      // A correcting source refutes/narrows/supersedes its old target.
      addEdge(relation.kind, keyOf('fact', relation.source), keyOf('fact', relation.target), RELATION_LABELS[relation.kind] || relation.kind, relation);
    });
    nodes.sort((a, b) => a.key < b.key ? -1 : a.key > b.key ? 1 : 0);
    edges.sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
    const projectId = text(state.graph && state.graph.project && state.graph.project.id);
    const generation = Number(state.graph && state.graph.project && state.graph.project.generation) || 0;
    return {projectId, generation, nodes, edges, diagnostics};
  }

  function resolveNodeKey(nodes, selection) {
    if (selection === null || selection === undefined) return null;
    if (typeof selection === 'object') {
      if (!TYPES.includes(selection.type) || typeof selection.id !== 'string') return null;
      const key = keyOf(selection.type, selection.id);
      return nodes.some(node => node.key === key) ? key : null;
    }
    if (typeof selection !== 'string') return null;
    if (nodes.some(node => node.key === selection)) return selection;
    const matches = nodes.filter(node => node.id === selection);
    return matches.length === 1 ? matches[0].key : null;
  }

  return {mapState, resolveNodeKey};
});
