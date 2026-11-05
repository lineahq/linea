import type { Destination } from './destinations';

/**
 * Permission is a catalog permission (internal/debugger/authz). The session lists
 * those the signed-in principal holds; the server still checks every
 * request, so this only shapes what the interface offers.
 */
export type Permission =
  | 'run.read'
  | 'run.submit'
  | 'run.delete'
  | 'artifact.read'
  | 'artifact.write'
  | 'capture.read'
  | 'capture.start'
  | 'capture.raw'
  | 'fleet.read'
  | 'fleet.config.write'
  | 'fleet.rollout'
  | 'runtime.read'
  | 'runtime.manage'
  | 'runtime.build'
  | 'audit.read'
  | 'token.own'
  | 'token.manage'
  | 'access.manage'
  | 'project.manage'
  | 'library.publish';

/**
 * Held is what the interface knows of the caller's permissions: a list from
 * the session, or undefined while it is unknown (loading, or a server that
 * does not send one), when everything is offered and the server decides.
 */
export type Held = readonly string[] | undefined;

/** allows tells whether held permissions include perm; unknown allows. */
export function allows(held: Held, perm: Permission): boolean {
  return held === undefined || held.includes(perm);
}

/**
 * denial is the reason an action needing perm is disabled, naming the
 * missing permission, or undefined when it is allowed.
 */
export function denial(held: Held, perm: Permission): string | undefined {
  return allows(held, perm) ? undefined : `Needs the ${perm} permission, which your role does not have`;
}

/**
 * DESTINATION_PERMISSION is what a rail destination needs to be of any use.
 * A destination nobody in the role can use is hidden, so the
 * OTTL playground, which runs statements, needs run.submit.
 */
export const DESTINATION_PERMISSION: Record<Destination, Permission> = {
  debugger: 'run.read',
  playground: 'run.submit',
  capture: 'capture.read',
  fleet: 'fleet.read',
  runtimes: 'runtime.read',
  saved: 'artifact.read',
  audit: 'audit.read',
  access: 'access.manage',
};

/** shows tells whether the rail lists a destination. */
export function shows(held: Held, d: Destination): boolean {
  return allows(held, DESTINATION_PERMISSION[d]);
}

/**
 * signInHref is the sign-in page that returns to the current in-app
 * location; the server accepts only same-site paths.
 */
export function signInHref(location: string): string {
  const back = location.startsWith('/') && !location.startsWith('//') ? location : '/';
  return `/auth/login?return_to=${encodeURIComponent(back)}`;
}

/** principalLabel names a principal for the user menu. */
export function principalLabel(p: { id: string; kind: string; name?: string }): string {
  if (p.kind === 'local') return 'Local owner';
  return p.name || p.id;
}
