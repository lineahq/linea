import { describe, expect, it } from 'vitest';
import type { CollectorCatalog } from '../api/types';
import {
  catalogKeysOf, kindCounts, manifestEntries, manifestRequest, pickerGroups, runtimeComponents, stabilityText, supportedKeys, templateDiff,
} from './catalogPicks';

const catalog: CollectorCatalog = {
  collector_version: '0.163.0',
  source: 't',
  components: [
    { kind: 'receivers', type: 'otlp', gomod: 'go.opentelemetry.io/collector/receiver/otlpreceiver v0.163.0', class: 'core' },
    { kind: 'receivers', type: 'obi', gomod: 'go.opentelemetry.io/obi v0.13.0', import: 'go.opentelemetry.io/obi/collector', class: 'contrib', unsupported: 'local replace' },
    { kind: 'processors', type: 'k8s_attributes', gomod: 'github.com/open-telemetry/opentelemetry-collector-contrib/processor/k8sattributesprocessor v1.2.0', class: 'contrib' },
    { kind: 'exporters', type: 'debug', gomod: 'go.opentelemetry.io/collector/exporter/debugexporter v0.163.0', class: 'core' },
  ],
};

const builtIn = `dist:
  otelcol_version: 0.162.0

receivers:
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0
  - gomod: "go.opentelemetry.io/obi v0.13.0"
    import: go.opentelemetry.io/obi/collector

processors: # kept across releases
  # Stable since 0.161.0.
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/processor/k8sattributesprocessor v1.1.0
  - gomod: example.com/gone v1.0.0

providers:
  - gomod: go.opentelemetry.io/collector/confmap/provider/envprovider v1.68.0
`;

describe('manifest starts', () => {
  it('reads component entries by kind with their imports, skipping providers', () => {
    expect(manifestEntries(builtIn)).toEqual([
      { kind: 'receivers', module: 'go.opentelemetry.io/collector/receiver/otlpreceiver' },
      { kind: 'receivers', module: 'go.opentelemetry.io/obi', import: 'go.opentelemetry.io/obi/collector' },
      { kind: 'processors', module: 'github.com/open-telemetry/opentelemetry-collector-contrib/processor/k8sattributesprocessor' },
      { kind: 'processors', module: 'example.com/gone' },
    ]);
  });

  it('matches a manifest of one release to another release\'s catalog by module', () => {
    expect(catalogKeysOf(builtIn, catalog)).toEqual({
      keys: ['receivers/otlp', 'receivers/obi', 'processors/k8s_attributes'],
      missing: ['processors/example.com/gone'],
    });
  });

  it('lists the supported components for "all"', () => {
    expect(supportedKeys(catalog)).toEqual(['receivers/otlp', 'processors/k8s_attributes', 'exporters/debug']);
  });

  it('names what the picks added and removed', () => {
    expect(templateDiff(new Set(['receivers/otlp', 'exporters/debug']), ['receivers/otlp', 'processors/k8s_attributes']))
      .toEqual({ added: ['exporters/debug'], removed: ['processors/k8s_attributes'] });
  });

  it('keeps a preset only for an unchanged start it can write', () => {
    const template = ['receivers/otlp', 'exporters/debug'];
    const picked = new Set(template);
    expect(manifestRequest({ start: 'built-in', release: '0.162.0', builtIn: '0.162.0', picked, template }))
      .toEqual({ version: '0.162.0', preset: 'built-in' });
    // The built-in manifest is of its own release; another one lists the picks.
    expect(manifestRequest({ start: 'built-in', release: '0.163.0', builtIn: '0.162.0', picked, template }))
      .toEqual({ version: '0.163.0', components: ['exporters/debug', 'receivers/otlp'] });
    expect(manifestRequest({ start: 'all', release: '0.163.0', builtIn: '0.162.0', picked, template })).toEqual({ version: '0.163.0', preset: 'all' });
    expect(manifestRequest({ start: 'built-in', release: '0.162.0', builtIn: '0.162.0', picked: new Set(['receivers/otlp']), template }))
      .toEqual({ version: '0.162.0', components: ['receivers/otlp'] });
    expect(manifestRequest({ start: 'config', release: '0.162.0', builtIn: '0.162.0', picked, template }))
      .toEqual({ version: '0.162.0', components: ['exporters/debug', 'receivers/otlp'] });
  });
});

describe('picker view', () => {
  const picked = new Set(['receivers/otlp', 'exporters/debug']);

  it('counts the picks of each kind in pipeline order', () => {
    expect(kindCounts(catalog, picked)).toEqual([
      { kind: 'receivers', total: 2, picked: 1 }, { kind: 'processors', total: 1, picked: 0 }, { kind: 'exporters', total: 1, picked: 1 },
    ]);
  });

  it('filters by kind, search and selection', () => {
    const types = (v: Parameters<typeof pickerGroups>[2]) => pickerGroups(catalog, picked, v).map((g) => [g.kind, g.components.map((c) => c.type)]);
    expect(types({ filter: '', kind: '', selectedOnly: true })).toEqual([['receivers', ['otlp']], ['exporters', ['debug']]]);
    expect(types({ filter: '', kind: 'receivers', selectedOnly: false })).toEqual([['receivers', ['obi', 'otlp']]]);
    expect(types({ filter: 'k8s', kind: '', selectedOnly: true })).toEqual([]);
    expect(pickerGroups(catalog, picked, { filter: 'obi', kind: '', selectedOnly: false })[0]).toMatchObject({ picked: 1, total: 2 });
  });

  it('words stability, most mature first', () => {
    expect(stabilityText({ stability: { alpha: ['logs'], stable: ['traces', 'metrics'], development: [] } }))
      .toBe('stable: traces, metrics · alpha: logs · development');
    expect(stabilityText({})).toBe('');
  });
});

describe('runtime components', () => {
  const manifest = `${builtIn}
exporters:
  - gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.163.0
  - gomod: github.com/lineahq/linea/collector/livecapture v0.0.0
    import: github.com/lineahq/linea/collector/livecapture/captureexporter
`;

  it('names a runtime\'s components by the catalog, and custom ones by their package', () => {
    expect(runtimeComponents(manifest, catalog)).toEqual([
      { kind: 'receivers', items: [{ name: 'obi', module: 'go.opentelemetry.io/obi/collector', inCatalog: true }, { name: 'otlp', module: 'go.opentelemetry.io/collector/receiver/otlpreceiver', inCatalog: true }] },
      { kind: 'processors', items: [{ name: 'gone', module: 'example.com/gone', inCatalog: false }, { name: 'k8s_attributes', module: 'github.com/open-telemetry/opentelemetry-collector-contrib/processor/k8sattributesprocessor', inCatalog: true }] },
      { kind: 'exporters', items: [{ name: 'captureexporter', module: 'github.com/lineahq/linea/collector/livecapture/captureexporter', inCatalog: false }, { name: 'debug', module: 'go.opentelemetry.io/collector/exporter/debugexporter', inCatalog: true }] },
    ]);
  });

  it('names them by package without a catalog', () => {
    expect(runtimeComponents(manifest)[0].items).toEqual([
      { name: 'collector', module: 'go.opentelemetry.io/obi/collector' }, { name: 'otlpreceiver', module: 'go.opentelemetry.io/collector/receiver/otlpreceiver' },
    ]);
    expect(runtimeComponents('')).toEqual([]);
  });
});
