import { model, tokens, usd } from "@/lib/format";

export interface ModelShare {
  provider: string;
  model: string;
  total_tokens: number;
  cost_usd: number;
  calls: number;
}

/** Sorted horizontal bars, every row direct-labeled (single series, so no legend). */
export function ModelBars({ rows }: { rows: ModelShare[] }) {
  if (rows.length === 0) return <p className="empty">No usage in this window yet.</p>;
  const max = Math.max(...rows.map((r) => r.total_tokens), 1);
  const total = rows.reduce((s, r) => s + r.total_tokens, 0);
  return (
    <div role="table" aria-label="Tokens by model">
      {rows.map((r) => (
        <div className="hbar-row" role="row" key={`${r.provider}/${r.model}`} title={`${r.provider} · ${r.calls} calls · ${usd(r.cost_usd)}`}>
          <span className="name" role="cell">
            {model(r.model)} <span className="sub">{r.provider}</span>
          </span>
          <span className="hbar-track" role="cell">
            <span className="hbar-fill" style={{ display: "block", width: `${(r.total_tokens / max) * 100}%` }} />
          </span>
          <span className="num" role="cell">
            {tokens(r.total_tokens)} <span className="sub">{Math.round((r.total_tokens / total) * 100)}%</span>
          </span>
        </div>
      ))}
    </div>
  );
}
