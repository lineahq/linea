import type { ReactNode } from 'react';
import { Link, useRouterState } from '@tanstack/react-router';
import { savedSection } from './format';
import './saved.css';

/**
 * SavedHead is the header of the Saved work destination: sources (the
 * collection and each source's detail), suites (their list and results) and
 * the library shared with the current project.
 */
export function SavedHead({ actions }: { actions?: ReactNode }) {
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const section = savedSection(pathname);
  return (
    <header className="od-pagehead">
      <h1 className="od-pagehead__title">Saved work</h1>
      <nav className="od-subnav" aria-label="Section">
        {/* A source's detail page is part of Sources, though its path is deeper. */}
        <Link to="/saved" activeOptions={{ exact: true }} aria-current={section === 'sources' ? 'page' : undefined}>
          Sources
        </Link>
        <Link to="/saved/suites">Suites</Link>
        <Link to="/saved/library">Library</Link>
      </nav>
      <span style={{ flex: 1 }} />
      {actions}
    </header>
  );
}
