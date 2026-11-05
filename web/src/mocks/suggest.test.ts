import { describe, expect, it } from 'vitest';
import { parseAllDocuments } from 'yaml';
import type { Graph } from '../api/types';
import { concretePath, envReferences, globMatch, mockedComponents, podIdentities, suggestFile, suggestKube } from './suggest';

describe('envReferences', () => {
  it('lists references once, in order, and knows defaults', () => {
    const yaml = [
      'exporters:',
      '  otlp:',
      '    endpoint: ${env:BACKEND}:4317',
      '    headers: {authorization: "Bearer ${env:TOKEN}"}',
      '    tls: {insecure: ${INSECURE:-false}}',
      '# token: ${env:COMMENTED}',
      '    other: ${env:TOKEN} # ${env:TRAILING}',
      '    escaped: $${env:ESCAPED}',
      '    file: ${file:/etc/x}',
    ].join('\n');
    expect(envReferences(yaml)).toEqual([
      { name: 'BACKEND', hasDefault: false },
      { name: 'TOKEN', hasDefault: false },
      { name: 'INSECURE', hasDefault: true },
    ]);
  });
});

describe('globMatch', () => {
  it.each([
    ['/var/log/pods/*/*/*.log', '/var/log/pods/a_b_c/app/0.log', true],
    ['/var/log/pods/*/*/*.log', '/var/log/pods/a_b_c/app/x/0.log', false],
    ['/var/log/**/*.log', '/var/log/a/b/c.log', true],
    ['/var/log/**/*.log', '/var/log/c.log', true],
    ['/var/log/{app,web}.log', '/var/log/web.log', true],
    ['/var/log/app?.log', '/var/log/app1.log', true],
  ])('%s ~ %s', (pattern, path, want) => expect(globMatch(pattern, path)).toBe(want));
});

describe('concretePath', () => {
  it('names kubelet pod log directories', () => {
    expect(concretePath('/var/log/pods/*/*/*.log')).toBe('/var/log/pods/default_app-7d9f8b6c4-x2x9q_0b7c3f52-1d2e-4f6a-9b8c-7d6e5f4a3b2c/app/0.log');
  });
  it('fills other wildcards', () => {
    expect(concretePath('/var/log/**/*.log')).toBe('/var/log/app/app.log');
    expect(concretePath('/tmp/{a,b}-?.txt')).toBe('/tmp/a-0.txt');
  });
});

describe('suggestFile', () => {
  const container = {
    include: ['/var/log/pods/*/*/*.log'],
    exclude: ['/var/log/pods/*/otel-collector/*.log'],
    operators: [{ builder: { type: 'container' } }],
  };
  it('writes CRI lines at a readable path for the container parser', () => {
    const s = suggestFile(container);
    expect(s.warning).toBeUndefined();
    expect(s.content.split('\n')[0]).toMatch(/^2026-10-10T08:00:00\.000000000Z stdout F /);
  });
  it('avoids paths already mocked', () => {
    const first = suggestFile(container).path;
    expect(suggestFile(container, [first]).path).toBe(first.replace(/0\.log$/, '1.log'));
  });
  it('writes JSON for a json parser and warns about excluded paths', () => {
    const s = suggestFile({ include: ['/var/log/app/*.log'], exclude: ['/var/log/app/*'], operators: [{ type: 'json_parser' }] });
    expect(JSON.parse(s.content.split('\n')[0])).toMatchObject({ level: 'info' });
    expect(s.warning).toMatch(/may not read/);
  });
});

const spans = (attrs: Record<string, string>) => ({
  resourceSpans: [{ resource: { attributes: Object.entries(attrs).map(([key, v]) => ({ key, value: { stringValue: v } })) }, scopeSpans: [] }],
});

describe('podIdentities', () => {
  it('reads k8s resource attributes once per resource', () => {
    expect(podIdentities([spans({ 'service.name': 'cart', 'k8s.pod.name': 'cart-1' }), spans({ 'service.name': 'cart', 'k8s.pod.name': 'cart-1' })])).toEqual([
      { name: 'cart-1', namespace: undefined, uid: undefined, ip: undefined, service: 'cart' },
    ]);
  });
});

describe('suggestKube', () => {
  const cfg = {
    extract: {
      metadata: ['k8s.pod.name', 'k8s.deployment.name'],
      labels: [
        { from: 'pod', key: 'app.kubernetes.io/component', tag_name: 'app.component' },
        { from: 'namespace', key: 'team', tag_name: 'team' },
      ],
    },
    pod_association: [{ sources: [{ from: 'resource_attribute', name: 'k8s.pod.ip' }] }, { sources: [{ from: 'connection' }] }],
  };
  const objects = (manifests: string) => parseAllDocuments(manifests).map((d) => d.toJS() as Record<string, any>);

  it('mocks a pod reached by the client address when the input names none', () => {
    const s = suggestKube(cfg, [spans({ 'service.name': 'checkout' })]);
    expect(s.clientAddress).toBe('10.42.0.17');
    const [pod, ns] = objects(s.manifests);
    expect(pod.kind).toBe('Pod');
    expect(pod.metadata.name).toMatch(/^checkout-7d9f8b6c4-/);
    expect(pod.metadata.labels).toEqual({ 'app.kubernetes.io/component': 'example', 'pod-template-hash': '7d9f8b6c4' });
    expect(pod.metadata.ownerReferences[0]).toMatchObject({ kind: 'ReplicaSet', name: 'checkout-7d9f8b6c4' });
    expect(pod.status.podIP).toBe('10.42.0.17');
    expect(ns).toMatchObject({ kind: 'Namespace', metadata: { name: 'default', labels: { team: 'payments' } } });
  });

  it('keeps an existing client address', () => {
    expect(suggestKube(cfg, [], '10.1.2.3').clientAddress).toBe('10.1.2.3');
  });

  it('names pods after the input and needs no address when an attribute associates', () => {
    const s = suggestKube(cfg, [spans({ 'k8s.pod.name': 'web-0', 'k8s.namespace.name': 'shop', 'k8s.pod.ip': '10.0.0.9' })]);
    expect(s.clientAddress).toBeUndefined();
    const [pod, ns] = objects(s.manifests);
    expect(pod.metadata).toMatchObject({ name: 'web-0', namespace: 'shop' });
    expect(pod.status.podIP).toBe('10.0.0.9');
    expect(ns.metadata.name).toBe('shop');
  });

  it('says when connection association is unavailable', () => {
    const s = suggestKube({ pod_association: [{ sources: [{ from: 'resource_attribute', name: 'k8s.pod.uid' }] }] }, []);
    expect(s.notes.join(' ')).toMatch(/k8s.pod.uid/);
  });
});

describe('mockedComponents', () => {
  it('finds dependency-mocked components by provider', () => {
    const g = {
      components: [
        { ref: 'receiver/filelog', kind: 'receiver', id: 'filelog', type: 'filelog', mode: 'dependency_mocked' },
        { ref: 'receiver/otlp', kind: 'receiver', id: 'otlp', type: 'otlp', mode: 'simulated' },
        { ref: 'processor/k8sattributes/a', kind: 'processor', id: 'k8sattributes/a', type: 'k8sattributes', mode: 'dependency_mocked' },
      ],
    } as unknown as Graph;
    expect(mockedComponents(g).map((m) => [m.component.id, m.provider])).toEqual([
      ['filelog', 'files'],
      ['k8sattributes/a', 'kubernetes'],
    ]);
  });
});
