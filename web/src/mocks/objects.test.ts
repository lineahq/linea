import { describe, expect, it } from 'vitest';
import { parse } from 'yaml';
import { addKubeObjects, editScenario, readScenario } from '../scenario/scenarioDoc';
import {
  duplicateKubeObject,
  extractedKeys,
  fileTemplate,
  kubeObjectYAML,
  kubeSummary,
  objectTemplate,
  OBJECT_TEMPLATES,
  readKubeForms,
  replaceKubeObject,
  setKubeField,
  setKubeMap,
} from './objects';

const edit = (text: string, e: Parameters<typeof editScenario>[1]) => {
  const r = editScenario(text, e);
  if ('error' in r) throw new Error(r.error);
  return r.yaml;
};

const scenario = `version: 1
mocks:
  kubernetes:
    objects:
      # the checkout pod
      - apiVersion: v1
        kind: Pod
        metadata:
          name: checkout-1
          namespace: shop
          uid: u-1
          labels: {app: checkout, version: 2}
        spec: {nodeName: worker-1, containers: [{name: app, image: app:1}]}
        status: {phase: Running, podIP: 10.0.0.5, podIPs: [{ip: 10.0.0.5}, {ip: "fd00::5"}]}
      - apiVersion: v1
        kind: Namespace
        metadata: {name: shop}
      - kind: PodList
        metadata: {name: list}
        items: [{kind: Pod}]
`;

const pod = (y: string) => parse(y).mocks.kubernetes.objects[0];

describe('readKubeForms', () => {
  it('reads the fields the form edits', () => {
    const [p, ns, list] = readKubeForms(scenario);
    expect(p).toEqual({
      kind: 'Pod',
      name: 'checkout-1',
      namespace: 'shop',
      uid: 'u-1',
      ip: '10.0.0.5',
      node: 'worker-1',
      labels: [
        ['app', 'checkout'],
        ['version', '2'],
      ],
      annotations: [],
    });
    expect(kubeSummary(p)).toBe('shop/checkout-1 · 10.0.0.5 · on worker-1 · 2 labels');
    expect(ns.yamlOnly).toBeUndefined();
    expect(list.yamlOnly).toMatch(/list of 1/);
  });
});

describe('form edits', () => {
  it('write fields at their manifest paths and keep the rest', () => {
    let y = edit(scenario, setKubeField(0, 'ip', '10.0.0.9'));
    expect(y).toContain('# the checkout pod');
    expect(pod(y).status).toEqual({ phase: 'Running', podIP: '10.0.0.9', podIPs: [{ ip: '10.0.0.9' }, { ip: 'fd00::5' }] });
    y = edit(y, setKubeField(0, 'node', ''));
    expect(pod(y).spec).toEqual({ containers: [{ name: 'app', image: 'app:1' }] });
    y = edit(y, setKubeField(1, 'namespace', 'x'));
    expect(parse(y).mocks.kubernetes.objects[1].metadata).toEqual({ name: 'shop', namespace: 'x' });
    expect(() => edit(y, setKubeField(0, 'name', ' '))).toThrow(/name/);
  });

  it('give a Pod its IP in both status fields, and remove both', () => {
    const bare = edit('', addKubeObjects('kind: Pod\nmetadata: {name: p}\n'));
    const y = edit(bare, setKubeField(0, 'ip', '10.1.1.1'));
    expect(pod(y).status).toEqual({ podIP: '10.1.1.1', podIPs: [{ ip: '10.1.1.1' }] });
    expect(pod(edit(y, setKubeField(0, 'ip', ''))).status).toBeUndefined();
  });

  it('replace labels as strings, keeping unchanged nodes', () => {
    const y = edit(
      scenario,
      setKubeMap(0, 'labels', [
        ['app', 'checkout'],
        ['version', '2'],
        ['team', 'payments'],
      ]),
    );
    expect(y).toContain('version: 2');
    expect(pod(y).metadata.labels).toEqual({ app: 'checkout', version: 2, team: 'payments' });
    expect(pod(edit(y, setKubeMap(0, 'labels', [['version', '3']]))).metadata.labels).toEqual({ version: '3' });
    expect(pod(edit(y, setKubeMap(0, 'labels', []))).metadata.labels).toBeUndefined();
    expect(() =>
      edit(
        y,
        setKubeMap(0, 'labels', [
          ['a', '1'],
          ['a', '2'],
        ]),
      ),
    ).toThrow(/already/);
  });
});

