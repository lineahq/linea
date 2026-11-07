import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import { api } from '../api/client';
import { Icon } from '../ui/Icon';
import { useDenial } from '../ui/session';
import { verificationView, type Provenance, type VerificationView } from './recommendations';
import { runLabel } from './runFacts';

const TONE_ICON = { pass: 'circle-check', warn: 'triangle-alert', fail: 'circle-x', neutral: 'info' } as const;
const TONE_CALLOUT = { pass: 'info', warn: 'warn', fail: 'fail', neutral: 'info' } as const;

/**
 * VerifyRecommendation runs one recommendation's processor order on the run's
 * inputs (on demand: one more run) and shows what the server's comparison
 * found, with the provenance it earns and a link to compare the two runs.
 * The result is not stored; reopening the run shows the rule's own
 * confidence again. The user's configuration and draft are never changed.
 */
export function VerifyRecommendation({ runId, index, provenance }: { runId: string; index: number; provenance: Provenance }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const denied = useDenial('run.submit');
  const verify = useMutation({
    mutationFn: () => api.verifyRecommendation(runId, index),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ['runs'] }),
  });
  const v: VerificationView | undefined = verify.data && verificationView(verify.data, runLabel(runId));
  const shown = v?.provenance ?? provenance;
  return (
    <>
      {v && (
        <div className={`od-callout od-callout--${TONE_CALLOUT[v.tone]}`} role="status" aria-label="Verification">
          <Icon name={TONE_ICON[v.tone]} size={16} />
          <div className="od-callout__main">
            <div className="od-callout__body">{v.text}</div>
            {v.assertions.map((a, i) => <div key={`a${i}`} className="od-callout__body">{a}</div>)}
            {v.reasons.map((r, i) => <div key={i} className="od-callout__body od-muted">{r}</div>)}
            <div style={{ display: 'flex', gap: 6, marginTop: 4 }}>
              <button type="button" className="od-btn od-btn--link od-btn--sm"
                onClick={() => void navigate({ to: '/', search: { run: v.candidate, compare: runId } })}>
                <span>Compare with candidate run {runLabel(v.candidate)}</span><Icon name="arrow-right" size={14} />
              </button>
            </div>
          </div>
        </div>
      )}
      {verify.error && <div className="lim-warning" role="alert" style={{ fontSize: 12 }}>⚠ Not verified: {verify.error.message}</div>}
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
        <span className={`od-prov od-prov--${shown.tone}`}><Icon name="eye" size={12} />{shown.text}</span>
        {!v && (
          <button type="button" className="od-btn od-btn--sm" disabled={verify.isPending || !!denied}
            title={denied ?? "Run this change on this run's input, without changing your configuration, and compare the output (one more run)"}
            onClick={() => verify.mutate()}>
            <Icon name="play" size={14} /><span>{verify.isPending ? 'Verifying…' : 'Verify on this input'}</span>
          </button>
        )}
      </div>
    </>
  );
}
