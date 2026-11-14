import { useEffect, useState } from "react";
import { Check, Copy as CopyIcon, KeyRound, X } from "lucide-react";
import { nodeAdmissionsApi } from "../backend";
import type { ClusterSummary } from "../types";
import { copyText } from "../utils/clipboard";
import {
  parseLabelLines,
  parseTaintLines,
  robotTaint,
  s1uRobotLabels,
  type IssuedNodeAdmission,
} from "../utils/siteNodeDisplay";

const ttlOptions = [
  { seconds: 7200, zh: "2 小时", en: "2 hours" },
  { seconds: 86400, zh: "24 小时", en: "24 hours" },
  { seconds: 604800, zh: "7 天", en: "7 days" },
];

function CopyButton({ value, zh }: { value: string; zh: boolean }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      className={copied ? "secondary-button copy-confirmed" : "secondary-button"}
      onClick={() => void copyText(value).then((ok) => {
        setCopied(ok);
        if (ok) window.setTimeout(() => setCopied(false), 1500);
      })}
    >
      {copied ? <Check size={14} /> : <CopyIcon size={14} />}{copied ? (zh ? "已复制" : "Copied") : (zh ? "复制" : "Copy")}
    </button>
  );
}

// The answer that carries the code: shown once, never fetched again.
export function IssuedJoinCode({ issued, zh }: { issued: IssuedNodeAdmission; zh: boolean }) {
  return (
    <section className="station-drawer-section join-code-result" aria-live="polite">
      <div className="cert-error join-code-once" role="note">
        {zh ? "入网码和命令只显示这一次，关闭后无法再查看；丢了就在入网记录里重发。" : "The code and the command are shown this once. If they are lost, reissue from the admission records."}
      </div>
      <div className="join-code-field">
        <span className="muted">{zh ? "入网码" : "Join code"}</span>
        <code className="inline-code join-code-value">{issued.code}</code>
        <CopyButton value={issued.code} zh={zh} />
      </div>
      <div className="join-code-field join-code-command">
        <span className="muted">{zh ? `在 ${issued.admission.node_name} 上以有 sudo 权限的用户执行这一行：` : `Run this line on ${issued.admission.node_name} as a user with sudo:`}</span>
        <pre className="join-code-line">{issued.command}</pre>
        <CopyButton value={issued.command} zh={zh} />
      </div>
      <small className="muted">
        {zh
          ? `有效至 ${new Date(issued.admission.expires_at).toLocaleString()}，用过一次即失效。机器上的 WireGuard 私钥在本机生成，不离开本机。`
          : `Valid until ${new Date(issued.admission.expires_at).toLocaleString()} and for one machine. The machine's WireGuard private key is made on it and never leaves it.`}
      </small>
    </section>
  );
}

