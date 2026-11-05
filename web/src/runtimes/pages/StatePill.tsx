import { Icon } from '../../ui/Icon';
import type { StateChip } from './runtimeList';

/** StatePill is a state chip: its icon, colour and label. */
export function StatePill({ chip }: { chip: StateChip }) {
  return (
    <span className={`od-chip od-chip--${chip.tone}`}>
      <Icon name={chip.icon} size={12} />{chip.label}
    </span>
  );
}
