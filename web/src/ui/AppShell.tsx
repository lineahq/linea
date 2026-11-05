import { useEffect, useRef, useState } from 'react';
import { Link, Outlet, useRouterState } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { api, ApiError } from '../api/client';
import type { Session } from '../api/types';
import { Icon, type IconName } from './Icon';
import { applyTheme, setThemePreference, useThemePreference, type ThemePreference } from './theme';
import { activeBuildCount, activeCaptureCount, destinationOf, type Destination } from './destinations';
import { buildsQuery } from '../runtimes/pages/queries';
import { finished } from '../runtimes/runtimes';
import { principalLabel, shows, signInHref } from './access';
import { sessionQuery } from './session';
import { ProjectMenu } from './ProjectMenu';

const DESTINATIONS: { id: Destination; to: string; label: [string, string?]; icon: IconName }[] = [
  { id: 'debugger', to: '/', label: ['Debugger'], icon: 'nav-debugger' },
  { id: 'playground', to: '/playground', label: ['OTTL', 'playground'], icon: 'flask-conical' },
  { id: 'capture', to: '/captures', label: ['Live', 'capture'], icon: 'nav-capture' },
  { id: 'fleet', to: '/fleet', label: ['Fleet'], icon: 'nav-fleet' },
  { id: 'runtimes', to: '/runtimes', label: ['Runtimes'], icon: 'nav-runtimes' },
  { id: 'saved', to: '/saved', label: ['Saved', 'work'], icon: 'nav-saved' },
  { id: 'audit', to: '/audit', label: ['Audit', 'log'], icon: 'nav-audit' },
  { id: 'access', to: '/access', label: ['Access'], icon: 'nav-access' },
];

/**
 * AppShell is the root layout: the destination rail and the page beside it.
 * Each page renders its own header (a page head or the debugger's workbench
 * head). On a team server nobody signed in gets the sign-in page; the rail
 * lists only the destinations the caller's permissions make usable.
 */
export function AppShell() {
  useEffect(applyTheme, []);
  const session = useQuery(sessionQuery);
  if (session.error instanceof ApiError && session.error.status === 401) return <SignIn />;
  // Pages wait for the session: it carries the CSRF token their first
  // request that changes something needs.
  if (session.isPending) return <div className="od-app od-root" />;
  return <Shell session={session.data} />;
}

function Shell({ session }: { session?: Session }) {
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const current = destinationOf(pathname);
  const held = session?.permissions;
  // Badges count captures still collecting and builds still running; a
  // failed request simply shows no badge.
  const captures = useQuery({
    queryKey: ['captures', 'rail'],
    queryFn: () => api.captures(),
    refetchInterval: 10_000,
    refetchIntervalInBackground: false,
    retry: false,
  });
  const builds = useQuery(buildsQuery);
  const badges: Partial<Record<Destination, [number, string, string]>> = {
    capture: [activeCaptureCount(captures.data?.items), 'active capture', 'active captures'],
    runtimes: [activeBuildCount(builds.data?.builds, finished), 'build in progress', 'builds in progress'],
  };
  return (
    <div className="od-app od-root">
      <div className="od-shell">
        <nav className="od-rail" aria-label="Destinations">
          <span className="od-rail__logo" aria-hidden="true">
            <Icon name="logo" size={18} />
          </span>
          {DESTINATIONS.filter((d) => shows(held, d.id)).map((d) => {
            const [count, one, many] = badges[d.id] ?? [0, '', ''];
            return (
            <Link key={d.id} to={d.to} className="od-rail__item" aria-current={current === d.id ? 'page' : undefined}>
              {count > 0 && (
                <span className="od-rail__badge" title={`${count} ${count === 1 ? one : many}`}>
                  {count}
                </span>
              )}
              <Icon name={d.icon} size={20} />
              <span>
                {d.label[0]}
                {d.label[1] && (
                  <>
                    <br />
                    {d.label[1]}
                  </>
                )}
              </span>
            </Link>
            );
          })}
          <span className="od-rail__spacer" />
          {held && held.length > 0 && <ProjectMenu />}
          <SettingsMenu session={session} />
        </nav>
        <main className="od-main">
          {held && held.length === 0 ? <NoAccess session={session!} /> : <Outlet />}
        </main>
      </div>
    </div>
  );
}

const THEME_CHOICES: [ThemePreference, string][] = [
  ['system', 'Match system'],
  ['light', 'Light'],
  ['dark', 'Dark'],
];

/** SignIn is the whole page while nobody is signed in to a team server. */
function SignIn() {
  const href = useRouterState({ select: (s) => s.location.href });
  return (
    <div className="od-app od-root">
      <main className="od-signin">
        <Icon name="logo" size={28} />
        <h1>Sign in to the debugger</h1>
        <p>This is a team server. Sign in with your organization's account to continue.</p>
        <a className="od-btn od-btn--primary" href={signInHref(href)}>
          Sign in
        </a>
      </main>
    </div>
  );
}

/** NoAccess replaces the page for a principal no role binding names. */
function NoAccess({ session }: { session: Session }) {
  return (
    <div className="od-signin">
      <h1>No access yet</h1>
      <p>
        You are signed in as {principalLabel(session.principal)}, but no role on this server includes you or your groups. Ask an administrator to add a
        binding for you.
      </p>
    </div>
  );
}

function SettingsMenu({ session }: { session?: Session }) {
  const [open, setOpen] = useState(false);
  const [signOut, setSignOut] = useState<{ pending: boolean; error?: string }>({ pending: false });
  const pref = useThemePreference();
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent | KeyboardEvent) => {
      if (e instanceof KeyboardEvent ? e.key === 'Escape' : !ref.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', close);
    document.addEventListener('keydown', close);
    return () => {
      document.removeEventListener('mousedown', close);
      document.removeEventListener('keydown', close);
    };
  }, [open]);
  return (
    <div ref={ref} className="od-rail__settings">
      <button type="button" className="od-rail__item" aria-expanded={open} aria-haspopup="menu" onClick={() => setOpen(!open)}>
        <Icon name="nav-settings" size={20} />
        <span>Settings</span>
      </button>
      {open && (
        <div className="od-menu od-rail__menu" role="menu" aria-label="Settings">
          {session && (
            <>
              <div className="od-menu__label">Signed in as</div>
              <div className="od-rail__who" title={session.principal.id}>
                {principalLabel(session.principal)}
              </div>
              {session.csrf_token && (
                <button
                  type="button"
                  role="menuitem"
                  className="od-menu__item"
                  disabled={signOut.pending}
                  onClick={() => {
                    setSignOut({ pending: true });
                    api.signOut().then(
                      () => window.location.assign('/'),
                      (e: Error) => setSignOut({ pending: false, error: e.message }),
                    );
                  }}
                >
                  <span style={{ width: 14 }} />
                  {signOut.pending ? 'Signing out…' : 'Sign out'}
                </button>
              )}
              {signOut.error && (
                <div className="od-rail__who" role="alert">
                  Could not sign out: {signOut.error}
                </div>
              )}
            </>
          )}
          <div className="od-menu__label">Theme</div>
          {THEME_CHOICES.map(([value, label]) => (
            <button
              key={value}
              type="button"
              role="menuitemradio"
              aria-checked={pref === value}
              className="od-menu__item"
              onClick={() => {
                setThemePreference(value);
                setOpen(false);
              }}
            >
              {pref === value ? <Icon name="check" size={14} /> : <span style={{ width: 14 }} />}
              {label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