describe('one object as YAML', () => {
  it('round-trip and refuses more than one object', () => {
    const text = kubeObjectYAML(scenario, 0);
    expect(parse(text).metadata.name).toBe('checkout-1');
    const y = edit(scenario, replaceKubeObject(1, 'kind: Namespace\nmetadata:\n  name: other\n'));
    expect(parse(y).mocks.kubernetes.objects[1]).toEqual({ kind: 'Namespace', metadata: { name: 'other' } });
    expect(() => edit(scenario, replaceKubeObject(1, 'kind: A\nmetadata: {name: a}\n---\nkind: B\nmetadata: {name: b}\n'))).toThrow(/exactly one/);
    expect(() => edit(scenario, replaceKubeObject(1, 'metadata: {name: a}\n'))).toThrow(/kind/);
  });
});

describe('new objects', () => {
  it('get names, UIDs and IPs of their own', () => {
    let y = '';
    for (const t of OBJECT_TEMPLATES) y = edit(y, addKubeObjects(objectTemplate(t.kind, readKubeForms(y))));
    y = edit(y, addKubeObjects(objectTemplate('Pod', readKubeForms(y))));
    const forms = readKubeForms(y);
    expect(forms.map((f) => [f.kind, f.name])).toEqual([
      ['Pod', 'app-7d9f8b6c4-x2x9q'],
      ['Namespace', 'default'],
      ['Node', 'worker-1'],
      ['ReplicaSet', 'app-7d9f8b6c4'],
      ['Pod', 'app-7d9f8b6c4-x2x9q-2'],
    ]);
    expect(forms[0].ip).not.toBe(forms[4].ip);
    expect(new Set(forms.map((f) => f.uid).filter(Boolean)).size).toBe(4);
    expect(readScenario(y)).toHaveProperty('model');
  });

  it('duplicate an object apart from the original', () => {
    const y = edit(scenario, duplicateKubeObject(0, readKubeForms(scenario)));
    const [a, b] = readKubeForms(y);
    expect(b).toMatchObject({ kind: 'Pod', name: 'checkout-2', namespace: 'shop', node: 'worker-1', labels: a.labels });
    expect(b.ip).not.toBe(a.ip);
    expect(b.uid).not.toBe(a.uid);
  });
});

describe('fileTemplate', () => {
  it('uses the receiver include patterns, else a path for the format', () => {
    expect(fileTemplate('plain', { include: ['/var/log/app/*.log'] }, []).path).toBe('/var/log/app/app.log');
    expect(fileTemplate('container', {}, []).path).toMatch(/^\/var\/log\/pods\/default_app-.*\/app\/0\.log$/);
    expect(fileTemplate('json', {}, []).content).toMatch(/^\{"time"/);
    expect(fileTemplate('empty', {}, ['/var/log/app.log'])).toEqual({ path: '/var/log/app1.log', content: '' });
  });
});

describe('extractedKeys', () => {
  it('lists the keys extracted from each kind', () => {
    const cfg = {
      extract: { labels: [{ key: 'app' }, { key: 'team', from: 'namespace' }, { key_regex: 'x.*' }], annotations: [{ key: 'owner', from: 'pod' }] },
    };
    expect(extractedKeys(cfg, 'Pod', 'labels')).toEqual(['app']);
    expect(extractedKeys(cfg, 'Namespace', 'labels')).toEqual(['team']);
    expect(extractedKeys(cfg, 'Pod', 'annotations')).toEqual(['owner']);
    expect(extractedKeys({}, 'Node', 'labels')).toEqual([]);
  });
});
