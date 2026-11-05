import { isMap, isScalar, isSeq, parseDocument } from 'yaml';
import { quickDoc } from '../input/quickInput';
import { addInput, editScenario, moveItem, setField } from '../scenario/scenarioDoc';
import type { WorkspaceState } from './store';

/**
 * LegacyQuick is the separate quick-run draft that workspaces kept before
 * the quick run became a document with one input.
 */
interface LegacyQuick {
  inputJSON?: string;
  inputReceiver?: string;
  inputClientAddress?: string;
  quickSetupYAML?: string;
  runMode?: 'quick' | 'scenario';
  inputName?: string;
}

const LEGACY: (keyof LegacyQuick)[] = ['inputJSON', 'inputReceiver', 'inputClientAddress', 'quickSetupYAML', 'runMode', 'inputName'];

/**
 * migrate folds a stored legacy quick-run draft into the document. The mode
 * the user was in wins: a quick run with an input becomes the document, as
 * one input with its mocks, or joins an existing scenario draft; otherwise
 * the scenario stays. Nothing else is changed for a workspace without legacy
 * fields; a stored scenario directory is dropped.
 */
export function migrate(stored: WorkspaceState & LegacyQuick & { scenarioDir?: string }): WorkspaceState {
  // A scenario directory was sent for the server to read; it reads none.
  if ('scenarioDir' in stored) {
    const { scenarioDir: _, ...kept } = stored;
    stored = kept;
  }
  if (!LEGACY.some((k) => k in stored)) return stored;
  const { inputJSON = '', inputReceiver = '', inputClientAddress = '', quickSetupYAML = '', runMode, ...rest } = stored;
  delete (rest as LegacyQuick).inputName;
  const out: WorkspaceState = { ...rest };
  if (out.baselines) {
    const { quick: _, ...kept } = out.baselines as Record<string, string>;
    out.baselines = kept;
  }
  if (out.saved) {
    const { fixture: _, ...kept } = out.saved;
    out.saved = kept;
  }
  const quick = (runMode ?? (stored.scenarioYAML.trim() ? 'scenario' : 'quick')) === 'quick';
  if (!quick || !inputJSON.trim()) return out;
  if (!stored.scenarioYAML.trim()) {
    const doc = quickDoc({ fixture: inputJSON, receiver: inputReceiver, clientAddress: inputClientAddress, setup: quickSetupYAML });
    if (!('error' in doc)) {
      Object.assign(out, { scenarioYAML: doc.yaml, scenarioPath: '' });
      out.baselines = { ...out.baselines, scenario: undefined };
      if (out.saved) delete out.saved.scenario;
    }
    return out;
  }
  // Both drafts hold work: the quick input joins the scenario as its first
  // input, and its mocks fill only what the scenario does not declare, so
  // nothing the user wrote is lost.
  const merged = editScenario(stored.scenarioYAML, (doc) => {
    const setup = parseDocument(quickSetupYAML);
    if (!setup.errors.length && isMap(setup.contents)) {
      for (const p of setup.contents.items) {
        const k = isScalar(p.key) ? p.key.value : p.key;
        if (typeof k === 'string' && k !== 'version' && !doc.has(k)) doc.set(k, p.value);
      }
    }
    const inputs = doc.get('inputs');
    const at = isSeq(inputs) ? inputs.items.length : 0;
    addInput({ receiver: inputReceiver, data: inputJSON, raw: true })(doc);
    if (inputClientAddress.trim()) setField(['inputs', at, 'client_address'], inputClientAddress.trim())(doc);
    if (at > 0) moveItem('inputs', at, 0)(doc);
  });
  if (!('error' in merged)) out.scenarioYAML = merged.yaml;
  return out;
}
