// A tiny ECharts wrapper plus option builders that apply the dashboard's
// chart rules: one y-axis per chart, fixed categorical colour order, 2px
// lines, recessive grid, legend only for 2+ series, axis tooltips, units in
// axis names, rates and latencies from zero.
import { useEffect, useRef } from 'react';
import * as echarts from 'echarts/core';
import { BarChart, HeatmapChart, LineChart, ScatterChart } from 'echarts/charts';
import {
  GridComponent,
  LegendComponent,
  MarkAreaComponent,
  MarkLineComponent,
  TooltipComponent,
  VisualMapComponent,
} from 'echarts/components';
import { CanvasRenderer } from 'echarts/renderers';
import type { EChartsCoreOption } from 'echarts/core';
import type { Palette } from '../lib/theme';
import { fmtAxis } from '../lib/format';

echarts.use([
  LineChart,
  BarChart,
  HeatmapChart,
  ScatterChart,
  GridComponent,
  LegendComponent,
  TooltipComponent,
  MarkAreaComponent,
  MarkLineComponent,
  VisualMapComponent,
  CanvasRenderer,
]);

export type Option = EChartsCoreOption;

const FONT = 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif';

export function EChart({ option, height = 240, label }: { option: Option; height?: number; label?: string }) {
  const ref = useRef<HTMLDivElement>(null);
  const inst = useRef<echarts.ECharts | null>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const c = echarts.init(el, undefined, { renderer: 'canvas' });
    inst.current = c;
    const ro = new ResizeObserver(() => c.resize());
    ro.observe(el);
    return () => {
      ro.disconnect();
      c.dispose();
      inst.current = null;
    };
  }, []);

  useEffect(() => {
    inst.current?.setOption(option, { notMerge: true, lazyUpdate: true });
  }, [option]);

  return <div ref={ref} className="echart" style={{ height }} role="img" aria-label={label} />;
}

/** A titled chart; the title names the measure (single series needs no legend). */
export function Figure({
  title,
  option,
  height,
  note,
}: {
  title: string;
  option: Option;
  height?: number;
  note?: string;
}) {
  return (
    <figure className="figure">
      <figcaption>{title}</figcaption>
      <EChart option={option} height={height} label={title} />
      {note && <p className="figure-note">{note}</p>}
    </figure>
  );
}

// ---- option builders ----

function esc(s: string): string {
  return s.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c] as string);
}

function axisStyle(P: Palette) {
  return {
    axisLine: { lineStyle: { color: P.line } },
    axisTick: { show: false },
    axisLabel: { color: P.muted, fontSize: 11, fontFamily: FONT, hideOverlap: true },
    splitLine: { lineStyle: { color: P.grid, width: 1 } },
    nameTextStyle: { color: P.text2, fontSize: 11, fontFamily: FONT },
  };
}

function tooltipBase(P: Palette) {
  return {
    backgroundColor: P.surface,
    borderColor: P.line,
    borderWidth: 1,
    textStyle: { color: P.text, fontSize: 12, fontFamily: FONT },
    extraCssText: 'box-shadow:0 2px 8px rgba(0,0,0,.15);border-radius:6px;',
    confine: true,
  };
}

function legend(P: Palette, n: number) {
  if (n < 2) return { show: false };
  return {
    show: true,
    top: 0,
    left: 0,
    icon: 'roundRect',
    itemWidth: 14,
    itemHeight: 3,
    textStyle: { color: P.text2, fontSize: 12, fontFamily: FONT },
  };
}

function grid(hasLegend: boolean, hasXName: boolean) {
  return {
    left: 4,
    right: 14,
    top: hasLegend ? 44 : 30,
    bottom: hasXName ? 4 : 2,
    outerBoundsMode: 'same',
    outerBoundsContain: 'all',
  };
}

interface TipParam {
  marker?: string;
  seriesName?: string;
  value?: unknown;
  axisValue?: unknown;
  name?: string;
}

export interface LineSeries {
  name: string;
  /** [x, y] pairs; y null breaks the line. */
  data: [number, number | null][];
  /** Categorical slot 0–3 (fixed order). */
  slot?: number;
  points?: boolean;
  /** Line with an 8 px marker (2 px surface ring) at every point. */
  markers?: boolean;
  /** Marker size when markers is set (default 9, ring included). */
  markerSize?: number;
  /** true: step at the midpoint; 'start': each value holds from the previous x (per-interval bins). */
  step?: boolean | 'start';
  /** A light wash of the series hue under the line. */
  area?: boolean;
  dashed?: boolean;
  /** Override colour (only for a neutral "other" series). */
  color?: string;
}

