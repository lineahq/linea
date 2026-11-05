import { Icon } from '../ui/Icon';
import { chipClass, type Fact } from './runFacts';

/** FactChip shows one independent fact about a run; `label` prefixes its tooltip. */
export function FactChip({ fact, label }: { fact: Fact; label: string }) {
  return (
    <span className={chipClass(fact.tone)} title={fact.title ? `${label}: ${fact.title}` : label}>
      <Icon name={fact.icon} size={12} />
      {fact.text}
    </span>
  );
}

/** FactPlaceholder stands in for a fact that needs the run's report, which is still loading. */
export function FactPlaceholder() {
  return <span className="od-skel" style={{ width: 72, height: 18 }} aria-hidden="true" />;
}
