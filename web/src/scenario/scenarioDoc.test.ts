import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import {
  addFile,
  addInput,
  duplicateItem,
  moveItem,
  receiverSignals,
  setInputData,
  addKubeObjects,
  editScenario,
  removeMock,
  removeProvider,
  addProviderSettings,
  setFileText,
  setProviderSettings,
  readScenario,
  receiverIds,
  removeItem,
  setConditions,
  setEnvironment,
  setHeaders,
  setField,
  setInputSource,
  setTarget,
  signalOfPayload,
  targetChoices,
  targetKey,
  type Edit,
} from './scenarioDoc';
import type { Graph, Report } from '../api/types';

const scenario = `# Error logs must reach the pager.
version: 1
name: paging
inputs:
  - receiver: otlp
    file: logs.json # relative to the scenario
environment:
  TOKEN: abc
execution:
  timeout: 30s
assertions:
  - name: pages
    target: {exporter: otlp/pager, signal: logs}
    # one error log
    count: {equals: 1}
`;

const component = (ref: string, kind: string) => ({ ref, kind, id: ref.slice(kind.length + 1), type: '', mode: '' });
const graph: Graph = {
  components: [
    component('receiver/otlp', 'receiver'),
    component('receiver/filelog', 'receiver'),
    component('processor/transform/route', 'processor'),
    component('processor/filter', 'processor'),
    component('exporter/otlp/pager', 'exporter'),
    component('exporter/otlp/archive', 'exporter'),
    component('connector/forward', 'connector'),
  ],
  pipelines: [
    {
      id: 'logs/pager',
      signal: 'logs',
      receivers: ['receiver/otlp', 'connector/forward'],
      processors: ['processor/transform/route', 'processor/filter'],
      exporters: ['exporter/otlp/pager'],
    },
    {
      id: 'logs/archive',
      signal: 'logs',
      receivers: ['receiver/otlp', 'receiver/filelog'],
      exporters: ['exporter/otlp/archive', 'exporter/otlp/pager', 'connector/forward'],
    },
  ],
};

function edit(text: string, e: Edit): string {
  const r = editScenario(text, e);
  if ('error' in r) throw new Error(r.error);
  return r.yaml;
}

describe('readScenario', () => {
  it('reads the form model', () => {
    const r = readScenario(scenario);
    if ('error' in r) throw new Error(r.error);
    expect(r.model).toEqual({
      name: 'paging',
      timeout: '30s',
      settle: '',
      timestamps: '',
      environment: [['TOKEN', 'abc']],
      headers: [],
      inputs: [{ receiver: 'otlp', signal: '', file: 'logs.json', inlineSize: undefined, data: undefined, clientAddress: '', after: '', headers: [] }],
      files: [],
      kubeObjects: [],
      kubeFiles: [],
      providers: [],
      assertions: [
        { name: 'pages', target: { exporter: 'otlp/pager', signal: 'logs' }, conditions: '# one error log\ncount: { equals: 1 }\n' },
      ],
    });
  });

  it('reports text it cannot read', () => {
    expect(readScenario('a: [')).toHaveProperty('error');
    expect(readScenario('- 1')).toEqual({ error: 'The scenario must be a YAML mapping.' });
  });
});

