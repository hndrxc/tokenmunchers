"use client";

import { useEffect, useRef, useState } from "react";

import { dayLabel, tokens } from "@/lib/format";

interface Point {
  day: string;
  value: number;
}

/** Single-series daily bar chart with hover tooltip and a table view. */
export function DailyBars({ points, unit = "tokens" }: { points: Point[]; unit?: string }) {
  const [hover, setHover] = useState<number | null>(null);
  const ref = useRef<HTMLDivElement>(null);
  const [w, setW] = useState(640);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) => setW(Math.max(240, Math.round(entry.contentRect.width))));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  const h = 170;
  const padL = 40;
  const padB = 22;
  const padT = 8;
  const plotW = w - padL;
  const plotH = h - padB - padT;
  const max = niceMax(Math.max(0, ...points.map((p) => p.value)));
  const col = plotW / points.length;
  const gap = 2;
  const bw = Math.max(2, col - gap);
  const y = (v: number) => padT + plotH - (v / max) * plotH;
  const ticks = [0, max / 2, max];

  return (
    <div className="chart" ref={ref}>
      <svg viewBox={`0 0 ${w} ${h}`} width={w} height={h} role="img" aria-label={`Daily ${unit}, last ${points.length} days`} onMouseLeave={() => setHover(null)}>
        {ticks.map((t) => (
          <g key={t}>
            <line x1={padL} x2={w} y1={y(t)} y2={y(t)} stroke={t === 0 ? "var(--axis)" : "var(--grid)"} strokeWidth={1} />
            <text className="tick" x={padL - 6} y={y(t) + 4} textAnchor="end">
              {tokens(t)}
            </text>
          </g>
        ))}
        {points.map((p, i) => {
          const x = padL + i * col + gap / 2;
          const bh = p.value > 0 ? Math.max(2, (p.value / max) * plotH) : 0;
          return (
            <g key={p.day}>
              {bh > 0 && <path d={roundedTop(x, y(0) - bh, bw, bh, Math.min(4, bw / 2))} fill={hover === i ? "var(--bar-hover)" : "var(--bar)"} />}
              <rect x={padL + i * col} y={padT} width={col} height={plotH} fill="transparent" onMouseEnter={() => setHover(i)} />
              {((w >= 480 ? i % 7 === (points.length - 1) % 7 : i % 14 === (points.length - 1) % 14) || i === points.length - 1) && (
                <text className="tick" x={x + bw / 2} y={h - 6} textAnchor="middle">
                  {i === points.length - 1 ? "Today" : dayLabel(p.day)}
                </text>
              )}
            </g>
          );
        })}
      </svg>
      {hover !== null && (
        <div className="tooltip" style={{ left: `${((padL + hover * col + col / 2) / w) * 100}%`, top: `${(y(points[hover].value) / h) * 100}%` }}>
          <strong>{tokens(points[hover].value)} {unit}</strong>
          {hover === points.length - 1 ? "Today" : dayLabel(points[hover].day)}
        </div>
      )}
      <details className="table-view">
        <summary>Show as table</summary>
        <table>
          <thead>
            <tr>
              <th>Day</th>
              <th>{unit}</th>
            </tr>
          </thead>
          <tbody>
            {points.map((p) => (
              <tr key={p.day}>
                <td>{p.day}</td>
                <td>{p.value.toLocaleString("en-US")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </details>
    </div>
  );
}

function niceMax(v: number): number {
  if (v <= 0) return 1000;
  const mag = 10 ** Math.floor(Math.log10(v));
  for (const m of [1, 2, 2.5, 5, 10]) if (v <= m * mag) return m * mag;
  return 10 * mag;
}

function roundedTop(x: number, y: number, w: number, h: number, r: number): string {
  const rr = Math.min(r, h);
  return `M${x},${y + h}V${y + rr}Q${x},${y} ${x + rr},${y}H${x + w - rr}Q${x + w},${y} ${x + w},${y + rr}V${y + h}Z`;
}
