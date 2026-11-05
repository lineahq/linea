import { describe, expect, it } from 'vitest';
import { auditQuery, NO_FILTERS, outcomeView, resourcesText, scopeText } from './auditView';

describe('auditQuery', () => {
  it('sends only the set filters', () => {
    expect(auditQuery(NO_FILTERS, '')).toEqual({ principal: undefined, operation: undefined, outcome: undefined, cursor: undefined, limit: 50 });
    expect(auditQuery({ principal: ' user:alice ', operation: 'SubmitRun', outcome: 'denied' }, 'c2')).toEqual({
      principal: 'user:alice',
      operation: 'SubmitRun',
      outcome: 'denied',
      cursor: 'c2',
      limit: 50,
    });
  });
});

describe('outcomeView', () => {
  it('tells refusals from failures and successes', () => {
    expect(outcomeView({ outcome: 'denied' })).toEqual({ label: 'Denied', tone: 'fail' });
    expect(outcomeView({ outcome: 'allowed', code: 'not_found' })).toEqual({ label: 'Failed: not_found', tone: 'warn' });
    expect(outcomeView({ outcome: 'allowed' })).toEqual({ label: 'Allowed', tone: 'pass' });
  });
});

describe('scopeText', () => {
  it('words the scope', () => {
    expect(scopeText('*')).toBe('Platform');
    expect(scopeText('project:payments')).toBe('Project payments');
    expect(scopeText('')).toBe('Filtered by project');
  });
});

describe('resourcesText', () => {
  it('lists the first IDs and counts the rest', () => {
    expect(resourcesText([])).toBe('—');
    expect(resourcesText(['r1'])).toBe('r1');
    expect(resourcesText(['r1', 'r2', 'r3', 'r4'])).toBe('r1, r2 and 2 more');
  });
});
