import { model, tokens, usd } from "@/lib/format";

export interface ModelShare {
  provider: string;
  model: string;
  total_tokens: number | null;
  cost_usd: number | null;
  calls: number | null;
}

/** Sorted horizontal bars, every row direct-labeled (single series, so no legend). */
export function ModelBars({ rows, totalTokens }: { rows: ModelShare[]; totalTokens?: number | null }) {
  if (rows.length === 0) return <p className="empty">No usage in this window yet.</p>;
  const max = Math.max(...rows.flatMap(r => r.total_tokens === null ? [] : [r.total_tokens]), 1);
  const total = totalTokens === undefined ? rows.some(r => r.total_tokens === null) ? null : rows.reduce((s, r) => s + (r.total_tokens as number), 0) : totalTokens;
  return (
    <div role="table" aria-label="Tokens by model">
      {rows.map((r) => (
        <div className="hbar-row" role="row" key={`${r.provider}/${r.model}`} title={`${r.provider} · ${r.calls === null ? "Unknown" : r.calls} calls · ${usd(r.cost_usd)}`}>
          <span className="name" role="cell">
            {model(r.model)} <span className="sub">{r.provider}</span>
          </span>
          <span className="hbar-track" role="cell">
            {r.total_tokens !== null && <span className="hbar-fill" style={{ display: "block", width: `${(r.total_tokens / max) * 100}%` }} />}
          </span>
          <span className="num" role="cell">
            {tokens(r.total_tokens)} {r.total_tokens !== null && total !== null && total > 0 && <span className="sub">{Math.round((r.total_tokens / total) * 100)}%</span>}
          </span>
        </div>
      ))}
    </div>
  );
}
