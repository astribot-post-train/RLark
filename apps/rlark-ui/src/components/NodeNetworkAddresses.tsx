import { useEffect, useState } from "react";
import { Copy } from "lucide-react";
import type { CRDNode } from "../types";
import { copyText } from "../utils/clipboard.js";
import {
  nodeNetworkModel,
  type NodeNetworkDisplay,
} from "../utils/nodeNetwork.js";
import { formatChinaDateTime } from "../utils/time.js";

export function NodeNetworkAddresses({
  node,
  network,
  zh,
}: {
  node: CRDNode;
  network?: NodeNetworkDisplay;
  zh: boolean;
}) {
  const model = nodeNetworkModel(node, network);
  const [feedback, setFeedback] = useState("");
  useEffect(() => {
    if (!feedback) return;
    const timeout = window.setTimeout(() => setFeedback(""), 2000);
    return () => window.clearTimeout(timeout);
  }, [feedback]);
  const messages = {
    unreported: zh ? "未上报" : "Not reported",
    gateway: zh ? "经站点网关" : "Via site gateway",
    stale: zh ? "上次上报" : "Last reported",
    unavailable: zh ? "数据不可用" : "Unavailable",
  };
  const copy = async (address: string) => {
    let ok = false;
    try {
      ok = await copyText(address);
    } catch {
      /* Report clipboard failures below. */
    }
    setFeedback(
      ok
        ? zh
          ? `已复制 ${address}`
          : `Copied ${address}`
        : zh
          ? "复制失败，请手动选择地址"
          : "Copy failed; select the address manually",
    );
  };
  return (
    <span className="node-network-addresses">
      {!model.addresses.some((entry) => entry.kind === "node") && (
        <span className="node-network-line">
          <span className="node-network-label">{zh ? "节点" : "Node"}</span>
          <span>—</span>
        </span>
      )}
      {model.addresses.map((entry) => (
        <span
          className="node-network-line"
          key={`${entry.kind}/${entry.interface ?? ""}/${entry.address}`}
        >
          <span className="node-network-label">
            {entry.kind === "wg" ? "WG" : zh ? "节点" : "Node"}
          </span>
          <code
            className="node-row-ip"
            title={[entry.interface, entry.cidr ?? entry.address]
              .filter(Boolean)
              .join(" · ")}
          >
            {entry.address}
          </code>
          {!(entry.kind === "wg" && model.message === "stale") && <button
            type="button"
            className="node-network-copy"
            aria-label={`${zh ? "复制" : "Copy"} ${entry.kind === "wg" ? "WG" : zh ? "节点 IP" : "node IP"} ${entry.address}`}
            title={zh ? "复制 IP" : "Copy IP"}
            onClick={(event) => {
              event.stopPropagation();
              void copy(entry.address);
            }}
          >
            <Copy size={12} />
          </button>}
        </span>
      ))}
      {model.message && (
        <span
          className={`node-network-line node-network-note ${model.message}`}
        >
          <span className="node-network-label">WG</span>
          <span>
            {messages[model.message]}
            {model.message === "stale" && model.reportedAt
              ? ` · ${formatChinaDateTime(model.reportedAt)}`
              : ""}
          </span>
        </span>
      )}
      <span className="node-network-feedback" role="status">
        {feedback}
      </span>
    </span>
  );
}
