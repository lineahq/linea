import type { ReactNode } from 'react';
import { PageHead, type SubNavItem } from '../../ui/PageHead';

/**
 * CaptureHead is the header every Live capture list page shares: the title,
 * the section navigation and the page's own actions. Collectors, groups and
 * config bisect live under Fleet; stopping a capture is on its
 * detail page.
 */
export function CaptureHead({ actions }: { actions?: ReactNode }) {
  const nav: SubNavItem[] = [
    { to: '/captures', label: 'Captures', exact: true },
    { to: '/fleet', label: 'Collectors in Fleet', exact: true },
  ];
  return <PageHead title="Live capture" nav={nav} actions={actions} />;
}
