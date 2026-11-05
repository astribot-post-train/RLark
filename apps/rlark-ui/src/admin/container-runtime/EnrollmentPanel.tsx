import { useEffect, useRef, useState } from "react";
import type {
  Client,
  Detail,
  Enrollment,
  EnrollmentRequest,
  EnrollmentTarget,
} from "./api";
import {
  enrollmentIntent,
  enrollmentTargets,
  parseRetainedEnrollment,
  terminalEnrollmentPhases as terminalPhases,
} from "./enrollment";
import { StatusDot, type Tone } from "./Charts";

export function EnrollmentPanel({
  detail,
  client,
  lang,
  onChanged,
  blocked,
  focusNode,
}: {
  detail: Detail;
  client: Client;
  lang: "zh" | "en";
  onChanged: () => void;
  blocked: boolean;
  // Node name whose SSH address input should receive focus (row shortcut).
  focusNode?: { name: string; seq: number };
}) {
  const zh = lang === "zh";
  const key = `rlark.enrollment.${detail.cluster.uid}`;
  const [retained, setRetained] = useState<EnrollmentRequest | undefined>(
    () => {
      try {
        return parseRetainedEnrollment(
          sessionStorage.getItem(key),
          detail.cluster.uid,
        );
      } catch {
        return undefined;
      }
    },
  );
  const [nodes, setNodes] = useState(
    () => retained?.nodes ?? enrollmentTargets(detail),
  );
  const [release, setRelease] = useState(retained?.release ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const controller = useRef<AbortController | undefined>(undefined);
  useEffect(() => {
    if (retained) return;
    const known = enrollmentTargets(detail);
    setNodes((current) =>
      current.map((node) => {
        const observed = known.find(
          (n) => n.name === node.name && n.address === node.address,
        );
        return observed?.nodeUID && !node.nodeUID
          ? { ...node, nodeUID: observed.nodeUID }
          : node;
      }),
    );
  }, [detail, retained]);
  useEffect(() => () => controller.current?.abort(), []);
  const root = useRef<HTMLElement>(null);
  useEffect(() => {
    if (!focusNode) return;
    // Wait one frame so the drawer is visible before moving focus.
    const frame = requestAnimationFrame(() => {
      const input = [
        ...(root.current?.querySelectorAll<HTMLInputElement>(
          "input[data-enroll-address]",
        ) ?? []),
      ].find((el) => el.dataset.enrollAddress === focusNode.name);
      input?.scrollIntoView({ block: "center" });
      input?.focus();
      input?.select();
    });
    return () => cancelAnimationFrame(frame);
  }, [focusNode]);
  function update(index: number, patch: Partial<EnrollmentTarget>) {
    setNodes((current) =>
      current.map((n, i) => (i === index ? { ...n, ...patch } : n)),
    );
  }
  async function submit() {
    let request: EnrollmentRequest;
    try {
      request =
        retained ??
        enrollmentIntent(
          crypto.randomUUID(),
          detail.cluster.uid,
          release,
          nodes,
        );
    } catch (e) {
      setError(String(e));
      return;
    }
    setRetained(request);
    try {
      sessionStorage.setItem(key, JSON.stringify(request));
    } catch {
      /* Same mounted form retains the request. */
    }
    setBusy(true);
    setError("");
    const active = new AbortController();
    controller.current = active;
    try {
      await client.startNodes(detail.cluster, request, active.signal);
      if (active.signal.aborted) return;
      sessionStorage.removeItem(key);
      setRetained(undefined);
      onChanged();
    } catch (e) {
      if (!active.signal.aborted)
        setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (!active.signal.aborted) setBusy(false);
    }
  }
  const disabled = busy || !!retained;
  return (
    <section className="enrollment-panel" ref={root}>
      <p className="muted enrollment-lead">
        {zh
          ? "A 为网关兼计算节点，B/C 经 A 的 SSH 接入。私钥不经页面传输。"
          : "A is the gateway and compute node; workers join through A over SSH. Private keys never pass through this page."}
      </p>
      {!detail.enrollmentEnabled && (
        <p className="notice">
          {zh
            ? "管理员尚未启用 SSH 接入控制器和 Gateway 接入入口。"
            : "SSH enrollment controller and Gateway capability must be enabled by an administrator."}
        </p>
      )}
      <fieldset disabled={disabled || blocked || !detail.enrollmentEnabled}>
        <label>
          {zh ? "Agent 版本" : "Agent version"}
          <select value={release} onChange={(e) => setRelease(e.target.value)}>
            <option value="">
              {zh
                ? "最新可用版本（提交时固定）"
                : "Latest available (pinned on submit)"}
            </option>
            {detail.releases
              .filter(
                (r) =>
                  r.available && r.os === "linux" && r.architecture === "amd64",
              )
              .map((r) => (
                <option key={r.uid} value={r.name}>
                  {r.version} · {r.name}
                </option>
              ))}
          </select>
        </label>
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                {(zh
                  ? ["节点名", "SSH IP 地址", "SSH 端口", "角色", "操作"]
                  : ["Node", "SSH IP address", "SSH port", "Role", "Actions"]
                ).map((s) => (
                  <th key={s}>{s}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {nodes.map((node, i) => (
                <tr key={i}>
                  <td>
                    <input
                      aria-label={zh ? "节点名" : "Node name"}
                      value={node.name}
                      readOnly={!!node.nodeUID}
                      onChange={(e) => update(i, { name: e.target.value })}
                    />
                  </td>
                  <td>
                    <input
                      aria-label={zh ? "SSH IP 地址" : "SSH IP address"}
                      data-enroll-address={node.name}
                      value={node.address}
                      onChange={(e) => update(i, { address: e.target.value })}
                    />
                  </td>
                  <td>
                    <input
                      aria-label={zh ? "SSH 端口" : "SSH port"}
                      type="number"
                      min="1"
                      max="65535"
                      value={node.port}
                      onChange={(e) =>
                        update(i, { port: Number(e.target.value) })
                      }
                    />
                  </td>
                  <td>
                    <select
                      aria-label={zh ? "角色" : "Role"}
                      value={node.role}
                      disabled={!!node.nodeUID}
                      onChange={(e) =>
                        update(i, {
                          role: e.target.value as EnrollmentTarget["role"],
                        })
                      }
                    >
                      <option value="Gateway">
                        {zh ? "网关 + 计算" : "Gateway + compute"}
                      </option>
                      <option value="Worker">Worker</option>
                    </select>
                  </td>
                  <td>
                    <button
                      type="button"
                      onClick={() =>
                        setNodes((current) =>
                          current.filter((_, index) => i !== index),
                        )
                      }
                    >
                      {zh ? "移除此行" : "Remove row"}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <button
          type="button"
          onClick={() =>
            setNodes((current) => [
              ...current,
              {
                name: `node-${current.length + 1}`,
                address: "",
                role: "Worker",
                port: 22,
              },
            ])
          }
        >
          {zh ? "增加 Notebook 节点" : "Add Notebook node"}
        </button>
      </fieldset>
      <p className="muted">
        {zh
          ? "主机停机时请先在金山云启动。更换实例/IP 不会自动接管旧节点。"
          : "Start stopped Notebooks in Kingsoft Cloud first. Replaced instances/IPs are never silently adopted."}
      </p>
      {retained && (
        <p className="notice">
          {zh
            ? "结果待确认，请保留原请求并重试："
            : "Unconfirmed result; retry the retained request: "}
          {retained.operationID}
        </p>
      )}
      {error && <p role="alert">{error}</p>}
      <button
        className="primary"
        disabled={busy || blocked || !detail.enrollmentEnabled}
        onClick={() => void submit()}
      >
        {busy
          ? zh
            ? "提交中…"
            : "Submitting…"
          : retained
            ? zh
              ? "重试同一接入操作"
              : "Retry same operation"
            : zh
              ? "保存并启动接入（全部服务）"
              : "Save and start / join (all services)"}
      </button>
    </section>
  );
}

function phaseTone(op: Enrollment): Tone {
  if (op.cancelRequested || op.phase === "Cancelled") return "neutral";
  if (op.phase === "Succeeded") return "good";
  if (op.phase === "Failed") return "bad";
  return "info";
}
// Enrollment operations as a table. Cancellation keeps its own request state
// so it is independent from the drawer form; the API call is unchanged.
export function EnrollmentOperations({
  detail,
  client,
  lang,
  onChanged,
  blocked,
}: {
  detail: Detail;
  client: Client;
  lang: "zh" | "en";
  onChanged: () => void;
  blocked: boolean;
}) {
  const zh = lang === "zh";
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const controller = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => controller.current?.abort(), []);
  async function cancel(operation: Enrollment) {
    setBusy(true);
    setError("");
    const active = new AbortController();
    controller.current = active;
    try {
      await client.cancelEnrollment(detail.cluster, operation, active.signal);
      if (!active.signal.aborted) onChanged();
    } catch (e) {
      if (!active.signal.aborted)
        setError(e instanceof Error ? e.message : String(e));
    } finally {
      if (!active.signal.aborted) setBusy(false);
    }
  }
  if (!detail.enrollments.length)
    return (
      <p className="muted table-empty">
        {zh ? "暂无接入操作" : "No enrollment operations"}
      </p>
    );
  const head = zh
    ? ["操作 ID", "版本", "阶段", "节点进度", "操作"]
    : ["Operation", "Release", "Phase", "Node progress", "Actions"];
  return (
    <>
      {error && (
        <p role="alert" className="error tab-alert">
          {error}
        </p>
      )}
      <div className="table-scroll">
        <table className="enrollment-ops">
          <thead>
            <tr>
              {head.map((s, i) => (
                <th key={s} className={i === head.length - 1 ? "col-actions" : undefined}>
                  {s}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {detail.enrollments.map((op) => (
              <tr key={op.uid}>
                <td>
                  <code>{op.operationID}</code>
                </td>
                <td>{op.releaseVersion || op.releaseName}</td>
                <td>
                  <StatusDot
                    label={
                      op.cancelRequested
                        ? zh ? "取消中/已取消" : "Cancellation requested"
                        : op.phase || (zh ? "待处理" : "Pending")
                    }
                    tone={phaseTone(op)}
                    detail={
                      op.conditions?.length
                        ? op.conditions.map((c) => `${c.reason}: ${c.message}`).join(" · ")
                        : undefined
                    }
                  />
                </td>
                <td>
                  <ul className="op-nodes">
                    {op.targets.map((target) => {
                      const status = op.nodes?.find((n) => n.name === target.name);
                      return (
                        <li key={target.name}>
                          <b>{target.name}</b>
                          <span>{target.role} · {target.address}</span>
                          <span>
                            {status?.reachability || "Unknown"} · {status?.phase || "Pending"}
                            {status?.reason ? ` · ${status.reason}` : ""}
                            {status?.services?.length ? ` · ${status.services.join(", ")}` : ""}
                          </span>
                        </li>
                      );
                    })}
                  </ul>
                </td>
                <td className="col-actions">
                  {!terminalPhases.includes(op.phase) && !op.cancelRequested && (
                    <button
                      type="button"
                      disabled={busy || blocked || !detail.enrollmentEnabled}
                      title={zh ? "保留已接入节点" : "Joined nodes are retained"}
                      onClick={() => void cancel(op)}
                    >
                      {zh ? "取消接入" : "Cancel"}
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}
