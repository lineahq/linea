import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/client';
import { editProblem } from './candidates';

describe('statement edit problems', () => {
  it('quotes the current statement of a stale edit', () => {
    const stale = new ApiError(409, 'stale statement edit', { code: 'conflict', error: 'stale statement edit', current_statement: 'set(x, 2)' });
    expect(editProblem(stale)).toBe('The statement has changed in the editor since this run (it now reads set(x, 2)); rerun first.');
    expect(editProblem(new ApiError(409, 'stale', { code: 'conflict', error: 'stale' }))).toBe('The statement has changed in the editor since this run; rerun first.');
  });
  it("passes the server's other messages through", () => {
    expect(editProblem(new ApiError(400, 'unsupported statement shape; provide complete YAML'))).toBe('unsupported statement shape; provide complete YAML');
  });
});
