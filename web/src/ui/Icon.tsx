import type { ReactNode } from 'react';

// Icon set for the web client, extracted from the product-ui design artboards
// (docs/design/product-ui). Most are Lucide icons on a 24x24 grid; the node,
// signal, mode and diff marks are custom 16x16 or 12x12 glyphs. Names are the
// Lucide names where one applies, otherwise a descriptive kebab-case name.

type IconData = {
  /** SVG viewBox; defaults to the 24x24 Lucide grid. */
  viewBox?: string;
  /** Default stroke width; defaults to 1.75. */
  strokeWidth?: number;
  body: ReactNode;
};

const DEFAULT_VIEW_BOX = '0 0 24 24';
const DEFAULT_STROKE_WIDTH = 1.75;

const BASE = {
  connector: {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <>
        <path d="M1.75 8h4" />
        <path d="M5.75 8c2 0 2-4.25 4.25-4.25h4" />
        <path d="M5.75 8c2 0 2 4.25 4.25 4.25h4" />
      </>
    ),
  },
  workflow: {
    body: (
      <>
        <rect width="8" height="8" x="3" y="3" rx="2" />
        <path d="M7 11v4a2 2 0 0 0 2 2h4" />
        <rect width="8" height="8" x="13" y="13" rx="2" />
      </>
    ),
  },
  radio: {
    body: (
      <>
        <path d="M16.247 7.761a6 6 0 0 1 0 8.478" />
        <path d="M19.075 4.933a10 10 0 0 1 0 14.134" />
        <path d="M4.925 19.067a10 10 0 0 1 0-14.134" />
        <path d="M7.753 16.239a6 6 0 0 1 0-8.478" />
        <circle cx="12" cy="12" r="2" />
      </>
    ),
  },
  package: {
    body: (
      <>
        <path d="M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z" />
        <path d="m3.3 7 8.7 5 8.7-5" />
        <path d="M12 22V12" />
      </>
    ),
  },
  library: {
    body: (
      <>
        <path d="m16 6 4 14" />
        <path d="M12 6v14" />
        <path d="M8 8v12" />
        <path d="M4 4v16" />
      </>
    ),
  },
  'life-buoy': {
    body: (
      <>
        <circle cx="12" cy="12" r="10" />
        <path d="m4.93 4.93 4.24 4.24" />
        <path d="m14.83 9.17 4.24-4.24" />
        <path d="m14.83 14.83 4.24 4.24" />
        <path d="m9.17 14.83-4.24 4.24" />
        <circle cx="12" cy="12" r="4" />
      </>
    ),
  },
  settings: {
    body: (
      <>
        <path d="M9.671 4.136a2.34 2.34 0 0 1 4.659 0 2.34 2.34 0 0 0 3.319 1.915 2.34 2.34 0 0 1 2.33 4.033 2.34 2.34 0 0 0 0 3.831 2.34 2.34 0 0 1-2.33 4.033 2.34 2.34 0 0 0-3.319 1.915 2.34 2.34 0 0 1-4.659 0 2.34 2.34 0 0 0-3.32-1.915 2.34 2.34 0 0 1-2.33-4.033 2.34 2.34 0 0 0 0-3.831A2.34 2.34 0 0 1 6.35 6.051a2.34 2.34 0 0 0 3.319-1.915" />
        <circle cx="12" cy="12" r="3" />
      </>
    ),
  },
  'octagon-alert': {
    body: (
      <>
        <path d="M12 16h.01" />
        <path d="M12 8v4" />
        <path d="M15.312 2a2 2 0 0 1 1.414.586l4.688 4.688A2 2 0 0 1 22 8.688v6.624a2 2 0 0 1-.586 1.414l-4.688 4.688a2 2 0 0 1-1.414.586H8.688a2 2 0 0 1-1.414-.586l-4.688-4.688A2 2 0 0 1 2 15.312V8.688a2 2 0 0 1 .586-1.414l4.688-4.688A2 2 0 0 1 8.688 2z" />
      </>
    ),
  },
  copy: {
    body: (
      <>
        <rect width="14" height="14" x="8" y="8" rx="2" ry="2" />
        <path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2" />
      </>
    ),
  },
  info: {
    body: (
      <>
        <circle cx="12" cy="12" r="10" />
        <path d="M12 16v-4" />
        <path d="M12 8h.01" />
      </>
    ),
  },
  plus: {
    body: (
      <>
        <path d="M5 12h14" />
        <path d="M12 5v14" />
      </>
    ),
  },
  'triangle-alert': {
    body: (
      <>
        <path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3" />
        <path d="M12 9v4" />
        <path d="M12 17h.01" />
      </>
    ),
  },
  'chevron-right': {
    body: (
      <path d="m9 18 6-6-6-6" />
    ),
  },
  'circle-pause': {
    body: (
      <>
        <circle cx="12" cy="12" r="10" />
        <line x1="10" x2="10" y1="15" y2="9" />
        <line x1="14" x2="14" y1="15" y2="9" />
      </>
    ),
  },
  square: {
    body: (
      <rect width="18" height="18" x="3" y="3" rx="2" />
    ),
  },
  'trash-2': {
    body: (
      <>
        <path d="M10 11v6" />
        <path d="M14 11v6" />
        <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6" />
        <path d="M3 6h18" />
        <path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
      </>
    ),
  },
  search: {
    body: (
      <>
        <path d="m21 21-4.34-4.34" />
        <circle cx="11" cy="11" r="8" />
      </>
    ),
  },
  save: {
    body: (
      <>
        <path d="M15.2 3a2 2 0 0 1 1.4.6l3.8 3.8a2 2 0 0 1 .6 1.4V19a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z" />
        <path d="M17 21v-7a1 1 0 0 0-1-1H8a1 1 0 0 0-1 1v7" />
        <path d="M7 3v4a1 1 0 0 0 1 1h7" />
      </>
    ),
  },
  'file-plus': {
    body: (
      <>
        <path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z" />
        <path d="M14 2v5a1 1 0 0 0 1 1h5" />
        <path d="M9 15h6" />
        <path d="M12 18v-6" />
      </>
    ),
  },
  check: {
    body: (
      <path d="M20 6 9 17l-5-5" />
    ),
  },
  'circle-stop': {
    body: (
      <>
        <circle cx="12" cy="12" r="10" />
        <rect x="9" y="9" width="6" height="6" rx="1" />
      </>
    ),
  },
  'loader-circle': {
    body: (
      <path d="M21 12a9 9 0 1 1-6.219-8.56" />
    ),
  },
  x: {
    body: (
      <>
        <path d="M18 6 6 18" />
        <path d="m6 6 12 12" />
      </>
    ),
  },
  clock: {
    body: (
      <>
        <circle cx="12" cy="12" r="10" />
        <path d="M12 6v6l4 2" />
      </>
    ),
  },
  'node-receiver': {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <>
        <path d="M1.75 8H9.5" />
        <path d="M6.75 5.25 9.5 8l-2.75 2.75" />
        <path d="M12.75 2.75v10.5" />
      </>
    ),
  },
  'chevron-down': {
    body: (
      <path d="m6 9 6 6 6-6" />
    ),
  },
  'circle-check': {
    body: (
      <>
        <circle cx="12" cy="12" r="10" />
        <path d="m16 9-5.5 5.5L8 12" />
      </>
    ),
  },
  pencil: {
    body: (
      <>
        <path d="M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z" />
        <path d="m15 5 4 4" />
      </>
    ),
  },
  'circle-minus': {
    body: (
      <>
        <circle cx="12" cy="12" r="10" />
        <path d="M8 12h8" />
      </>
    ),
  },
  'circle-x': {
    body: (
      <>
        <circle cx="12" cy="12" r="10" />
        <path d="m15 9-6 6" />
        <path d="m9 9 6 6" />
      </>
    ),
  },
  'signal-logs': {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <>
        <path d="M2.5 4h11" />
        <path d="M2.5 8h11" />
        <path d="M2.5 12h7" />
      </>
    ),
  },
  'signal-traces': {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <>
        <path d="M2 3.5h6" />
        <path d="M5 8h7" />
        <path d="M8 12.5h6" />
      </>
    ),
  },
  'node-processor': {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <>
        <path d="M2 5h12" />
        <path d="M2 11h12" />
        <circle cx="6" cy="5" r="1.75" style={{ fill: 'var(--bg-surface)' }} />
        <circle cx="10.5" cy="11" r="1.75" style={{ fill: 'var(--bg-surface)' }} />
      </>
    ),
  },
  'node-exporter': {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <>
        <path d="M3.25 2.75v10.5" />
        <path d="M6.5 8h7.75" />
        <path d="M11.5 5.25 14.25 8 11.5 10.75" />
      </>
    ),
  },
  download: {
    body: (
      <>
        <path d="M12 15V3" />
        <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
        <path d="m7 10 5 5 5-5" />
      </>
    ),
  },
  'git-compare': {
    body: (
      <>
        <circle cx="18" cy="18" r="3" />
        <circle cx="6" cy="6" r="3" />
        <path d="M13 6h3a2 2 0 0 1 2 2v7" />
        <path d="M11 18H8a2 2 0 0 1-2-2V9" />
      </>
    ),
  },
  'run-marker': {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <>
        <circle cx="8" cy="8" r="5.25" />
        <circle cx="8" cy="8" r="2.25" fill="currentColor" stroke="none" />
      </>
    ),
  },
  'arrow-right': {
    body: (
      <>
        <path d="M5 12h14" />
        <path d="m12 5 7 7-7 7" />
      </>
    ),
  },
  ban: {
    body: (
      <>
        <circle cx="12" cy="12" r="10" />
        <path d="M4.929 4.929 19.07 19.071" />
      </>
    ),
  },
  play: {
    body: (
      <path d="M5 5a2 2 0 0 1 3.008-1.728l11.997 6.998a2 2 0 0 1 .003 3.458l-12 7A2 2 0 0 1 5 19z" />
    ),
  },
  'flask-conical': {
    body: (
      <>
        <path d="M14 2v6a2 2 0 0 0 .245.96l5.51 10.08A2 2 0 0 1 18 22H6a2 2 0 0 1-1.755-2.96l5.51-10.08A2 2 0 0 0 10 8V2" />
        <path d="M6.453 15h11.094" />
        <path d="M8.5 2h7" />
      </>
    ),
  },
  'file-diff': {
    body: (
      <>
        <path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z" />
        <path d="M9 10h6" />
        <path d="M12 13V7" />
        <path d="M9 17h6" />
      </>
    ),
  },
  'diff-changed': {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <path d="M2.5 9.5c1.5-3 3-3 5.5-1.5s4 1.5 5.5-1.5" />
    ),
  },
  history: {
    body: (
      <>
        <path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8" />
        <path d="M3 3v5h5" />
        <path d="M12 7v5l4 2" />
      </>
    ),
  },
  ellipsis: {
    body: (
      <>
        <circle cx="12" cy="12" r="1" />
        <circle cx="19" cy="12" r="1" />
        <circle cx="5" cy="12" r="1" />
      </>
    ),
  },
  eye: {
    body: (
      <>
        <path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0" />
        <circle cx="12" cy="12" r="3" />
      </>
    ),
  },
  crosshair: {
    body: (
      <>
        <line x1="2" x2="5" y1="12" y2="12" />
        <line x1="19" x2="22" y1="12" y2="12" />
        <line x1="12" x2="12" y1="2" y2="5" />
        <line x1="12" x2="12" y1="19" y2="22" />
        <circle cx="12" cy="12" r="7" />
        <circle cx="12" cy="12" r="3" />
      </>
    ),
  },
  'circle-help': {
    body: (
      <>
        <circle cx="12" cy="12" r="10" />
        <path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3" />
        <path d="M12 17h.01" />
      </>
    ),
  },
  'mode-mocked': {
    viewBox: '0 0 12 12',
    body: (
      <>
        <circle cx="6" cy="6" r="4.25" fill="none" stroke="currentColor" strokeWidth="1.5" />
        <path d="M6 1.75a4.25 4.25 0 0 0 0 8.5z" fill="currentColor" stroke="none" />
      </>
    ),
  },
  'mode-captured': {
    viewBox: '0 0 12 12',
    body: (
      <>
        <circle cx="6" cy="6" r="4.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
        <circle cx="6" cy="6" r="1.9" fill="currentColor" stroke="none" />
      </>
    ),
  },
  'mode-simulated': {
    viewBox: '0 0 12 12',
    body: (
      <circle cx="6" cy="6" r="4.25" fill="none" stroke="currentColor" strokeWidth="1.5" strokeDasharray="2.2 1.6" />
    ),
  },
  extension: {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <>
        <rect x="3" y="4.5" width="10" height="8.75" rx="1.5" />
        <path d="M6 4.5V2.25" />
        <path d="M10 4.5V2.25" />
      </>
    ),
  },
  'mode-real': {
    viewBox: '0 0 12 12',
    body: (
      <circle cx="6" cy="6" r="4.5" fill="currentColor" stroke="none" />
    ),
  },
  'zoom-out': {
    body: (
      <>
        <circle cx="11" cy="11" r="8" />
        <line x1="21" x2="16.65" y1="21" y2="16.65" />
        <line x1="8" x2="14" y1="11" y2="11" />
      </>
    ),
  },
  'zoom-in': {
    body: (
      <>
        <circle cx="11" cy="11" r="8" />
        <line x1="21" x2="16.65" y1="21" y2="16.65" />
        <line x1="11" x2="11" y1="8" y2="14" />
        <line x1="8" x2="14" y1="11" y2="11" />
      </>
    ),
  },
  scan: {
    body: (
      <>
        <path d="M8 3H5a2 2 0 0 0-2 2v3" />
        <path d="M21 8V5a2 2 0 0 0-2-2h-3" />
        <path d="M3 16v3a2 2 0 0 0 2 2h3" />
        <path d="M16 21h3a2 2 0 0 0 2-2v-3" />
      </>
    ),
  },
  timer: {
    body: (
      <>
        <line x1="10" x2="14" y1="2" y2="2" />
        <line x1="12" x2="15" y1="14" y2="11" />
        <circle cx="12" cy="14" r="8" />
      </>
    ),
  },
  'circle-dashed': {
    body: (
      <>
        <path d="M10.1 2.182a10 10 0 0 1 3.8 0" />
        <path d="M13.9 21.818a10 10 0 0 1-3.8 0" />
        <path d="M17.609 3.721a10 10 0 0 1 2.69 2.7" />
        <path d="M2.182 13.9a10 10 0 0 1 0-3.8" />
        <path d="M20.279 17.609a10 10 0 0 1-2.7 2.69" />
        <path d="M21.818 10.1a10 10 0 0 1 0 3.8" />
        <path d="M3.721 6.391a10 10 0 0 1 2.7-2.69" />
        <path d="M6.391 20.279a10 10 0 0 1-2.69-2.7" />
      </>
    ),
  },
  'panel-right': {
    body: (
      <>
        <rect width="18" height="18" x="3" y="3" rx="2" />
        <path d="M15 3v18" />
      </>
    ),
  },
  'chevron-left': {
    body: (
      <path d="m15 18-6-6 6-6" />
    ),
  },
  'diff-added': {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <>
        <path d="M8 3.5v9" />
        <path d="M3.5 8h9" />
      </>
    ),
  },
  'diff-removed': {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <path d="M3.5 8h9" />
    ),
  },
  'key-round': {
    body: (
      <>
        <path d="M2.586 17.414A2 2 0 0 0 2 18.828V21a1 1 0 0 0 1 1h3a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h1a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h.172a2 2 0 0 0 1.414-.586l.814-.814a6.5 6.5 0 1 0-4-4z" />
        <circle cx="16.5" cy="7.5" r=".5" fill="currentColor" />
      </>
    ),
  },
  'book-open': {
    body: (
      <>
        <path d="M12 5v16" />
        <path d="M20.001 19A2 2 0 0022 17V5a2 2 0 00-1.999-2L16 3.002A5 5 0 0012 5a5 5 0 00-4-2H4a2 2 0 00-2 2v12a2 2 0 001.999 2H8a5 5 0 014 2 5 5 0 014-2z" />
      </>
    ),
  },
  lock: {
    body: (
      <>
        <rect width="18" height="11" x="3" y="11" rx="2" ry="2" />
        <path d="M7 11V7a5 5 0 0 1 10 0v4" />
      </>
    ),
  },
  draft: {
    viewBox: '0 0 16 16',
    strokeWidth: 1.5,
    body: (
      <>
        <path d="M10.5 2.75 13.25 5.5 6 12.75H3.25V10z" />
        <path d="M9 4.25 11.75 7" />
      </>
    ),
  },
  'mode-standin': {
    viewBox: '0 0 12 12',
    body: (
      <>
        <path d="M6 1.25 10.75 6 6 10.75 1.25 6z" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" />
        <circle cx="6" cy="6" r="1.4" fill="currentColor" stroke="none" />
      </>
    ),
  },
  hammer: {
    body: (
      <>
        <path d="m15 12-9.373 9.373a1 1 0 0 1-3.001-3L12 9" />
        <path d="m18 15 4-4" />
        <path d="m21.5 11.5-1.914-1.914A2 2 0 0 1 19 8.172v-.344a2 2 0 0 0-.586-1.414l-1.657-1.657A6 6 0 0 0 12.516 3H9l1.243 1.243A6 6 0 0 1 12 8.485V10l2 2h1.172a2 2 0 0 1 1.414.586L18.5 14.5" />
      </>
    ),
  },
  'arrow-up-right': {
    body: (
      <>
        <path d="M7 7h10v10" />
        <path d="M7 17 17 7" />
      </>
    ),
  },
  layers: {
    body: (
      <>
        <path d="M12.83 2.18a2 2 0 0 0-1.66 0L2.6 6.08a1 1 0 0 0 0 1.83l8.58 3.91a2 2 0 0 0 1.66 0l8.58-3.9a1 1 0 0 0 0-1.83z" />
        <path d="M2 12a1 1 0 0 0 .58.91l8.6 3.91a2 2 0 0 0 1.65 0l8.58-3.9A1 1 0 0 0 22 12" />
        <path d="M2 17a1 1 0 0 0 .58.91l8.6 3.91a2 2 0 0 0 1.65 0l8.58-3.9A1 1 0 0 0 22 17" />
      </>
    ),
  },
  'file-code': {
    body: (
      <>
        <path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z" />
        <path d="M14 2v5a1 1 0 0 0 1 1h5" />
        <path d="M10 12.5 8 15l2 2.5" />
        <path d="m14 12.5 2 2.5-2 2.5" />
      </>
    ),
  },
  braces: {
    body: (
      <>
        <path d="M8 3H7a2 2 0 0 0-2 2v5a2 2 0 0 1-2 2 2 2 0 0 1 2 2v5c0 1.1.9 2 2 2h1" />
        <path d="M16 21h1a2 2 0 0 0 2-2v-5c0-1.1.9-2 2-2a2 2 0 0 1-2-2V5a2 2 0 0 0-2-2h-1" />
      </>
    ),
  },
  upload: {
    body: (
      <>
        <path d="M12 3v12" />
        <path d="m17 8-5-5-5 5" />
        <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
      </>
    ),
  },
  'list-tree': {
    body: (
      <>
        <path d="M8 5h13" />
        <path d="M13 12h8" />
        <path d="M13 19h8" />
        <path d="M3 10a2 2 0 0 0 2 2h3" />
        <path d="M3 5v12a2 2 0 0 0 2 2h3" />
      </>
    ),
  },
  'arrow-left': {
    body: (
      <>
        <path d="m12 19-7-7 7-7" />
        <path d="M19 12H5" />
      </>
    ),
  },
  funnel: {
    body: (
      <path d="M10 20a1 1 0 0 0 .553.895l2 1A1 1 0 0 0 14 21v-7a2 2 0 0 1 .517-1.341L21.74 4.67A1 1 0 0 0 21 3H3a1 1 0 0 0-.742 1.67l7.225 7.989A2 2 0 0 1 10 14z" />
    ),
  },
  plug: {
    body: (
      <>
        <path d="M12 22v-5" />
        <path d="M15 8V2" />
        <path d="M17 8a1 1 0 0 1 1 1v4a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V9a1 1 0 0 1 1-1z" />
        <path d="M9 8V2" />
      </>
    ),
  },
  'folder-open': {
    body: (
      <path d="m6 14 1.5-2.9A2 2 0 0 1 9.24 10H20a2 2 0 0 1 1.94 2.5l-1.54 6a2 2 0 0 1-1.95 1.5H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h3.9a2 2 0 0 1 1.69.9l.81 1.2a2 2 0 0 0 1.67.9H18a2 2 0 0 1 2 2v2" />
    ),
  },
  bug: {
    body: (
      <>
        <path d="M12 20v-9" />
        <path d="M14 7a4 4 0 0 1 4 4v3a6 6 0 0 1-12 0v-3a4 4 0 0 1 4-4z" />
        <path d="M14.12 3.88 16 2" />
        <path d="M21 21a4 4 0 0 0-3.81-4" />
        <path d="M21 5a4 4 0 0 1-3.55 3.97" />
        <path d="M22 13h-4" />
        <path d="M3 21a4 4 0 0 1 3.81-4" />
        <path d="M3 5a4 4 0 0 0 3.55 3.97" />
        <path d="M6 13H2" />
        <path d="m8 2 1.88 1.88" />
        <path d="M9 7.13V6a3 3 0 1 1 6 0v1.13" />
      </>
    ),
  },
  'file-text': {
    body: (
      <>
        <path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z" />
        <path d="M14 2v5a1 1 0 0 0 1 1h5" />
        <path d="M10 9H8" />
        <path d="M16 13H8" />
        <path d="M16 17H8" />
      </>
    ),
  },
  bookmark: {
    body: (
      <path d="M17 3a2 2 0 0 1 2 2v15a1 1 0 0 1-1.496.868l-4.512-2.578a2 2 0 0 0-1.984 0l-4.512 2.578A1 1 0 0 1 5 20V5a2 2 0 0 1 2-2z" />
    ),
  },
  'rotate-ccw': {
    body: (
      <>
        <path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8" />
        <path d="M3 3v5h5" />
      </>
    ),
  },
  link: {
    body: (
      <>
        <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
        <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
      </>
    ),
  },
  server: {
    body: (
      <>
        <rect width="20" height="8" x="2" y="2" rx="2" ry="2" />
        <rect width="20" height="8" x="2" y="14" rx="2" ry="2" />
        <path d="M6 6h.01" />
        <path d="M6 18h.01" />
      </>
    ),
  },
  rocket: {
    body: (
      <>
        <path d="M4.5 16.5c-1.5 1.26-2 5-2 5s3.74-.5 5-2c.71-.84.7-2.13-.09-2.91a2.18 2.18 0 0 0-2.91-.09z" />
        <path d="m12 15-3-3a22 22 0 0 1 2-3.95A12.88 12.88 0 0 1 22 2c0 2.72-.78 7.5-6 11a22.35 22.35 0 0 1-4 2z" />
        <path d="M9 12H4s.55-3.03 2-4c1.62-1.08 5 0 5 0" />
        <path d="M12 15v5s3.03-.55 4-2c1.08-1.62 0-5 0-5" />
      </>
    ),
  },
  users: {
    body: (
      <>
        <path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" />
        <circle cx="9" cy="7" r="4" />
        <path d="M22 21v-2a4 4 0 0 0-3-3.87" />
        <path d="M16 3.13a4 4 0 0 1 0 7.75" />
      </>
    ),
  },
} satisfies Record<string, IconData>;

