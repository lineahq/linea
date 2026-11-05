import { Icon, type IconName } from '../ui/Icon';
import type { SaveState } from './headModel';

const SAVE_CHIP: Record<SaveState['tone'], string> = {
  saved: 'od-chip od-chip--pass',
  changed: 'od-chip od-chip--warn',
  unsaved: 'od-chip od-chip--outline',
};

/** SaveIdent names the open configuration or scenario with its save state. */
export function SaveIdent({ icon, what, state }: { icon: IconName; what: string; state: SaveState }) {
  return (
    <span role="group" className="od-ident__meta" style={{ minWidth: 0 }} aria-label={`${what} ${state.name}: ${state.text}`}>
      <Icon name={icon} size={14} />
      <span className="od-ident__name" title={`${what}: ${state.name}`}>{state.name}</span>
      <span className={SAVE_CHIP[state.tone]}>{state.text}</span>
    </span>
  );
}
