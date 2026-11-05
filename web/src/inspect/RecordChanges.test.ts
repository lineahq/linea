// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Diff, OTTLReport } from '../api/types';
import { RecordChanges } from './RecordChanges';

vi.mock('./DiffViews', () => ({ SideBySide: () => null, RawJSON: () => null }));
let host: HTMLDivElement, root: Root;
beforeEach(() => { (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true; host = document.createElement('div'); document.body.append(host); root = createRoot(host); });
afterEach(() => { act(() => root.unmount()); host.remove(); });

const log = (message: string) => ({ body: { stringValue: message }, attributes: [{ key: 'user.id', value: { stringValue: 'jane' } }] });
const input = (n = 2) => ({ resourceLogs: [{ resource: { attributes: [{ key: 'service.name', value: { stringValue: 'accounts' } }] }, scopeLogs: [{ scope: { name: 'app' }, logRecords: Array.from({ length: n }, (_, i) => log(i === 0 ? 'Login' : i === 1 ? 'Health check' : `Message ${i}`)) }] }] });
const counts = { items: 2, resources: 1, scopes: 1 };
const entity = (i: number) => `resource[0].scope[0].log_record[${i}]`;
const diff = (d: Partial<Diff> = {}): Diff => ({ identity: 'content', before: counts, after: counts, ...d });
const edit = { entity: entity(0), path: 'attributes["user.id"]', op: 'modified', before: 'jane', after: 'hashed' };
type Props = Partial<Parameters<typeof RecordChanges>[0]>;
function render(props: Props = {}) { act(() => root.render(createElement(RecordChanges, { diff: diff({ changes: [edit] }), before: input(), after: input(), ...props } as Parameters<typeof RecordChanges>[0]))); }
const rows = () => [...host.querySelectorAll<HTMLButtonElement>('.rv-row')];
const text = (selector: string) => host.querySelector(selector)!.textContent!;
const click = (el: Element) => act(() => (el as HTMLElement).click());
const pill = (label: string) => [...host.querySelectorAll<HTMLButtonElement>('.rv-filters button')].find((b) => b.textContent!.startsWith(label))!;

describe('record changes', () => {
  it('lists every record as one row that states the answer, with the first changed record open', () => {
    render();
    expect(rows()).toHaveLength(2);
    expect(rows()[0].textContent).toContain('Login');
    expect(rows()[0].textContent).toContain('Changed in record');
    expect(rows()[0].querySelector('.rv-pill-mod')!.textContent).toBe('1 change');
    expect(rows()[1].textContent).toContain('No change');
    expect(rows().map((r) => r.getAttribute('aria-expanded'))).toEqual(['true', 'false']);
    expect(text('.rv-panel .rv-path')).toBe('log.attributes["user.id"]');
    expect(text('.rv-panel .rv-before')).toBe('"jane"');
    expect(text('.rv-panel .rv-after')).toBe('"hashed"');
  });

  it('labels every column, so the badge reads as the log severity', () => {
    render();
    expect([...host.querySelectorAll('.rv-head > span')].map((e) => e.textContent)).toEqual(['', 'Severity', 'Message', 'Service', 'Change', '']);
    expect(rows()[0].querySelector('.rv-cell-badge .rv-badge')!.getAttribute('title')).toBe('Severity');
    expect(rows()[0].querySelector('.rv-cell.rv-service')!.textContent).toBe('accounts');
    expect(rows()[0].querySelector('.rv-title-line .rv-badge')).toBeNull();
  });

  it('labels the recorded input and output lists without a change column', () => {
    render({ diff: undefined });
    const heads = [...host.querySelectorAll('.rv-head')].map((h) => [...h.children].map((e) => e.textContent));
    expect(heads).toEqual([['', 'Severity', 'Message', 'Service', ''], ['', 'Severity', 'Message', 'Service', '']]);
  });

  it('keeps one record open at a time and closes it again on a second click', () => {
    render();
    click(rows()[1]);
    expect(rows().map((r) => r.getAttribute('aria-expanded'))).toEqual(['false', 'true']);
    expect(host.querySelectorAll('.rv-panel')).toHaveLength(1);
    expect(text('.rv-panel')).toContain('left the record as it arrived');
    click(rows()[1]);
    expect(host.querySelector('.rv-panel')).toBeNull();
  });

  it('shows the whole record on request, grouped by where each field lives', () => {
    render();
    expect([...host.querySelectorAll('.rv-group-head b')].map((e) => e.textContent)).toEqual(['Record']);
    click(host.querySelector('.rv-more')!);
    expect([...host.querySelectorAll('.rv-group-head b')].map((e) => e.textContent)).toEqual(['Resource', 'Scope', 'Record']);
    const paths = [...host.querySelectorAll('.rv-path')].map((e) => e.textContent);
    expect(paths).toEqual(['resource.attributes["service.name"]', 'instrumentation_scope.name', 'log.body', 'log.attributes["user.id"]']);
    expect(host.querySelector('.rv-more')!.textContent).toBe('Show changed fields only');
  });

  it('filters by outcome with counts and hides outcomes that did not happen', () => {
    render({ diff: diff({ changes: [edit], removed: [{ ref: entity(1), name: 'Health check' }] }) });
    expect([...host.querySelectorAll('.rv-filters button')].map((b) => b.textContent)).toEqual(['All2', 'Changed1', 'Dropped1']);
    click(pill('Dropped'));
    expect(rows()).toHaveLength(1);
    expect(rows()[0].textContent).toContain('Health check');
    expect(rows()[0].querySelector('.rv-pill-drop')).not.toBeNull();
  });

  it('shows a dropped record from the input and a created one from the output', () => {
    const output = { resourceLogs: [{ scopeLogs: [{ logRecords: [log('Created log')] }] }] };
    render({ diff: diff({ changes: [], removed: [{ ref: entity(0), name: 'Login' }], added: [{ ref: entity(0), name: 'Created log' }] }), after: output });
    expect(rows().map((r) => r.querySelector('.rv-pill')!.textContent)).toEqual(['dropped', 'unchanged', 'new']);
    click(rows()[2]);
    expect(host.querySelector('.rv-panel .rv-field-added')).not.toBeNull();
    expect(text('.rv-panel')).toContain('"Created log"');
    expect(host.querySelector('.rv-panel .rv-field-removed')).toBeNull();
  });

  it('puts shared resource edits under Resource and says how many records share them', () => {
    render({ diff: diff({ changes: [{ entity: 'resource[0]', path: 'attributes["env"]', op: 'added', after: 'prod', after_type: 'Str' }] }) });
    expect(rows().map((r) => r.textContent!.includes('Changed in resource'))).toEqual([true, true]);
    expect(text('.rv-group-head')).toContain('Resource');
    expect(text('.rv-group-head')).toContain('edit shared by 2 records');
    expect(text('.rv-path')).toBe('resource.attributes["env"]');
  });

  it('does not call unmatched records unchanged, drops or additions', () => {
    render({ diff: diff({ changes: [], matching: { object: 0, exact: 0, high: 0, medium: 0, unmatched_before: 2, unmatched_after: 2 } }) });
    expect(text('.rv-notice')).toContain('Their changes are unknown');
    expect(rows().map((r) => r.querySelector('.rv-pill')!.textContent)).toEqual(['no change found', 'no change found']);
  });

  it('does not label explicitly reported removals as unknown matches', () => {
    render({ diff: diff({ changes: [], removed: [{ ref: entity(0), name: 'Login' }], matching: { object: 0, exact: 1, high: 0, medium: 0, unmatched_before: 1, unmatched_after: 0 } }) });
    expect(host.querySelector('.rv-notice')).toBeNull();
  });

  it('exposes the match confidence of a content-matched edit', () => {
    render({ diff: diff({ changes: [{ ...edit, confidence: 'medium' }] }) });
    expect(rows()[0].textContent).toContain('medium-confidence match');
    expect(text('.rv-panel')).toContain('medium-confidence match');
  });

  it('keeps the whole input reachable through pagination and opens the page of a selected record', () => {
    const many = input(30);
    render({ diff: diff({ changes: [] }), before: many, after: many });
    expect(rows()).toHaveLength(25);
    click([...host.querySelectorAll('button')].find((b) => b.textContent === 'Next changes')!);
    expect(rows()).toHaveLength(5);
    act(() => root.render(createElement(RecordChanges, { diff: diff({ changes: [] }), before: many, after: many, record: entity(29) })));
    expect(host.textContent).toContain('Page 2 of 2');
    expect(host.querySelector('.rv-row[aria-current="true"]')!.getAttribute('data-record')).toBe(entity(29));
    expect(host.querySelector('.rv-row[aria-expanded="true"]')!.getAttribute('data-record')).toBe(entity(29));
  });

  it('returns from a selected record to all records without another picker', () => {
    const onClearRecord = vi.fn();
    render({ record: entity(1), onClearRecord });
    click([...host.querySelectorAll('button')].find((b) => b.textContent === 'Show all records')!);
    expect(onClearRecord).toHaveBeenCalledOnce();
  });

  it('offers one contextual follow action per open record', () => {
    const follow = vi.fn(), resolver = vi.fn((e: string, side: string) => e === entity(0) && side === 'before' ? follow : undefined);
    render({ onFollow: resolver });
    expect(host.querySelectorAll('.rv-follow')).toHaveLength(1);
    click(host.querySelector('.rv-follow')!);
    expect(follow).toHaveBeenCalledOnce();
  });

  it('names the statement that made each change of a transform and lists the statements', () => {
    const report = { verification: 'replay_verified', groups: [{ index: 0, statements: [
      { index: 0, text: 'set(log.attributes["env"], "prod")', matched: 2, not_matched: 0 },
      { index: 1, text: 'replace_pattern(log.attributes["user.id"], ".+", "x")', matched: 1, not_matched: 1, changes: [{ entity: entity(0), path: 'attributes["user.id"]', op: 'modified' }] },
    ] }] } as unknown as OTTLReport;
    const open = vi.fn();
    render({ report, onOpenStatements: open });
    expect(text('.rv-by')).toBe('S1');
    expect([...host.querySelectorAll('.rv-statements li')].map((li) => li.classList.contains('rv-applied'))).toEqual([false, true]);
    expect(text('.rv-statements li:nth-child(2)')).toContain('replace_pattern');
    click([...host.querySelectorAll('.rv-statements button')].find((b) => b.textContent === 'Edit statements')!);
    expect(open).toHaveBeenCalledOnce();
  });

  it('copies the OTTL path and says when the clipboard refuses', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    Object.assign(navigator, { clipboard: { writeText } });
    render();
    await act(async () => { click(host.querySelector('.rv-copy')!); });
    expect(writeText).toHaveBeenCalledWith('log.attributes["user.id"]');
    expect(text('.rv-copy')).toBe('Copied');
    writeText.mockRejectedValueOnce(new Error('denied'));
    await act(async () => { click(host.querySelector('.rv-copy')!); });
    expect(text('.rv-copy')).toBe('Not copied');
  });

  it('offers the raw JSON diff and the recorded output, collapsed', () => {
    render();
    expect(text('.raw-json-diff > summary')).toBe('Raw JSON diff');
    expect(host.querySelector('.raw-json-content')).toBeNull();
    expect(host.querySelector<HTMLDetailsElement>('.rv-recorded')!.open).toBe(false);
  });
});