export type IconName =
  | keyof typeof BASE
  | 'logo'
  | 'nav-debugger'
  | 'nav-capture'
  | 'nav-fleet'
  | 'nav-runtimes'
  | 'nav-saved'
  | 'nav-audit'
  | 'nav-access'
  | 'nav-help'
  | 'nav-settings';

/** Every icon by name. The semantic aliases reuse the data of the icon they name. */
export const ICONS: Record<IconName, IconData> = {
  ...BASE,
  // Left navigation rail.
  logo: BASE.connector,
  'nav-debugger': BASE.workflow,
  'nav-capture': BASE.radio,
  'nav-fleet': BASE.server,
  'nav-runtimes': BASE.package,
  'nav-saved': BASE.library,
  'nav-audit': BASE.history,
  'nav-access': BASE.users,
  'nav-help': BASE['life-buoy'],
  'nav-settings': BASE.settings,
};

export const ICON_NAMES = Object.keys(ICONS) as IconName[];

export function Icon({
  name,
  size = 16,
  strokeWidth,
  className,
  title,
}: {
  name: IconName;
  size?: number;
  strokeWidth?: number;
  className?: string;
  title?: string;
}) {
  const icon: IconData = ICONS[name];
  return (
    <svg
      className={'od-i ' + (className ?? '')}
      width={size}
      height={size}
      viewBox={icon.viewBox ?? DEFAULT_VIEW_BOX}
      fill="none"
      stroke="currentColor"
      strokeWidth={strokeWidth ?? icon.strokeWidth ?? DEFAULT_STROKE_WIDTH}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden={title ? undefined : true}
      role={title ? 'img' : undefined}
    >
      {title && <title>{title}</title>}
      {icon.body}
    </svg>
  );
}