export interface Band {
  from: number;
  to: number;
  label: string;
}

/** A horizontal reference line (a target or limit), labelled at its right end. */
export interface RefLine {
  y: number;
  label: string;
}

/** Smallest 1, 2, 2.5 or 5 × 10^k at or above x (a clean axis maximum). */
export function niceCeil(x: number): number {
  if (!(x > 0)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(x)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= x - 1e-9) return m * p;
  return 10 * p;
}

export interface LineOpts {
  series: LineSeries[];
  xName: string;
  yName: string;
  xType?: 'value' | 'time';
  yZero?: boolean;
  /** Fixed y minimum (e.g. 1 for MOS); overrides yZero. */
  yMin?: number;
  /** Base-2 log x-axis (for counts that double, such as session searches). */
  xLog?: boolean;
  xMin?: number;
  xMax?: number;
  yFmt?: (v: number) => string;
  xFmt?: (v: number) => string;
  bands?: Band[];
  animation?: boolean;
  yMax?: number;
  /** Whole-number x ticks (hop counts, stream counts). */
  xInteger?: boolean;
  /** Horizontal reference lines; the y-axis grows to show them. */
  refLines?: RefLine[];
}

export function lineOption(P: Palette, o: LineOpts): Option {
  const n = o.series.length;
  const yFmt = o.yFmt ?? ((v: number) => fmtAxis(v));
  const xFmt = o.xFmt ?? ((v: number) => fmtAxis(v));
  const ax = axisStyle(P);
  const bandData = (o.bands ?? []).map((b) => [
    { xAxis: b.from, name: b.label },
    { xAxis: b.to },
  ]);
  const refs = o.refLines ?? [];
  // A reference line above the data would be cut off: make room for it
  // (with headroom for its label) on a clean maximum.
  let yMax = o.yMax;
  if (yMax === undefined && refs.length) {
    const top = Math.max(...refs.map((r) => r.y));
    const dataMax = Math.max(0, ...o.series.flatMap((s) => s.data.map((d) => d[1] ?? 0)));
    if (top >= dataMax) yMax = niceCeil(top * 1.15);
  }
  const refLine = refs.length
    ? {
        silent: true,
        symbol: 'none',
        animation: false,
        lineStyle: { color: P.text2, width: 1, type: 'dashed' },
        label: { position: 'insideEndTop', color: P.text2, fontSize: 11, fontFamily: FONT },
        emphasis: { disabled: true },
        data: refs.map((r) => ({ yAxis: r.y, label: { formatter: r.label } })),
      }
    : undefined;
  return {
    animation: o.animation ?? false,
    textStyle: { fontFamily: FONT },
    color: P.series,
    legend: legend(P, n),
    grid: grid(n >= 2, true),
    tooltip: {
      ...tooltipBase(P),
      trigger: 'axis',
      axisPointer: { type: 'line', lineStyle: { color: P.muted, type: 'dashed', width: 1 } },
      formatter: (ps: TipParam | TipParam[]) => {
        const arr = Array.isArray(ps) ? ps : [ps];
        if (!arr.length) return '';
        const v0 = arr[0].value as [number, number | null] | undefined;
        const head = v0 ? xFmt(Number(v0[0])) : '';
        const rows = arr
          .map((p) => {
            const v = p.value as [number, number | null] | undefined;
            if (!v || v[1] === null || v[1] === undefined) return '';
            // Series sampled at different x: name the point's own x.
            const at = v0 && Number(v[0]) !== Number(v0[0]) ? ` <span style="opacity:.75">(${esc(xFmt(Number(v[0])))})</span>` : '';
            return `<div>${p.marker ?? ''}${esc(p.seriesName ?? '')}: <b>${esc(yFmt(Number(v[1])))}</b>${at}</div>`;
          })
          .join('');
        return `<div style="font-weight:600;margin-bottom:2px">${esc(head)}</div>${rows}`;
      },
    },
    xAxis: {
      type: o.xLog ? 'log' : (o.xType ?? 'value'),
      logBase: o.xLog ? 2 : undefined,
      name: o.xName,
      nameLocation: 'middle',
      nameGap: 26,
      min: o.xMin ?? 'dataMin',
      max: o.xMax ?? 'dataMax',
      minInterval: o.xInteger && !o.xLog ? 1 : undefined,
      ...ax,
      splitLine: { show: false },
      axisLabel: {
        ...ax.axisLabel,
        formatter:
          o.xType === 'time'
            ? { year: '{yyyy}', month: '{MMM}', day: '{d} {MMM}', hour: '{HH}:{mm}', minute: '{HH}:{mm}', second: '{HH}:{mm}:{ss}' }
            : (v: number) => fmtAxis(v),
      },
    },
    yAxis: {
      type: 'value',
      name: o.yName,
      nameLocation: 'end',
      nameGap: 10,
      min: o.yMin ?? (o.yZero === false ? undefined : 0),
      max: yMax,
      ...ax,
      nameTextStyle: { ...ax.nameTextStyle, align: 'left' },
      axisLine: { show: false },
      axisLabel: { ...ax.axisLabel, formatter: (v: number) => fmtAxis(v) },
    },
    series: o.series.map((s, i) => {
      const c = s.color ?? P.series[(s.slot ?? i) % 4];
      return {
        type: 'line',
        name: s.name,
        data: s.data,
        step: s.step === true ? 'middle' : s.step || undefined,
        areaStyle: s.area ? { color: c, opacity: 0.1 } : undefined,
        // A line needs two points; show markers while a series is that short.
        showSymbol: !!s.points || !!s.markers || s.data.length < 3,
        symbol: 'circle',
        symbolSize: s.markers ? (s.markerSize ?? 9) : s.points ? 5 : 4,
        connectNulls: false,
        color: c,
        lineStyle: { width: s.points ? 0 : 2, color: c, type: s.dashed ? 'dashed' : 'solid' },
        itemStyle: s.markers ? { color: c, borderColor: P.surface, borderWidth: 2 } : { color: c },
        emphasis: { disabled: true },
        z: s.dashed ? 2 : 3,
        markArea:
          i === 0 && bandData.length
            ? {
                silent: true,
                itemStyle: { color: P.band },
                label: { color: P.text2, fontSize: 11, position: 'insideTop', fontFamily: FONT },
                data: bandData,
              }
            : undefined,
        markLine: i === 0 ? refLine : undefined,
      };
    }),
  };
}

