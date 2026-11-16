import { useState, type FormEvent } from 'react';
import { useSearch } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { api, ApiError } from '../api/client';
import { Icon } from './Icon';
import { oidcHref, parseSignInSearch, safeReturnTo, signInBlocker, signInChoices, signInMessage } from './signin';

/**
 * SignInPage is `/sign-in`, where `GET /auth/login` sends a browser when the
 * server has the built-in admin account. It renders without the shell and
 * without a session: the user name and password form for the admin account,
 * and "Sign in with <provider>" when the server also has OIDC.
 */
export function SignInPage() {
  const returnTo = parseSignInSearch(useSearch({ strict: false }) as Record<string, unknown>).return_to;
  const methods = useQuery({ queryKey: ['auth', 'methods'], queryFn: api.signInMethods, retry: false, staleTime: Infinity });
  const choices = methods.data ? signInChoices(methods.data) : undefined;
  return (
    <main className="od-signin">
      <Icon name="logo" size={28} />
      <h1>Sign in to the debugger</h1>
      {methods.isPending && <p role="status">Loading the sign-in methods…</p>}
      {methods.error && (
        <div className="od-callout od-callout--fail od-signin__wide" role="alert">
          <Icon name="circle-x" size={16} />
          <div className="od-callout__main">
            <div className="od-callout__title">The sign-in methods could not be read</div>
            <div className="od-callout__body">{methods.error instanceof Error ? methods.error.message : 'The server did not answer.'}</div>
            <div className="od-callout__actions">
              <button type="button" className="od-btn od-btn--sm" onClick={() => void methods.refetch()}>
                Try again
              </button>
            </div>
          </div>
        </div>
      )}
      {choices && (
        <>
          <p>{choices.password ? 'Sign in with the admin account to continue.' : 'Sign in with your organization’s account to continue.'}</p>
          <div className="od-card od-signin__wide">
            <div className="od-card__body od-signin__form">
              {choices.password && <PasswordForm returnTo={returnTo} />}
              {choices.provider && (
                <a className={choices.password ? 'od-btn' : 'od-btn od-btn--primary'} href={oidcHref(returnTo)}>
                  Sign in with {choices.provider}
                </a>
              )}
              {!choices.password && !choices.provider && <p>This server offers no way to sign in.</p>}
            </div>
          </div>
        </>
      )}
    </main>
  );
}

function PasswordForm({ returnTo }: { returnTo: string | undefined }) {
  const [user, setUser] = useState('admin');
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string>();
  const [pending, setPending] = useState(false);
  const blocker = signInBlocker(user, password);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (blocker || pending) return;
    setPending(true);
    setError(undefined);
    api.signInWithPassword(user.trim(), password).then(
      // The session cookie is set: load the page the user came for, as a new
      // page so the shell reads the session from the start.
      () => window.location.assign(safeReturnTo(returnTo)),
      (e: unknown) => {
        setPending(false);
        setPassword('');
        setError(e instanceof ApiError ? signInMessage(e.status, e.message) : signInMessage(undefined));
      },
    );
  };
  return (
    <form className="od-signin__form" onSubmit={submit}>
      <div className="od-field">
        <label htmlFor="signin-user">User name</label>
        <input id="signin-user" className="od-input" name="user" autoComplete="username" spellCheck={false} value={user} readOnly={pending} onChange={(e) => setUser(e.target.value)} />
      </div>
      <div className="od-field">
        <label htmlFor="signin-password">Password</label>
        <input
          id="signin-password"
          className="od-input"
          name="password"
          type="password"
          autoComplete="current-password"
          autoFocus
          value={password}
          readOnly={pending}
          aria-invalid={error ? true : undefined}
          onChange={(e) => setPassword(e.target.value)}
        />
      </div>
      <div role="alert">
        {error && (
          <div className="od-field__error">
            <Icon name="circle-x" size={12} />
            {error}
          </div>
        )}
      </div>
      <button type="submit" className="od-btn od-btn--primary" disabled={pending || blocker !== undefined} title={blocker}>
        {pending ? 'Signing in…' : 'Sign in'}
      </button>
    </form>
  );
}
