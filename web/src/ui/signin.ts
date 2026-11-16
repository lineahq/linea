/**
 * Pure logic of the sign-in page (`/sign-in`, ui/SignInPage.tsx) for the team
 * server's /auth endpoints (internal/debugger/platform/signin.go).
 */

/** SignInMethods is GET /auth/methods: what the server offers to sign in with. */
export interface SignInMethods {
  /** The built-in admin account's password form. */
  password: boolean;
  /** Sign-in with the identity provider; absent without one. */
  oidc?: { issuer: string };
}

/**
 * safeReturnTo is where a sign-in goes back to: a path on this server, else
 * `/`. It accepts what the server's own check accepts: a value that starts
 * with `/` but not `//` or `/\`, which a browser would read as another host.
 */
export function safeReturnTo(value: string | null | undefined): string {
  if (!value || !value.startsWith('/') || value.startsWith('//') || value.startsWith('/\\')) return '/';
  return value;
}

/** parseSignInSearch reads `return_to` from the page's search, checked by safeReturnTo. */
export function parseSignInSearch(s: Record<string, unknown>): { return_to?: string } {
  return { return_to: typeof s.return_to === 'string' && s.return_to ? safeReturnTo(s.return_to) : undefined };
}

/** oidcHref starts the provider's sign-in; it is a full navigation, not a fetch. */
export function oidcHref(returnTo: string | undefined): string {
  return `/auth/login?method=oidc&return_to=${encodeURIComponent(safeReturnTo(returnTo))}`;
}

/** providerName names the identity provider on its button: the issuer's host. */
export function providerName(issuer: string): string {
  try {
    return new URL(issuer).host || issuer;
  } catch {
    return issuer;
  }
}

/** Choices is what the sign-in page shows for the server's methods. */
export interface Choices {
  /** Show the user name and password form. */
  password: boolean;
  /** The provider's name for the secondary action, when OIDC is offered. */
  provider?: string;
}

/** signInChoices maps /auth/methods to the page's form and provider button. */
export function signInChoices(methods: SignInMethods): Choices {
  return { password: methods.password, provider: methods.oidc ? providerName(methods.oidc.issuer) : undefined };
}

/**
 * signInMessage words a failed password sign-in for the inline alert. status
 * is the HTTP status of the answer, undefined when none arrived; message is
 * the server's plain-text body, shown only for a server fault.
 */
export function signInMessage(status: number | undefined, message?: string): string {
  switch (status) {
    case undefined:
      return 'Could not reach the server. Check the connection and try again.';
    case 401:
      return 'Wrong user name or password.';
    case 429:
      return 'Too many failed sign-ins. Wait a minute, then try again.';
    case 404:
      return 'This server has no password sign-in.';
    case 400:
    case 403:
    case 415:
      return 'The server refused the sign-in request. Reload the page and try again.';
    default:
      return message ? `Sign-in failed: ${message}` : `Sign-in failed (HTTP ${status}).`;
  }
}

/** signInBlocker is why the form cannot be submitted yet, or undefined when it can. */
export function signInBlocker(user: string, password: string): string | undefined {
  if (!user.trim()) return 'Enter the user name.';
  if (!password) return 'Enter the password.';
  return undefined;
}

/** isSignInPath tells whether a pathname is the sign-in page, which renders without the shell. */
export function isSignInPath(pathname: string): boolean {
  return pathname.replace(/\/+$/, '') === '/sign-in';
}
