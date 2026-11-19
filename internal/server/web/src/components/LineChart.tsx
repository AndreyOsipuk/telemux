// Простой линейный график на SVG для истории нагрузки машины: несколько рядов по
// общей оси времени, разрывы там, где значения нет (null), подсказка со значениями
// под курсором. Без библиотек графиков - панели хватает этого.

import { useMemo, useRef, useState } from 'react';
import { theme } from 'antd';
import dayjs from 'dayjs';

import './LineChart.css';

export interface ChartSeries {
  key: string;
  label: string;
  color: string;
  values: (number | null)[];
}

interface LineChartProps {
  // Время точек, мс; по возрастанию, длина = длине values каждого ряда.
  times: number[];
  series: ChartSeries[];
  format: (v: number) => string;
  // Верх шкалы: фиксированный (100 для процентов) или по максимуму данных.
  yMax?: number;
  height?: number;
  // Подпись времени: короткая (часы) для окон до суток, иначе с датой.
  longRange?: boolean;
  testId?: string;
}

const W = 600; // ширина в единицах viewBox; SVG растягивается по контейнеру
const PAD = { top: 8, right: 8, bottom: 20, left: 44 };

// Отрезки ряда без null: каждый - своя ломаная.
export function segments(values: (number | null)[]): [number, number][][] {
  const out: [number, number][][] = [];
  let cur: [number, number][] = [];
  values.forEach((v, i) => {
    if (v === null || Number.isNaN(v)) {
      if (cur.length) out.push(cur);
      cur = [];
    } else cur.push([i, v]);
  });
  if (cur.length) out.push(cur);
  return out;
}

// «Круглый» верх шкалы: 1, 2, 5 × 10^n не меньше максимума.
export function niceMax(max: number): number {
  if (!(max > 0)) return 1;
  const p = 10 ** Math.floor(Math.log10(max));
  for (const m of [1, 2, 5, 10]) if (m * p >= max) return m * p;
  return 10 * p;
}

export default function LineChart({ times, series, format, yMax, height = 160, longRange = false, testId }: LineChartProps) {
  const { token } = theme.useToken();
  const svgRef = useRef<SVGSVGElement>(null);
  const [hover, setHover] = useState<number | null>(null);

  const top = useMemo(() => {
    if (yMax !== undefined) return yMax;
    let m = 0;
    for (const s of series) for (const v of s.values) if (v !== null && v > m) m = v;
    return niceMax(m);
  }, [series, yMax]);

  const n = times.length;
  const t0 = times[0] ?? 0;
  const t1 = times[n - 1] ?? 1;
  const span = Math.max(t1 - t0, 1);
  const iw = W - PAD.left - PAD.right;
  const ih = height - PAD.top - PAD.bottom;
  const x = (i: number) => PAD.left + (n <= 1 ? iw / 2 : ((times[i] - t0) / span) * iw);
  const y = (v: number) => PAD.top + ih - (Math.min(v, top) / top) * ih;
  const timeFmt = longRange ? 'DD.MM HH:mm' : 'HH:mm';

  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const svg = svgRef.current;
    if (!svg || n === 0) return;
    const r = svg.getBoundingClientRect();
    const px = ((e.clientX - r.left) / r.width) * W;
    const t = t0 + ((px - PAD.left) / iw) * span;
    // Ближайшая точка по времени (times отсортированы).
    let lo = 0;
    let hi = n - 1;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (times[mid] < t) lo = mid + 1;
      else hi = mid;
    }
    const i = lo > 0 && Math.abs(times[lo - 1] - t) < Math.abs(times[lo] - t) ? lo - 1 : lo;
    setHover(i);
  };

  const ticks = [0, 0.5, 1].map((f) => top * f);
  const xTicks = n > 1 ? [0, Math.floor((n - 1) / 2), n - 1] : n === 1 ? [0] : [];
  const hx = hover !== null ? x(hover) : 0;

  return (
    <div style={{ position: 'relative' }} data-testid={testId}>
      <svg
        ref={svgRef}
        viewBox={`0 0 ${W} ${height}`}
        width="100%"
        height={height}
        preserveAspectRatio="none"
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
        role="img"
      >
        {ticks.map((v) => (
          <g key={v}>
            <line x1={PAD.left} x2={W - PAD.right} y1={y(v)} y2={y(v)} stroke={token.colorBorderSecondary} strokeWidth={1} vectorEffect="non-scaling-stroke" />
            <text x={PAD.left - 6} y={y(v) + 4} textAnchor="end" fontSize={10} fill={token.colorTextTertiary}>
              {format(v)}
            </text>
          </g>
        ))}
        {xTicks.map((i, k) => (
          <text
            key={i}
            x={x(i)}
            y={height - 4}
            textAnchor={k === 0 ? 'start' : k === xTicks.length - 1 ? 'end' : 'middle'}
            fontSize={10}
            fill={token.colorTextTertiary}
          >
            {dayjs(times[i]).format(timeFmt)}
          </text>
        ))}
        {series.map((s) =>
          segments(s.values).map((seg, k) =>
            seg.length === 1 ? (
              <circle key={`${s.key}-${k}`} cx={x(seg[0][0])} cy={y(seg[0][1])} r={1.5} fill={s.color} />
            ) : (
              <polyline
                key={`${s.key}-${k}`}
                points={seg.map(([i, v]) => `${x(i)},${y(v)}`).join(' ')}
                fill="none"
                stroke={s.color}
                strokeWidth={1.5}
                vectorEffect="non-scaling-stroke"
                strokeLinejoin="round"
              />
            ),
          ),
        )}
        {hover !== null && (
          <line x1={hx} x2={hx} y1={PAD.top} y2={PAD.top + ih} stroke={token.colorTextQuaternary} strokeWidth={1} vectorEffect="non-scaling-stroke" />
        )}
      </svg>
      {hover !== null && (
        <div
          className="chart-tip"
          style={{
            left: `${(hx / W) * 100}%`,
            transform: hx > W / 2 ? 'translateX(calc(-100% - 8px))' : 'translateX(8px)',
            background: token.colorBgElevated,
            boxShadow: token.boxShadowSecondary,
            borderRadius: token.borderRadius,
          }}
        >
          <div className="chart-tip-time">{dayjs(times[hover]).format('DD.MM.YYYY HH:mm')}</div>
          {series.map((s) => (
            <div key={s.key}>
              <span className="chart-dot" style={{ background: s.color }} />
              {s.label}: <b>{s.values[hover] === null ? '-' : format(s.values[hover] as number)}</b>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// Легенда под графиком.
export function ChartLegend({ series }: { series: Pick<ChartSeries, 'key' | 'label' | 'color'>[] }) {
  return (
    <div className="chart-legend">
      {series.map((s) => (
        <span key={s.key}>
          <span className="chart-dot" style={{ background: s.color }} />
          {s.label}
        </span>
      ))}
    </div>
  );
}
