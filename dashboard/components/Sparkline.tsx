import { dayLabel, tokens } from "@/lib/format";

/** 30 tiny daily bars; native <title> tooltips per bar. Single series, no legend. */
export function Sparkline({ days, values, label }: { days: string[]; values: Record<string, number | null>; label: string }) {
  const w = 120;
  const h = 26;
  const gap = 1;
  const bw = (w - gap * (days.length - 1)) / days.length;
  const max = Math.max(1, ...days.map((d) => values[d] ?? 0));
  return (
    <svg viewBox={`0 0 ${w} ${h}`} width={w} height={h} role="img" aria-label={`${label}: daily tokens, last ${days.length} days`}>
      <line x1={0} x2={w} y1={h - 0.5} y2={h - 0.5} stroke="var(--grid)" strokeWidth={1} />
      {days.map((d, i) => {
        const v = Object.hasOwn(values, d) ? values[d] : 0;
        const bh = v === null ? h - 2 : v > 0 ? Math.max(2, (v / max) * (h - 2)) : 0;
        return (
          <rect key={d} x={i * (bw + gap)} y={h - bh} width={bw} height={bh} fill="var(--bar)" opacity={v === null ? 0.25 : 1}>
            <title>{`${dayLabel(d)}: ${tokens(v)} tokens`}</title>
          </rect>
        );
      })}
    </svg>
  );
}
