import type { Diagnostic } from '../api/types';
import { locateComponent, yamlSyntaxErrors, type Location } from './locate';

export interface Marker {
  severity: 'error' | 'warning' | 'info';
  message: string;
  location: Location;
  /** The diagnostic code, or "yaml" for client-side syntax errors. */
  code: string;
}

const PATH = /(?:receivers|processors|exporters|connectors|extensions|pipelines)::([^:\s"']+)/;
const top: Location = { startLine: 1, startColumn: 1, endLine: 1, endColumn: 2 };

/** diagnosticLocation places a diagnostic on its component's key. */
export function diagnosticLocation(text: string, d: Diagnostic): Location | undefined {
  const id = d.component || PATH.exec(d.message)?.[1];
  return id ? locateComponent(text, id) : undefined;
}

/**
 * configMarkers combines YAML syntax errors, found locally while typing,
 * with the Collector's diagnostics, placed on the component they name.
 */
export function configMarkers(text: string, diagnostics: Diagnostic[]): Marker[] {
  const syntax = yamlSyntaxErrors(text);
  if (syntax.length > 0) {
    return syntax.map((e) => ({ severity: 'error', message: e.message, location: e.location, code: 'yaml' }));
  }
  return diagnostics.map((d) => ({
    severity: d.severity === 'warning' ? 'warning' : 'error',
    message: d.hint ? `${d.message}\n${d.hint}` : d.message,
    location: diagnosticLocation(text, d) ?? top,
    code: d.code,
  }));
}
