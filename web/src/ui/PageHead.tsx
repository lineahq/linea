import type { ReactNode } from 'react';
import { Link } from '@tanstack/react-router';
import { Icon } from './Icon';

export interface SubNavItem {
  to: string;
  label: string;
  /** A number, or text such as "25+" when the list may continue on another page. */
  count?: number | string;
  /** Also current on deeper paths (default true except for the section root). */
  exact?: boolean;
}

/**
 * PageHead is the header of a destination page: its title, the section
 * navigation and page-level actions on the right.
 */
export function PageHead({ title, nav, actions }: { title: string; nav?: SubNavItem[]; actions?: ReactNode }) {
  return (
    <header className="od-pagehead">
      <h1 className="od-pagehead__title">{title}</h1>
      {nav && (
        <nav className="od-subnav" aria-label="Section">
          {nav.map((item) => (
            <Link
              key={item.to}
              to={item.to}
              activeOptions={{ exact: item.exact ?? false, includeSearch: false }}
              activeProps={{ 'aria-current': 'page' }}
            >
              {item.label}
              {item.count !== undefined && <span className="od-tab__count">{item.count}</span>}
            </Link>
          ))}
        </nav>
      )}
      <span style={{ flex: 1 }} />
      {actions}
    </header>
  );
}

/** Crumbs is the breadcrumb trail of a detail page; the last item is the current object. */
export function Crumbs({ items }: { items: { label: ReactNode; to?: string }[] }) {
  return (
    <nav className="od-crumbs" aria-label="Breadcrumb">
      {items.map((item, i) => (
        <span key={i} style={{ display: 'contents' }}>
          {i > 0 && <Icon name="chevron-right" size={14} />}
          {item.to ? <Link to={item.to}>{item.label}</Link> : <span>{item.label}</span>}
        </span>
      ))}
    </nav>
  );
}

/** Pending marks a route whose screen is not built yet. */
export function Pending({ title }: { title: string }) {
  return (
    <>
      <PageHead title={title} />
      <div className="od-page">
        <div className="od-empty">
          <div className="od-empty__title">{title}</div>
          <div className="od-empty__body">This screen is being rebuilt.</div>
        </div>
      </div>
    </>
  );
}