describe('editScenario', () => {
  it('keeps comments when a field changes', () => {
    const y = edit(scenario, setField(['name'], 'renamed'));
    expect(y).toContain('# Error logs must reach the pager.');
    expect(y).toContain('# relative to the scenario');
    expect(y).toContain('# one error log');
    expect(parse(y).name).toBe('renamed');
  });

  it('removes an emptied field and its empty parent', () => {
    const y = edit(scenario, setField(['execution', 'timeout'], ''));
    expect(parse(y).execution).toBeUndefined();
  });

  it('reads and edits the timeline', () => {
    let y = edit(scenario, setField(['execution', 'settle'], '2s'));
    y = edit(y, setField(['execution', 'timestamps'], 'rebase'));
    y = edit(y, addInput({ receiver: 'otlp', file: 'late.json' }));
    y = edit(y, setField(['inputs', 1, 'after'], '3s'));
    const r = readScenario(y);
    if ('error' in r) throw new Error(r.error);
    expect(r.model.settle).toBe('2s');
    expect(r.model.timestamps).toBe('rebase');
    expect(r.model.inputs.map((i) => i.after)).toEqual(['', '3s']);
    expect(parse(y).execution).toEqual({ timeout: '30s', settle: '2s', timestamps: 'rebase' });
    y = edit(y, setField(['inputs', 1, 'after'], ''));
    expect(parse(y).inputs[1]).toEqual({ receiver: 'otlp', file: 'late.json' });
  });

  it('starts an empty scenario at version 1', () => {
    const y = edit('', addInput({ receiver: 'otlp', file: 'x.json' }));
    expect(parse(y)).toEqual({ version: 1, inputs: [{ receiver: 'otlp', file: 'x.json' }] });
  });

  it('inlines pasted OTLP JSON as a block literal', () => {
    const json = '{"resourceLogs":[]}';
    const y = edit(scenario, setInputSource(0, { data: json }));
    expect(y).toContain('data: |');
    const input = parse(y).inputs[0];
    expect(input.file).toBeUndefined();
    expect(JSON.parse(input.data)).toEqual({ resourceLogs: [] });
    const r = readScenario(y);
    if ('error' in r) throw new Error(r.error);
    expect(r.model.inputs[0].inlineSize).toBeGreaterThan(0);
    expect(() => edit(scenario, addInput({ receiver: 'otlp', data: '{' }))).toThrow();
  });

  it('replaces the environment, dropping blank keys', () => {
    const y = edit(scenario, setEnvironment([['TOKEN', 'xyz'], ['', 'ignored'], ['REGION', 'eu']]));
    expect(parse(y).environment).toEqual({ TOKEN: 'xyz', REGION: 'eu' });
    expect(parse(edit(scenario, setEnvironment([]))).environment).toBeUndefined();
    const typed = edit(edit(scenario, setEnvironment([['NEW', '']])), setEnvironment([['NEW', 'v']]));
    expect(parse(typed).environment.NEW).toBe('v');
    expect(typed).not.toContain('"v"');
    expect(editScenario(scenario, setEnvironment([['A', '1'], ['A', '2']]))).toEqual({ error: 'The environment already has A.' });
  });

  it('reads and edits headers of an input and of every input', () => {
    const withHeaders = `version: 1
defaults:
  headers:
    user-agent: client/1.0
inputs:
  - receiver: otlp
    headers:
      x-team: [a, b]
    file: in.json
`;
    const read = readScenario(withHeaders);
    if (!('model' in read)) throw new Error(read.error);
    expect(read.model.headers).toEqual([['user-agent', 'client/1.0']]);
    expect(read.model.inputs[0].headers).toEqual([['x-team', 'a, b']]);
    let y = edit(withHeaders, setHeaders(['inputs', 0, 'headers'], [['x-team', 'a, b'], ['X-Project', 'p1']]));
    expect(parse(y).inputs[0].headers).toEqual({ 'x-team': ['a', 'b'], 'X-Project': 'p1' });
    y = edit(y, setHeaders(['defaults', 'headers'], []));
    expect(parse(y).defaults).toBeUndefined();
    expect(editScenario(y, setHeaders(['inputs', 0, 'headers'], [['X-A', '1'], ['x-a', '2']]))).toEqual({ error: 'The headers already have x-a.' });
    expect(editScenario(y, setHeaders(['inputs', 0, 'headers'], [['bad name', '1']]))).toEqual({ error: 'bad name is not a valid header name.' });
    expect(editScenario(y, setHeaders(['inputs', 0, 'headers'], [['trace-bin', '1']]))).toHaveProperty('error');
    expect(parse(edit(y, setHeaders(['inputs', 0, 'headers'], []))).inputs[0].headers).toBeUndefined();
  });

  it('sets a target and conditions', () => {
    let y = edit(scenario, setTarget(0, { processor: 'filter', pipeline: 'logs/pager', signal: 'logs' }));
    expect(y).toContain('target: { processor: filter, pipeline: logs/pager, signal: logs }');
    y = edit(y, setConditions(0, 'dropped: {max: 1}\nattributes:\n  absent: [secret]\n'));
    const a = parse(y).assertions[0];
    expect(a.count).toBeUndefined();
    expect(a.dropped).toEqual({ max: 1 });
    expect(a.attributes).toEqual({ absent: ['secret'] });
    expect(editScenario(y, setConditions(0, 'bogus: 1'))).toHaveProperty('error');
    expect(editScenario(y, setConditions(0, 'count: ['))).toHaveProperty('error');
  });

  it('removes items', () => {
    let y = edit(scenario, duplicateItem('assertions', 0));
    expect(parse(y).assertions).toHaveLength(2);
    y = edit(y, removeItem('assertions', 0));
    y = edit(y, removeItem('assertions', 0));
    expect(parse(y).assertions).toBeUndefined();
  });
});

