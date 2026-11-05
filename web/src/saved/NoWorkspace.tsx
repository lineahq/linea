import { Link } from '@tanstack/react-router';
import { Icon } from '../ui/Icon';

/**
 * NoWorkspace is the empty state of a server that cannot list saved work (the
 * artifact endpoints answer 503). A team server keeps saved work in
 * PostgreSQL and object storage.
 */
export function NoWorkspace() {
  return (
    <div className="od-card">
      <div className="od-empty">
        <div className="od-empty__glyph"><Icon name="library" size={18} /></div>
        <div className="od-empty__title">Saved work is unavailable</div>
        <div className="od-empty__body">
          This server cannot list saved sources, revisions and tests right now. The team server keeps them in PostgreSQL and object storage, so check that both are reachable. Drafts in the debugger work without saved work.
        </div>
        <div className="od-empty__actions"><Link to="/" className="od-btn">Open the debugger</Link></div>
      </div>
    </div>
  );
}
