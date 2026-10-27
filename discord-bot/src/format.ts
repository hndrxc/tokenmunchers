function trim(n: number): string {
  return n >= 100 ? n.toFixed(0) : n >= 10 ? n.toFixed(1).replace(/\.0$/, "") : n.toFixed(2).replace(/\.?0+$/, "");
}

export function formatTokens(n: number | bigint): string {
  const num = typeof n === "bigint" ? Number(n) : n;
  if (num >= 1e9) return `${trim(num / 1e9)}B`;
  if (num >= 1e6) return `${trim(num / 1e6)}M`;
  if (num >= 1e3) return `${trim(num / 1e3)}k`;
  return String(Math.round(num));
}

export function formatUsd(n: number): string {
  if (n === 0) return "$0";
  if (n < 0.01) return "<$0.01";
  if (n >= 1000) return `$${Math.round(n).toLocaleString("en-US")}`;
  return `$${n.toFixed(2)}`;
}

export function escapeDiscordMarkdown(text: string): string {
  const singleSpaced = text.replace(/\s+/g, " ");
  return singleSpaced.replace(/([\\*_`~|<>:@#])/g, "\\$1");
}

export function truncate(text: string, maxLength: number): string {
  if (text.length <= maxLength) return text;
  return text.slice(0, Math.max(0, maxLength - 1)) + "…";
}
