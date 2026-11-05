import { useId } from "react";

// Status-tone charts for the container cluster console. Colors come from the
// shared theme tokens (see style.css `.tone-*`); every mark also carries a
// visible label and count, so state is never conveyed by color alone.
export type Tone = "good" | "warn" | "bad" | "info" | "neutral";
export type Segment = { label: string; value: number; tone: Tone };

const R = 26;
const STROKE = 8;
const C = 2 * Math.PI * R;
// Surface gap between adjacent arcs, in circumference units.
const GAP = 2.5;

export function Donut({
  title,
  center,
  segments,
}: {
  title: string;
  center: string;
  segments: Segment[];
}) {
  const total = segments.reduce((sum, s) => sum + s.value, 0);
  const visible = segments.filter((s) => s.value > 0);
  let offset = 0;
  return (
    <figure className="chart-card">
      <figcaption>{title}</figcaption>
      <div className="chart-body">
        <svg
          className="donut"
          viewBox="0 0 64 64"
          role="img"
          aria-label={`${title}: ${segments.map((s) => `${s.label} ${s.value}`).join(", ")}`}
        >
          <circle className="donut-track" cx="32" cy="32" r={R} />
          {total > 0 &&
            visible.map((s) => {
              const length = (s.value / total) * C;
              const gap = visible.length > 1 ? GAP : 0;
              const arc = (
                <circle
                  key={s.label}
                  className={`donut-arc tone-${s.tone}`}
                  cx="32"
                  cy="32"
                  r={R}
                  strokeWidth={STROKE}
                  strokeDasharray={`${Math.max(length - gap, 0.01)} ${C}`}
                  strokeDashoffset={-offset}
                >
                  <title>{`${s.label}: ${s.value}`}</title>
                </circle>
              );
              offset += length;
              return arc;
            })}
          <text x="32" y="32" className="donut-center">
            {center}
          </text>
        </svg>
        <Legend segments={segments} />
      </div>
    </figure>
  );
}

// Horizontal 100% bar for distributions that read better as a proportion.
export function SegmentBar({
  title,
  center,
  segments,
}: {
  title: string;
  center: string;
  segments: Segment[];
}) {
  const total = segments.reduce((sum, s) => sum + s.value, 0);
  return (
    <figure className="chart-card">
      <figcaption>
        {title}
        <strong className="chart-headline">{center}</strong>
      </figcaption>
      <div
        className="segment-bar"
        role="img"
        aria-label={`${title}: ${segments.map((s) => `${s.label} ${s.value}`).join(", ")}`}
      >
        {total === 0 ? (
          <span className="segment-empty" />
        ) : (
          segments
            .filter((s) => s.value > 0)
            .map((s) => (
              <span
                key={s.label}
                className={`segment tone-${s.tone}`}
                style={{ flexGrow: s.value }}
                title={`${s.label}: ${s.value}`}
              />
            ))
        )}
      </div>
      <Legend segments={segments} />
    </figure>
  );
}

function Legend({ segments }: { segments: Segment[] }) {
  return (
    <ul className="chart-legend">
      {segments.map((s) => (
        <li key={s.label}>
          <i className={`tone-${s.tone}`} aria-hidden="true" />
          <span>{s.label}</span>
          <b>{s.value}</b>
        </li>
      ))}
    </ul>
  );
}

// A labelled status dot whose detail (reason/message) is exposed as a
// keyboard-focusable tooltip instead of inline prose.
export function StatusDot({
  label,
  tone,
  detail,
}: {
  label: string;
  tone: Tone;
  detail?: string;
}) {
  const id = useId();
  return (
    <span
      className="status-dot"
      tabIndex={detail ? 0 : undefined}
      aria-describedby={detail ? id : undefined}
    >
      <i className={`tone-${tone}`} aria-hidden="true" />
      {label}
      {detail && (
        <span role="tooltip" id={id} className="tip">
          {detail}
        </span>
      )}
    </span>
  );
}

// An ⓘ affordance replacing paragraphs of helper copy.
export function Hint({ text }: { text: string }) {
  const id = useId();
  return (
    <span className="hint" tabIndex={0} aria-describedby={id} aria-label="ⓘ">
      ⓘ
      <span role="tooltip" id={id} className="tip">
        {text}
      </span>
    </span>
  );
}
