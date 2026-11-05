import { describe, expect, it } from 'vitest';
import { debuggerState, readiness, type ReadinessInput } from './state';

describe('debuggerState', () => {
  it.each([
    ['nothing loaded', { hasConfig: false }, 'start'],
    ['a configuration draft', { hasConfig: true }, 'prepare'],
    ['a queued run', { runId: 'r', run: { state: 'queued' }, hasConfig: true }, 'running'],
    ['a running run', { runId: 'r', run: { state: 'running' }, hasConfig: false }, 'running'],
    ['a finished run', { runId: 'r', run: { state: 'done' }, hasConfig: true }, 'investigate'],
    ['a failed run', { runId: 'r', run: { state: 'error' }, hasConfig: false }, 'investigate'],
    ['a cancelled run', { runId: 'r', run: { state: 'cancelled' }, hasConfig: true }, 'investigate'],
    ['a run still loading', { runId: 'r', hasConfig: false }, 'investigate'],
    ['a compared run', { runId: 'r', run: { state: 'done' }, compare: 'o', hasConfig: true }, 'compare'],
    ['a compare link without a run', { compare: 'o', hasConfig: true }, 'prepare'],
    ['the start state asked for over a draft', { hasConfig: true, start: true }, 'start'],
    ['a run link with start', { runId: 'r', run: { state: 'done' }, hasConfig: true, start: true }, 'investigate'],
  ] as const)('%s', (_, input, want) => {
    expect(debuggerState(input)).toBe(want);
  });
});

const ready: ReadinessInput = { configText: 'receivers: {}', inspection: { valid: true }, runtime: { name: 'built-in' } };

describe('readiness', () => {
  it('is ready with a valid configuration, an input and a verified runtime', () => {
    expect(readiness(ready)).toEqual({ tone: 'ok', label: 'Ready to run', problems: [] });
  });

  it('prompts for an input alone without calling it a failure', () => {
    const r = readiness({ ...ready, inputBlocker: 'Add an input to run' });
    expect(r.tone).toBe('warn');
    expect(r.label).toBe('Add an input to run');
    expect(r.problems.map((p) => p.fix?.action)).toEqual(['inputs']);
  });

  it('prompts for a configuration when there is none', () => {
    const r = readiness({ configText: '  ', inputBlocker: 'The configuration is not valid' });
    expect(r).toMatchObject({ tone: 'warn', label: 'Add a configuration to run' });
    expect(r.problems).toHaveLength(1);
  });

  it('counts blocking problems and offers each remedy', () => {
    const r = readiness({
      ...ready,
      inspection: { valid: false, diagnostics: [{ severity: 'error', code: 'unknown_key', message: 'unknown key log_statment', component: 'transform/normalize' }] },
      inputBlocker: 'Add an input to run',
      runtime: { name: 'acme', missing: true },
    });
    expect(r.tone).toBe('blocked');
    expect(r.label).toBe('3 problems block running');
    expect(r.problems.map((p) => p.kind)).toEqual(['config', 'input', 'runtime']);
    expect(r.problems[0].text).toBe('transform/normalize: unknown key log_statment');
    expect(r.problems.every((p) => p.fix)).toBe(true);
  });

  it('says one problem in the singular', () => {
    const r = readiness({ ...ready, inspection: { valid: false, diagnostics: [{ severity: 'error', code: 'x', message: 'bad' }] } });
    expect(r.label).toBe('1 problem blocks running');
  });

  it('reports a YAML error with its line, ahead of validation', () => {
    const r = readiness({ ...ready, yamlError: { message: 'bad indent', line: 9 }, inspection: { valid: false } });
    expect(r.problems).toHaveLength(1);
    expect(r.problems[0]).toMatchObject({ text: 'YAML syntax error on line 9: bad indent', fix: { action: 'config', line: 9 } });
  });

  it('keeps warnings out of the blocking count', () => {
    const r = readiness({
      ...ready,
      inspection: { valid: false, diagnostics: [{ severity: 'warning', code: 'w', message: 'meh' }] },
    });
    expect(r.problems.map((p) => p.text)).toEqual(['The configuration is not valid']);
    const unverified = readiness({ ...ready, runtime: { name: 'custom', unverified: true } });
    expect(unverified.tone).toBe('warn');
    expect(unverified.label).toBe('Ready to run, 1 warning');
    expect(unverified.problems[0].blocking).toBe(false);
  });

  it('waits for the first validation and reports a failed one', () => {
    expect(readiness({ configText: 'a: 1' })).toMatchObject({ tone: 'warn', label: 'Checking the configuration' });
    const failed = readiness({ configText: 'a: 1', validationError: 'network down' });
    expect(failed.tone).toBe('blocked');
    expect(failed.problems[0].text).toContain('network down');
  });

  it('caps long diagnostic lists', () => {
    const diagnostics = Array.from({ length: 8 }, (_, n) => ({ severity: 'error', code: 'e', message: `m${n}` }));
    const r = readiness({ ...ready, inspection: { valid: false, diagnostics } });
    expect(r.problems).toHaveLength(6);
    expect(r.problems[5].text).toBe('3 more configuration problems');
  });

  it('shows a run in progress', () => {
    expect(readiness({ ...ready, running: true })).toEqual({ tone: 'running', label: 'Running', problems: [] });
  });

  it('names a malformed scenario at its inputs', () => {
    const r = readiness({ ...ready, inputBlocker: 'Name a file or add OTLP JSON' });
    expect(r.problems[0]).toMatchObject({ kind: 'scenario', fix: { action: 'inputs' } });
    expect(r.label).toBe('1 problem blocks running');
  });
});
