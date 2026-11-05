// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { CaptureMetadataEditor } from './CaptureMetadataEditor';
import { api } from '../api/client';
import type { CaptureSession } from '../api/types';
let host: HTMLDivElement, root: Root, client: QueryClient;
const session = { id: 'capture-one', metadata: { revision: 2, name: 'Original', description: 'Keep', updated_at: '' } } as CaptureSession;
beforeEach(() => { (globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true; vi.spyOn(api, 'capture').mockResolvedValue(session); vi.spyOn(api, 'updateCaptureMetadata').mockImplementation(async (_id, request) => ({ ...session, metadata: { ...request, revision: request.revision + 1, updated_at: '' } })); host = document.createElement('div'); document.body.append(host); root = createRoot(host); client = new QueryClient({ defaultOptions: { queries: { retry: false } } }); });
afterEach(() => { act(() => root.unmount()); client.clear(); host.remove(); vi.restoreAllMocks(); });
async function settle(until: () => boolean) { for (let i = 0; i < 100 && !until(); i++) await act(() => new Promise(r => setTimeout(r, 5))); expect(until()).toBe(true); }
async function render() { await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(CaptureMetadataEditor, { session })))); }
function click(text: string) { act(() => [...host.querySelectorAll('button')].find(b => b.textContent === text)!.click()); }
function name(value: string) { const input = host.querySelector<HTMLInputElement>('input')!; act(() => { Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(input, value); input.dispatchEvent(new Event('input', { bubbles: true })); }); }
it('saves only editable details using the loaded revision and updates the shared cache', async () => { await render(); click('Edit details'); await settle(() => !!host.querySelector('input')); name('Checkout incident'); click('Save details'); await settle(() => !host.querySelector('dialog')); expect(api.updateCaptureMetadata).toHaveBeenCalledWith('capture-one', { revision: 2, name: 'Checkout incident', description: 'Keep' }); expect(client.getQueryData<CaptureSession>(['capture-session', 'capture-one'])?.metadata?.revision).toBe(3); });
it('retains unsaved edits when a concurrent update is refused', async () => { vi.mocked(api.updateCaptureMetadata).mockRejectedValue(new Error('Capture details changed; reload before saving')); await render(); click('Edit details'); await settle(() => !!host.querySelector('input')); name('My draft'); click('Save details'); await settle(() => !!host.querySelector('[role="alert"]')); expect(host.querySelector<HTMLInputElement>('input')!.value).toBe('My draft'); expect(host.querySelector('dialog')).not.toBeNull(); });
