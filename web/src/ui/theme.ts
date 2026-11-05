import { useSyncExternalStore } from 'react';

/** ThemePreference is the viewer's choice; 'system' follows the OS. */
export type ThemePreference = 'system' | 'light' | 'dark';
export type Theme = 'light' | 'dark';

const KEY = 'linea.theme.v1';
const query = '(prefers-color-scheme: dark)';
const listeners = new Set<() => void>();

/** parsePreference reads a stored value, falling back to 'system'. */
export function parsePreference(value: string | null | undefined): ThemePreference {
  return value === 'light' || value === 'dark' ? value : 'system';
}

/** resolveTheme turns a preference and the system setting into a theme. */
export function resolveTheme(pref: ThemePreference, systemDark: boolean): Theme {
  return pref === 'system' ? (systemDark ? 'dark' : 'light') : pref;
}

function readPreference(): ThemePreference {
  try {
    return parsePreference(window.localStorage.getItem(KEY));
  } catch {
    return 'system';
  }
}

let preference: ThemePreference = typeof window === 'undefined' ? 'system' : readPreference();

function systemDark(): boolean {
  return typeof window !== 'undefined' && !!window.matchMedia?.(query).matches;
}

function current(): Theme {
  return resolveTheme(preference, systemDark());
}

/** applyTheme sets data-theme on the document root, which the design tokens read. */
export function applyTheme() {
  if (typeof document !== 'undefined') document.documentElement.dataset.theme = current();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  const m = window.matchMedia?.(query);
  const onChange = () => {
    applyTheme();
    listener();
  };
  m?.addEventListener('change', onChange);
  return () => {
    listeners.delete(listener);
    m?.removeEventListener('change', onChange);
  };
}

/** setThemePreference stores the choice (a convenience; storage may be unavailable). */
export function setThemePreference(next: ThemePreference) {
  preference = next;
  try {
    if (next === 'system') window.localStorage.removeItem(KEY);
    else window.localStorage.setItem(KEY, next);
  } catch {
    // Storage is optional; the choice still applies to this page.
  }
  applyTheme();
  listeners.forEach((l) => l());
}

export function useThemePreference(): ThemePreference {
  return useSyncExternalStore(subscribe, () => preference, () => 'system');
}

export function useTheme(): Theme {
  return useSyncExternalStore(subscribe, current, () => 'light');
}