describe('mock fixtures', () => {
  const mocked = `version: 1
mocks:
  # the node's container logs
  files:
    - path: /var/log/pods/shop_a_1/app/0.log
      file: raw.log
  kubernetes:
    files: [cluster.yaml]
    objects:
      - {apiVersion: v1, kind: Namespace, metadata: {name: shop}}
inputs:
  - data: '{"resourceSpans":[]}'
    client_address: 10.42.0.17
`;

  it('reads files, objects and client addresses', () => {
    const r = readScenario(mocked);
    if ('error' in r) throw new Error(r.error);
    expect(r.model.files).toEqual([{ path: '/var/log/pods/shop_a_1/app/0.log', existing: '', content: '', file: 'raw.log' }]);
    expect(r.model.kubeObjects).toEqual([{ kind: 'Namespace', name: 'shop', namespace: '', items: undefined }]);
    expect(r.model.kubeFiles).toEqual(['cluster.yaml']);
    expect(r.model.inputs[0].clientAddress).toBe('10.42.0.17');
  });

  it('adds a file and edits its text as block literals', () => {
    let y = edit('', addFile('/var/log/app.log', 'line 1'));
    expect(y).toContain('content: |');
    expect(parse(y)).toEqual({ version: 1, mocks: { files: [{ path: '/var/log/app.log', content: 'line 1\n' }] } });
    y = edit(y, setFileText(0, 'existing', 'old\n'));
    expect(parse(y).mocks.files[0].existing).toBe('old\n');
    y = edit(y, setFileText(0, 'existing', ''));
    expect(parse(y).mocks.files[0].existing).toBeUndefined();
    expect(editScenario('', addFile(' '))).toHaveProperty('error');
  });

  it('replaces a file reference with content', () => {
    const y = edit(mocked, setFileText(0, 'content', 'typed'));
    expect(parse(y).mocks.files[0]).toEqual({ path: '/var/log/pods/shop_a_1/app/0.log', content: 'typed\n' });
    expect(y).toContain("# the node's container logs");
  });

  it('adds objects from manifests, expanding lists', () => {
    const manifests = `apiVersion: v1
kind: Pod
metadata: {name: a, namespace: shop}
---
apiVersion: v1
kind: List
items:
  - {apiVersion: v1, kind: Node, metadata: {name: worker-1}}
  - {apiVersion: apps/v1, kind: ReplicaSet, metadata: {name: a-1, namespace: shop}}
`;
    const y = edit(mocked, addKubeObjects(manifests));
    const r = readScenario(y);
    if ('error' in r) throw new Error(r.error);
    expect(r.model.kubeObjects.map((o) => `${o.kind} ${o.namespace}/${o.name}`)).toEqual([
      'Namespace /shop',
      'Pod shop/a',
      'Node /worker-1',
      'ReplicaSet shop/a-1',
    ]);
    expect(editScenario(mocked, addKubeObjects('kind: Pod\n'))).toEqual({ error: 'Each manifest needs a kind and metadata.name.' });
    expect(editScenario(mocked, addKubeObjects('---\n'))).toEqual({ error: 'The manifests hold no objects.' });
  });

  it('removes entries and the mocks they empty', () => {
    let y = edit(mocked, removeMock('kubernetes.objects', 0));
    expect(parse(y).mocks.kubernetes).toEqual({ files: ['cluster.yaml'] });
    y = edit(y, removeMock('kubernetes.files', 0));
    expect(parse(y).mocks.kubernetes).toBeUndefined();
    y = edit(y, removeMock('files', 0));
    expect(parse(y).mocks).toBeUndefined();
  });
});