export function JoinNodeDrawer({
  zh,
  clusters,
  clusterID: initialClusterID,
  apiServerHints,
  issued: initialIssued,
  onIssued,
  onClose,
}: {
  zh: boolean;
  clusters: ClusterSummary[];
  clusterID: string;
  // The API server the cluster's latest admission used, by cluster ID.
  apiServerHints: Record<string, string>;
  issued?: IssuedNodeAdmission;
  onIssued: (clusterID: string) => void;
  onClose: () => void;
}) {
  const [issued, setIssued] = useState(initialIssued);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [form, setForm] = useState(() => {
    const clusterID = initialClusterID || clusters[0]?.id || "";
    return {
      clusterID, nodeName: "", apiServer: apiServerHints[clusterID] ?? "", kind: "robot" as "robot" | "other", robotID: "",
      labels: s1uRobotLabels(""), taints: robotTaint, site: "", address: "", uplink: "", ttl: 7200,
    };
  });
  const update = (patch: Partial<typeof form>) => setForm((current) => ({ ...current, ...patch }));

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !pending) onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  const labels = parseLabelLines(form.labels);
  const taints = parseTaintLines(form.taints);
  const ready = form.clusterID && form.nodeName.trim() && /^[^:\s]+:\d+$/.test(form.apiServer.trim()) && !labels.error && !taints.error &&
    (form.kind === "other" || form.robotID.trim());

  const submit = async () => {
    if (pending || !ready) return;
    setPending(true);
    setError("");
    try {
      const answer = await nodeAdmissionsApi.issue({
        cluster_id: form.clusterID,
        node_name: form.nodeName.trim(),
        api_server: form.apiServer.trim(),
        ...(form.site.trim() ? { site: form.site.trim() } : {}),
        ...(form.address.trim() ? { address: form.address.trim() } : {}),
        ...(form.uplink.trim() ? { uplink: form.uplink.trim() } : {}),
        ...(Object.keys(labels.labels).length ? { labels: labels.labels } : {}),
        ...(taints.taints.length ? { taints: taints.taints } : {}),
        ttl_seconds: form.ttl,
      });
      setIssued(answer);
      onIssued(form.clusterID);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setPending(false);
    }
  };

  return <>
    <div className="addon-drawer-overlay stations-operation-overlay" onClick={() => !pending && onClose()} />
    <aside className="addon-drawer station-drawer station-operation-drawer" role="dialog" aria-modal="true" aria-labelledby="join-node-title">
      <div className="addon-drawer-header station-drawer-header">
        <div>
          <span className="eyebrow"><KeyRound size={13} />{zh ? "入网码" : "Join code"}</span>
          <h3 id="join-node-title">{issued ? (zh ? `${issued.admission.node_name} 的入网码` : `Join code for ${issued.admission.node_name}`) : (zh ? "加入节点" : "Add a node")}</h3>
          <small className="muted">{zh ? "给一台全新的机器发一次性入网码；在机器上执行一行命令，它就加入集群、建好隧道、出现在场站节点列表里。" : "Give a new machine a one-time code. One line on the machine joins it to the cluster, brings up its tunnel, and lists it here."}</small>
        </div>
        <button type="button" className="icon-button" aria-label={zh ? "关闭" : "Close"} title={zh ? "关闭" : "Close"} onClick={onClose} disabled={pending}><X size={16} /></button>
      </div>
      <div className="addon-drawer-body station-drawer-body">
        {issued ? <IssuedJoinCode issued={issued} zh={zh} /> : <>
          <label className="station-field">
            <span>{zh ? "集群" : "Cluster"}</span>
            <select value={form.clusterID} onChange={(event) => update({ clusterID: event.target.value, apiServer: apiServerHints[event.target.value] ?? form.apiServer })}>
              {clusters.map((cluster) => <option key={cluster.id} value={cluster.id}>{cluster.name} ({cluster.agentNamespace})</option>)}
            </select>
          </label>
          <label className="station-field"><span>{zh ? "节点名" : "Node name"}</span><input value={form.nodeName} onChange={(event) => update({ nodeName: event.target.value })} maxLength={253} autoComplete="off" placeholder="s1u-09-x86" /></label>
          <div className="station-join-auth" role="group" aria-label={zh ? "机器类型" : "Machine kind"}>
            <button type="button" className={form.kind === "robot" ? "is-selected" : ""} onClick={() => update({ kind: "robot", labels: s1uRobotLabels(form.robotID.trim()), taints: robotTaint })}>{zh ? "S1U 机器人" : "S1U robot"}</button>
            <button type="button" className={form.kind === "other" ? "is-selected" : ""} onClick={() => update({ kind: "other", labels: "", taints: "" })}>{zh ? "其他机器" : "Other machine"}</button>
          </div>
          {form.kind === "robot" && <label className="station-field"><span>{zh ? "机器人编号" : "Robot ID"}</span><input value={form.robotID} onChange={(event) => update({ robotID: event.target.value, labels: s1uRobotLabels(event.target.value.trim()) })} maxLength={63} autoComplete="off" placeholder="s1u-09" /></label>}
          <label className="station-field">
            <span>{zh ? "节点标签（每行 key=value）" : "Node labels (key=value per line)"}</span>
            <textarea value={form.labels} onChange={(event) => update({ labels: event.target.value })} rows={4} spellCheck={false} />
            {labels.error && <small className="cert-error">{zh ? `看不懂这一行：${labels.error}` : `Cannot read: ${labels.error}`}</small>}
          </label>
          <label className="station-field">
            <span>{zh ? "污点（每行 key=value:Effect）" : "Taints (key=value:Effect per line)"}</span>
            <textarea value={form.taints} onChange={(event) => update({ taints: event.target.value })} rows={2} spellCheck={false} />
            {taints.error && <small className="cert-error">{zh ? `看不懂这一行：${taints.error}` : `Cannot read: ${taints.error}`}</small>}
          </label>
          <label className="station-field"><span>{zh ? "集群 API 地址（机器能直接到达的 host:port）" : "Cluster API (host:port the machine reaches)"}</span><input value={form.apiServer} onChange={(event) => update({ apiServer: event.target.value })} maxLength={253} autoComplete="off" placeholder="10.11.14.128:6443" /></label>
          <div className="station-join-grid">
            <label className="station-field"><span>{zh ? "场站（不填用集群所在的）" : "Site (default: the cluster's)"}</span><input value={form.site} onChange={(event) => update({ site: event.target.value })} maxLength={63} autoComplete="off" /></label>
            <label className="station-field">
              <span>{zh ? "入网码有效期" : "Code valid for"}</span>
              <select value={form.ttl} onChange={(event) => update({ ttl: Number(event.target.value) })}>
                {ttlOptions.map((option) => <option key={option.seconds} value={option.seconds}>{zh ? option.zh : option.en}</option>)}
              </select>
            </label>
          </div>
          <details className="join-node-advanced">
            <summary>{zh ? "高级" : "Advanced"}</summary>
            <div className="station-join-grid">
              <label className="station-field"><span>{zh ? "固定隧道地址" : "Pinned tunnel address"}</span><input value={form.address} onChange={(event) => update({ address: event.target.value })} maxLength={15} autoComplete="off" placeholder={zh ? "不填自动分配" : "allocated when empty"} /></label>
              <label className="station-field"><span>{zh ? "上联网关" : "Uplink gateway"}</span><input value={form.uplink} onChange={(event) => update({ uplink: event.target.value })} maxLength={15} autoComplete="off" placeholder={zh ? "默认路由出不去时" : "when the default route does not lead out"} /></label>
            </div>
          </details>
          {error && <div className="cert-error" role="alert">{error}</div>}
          <small className="muted">{zh ? "管理面只保存入网码的哈希。同名节点已有入网记录时，新码替换它声明的内容并让它重新入网。" : "The management plane keeps only the code's hash. For a node that has an admission, a new code replaces what it declares and lets it join again."}</small>
        </>}
      </div>
      <div className="addon-drawer-footer">
        <button type="button" className="secondary-button" onClick={onClose} disabled={pending}>{zh ? "关闭" : "Close"}</button>
        {!issued && <button type="button" className="primary-button" onClick={() => void submit()} disabled={pending || !ready}>{pending ? (zh ? "发码中…" : "Issuing…") : (zh ? "发入网码" : "Issue code")}</button>}
      </div>
    </aside>
  </>;
}
