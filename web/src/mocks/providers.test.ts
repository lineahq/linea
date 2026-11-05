import { describe, expect, it } from 'vitest';
import type { Diagnostic } from '../api/types';
import { proposalText, proposals, providerCards, providerGaps, providersToAdd } from './providers';

const lc = { name: 'livecapture', package: 'github.com/lineahq/linea/collector/livecapture/livecapturedebug' };
const settings = [{ name: 'livecapture', settings: 'sessions: []\n' }];

describe('providerCards', () => {
  it('names the package of a provider the runtime has', () => {
    expect(providerCards(settings, [lc], 'lab')).toEqual([{ ...settings[0], package: lc.package }]);
  });

  it('says runs fail when the runtime lacks the provider', () => {
    expect(providerCards(settings, [], '')[0].problem).toBe(
      'Runs on the built-in runtime fail: it has no providers. Choose a runtime built with livecapture, or remove these settings.',
    );
    expect(providerCards(settings, [{ name: 'stamp', package: 'x' }], 'lab')[0].problem).toContain('the runtime lab fail: it has stamp.');
  });

  it('claims nothing while the runtime is unknown', () => {
    expect(providerCards(settings, undefined, 'lab')).toEqual([{ ...settings[0], package: undefined }]);
  });
});

describe('providersToAdd', () => {
  it('offers the providers without settings', () => {
    const stamp = { name: 'stamp', package: 'x' };
    expect(providersToAdd([lc, stamp], settings)).toEqual([stamp]);
    expect(providersToAdd(undefined, [])).toEqual([]);
  });
});

describe('proposals', () => {
  it('collects the settings each provider proposes, the first finding winning a key', () => {
    const f = (provider: string, settings: unknown) =>
      ({ code: 'mock_fixture_missing', component: 'extension/x', message: 'm', severity: 'warning', provider_settings: { provider, settings } }) as Diagnostic;
    const got = proposals([f('livecapture', { sessions: [1] }), f('livecapture', { sessions: [2], disabled: false }), f('stamp', ['not', 'a', 'map'])]);
    expect([...got]).toEqual([['livecapture', { sessions: [1], disabled: false }]]);
  });
});

describe('providerGaps', () => {
  const d = (component: string, code = 'mock_fixture_missing') => ({ code, component, message: 'm', severity: 'warning' }) as Diagnostic;
  it('keeps missing-fixture findings of components other than the built-in readers', () => {
    const gaps = providerGaps([d('extension/livecapture'), d('receiver/filelog'), d('extension/livecapture', 'other')], ['receiver/filelog']);
    expect(gaps.map((g) => g.component)).toEqual(['extension/livecapture']);
  });
});

describe('proposalText', () => {
  it('shows a proposal as YAML', () => {
    expect(proposalText({ sessions: [{ id: 's1' }] })).toBe('sessions:\n  - id: s1');
  });
});
