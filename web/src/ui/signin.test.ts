import { describe, expect, it } from 'vitest';
import { isSignInPath, oidcHref, parseSignInSearch, providerName, safeReturnTo, signInBlocker, signInChoices, signInMessage } from './signin';
import { signInHref } from './access';

describe('safeReturnTo', () => {
  it.each([
    ['/', '/'],
    ['/runs?run=r-1', '/runs?run=r-1'],
    ['/saved/a%2Fb#x', '/saved/a%2Fb#x'],
    ['//evil.example', '/'],
    ['/\\evil.example', '/'],
    ['https://evil.example/', '/'],
    ['javascript:alert(1)', '/'],
    ['runs', '/'],
    ['', '/'],
  ])('%s -> %s', (value, want) => {
    expect(safeReturnTo(value)).toBe(want);
  });
  it('falls back to / without a value', () => {
    expect(safeReturnTo(undefined)).toBe('/');
    expect(safeReturnTo(null)).toBe('/');
  });
  it('accepts what signInHref sends, whatever the location', () => {
    const back = '/runs?run=r-1&node=a%2Fb';
    const sent = new URL(signInHref(back), 'http://x').searchParams.get('return_to');
    expect(safeReturnTo(sent)).toBe(back);
  });
});

describe('parseSignInSearch', () => {
  it('keeps a safe return_to and drops the rest', () => {
    expect(parseSignInSearch({ return_to: '/runs' })).toEqual({ return_to: '/runs' });
    expect(parseSignInSearch({ return_to: '//evil.example' })).toEqual({ return_to: '/' });
    expect(parseSignInSearch({ return_to: '' })).toEqual({ return_to: undefined });
    expect(parseSignInSearch({ return_to: 7 })).toEqual({ return_to: undefined });
    expect(parseSignInSearch({})).toEqual({ return_to: undefined });
  });
});

describe('oidcHref', () => {
  it('asks for the provider and carries the encoded, checked return_to', () => {
    expect(oidcHref('/runs?run=r-1')).toBe('/auth/login?method=oidc&return_to=%2Fruns%3Frun%3Dr-1');
    expect(oidcHref('//evil.example')).toBe('/auth/login?method=oidc&return_to=%2F');
    expect(oidcHref(undefined)).toBe('/auth/login?method=oidc&return_to=%2F');
  });
});

describe('providerName', () => {
  it('is the issuer host', () => {
    expect(providerName('https://login.example.com/realms/linea')).toBe('login.example.com');
    expect(providerName('https://idp.example.com:8443')).toBe('idp.example.com:8443');
  });
  it('keeps an issuer that is not a URL', () => {
    expect(providerName('not a url')).toBe('not a url');
  });
});

describe('signInChoices', () => {
  it('maps the server methods to the form and the provider button', () => {
    expect(signInChoices({ password: true })).toEqual({ password: true, provider: undefined });
    expect(signInChoices({ password: true, oidc: { issuer: 'https://idp.example.com/x' } })).toEqual({ password: true, provider: 'idp.example.com' });
    expect(signInChoices({ password: false, oidc: { issuer: 'https://idp.example.com' } })).toEqual({ password: false, provider: 'idp.example.com' });
  });
});

describe('signInMessage', () => {
  it.each([
    [401, 'Wrong user name or password.'],
    [429, 'Too many failed sign-ins. Wait a minute, then try again.'],
    [404, 'This server has no password sign-in.'],
    [400, 'The server refused the sign-in request. Reload the page and try again.'],
    [403, 'The server refused the sign-in request. Reload the page and try again.'],
    [415, 'The server refused the sign-in request. Reload the page and try again.'],
  ])('HTTP %i', (status, want) => {
    expect(signInMessage(status, 'ignored body')).toBe(want);
  });
  it('shows the server text for a server fault', () => {
    expect(signInMessage(503, 'try again shortly')).toBe('Sign-in failed: try again shortly');
    expect(signInMessage(500)).toBe('Sign-in failed (HTTP 500).');
  });
  it('says the server was not reached when no answer arrived', () => {
    expect(signInMessage(undefined)).toContain('Could not reach the server');
  });
});

describe('signInBlocker', () => {
  it('needs a user name and a password', () => {
    expect(signInBlocker('', 'x')).toBe('Enter the user name.');
    expect(signInBlocker('  ', 'x')).toBe('Enter the user name.');
    expect(signInBlocker('admin', '')).toBe('Enter the password.');
    expect(signInBlocker('admin', 'x')).toBeUndefined();
  });
});

describe('isSignInPath', () => {
  it('matches only the sign-in page', () => {
    expect(isSignInPath('/sign-in')).toBe(true);
    expect(isSignInPath('/sign-in/')).toBe(true);
    expect(isSignInPath('/')).toBe(false);
    expect(isSignInPath('/sign-in/x')).toBe(false);
    expect(isSignInPath('/runs')).toBe(false);
  });
});