describe('provider settings', () => {
  const withProvider = `version: 1
mocks:
  providers:
    livecapture:
      # the session the run starts
      sessions:
        - request: {session_id: s1}
  files:
    - path: /tmp/a.log
      content: x
`;
  const edit = (text: string, e: Edit) => {
    const r = editScenario(text, e);
    if ('error' in r) throw new Error(r.error);
    return r.yaml;
  };

  it('reads each provider with its settings and their comments', () => {
    const r = readScenario(withProvider);
    if ('error' in r) throw new Error(r.error);
    expect(r.model.providers.map((p) => p.name)).toEqual(['livecapture']);
    expect(r.model.providers[0].settings).toContain('# the session the run starts');
    expect(parse(r.model.providers[0].settings)).toEqual({ sessions: [{ request: { session_id: 's1' } }] });
  });

  it('adds a provider with empty settings and replaces its settings', () => {
    let y = edit('version: 1\n', setProviderSettings('stamp', ''));
    expect(parse(y).mocks).toEqual({ providers: { stamp: {} } });
    y = edit(y, setProviderSettings('stamp', 'value: from-the-test\n'));
    expect(parse(y).mocks).toEqual({ providers: { stamp: { value: 'from-the-test' } } });
    const r = readScenario(y);
    if ('error' in r) throw new Error(r.error);
    expect(r.model.providers).toEqual([{ name: 'stamp', settings: 'value: from-the-test\n' }]);
  });

  it('adds the keys a provider proposes, keeping those set', () => {
    let y = edit('version: 1\n', addProviderSettings('livecapture', {}));
    expect(parse(y).mocks).toEqual({ providers: { livecapture: {} } });
    y = edit('version: 1\nmocks:\n  providers:\n    livecapture:\n', addProviderSettings('livecapture', { sessions: [{ id: 's1' }] }));
    expect(parse(y).mocks).toEqual({ providers: { livecapture: { sessions: [{ id: 's1' }] } } });
    y = edit(y, addProviderSettings('livecapture', { sessions: [], disabled: false }));
    expect(parse(y).mocks).toEqual({ providers: { livecapture: { sessions: [{ id: 's1' }], disabled: false } } });
  });

  it('refuses settings that are not a mapping', () => {
    expect(editScenario(withProvider, setProviderSettings('livecapture', '- a\n'))).toEqual({ error: 'The settings of livecapture must be a mapping (key: value).' });
    expect(editScenario(withProvider, setProviderSettings('livecapture', 'a: [\n'))).toHaveProperty('error');
  });

  it('removes a provider and the mapping it empties, keeping other mocks', () => {
    const y = edit(withProvider, removeProvider('livecapture'));
    expect(parse(y).mocks).toEqual({ files: [{ path: '/tmp/a.log', content: 'x' }] });
  });
});

