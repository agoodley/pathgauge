// Number, unit and time formatting shared by every page.

export function isNum(v: unknown): v is number {
  return typeof v === 'number' && Number.isFinite(v);
}

/** Rate in Mbit/s, switching to Gbit/s at 1000 (as the HTML report does). */
export function fmtMbps(m: number | null | undefined): string {
  if (!isNum(m)) return '—';
  if (Math.abs(m) >= 1000) return `${(m / 1000).toFixed(2)} Gbit/s`;
  if (Math.abs(m) >= 100) return `${m.toFixed(0)} Mbit/s`;
  if (Math.abs(m) >= 10) return `${m.toFixed(1)} Mbit/s`;
  return `${m.toFixed(2)} Mbit/s`;
}

/** Milliseconds with 1–2 decimals. */
export function fmtMs(v: number | null | undefined, digits?: number): string {
  if (!isNum(v)) return '—';
  const d = digits ?? (Math.abs(v) >= 100 ? 1 : 2);
  return `${v.toFixed(d)} ms`;
}

export function fmtPct(v: number | null | undefined, digits = 2): string {
  if (!isNum(v)) return '—';
  return `${v.toFixed(digits)}%`;
}

export function f1(v: number | null | undefined): string {
  return isNum(v) ? v.toFixed(1) : '—';
}
export function f2(v: number | null | undefined): string {
  return isNum(v) ? v.toFixed(2) : '—';
}
export function f0(v: number | null | undefined): string {
  return isNum(v) ? v.toFixed(0) : '—';
}

/** Compact number for axis labels. */
export function fmtAxis(v: number): string {
  const a = Math.abs(v);
  if (a >= 1e6) return `${+(v / 1e6).toFixed(1)}M`;
  if (a >= 1e4) return `${+(v / 1e3).toFixed(0)}k`;
  if (a >= 100) return v.toFixed(0);
  if (a === 0) return '0';
  // Enough significant digits that neighbouring ticks stay distinct.
  return `${+v.toPrecision(3)}`;
}

export function fmtBytes(b: number | null | undefined): string {
  if (!isNum(b)) return '—';
  const units = ['B', 'kB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = b;
  while (Math.abs(v) >= 1000 && i < units.length - 1) {
    v /= 1000;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

/** Scale for rate charts: Mbit/s, or Gbit/s when the data passes 1000. */
export function rateScale(maxMbps: number): { div: number; unit: string } {
  return maxMbps >= 1000 ? { div: 1000, unit: 'Gbit/s' } : { div: 1, unit: 'Mbit/s' };
}

export function parseTime(s: string | null | undefined): Date | null {
  if (!s) return null;
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? null : d;
}

const dtf = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' });
const dtfShort = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });

export function fmtTime(s: string | null | undefined): string {
  const d = parseTime(s);
  if (!d) return '—';
  return (d.getFullYear() === new Date().getFullYear() ? dtfShort : dtf).format(d);
}

export function fmtUnix(ts: number): string {
  return fmtTime(new Date(ts * 1000).toISOString());
}

export function fmtAgo(s: string | null | undefined): string {
  const d = parseTime(s);
  if (!d) return 'never';
  const sec = Math.round((Date.now() - d.getTime()) / 1000);
  if (sec < 0) return 'in ' + fmtDuration(-sec);
  if (sec < 10) return 'just now';
  if (sec < 60) return `${sec} s ago`;
  if (sec < 3600) return `${Math.floor(sec / 60)} min ago`;
  if (sec < 48 * 3600) return `${Math.floor(sec / 3600)} h ago`;
  return `${Math.floor(sec / 86400)} days ago`;
}

/** Duration in seconds as "1 h 4 min", "3 min 5 s", "12 s". */
export function fmtDuration(sec: number): string {
  if (!isNum(sec)) return '—';
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return `${sec} s`;
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  if (m < 60) return s ? `${m} min ${s} s` : `${m} min`;
  const h = Math.floor(m / 60);
  const mm = m % 60;
  if (h < 48) return mm ? `${h} h ${mm} min` : `${h} h`;
  return `${Math.round(h / 24)} days`;
}

export function between(a: string | null | undefined, b: string | null | undefined): number | null {
  const x = parseTime(a);
  const y = parseTime(b);
  if (!x || !y) return null;
  return (y.getTime() - x.getTime()) / 1000;
}

export function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

export function plural(n: number, one: string, many = one + 's'): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** Convert a Date to the value format of <input type="datetime-local">. */
export function toLocalInput(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function cap(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : s;
}
