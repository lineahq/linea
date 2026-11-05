import { describe, expect, it } from 'vitest';
import type { Graph } from '../api/types';
import { configStatus, topologySummary } from './configStatus';

const graph = {
  pipelines: [{ id: 'a' }, { id: 'b' }],
  components: [{ kind: 'receiver' }, { kind: 'exporter' }, { kind: 'extension' }],
} as unknown as Graph;

describe('topologySummary', () => {
  it('counts pipelines, components and extensions', () => {
    expect(topologySummary(graph)).toBe('2 pipelines · 2 components · 1 extension');
    expect(topologySummary({ pipelines: [{ id: 'a' }], components: [] } as unknown as Graph)).toBe('1 pipeline · 0 components');
    expect(topologySummary(undefined)).toBe('');
  });
});

describe('configStatus', () => {
  it('says nothing for an empty configuration', () => {
    expect(configStatus([], undefined, '  ')).toBeUndefined();
  });
  it('counts the problems found while typing', () => {
    expect(configStatus([{ severity: 'error' }, { severity: 'warning' }, { severity: 'error' }], { valid: false }, 'a')).toEqual({
      items: [{ tone: 'blocked', text: '2 errors' }, { tone: 'warn', text: '1 warning' }],
      detail: 'Checked as you type',
    });
  });
  it('states a valid configuration and what it declares', () => {
    expect(configStatus([], { valid: true, topology: graph }, 'a')).toEqual({
      items: [{ tone: 'ok', text: 'Valid YAML' }],
      detail: '2 pipelines · 2 components · 1 extension',
    });
  });
  it('names the runtime that checked a valid configuration', () => {
    const runtime = { kind: 'built_in' as const, collector_version: '0.162.0', verified: true };
    expect(configStatus([], { valid: true, topology: graph, runtime }, 'a')).toEqual({
      items: [{ tone: 'ok', text: 'Valid YAML' }],
      detail: '2 pipelines · 2 components · 1 extension · checked on Built-in · Collector 0.162.0',
    });
    expect(configStatus([], { valid: true, runtime }, 'a')?.detail).toBe('checked on Built-in · Collector 0.162.0');
  });
  it('waits for the first answer', () => {
    expect(configStatus([], undefined, 'a')?.detail).toBe('Checking the configuration…');
  });
});