export interface BarOpts {
  categories: string[];
  series: { name: string; values: (number | null)[]; slot?: number }[];
  xName: string;
  yName: string;
  yFmt?: (v: number) => string;
  horizontal?: boolean;
  /** Fixed maximum of the value axis (e.g. 100 for percentages). */
  yMax?: number;
}

export function barOption(P: Palette, o: BarOpts): Option {
  const n = o.series.length;
  const ax = axisStyle(P);
  const yFmt = o.yFmt ?? ((v: number) => fmtAxis(v));
  const catAxis = {
    type: 'category',
    data: o.categories,
    name: o.xName,
    nameLocation: 'middle',
    nameGap: o.horizontal ? 0 : 26,
    ...ax,
    splitLine: { show: false },
  };
  const valAxis = {
    type: 'value',
    name: o.yName,
    nameLocation: o.horizontal ? 'middle' : 'end',
    nameGap: o.horizontal ? 26 : 10,
    min: 0,
    max: o.yMax,
    ...ax,
    nameTextStyle: { ...ax.nameTextStyle, align: o.horizontal ? 'center' : 'left' },
    axisLine: { show: false },
    axisLabel: { ...ax.axisLabel, formatter: (v: number) => fmtAxis(v) },
  };
  return {
    animation: false,
    textStyle: { fontFamily: FONT },
    legend: legend(P, n),
    grid: grid(n >= 2, true),
    tooltip: {
      ...tooltipBase(P),
      trigger: 'axis',
      axisPointer: { type: 'shadow', shadowStyle: { color: P.band } },
      formatter: (ps: TipParam | TipParam[]) => {
        const arr = Array.isArray(ps) ? ps : [ps];
        if (!arr.length) return '';
        const head = String(arr[0].name ?? arr[0].axisValue ?? '');
        const rows = arr
          .map((p) =>
            p.value === null || p.value === undefined
              ? ''
              : `<div>${p.marker ?? ''}${esc(p.seriesName ?? '')}: <b>${esc(yFmt(Number(p.value)))}</b></div>`,
          )
          .join('');
        return `<div style="font-weight:600;margin-bottom:2px">${esc((o.xName ? o.xName + ' ' : '') + head)}</div>${rows}`;
      },
    },
    xAxis: o.horizontal ? valAxis : catAxis,
    yAxis: o.horizontal ? { ...catAxis, inverse: true, nameGap: 8 } : valAxis,
    series: o.series.map((s, i) => {
      const c = P.series[(s.slot ?? i) % 4];
      return {
        type: 'bar',
        name: s.name,
        data: s.values,
        barMaxWidth: 32,
        barGap: '8%',
        color: c,
        itemStyle: { color: c, borderRadius: o.horizontal ? [0, 4, 4, 0] : [4, 4, 0, 0] },
        emphasis: { disabled: true },
      };
    }),
  };
}

