// @vitest-environment jsdom
import { act, createElement } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SignInPage } from './SignInPage';
import { parseSignInSearch } from './signin';

interface Call { method: string; url: string; body?: string; headers: Record<string, string> }
let calls: Call[];
let methods: () => Response;
let password: () => Response;
let assign: ReturnType<typeof vi.fn>;
let host: HTMLDivElement, root: Root, client: QueryClient;

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  calls = [];
  assign = vi.fn();
  vi.stubGlobal('scrollTo', () => undefined);
  vi.stubGlobal('location', { ...window.location, assign });
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({ method: init?.method ?? 'GET', url, body: init?.body as string | undefined, headers: (init?.headers ?? {}) as Record<string, string> });
    if (url === '/auth/methods') return methods();
    if (url === '/auth/password') return password();
    throw new Error(`Unexpected request: ${url}`);
  }));
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  host = document.createElement('div');
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
  client.clear();
  host.remove();
  vi.unstubAllGlobals();
});

const json = (v: unknown) => new Response(JSON.stringify(v), { status: 200, headers: { 'Content-Type': 'application/json' } });
const plain = (status: number, text: string) => new Response(text, { status });

async function settle(until: () => boolean) {
  for (let i = 0; i < 200 && !until(); i++) await act(() => new Promise((r) => setTimeout(r, 5)));
  expect(until()).toBe(true);
}
async function render(entry: string) {
  const rootRoute = createRootRoute();
  const page = createRoute({ getParentRoute: () => rootRoute, path: '/sign-in', component: SignInPage, validateSearch: parseSignInSearch });
  const router = createRouter({ routeTree: rootRoute.addChildren([page]), history: createMemoryHistory({ initialEntries: [entry] }) });
  await act(async () => root.render(createElement(QueryClientProvider, { client }, createElement(RouterProvider, { router: router as never }))));
}
const text = () => document.body.textContent ?? '';
const input = (id: string) => document.getElementById(id) as HTMLInputElement | null;
const submit = () => document.querySelector('button[type="submit"]') as HTMLButtonElement | null;
const link = () => document.querySelector('a.od-btn') as HTMLAnchorElement | null;
const alertText = () => document.querySelector('[role="alert"]')?.textContent ?? '';

/** type sets an input's value the way a user's typing does, so React sees it. */
async function type(el: HTMLInputElement, value: string) {
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(el, value);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  });
}
const submitForm = async () => act(async () => document.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })));

describe('SignInPage', () => {
  it('shows the password form with the user name prefilled and the right autocomplete', async () => {
    methods = () => json({ password: true });
    await render('/sign-in?return_to=%2Fruns');
    await settle(() => input('signin-password') !== null);
    expect(input('signin-user')!.value).toBe('admin');
    expect(input('signin-user')!.autocomplete).toBe('username');
    expect(input('signin-password')!.autocomplete).toBe('current-password');
    expect(input('signin-password')!.type).toBe('password');
    expect(link()).toBeNull();
    expect(submit()!.disabled).toBe(true);
  });

  it('signs in as JSON and loads the page the user came for', async () => {
    methods = () => json({ password: true });
    password = () => new Response(null, { status: 204 });
    await render('/sign-in?return_to=%2Fruns%3Frun%3Dr-1');
    await settle(() => input('signin-password') !== null);
    await type(input('signin-password')!, 'correct horse');
    expect(submit()!.disabled).toBe(false);
    await submitForm();
    await settle(() => assign.mock.calls.length === 1);
    const post = calls.find((c) => c.url === '/auth/password')!;
    expect(post.method).toBe('POST');
    expect(post.headers['Content-Type']).toBe('application/json');
    expect(JSON.parse(post.body!)).toEqual({ user: 'admin', password: 'correct horse' });
    expect(assign).toHaveBeenCalledWith('/runs?run=r-1');
  });

  it('never returns to another site', async () => {
    methods = () => json({ password: true });
    password = () => new Response(null, { status: 204 });
    await render('/sign-in?return_to=%2F%2Fevil.example');
    await settle(() => input('signin-password') !== null);
    await type(input('signin-password')!, 'x');
    await submitForm();
    await settle(() => assign.mock.calls.length === 1);
    expect(assign).toHaveBeenCalledWith('/');
  });

  it.each([
    [401, 'wrong user name or password', 'Wrong user name or password.'],
    [429, 'too many failed sign-ins', 'Too many failed sign-ins. Wait a minute, then try again.'],
    [503, 'try again shortly', 'Sign-in failed: try again shortly'],
  ])('says what went wrong after HTTP %i, in an alert, and stays on the page', async (status, body, want) => {
    methods = () => json({ password: true });
    password = () => plain(status, body);
    await render('/sign-in');
    await settle(() => input('signin-password') !== null);
    await type(input('signin-password')!, 'nope');
    await submitForm();
    await settle(() => alertText() !== '');
    expect(alertText()).toBe(want);
    expect(assign).not.toHaveBeenCalled();
    expect(input('signin-password')!.value).toBe('');
    expect(input('signin-user')!.value).toBe('admin');
  });

  it('offers the provider beside the form, linking with the return path', async () => {
    methods = () => json({ password: true, oidc: { issuer: 'https://login.example.com/realms/linea' } });
    await render('/sign-in?return_to=%2Fsaved');
    await settle(() => link() !== null);
    expect(link()!.textContent).toBe('Sign in with login.example.com');
    expect(link()!.getAttribute('href')).toBe('/auth/login?method=oidc&return_to=%2Fsaved');
    expect(input('signin-password')).not.toBeNull();
  });

  it('offers only the provider when the server has no password sign-in', async () => {
    methods = () => json({ password: false, oidc: { issuer: 'https://idp.example.com' } });
    await render('/sign-in');
    await settle(() => link() !== null);
    expect(link()!.textContent).toBe('Sign in with idp.example.com');
    expect(input('signin-password')).toBeNull();
    expect(submit()).toBeNull();
  });

  it('says so when the methods cannot be read, and offers a retry', async () => {
    methods = () => plain(500, 'boom');
    await render('/sign-in');
    await settle(() => text().includes('could not be read'));
    expect(text()).toContain('boom');
    expect(text()).toContain('Try again');
    expect(input('signin-password')).toBeNull();
  });
});
