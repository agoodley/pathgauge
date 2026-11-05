// Light/dark theme: follows prefers-color-scheme unless the user picks one.
// The chart palette is selected per mode (not an automatic invert); the
// same values are declared as CSS custom properties in styles.css.
import { useSyncExternalStore } from 'react';

export type ThemePref = 'auto' | 'light' | 'dark';

const KEY = 'pathgauge-theme';
const mq = window.matchMedia('(prefers-color-scheme: dark)');
const listeners = new Set<() => void>();

function readPref(): ThemePref {
  const v = localStorage.getItem(KEY);
  return v === 'light' || v === 'dark' ? v : 'auto';
}

let pref: ThemePref = readPref();

function apply() {
  const root = document.documentElement;
  if (pref === 'auto') root.removeAttribute('data-theme');
  else root.setAttribute('data-theme', pref);
}
apply();

mq.addEventListener('change', () => listeners.forEach((l) => l()));

export function setThemePref(p: ThemePref) {
  pref = p;
  if (p === 'auto') localStorage.removeItem(KEY);
  else localStorage.setItem(KEY, p);
  apply();
  listeners.forEach((l) => l());
}

function subscribe(l: () => void) {
  listeners.add(l);
  return () => {
    listeners.delete(l);
  };
}

const snap = () => `${pref}|${pref === 'dark' || (pref === 'auto' && mq.matches) ? 'dark' : 'light'}`;

export function useTheme(): { pref: ThemePref; dark: boolean } {
  const s = useSyncExternalStore(subscribe, snap);
  const [p, mode] = s.split('|');
  return { pref: p as ThemePref, dark: mode === 'dark' };
}

export interface Palette {
  dark: boolean;
  series: [string, string, string, string];
  text: string;
  text2: string;
  muted: string;
  line: string;
  grid: string;
  surface: string;
  page: string;
  band: string;
  /** Sequential single-hue blue ramp, low → high magnitude. */
  seq: string[];
}

// Blue ramp steps 100…700.
const BLUE = ['#cde2fb', '#9ec5f4', '#6da7ec', '#3987e5', '#256abf', '#184f95', '#0d366b'];

const LIGHT: Palette = {
  dark: false,
  series: ['#2a78d6', '#eb6834', '#1baf7a', '#eda100'],
  text: '#0b0b0b',
  text2: '#52514e',
  muted: '#6f6e69',
  line: '#dedcd6',
  grid: '#e9e7e2',
  surface: '#fcfcfb',
  page: '#f4f3f0',
  band: 'rgba(42,120,214,0.08)',
  seq: BLUE,
};

const DARK: Palette = {
  dark: true,
  series: ['#3987e5', '#d95926', '#199e70', '#c98500'],
  text: '#f0efec',
  text2: '#c3c2b7',
  muted: '#9b9a92',
  line: '#383835',
  grid: '#2a2a28',
  surface: '#1a1a19',
  page: '#121211',
  band: 'rgba(57,135,229,0.12)',
  // On the dark surface the ramp anchor flips: low values recede toward the
  // surface (dark blue), high values stand out (light blue).
  seq: [...BLUE].reverse(),
};

export function usePalette(): Palette {
  return useTheme().dark ? DARK : LIGHT;
}