describe('topology helpers', () => {
  it('lists each exporter per signal and each processor instance', () => {
    expect(targetChoices(graph).map((c) => c.label)).toEqual([
      'exporter otlp/pager (logs)',
      'exporter otlp/archive (logs)',
      'processor transform/route in logs/pager',
      'processor filter in logs/pager',
    ]);
    expect(receiverIds(graph)).toEqual(['filelog', 'otlp']);
  });

  it('adds the endpoints a run simulated, per signal received or else per pipeline signal', () => {
    const report = {
      result: {
        simulated_endpoints: [
          { component: 'extension/livecapture', path: 'delivery', protocol: 'grpc', endpoint: 'c:4317' },
          { component: 'extension/livecapture', path: 'control', protocol: 'grpc', endpoint: 'c:443' },
        ],
        endpoint_outputs: [{ component: 'extension/livecapture', endpoint: 'delivery', signal: 'traces', batches: 1, counts: { items: 1, resources: 1, scopes: 1 } }],
      },
    } as unknown as Report;
    const endpoints = targetChoices(graph, report).filter((c) => c.target.endpoint);
    expect(endpoints.map((c) => [c.label, targetKey(c.target)])).toEqual([
      ['endpoint delivery of extension/livecapture (traces)', 'n|extension/livecapture|delivery|traces'],
      ['endpoint control of extension/livecapture (logs)', 'n|extension/livecapture|control|logs'],
    ]);
  });

  it('reads and writes an endpoint target', () => {
    const y = edit('version: 1\nassertions:\n  - target: {exporter: a, signal: logs}\n    count: {min: 1}\n', setTarget(0, { endpoint: { component: 'extension/livecapture', path: 'delivery' }, signal: 'logs' }));
    expect(y).toContain('  - target:\n      endpoint: { component: extension/livecapture, path: delivery }\n      signal: logs\n');
    expect(parse(y).assertions[0].target).toEqual({ endpoint: { component: 'extension/livecapture', path: 'delivery' }, signal: 'logs' });
  });

  it('infers the signal of a payload', () => {
    expect(signalOfPayload('{"resourceSpans":[]}')).toBe('traces');
    expect(signalOfPayload('{"resource_metrics":[]}')).toBe('metrics');
    expect(signalOfPayload('{"resourceLogs":[]}')).toBe('logs');
    expect(signalOfPayload('{}')).toBeUndefined();
    expect(signalOfPayload('nope')).toBeUndefined();
  });
});

describe('input card edits', () => {
  const two = `version: 1
inputs:
  - receiver: otlp # first
    file: a.json
  - receiver: otlp
    file: b.json
`;

  it('stores inline data exactly as typed', () => {
    for (const text of ['', '{"a": 1}', '  {\n    "a": 1\n  }\n', '{"a":\n\n1}']) {
      const y = edit(two, setInputData(1, text));
      const r = readScenario(y);
      if ('error' in r) throw new Error(r.error);
      expect(r.model.inputs[1].data).toBe(text);
      expect(r.model.inputs[1].file).toBeUndefined();
    }
  });

  it('moves and duplicates inputs with their comments', () => {
    const moved = edit(two, moveItem('inputs', 0, 1));
    expect(parse(moved).inputs.map((i: { file: string }) => i.file)).toEqual(['b.json', 'a.json']);
    expect(moved).toContain('# first');
    const dup = parse(edit(two, duplicateItem('inputs', 0)));
    expect(dup.inputs.map((i: { file: string }) => i.file)).toEqual(['a.json', 'a.json', 'b.json']);
  });

  it('adds raw inline data and omits an unknown receiver', () => {
    const y = edit('version: 1\n', addInput({ receiver: '', data: '{\n  "resourceLogs": []\n}\n', raw: true }));
    expect(parse(y).inputs).toEqual([{ data: '{\n  "resourceLogs": []\n}\n' }]);
  });

  it('maps receivers to the signals they feed', () => {
    expect(receiverSignals(graph)).toEqual(new Map([['otlp', ['logs']], ['filelog', ['logs']]]));
  });
});
