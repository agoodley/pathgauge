// The traffic-mix catalogue: session kinds, their limits and the named
// scenarios. Mirrors MixKinds, MixScenarios and ParseMix in
// internal/engine/mix.go (the server does not publish them); keep the
// two in step.

export type MixClass = 'rt' | 'video' | 'web' | 'stream' | 'bulk';

export interface MixKindDef {
  name: string;
  /** Same label the engine puts in results. */
  label: string;
  /** Counted noun for sentences: [one, many]. */
  noun: [string, string];
  cls: MixClass;
  /** Most sessions of this kind in one mix. */
  max: number;
  /** Real-time kinds: rate each way including IP/UDP headers, kbit/s. */
  wireKbps?: number;
}

/** Session kinds in display order (the engine's order). */
export const MIX_KINDS: MixKindDef[] = [
  { name: 'voice', label: 'Voice call (G.711)', noun: ['G.711 voice call', 'G.711 voice calls'], cls: 'rt', max: 256, wireKbps: (50 * (172 + 28) * 8) / 1000 },
  { name: 'voice-opus', label: 'Voice call (Opus)', noun: ['Opus voice call', 'Opus voice calls'], cls: 'rt', max: 256, wireKbps: (50 * (72 + 28) * 8) / 1000 },
  { name: 'video', label: 'Video call (720p, 1.5 Mbit/s)', noun: ['720p video call', '720p video calls'], cls: 'video', max: 64, wireKbps: 1500 * 1.03 },
  { name: 'video-hd', label: 'Video call (1080p, 3.8 Mbit/s)', noun: ['1080p video call', '1080p video calls'], cls: 'video', max: 64, wireKbps: 3800 * 1.03 },
  { name: 'gaming', label: 'Online game', noun: ['online game', 'online games'], cls: 'rt', max: 64, wireKbps: (60 * (120 + 28) * 8) / 1000 },
  { name: 'web', label: 'Web browsing', noun: ['web user', 'web users'], cls: 'web', max: 32 },
  { name: 'stream', label: 'Video streaming (HD, 5 Mbit/s)', noun: ['HD video stream', 'HD video streams'], cls: 'stream', max: 16 },
  { name: 'stream-4k', label: 'Video streaming (4K, 16 Mbit/s)', noun: ['4K video stream', '4K video streams'], cls: 'stream', max: 16 },
  { name: 'download', label: 'File download', noun: ['file download', 'file downloads'], cls: 'bulk', max: 16 },
  { name: 'upload', label: 'File upload', noun: ['file upload', 'file uploads'], cls: 'bulk', max: 16 },
];

/** Most call and game sessions (one UDP socket each) in one mix. */
export const MAX_RT_SESSIONS = 256;

export interface MixScenario {
  id: string;
  label: string;
  text: string;
  spec: string;
}

export const MIX_SCENARIOS: MixScenario[] = [
  { id: 'office', label: 'Office', text: 'A small office: calls and video meetings while people browse and files sync.', spec: 'voice:10,video:3,web:5,download:1,upload:1' },
  { id: 'home-working', label: 'Home working', text: 'One person on a video call at home while someone streams and a file uploads.', spec: 'video:1,web:1,stream:1,upload:1' },
  { id: 'call-centre', label: 'Call centre', text: 'Thirty agents on voice calls, with browsing and a download.', spec: 'voice:30,web:5,download:1' },
  { id: 'family', label: 'Family', text: 'A busy household: a video call, two streams, a game, browsing and a download.', spec: 'video:1,stream:2,gaming:1,web:2,download:1' },
];

export const DEFAULT_SCENARIO = 'office';

export type MixCounts = Record<string, number>;

export function mixKind(name: string): MixKindDef | undefined {
  return MIX_KINDS.find((k) => k.name === name);
}

export function isRealtime(k: MixKindDef): boolean {
  return k.cls === 'rt' || k.cls === 'video';
}

/**
 * Parse "kind:count,..." (a missing count means 1). Returns null for a
 * scenario name or anything the engine would reject by name; counts are
 * not range-checked here (see validateCounts).
 */
export function parseSpec(spec: string): MixCounts | null {
  const out: MixCounts = {};
  for (const raw of spec.toLowerCase().split(',')) {
    const part = raw.trim();
    if (!part) continue;
    const [name, n] = part.split(':').map((s) => s.trim());
    if (!mixKind(name)) return null;
    const count = n === undefined ? 1 : Number(n);
    if (!Number.isInteger(count) || count < 1) return null;
    out[name] = (out[name] ?? 0) + count;
  }
  return Object.keys(out).length ? out : null;
}

/** Counts back to the engine's "kind:count,..." form, in display order. */
export function formatSpec(c: MixCounts): string {
  return MIX_KINDS.filter((k) => (c[k.name] ?? 0) > 0)
    .map((k) => `${k.name}:${c[k.name]}`)
    .join(',');
}

export function scenarioCounts(id: string): MixCounts {
  const s = MIX_SCENARIOS.find((x) => x.id === id);
  return (s && parseSpec(s.spec)) || {};
}

/** "10 G.711 voice calls, 3 720p video calls, …" */
export function describeCounts(c: MixCounts): string {
  return MIX_KINDS.filter((k) => (c[k.name] ?? 0) > 0)
    .map((k) => {
      const n = c[k.name];
      return `${n} ${n === 1 ? k.noun[0] : k.noun[1]}`;
    })
    .join(', ');
}

/** Rate the calls and games need in each direction, Mbit/s. */
export function realtimeMbps(c: MixCounts): number {
  let kbps = 0;
  for (const k of MIX_KINDS) kbps += (k.wireKbps ?? 0) * (c[k.name] ?? 0);
  return kbps / 1000;
}

/**
 * Check custom counts against the engine's limits. Values are the raw
 * input strings; returns an error message or null.
 */
export function validateCounts(raw: Record<string, string>): { counts: MixCounts; error: string | null } {
  const counts: MixCounts = {};
  const errors: string[] = [];
  let rt = 0;
  for (const k of MIX_KINDS) {
    const s = (raw[k.name] ?? '').trim();
    if (s === '') continue;
    const n = Number(s);
    if (!Number.isInteger(n) || n < 0) {
      errors.push(`${k.label}: enter a whole number from 0 to ${k.max}.`);
      continue;
    }
    if (n > k.max) {
      errors.push(`${k.label}: at most ${k.max} sessions.`);
      continue;
    }
    if (n > 0) counts[k.name] = n;
    if (isRealtime(k)) rt += n;
  }
  if (errors.length) return { counts, error: errors.join(' ') };
  if (rt > MAX_RT_SESSIONS) return { counts, error: `Calls and games: at most ${MAX_RT_SESSIONS} sessions in total (this mix has ${rt}).` };
  if (!Object.keys(counts).length) return { counts, error: 'Add at least one session to the custom mix.' };
  return { counts, error: null };
}