export interface HeatOpts {
  xCats: string[];
  yCats: string[];
  /** [xIndex, yIndex, value, extra-tooltip-text] */
  data: [number, number, number, string][];
  xName: string;
  yName: string;
  unit: string;
}

export function heatmapOption(P: Palette, o: HeatOpts): Option {
  const ax = axisStyle(P);
  const vals = o.data.map((d) => d[2]);
  const max = vals.length ? Math.max(...vals) : 1;
  return {
    animation: false,
    textStyle: { fontFamily: FONT },
    grid: { left: 4, right: 14, top: 8, bottom: 56, outerBoundsMode: 'same', outerBoundsContain: 'all' },
    tooltip: {
      ...tooltipBase(P),
      trigger: 'item',
      formatter: (p: { value?: [number, number, number, string] }) => {
        const v = p.value;
        if (!v) return '';
        return `<div style="font-weight:600">${esc(o.yCats[v[1]])} ${esc(o.xCats[v[0]])}</div><div>${esc(v[3])}</div>`;
      },
    },
    xAxis: {
      type: 'category',
      data: o.xCats,
      name: o.xName,
      nameLocation: 'middle',
      nameGap: 24,
      ...ax,
      splitLine: { show: false },
      axisLine: { show: false },
    },
    yAxis: {
      type: 'category',
      data: o.yCats,
      name: o.yName,
      inverse: true,
      ...ax,
      splitLine: { show: false },
      axisLine: { show: false },
      nameTextStyle: { ...ax.nameTextStyle, align: 'right' },
    },
    visualMap: {
      type: 'continuous',
      min: 0,
      max: max > 0 ? max : 1,
      calculable: false,
      orient: 'horizontal',
      left: 'center',
      bottom: 0,
      itemWidth: 12,
      itemHeight: 180,
      text: [`${fmtAxis(max)} ${o.unit}`, `0 ${o.unit}`],
      textStyle: { color: P.text2, fontSize: 11, fontFamily: FONT },
      inRange: { color: P.seq },
      dimension: 2,
    },
    series: [
      {
        type: 'heatmap',
        data: o.data,
        itemStyle: { borderColor: P.surface, borderWidth: 2, borderRadius: 2 },
        emphasis: { itemStyle: { borderColor: P.text, borderWidth: 1 } },
      },
    ],
  };
}

/** A small axis-free trend line with a hover tooltip. */
export function sparkOption(P: Palette, values: number[], fmt: (v: number) => string): Option {
  return {
    animation: false,
    grid: { left: 2, right: 2, top: 4, bottom: 2 },
    tooltip: {
      ...tooltipBase(P),
      trigger: 'axis',
      axisPointer: { type: 'line', lineStyle: { color: P.muted, type: 'dashed', width: 1 } },
      formatter: (ps: TipParam | TipParam[]) => {
        const arr = Array.isArray(ps) ? ps : [ps];
        const v = arr[0]?.value;
        return v === undefined || v === null ? '' : `RTT p95 <b>${esc(fmt(Number(v)))}</b>`;
      },
    },
    xAxis: { type: 'category', show: false, boundaryGap: false, data: values.map((_, i) => i) },
    yAxis: { type: 'value', show: false, min: 0 },
    series: [
      {
        type: 'line',
        data: values,
        showSymbol: false,
        lineStyle: { width: 2, color: P.series[0] },
        itemStyle: { color: P.series[0] },
        areaStyle: { color: P.band },
        emphasis: { disabled: true },
      },
    ],
  };
}
