import type { AuditQuery } from '../api/client';
import type { AuditEvent } from '../api/types';

/** AuditFilters are the audit log's filter fields as the form holds them. */
export interface AuditFilters {
  principal: string;
  operation: string;
  outcome: '' | 'allowed' | 'denied';
}

export const NO_FILTERS: AuditFilters = { principal: '', operation: '', outcome: '' };

/** AUDIT_PAGE is how many events a page lists (the server allows 500). */
export const AUDIT_PAGE = 50;

/** auditQuery is the request for one page of events under the filters. */
export function auditQuery(f: AuditFilters, cursor: string): AuditQuery {
  return {
    principal: f.principal.trim() || undefined,
    operation: f.operation.trim() || undefined,
    outcome: f.outcome || undefined,
    cursor: cursor || undefined,
    limit: AUDIT_PAGE,
  };
}

/** outcomeView words an event's outcome: refused, failed with its code, or done. */
export function outcomeView(e: Pick<AuditEvent, 'outcome' | 'code'>): { label: string; tone: 'pass' | 'fail' | 'warn' } {
  if (e.outcome === 'denied') return { label: 'Denied', tone: 'fail' };
  if (e.code) return { label: `Failed: ${e.code}`, tone: 'warn' };
  return { label: 'Allowed', tone: 'pass' };
}

/** scopeText words where an operation applied. */
export function scopeText(scope: string | undefined): string {
  if (scope === '*') return 'Platform';
  if (scope?.startsWith('project:')) return `Project ${scope.slice('project:'.length)}`;
  return scope || 'Filtered by project';
}

/** resourcesText lists the first resource IDs, counting the rest. */
export function resourcesText(ids: readonly string[] | undefined, shown = 2): string {
  const all = ids ?? [];
  if (all.length === 0) return '—';
  const head = all.slice(0, shown).join(', ');
  return all.length > shown ? `${head} and ${all.length - shown} more` : head;
}
