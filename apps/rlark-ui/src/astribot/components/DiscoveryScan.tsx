import { Radar } from "lucide-react";
import type { SiteDiscoveryScan } from "../api";
import "./DiscoveryScan.css";

export function DiscoveryScanControl({
  zh,
  scan,
  starting,
  start,
  error,
  disabledReason,
}: {
  zh: boolean;
  scan: SiteDiscoveryScan | null | undefined;
  starting: boolean;
  start: () => Promise<void>;
  error: string;
  disabledReason?: string;
}) {
  const running = starting || scan?.phase === "Running";
  let detail = disabledReason;
  if (!detail && scan) {
    const completed = `${scan.completed}/${scan.total} Hub`;
    const time = new Date(scan.startedAt).toLocaleString(
      zh ? "zh-CN" : "en-GB",
      { hour12: false },
    );
    switch (scan.phase) {
      case "Running":
        detail = zh ? `${completed} 已返回结果` : `${completed} reported`;
        break;
      case "Succeeded":
        detail = zh ? `上次扫描 ${time}` : `Last scan ${time}`;
        break;
      case "Failed":
        detail = zh
          ? `${scan.failed} 个 Hub 扫描失败，可重试。`
          : `${scan.failed} Hub scan(s) failed. Try again.`;
        break;
      case "TimedOut":
        detail = zh
          ? `扫描超时，${completed} 已返回结果。`
          : `Scan timed out. ${completed} reported.`;
        break;
    }
  }
  return (
    <div className="station-discovery-control">
      <button
        type="button"
        className="primary-button"
        disabled={running || Boolean(disabledReason)}
        onClick={() => void start()}
      >
        <Radar size={15} aria-hidden="true" />
        {running
          ? zh
            ? "扫描中…"
            : "Scanning…"
          : zh
            ? "开始扫描"
            : "Start scan"}
      </button>
      {detail && <small role="status">{detail}</small>}
      {error && (
        <small className="station-discovery-error" role="alert">
          {error}
        </small>
      )}
    </div>
  );
}
