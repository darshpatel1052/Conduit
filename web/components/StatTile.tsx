// A stat tile, not a chart: one number whose job is to be read, with a small
// amount of context beneath it. A single value plotted as a chart is a chart
// that wastes the space a chart costs.

export type TileTone = "neutral" | "good" | "warning" | "critical";

const TONE_COLOR: Record<TileTone, string> = {
  neutral: "var(--text-primary)",
  good: "var(--status-good)",
  warning: "var(--status-warning)",
  critical: "var(--status-critical)",
};

export function StatTile({
  label,
  value,
  unit,
  detail,
  tone = "neutral",
}: {
  label: string;
  value: string;
  unit?: string;
  detail?: string;
  tone?: TileTone;
}) {
  return (
    <div className="panel px-4 py-3">
      <div
        className="text-[11px] uppercase tracking-wider"
        style={{ color: "var(--text-muted)" }}
      >
        {label}
      </div>
      <div className="mt-1.5 flex items-baseline gap-1">
        <span
          className="text-2xl leading-none font-semibold"
          style={{ color: TONE_COLOR[tone] }}
        >
          {value}
        </span>
        {unit && (
          <span className="text-xs" style={{ color: "var(--text-muted)" }}>
            {unit}
          </span>
        )}
      </div>
      {detail && (
        <div className="mt-1.5 text-xs tabular" style={{ color: "var(--text-secondary)" }}>
          {detail}
        </div>
      )}
    </div>
  );
}
