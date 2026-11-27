import { useEffect, useRef, useState, type FormEvent } from "react";
import { X } from "lucide-react";
import { ApiError } from "../../api";
import { clusterProvisionsApi } from "../api";
import {
  migrationConfirmation,
  type MigrationCandidate,
  type MigrationCandidateKey,
  type MigrationPreview,
} from "../lib/provisionMigration";
import "./provisionMigration.css";

const labels: Record<MigrationCandidateKey, [string, string]> = {
  cluster: ["管理面集群", "Management Cluster"],
  "site-network": ["站点网络声明", "Site network declaration"],
  "station-read": ["场站读取凭据", "Station read credential"],
  "station-operations": ["场站操作凭据", "Station operation credential"],
  release: ["发布凭据", "Release credential"],
  "agent-certificate": ["Agent 证书", "Agent certificate"],
};

const candidateStates: Record<MigrationCandidate["state"], [string, string]> = {
  review: ["待核对", "Review required"],
  recorded: ["已记录", "Recorded"],
  absent: ["未找到", "Absent"],
  blocked: [
    "归属冲突或身份不完整",
    "Ownership conflict or incomplete identity",
  ],
};

export function ProvisionMigrationDialog({
  name,
  uid,
  zh,
  onClose,
  onRecorded,
}: {
  name: string;
  uid: string;
  zh: boolean;
  onClose: () => void;
  onRecorded: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [reload, setReload] = useState(0);
  const [preview, setPreview] = useState<MigrationPreview>();
  const [selected, setSelected] = useState<Set<MigrationCandidateKey>>(
    () => new Set(),
  );
  const [evidence, setEvidence] = useState("");
  const [password, setPassword] = useState("");
  const [hostUID, setHostUID] = useState<string>();
  const [hostConfirmed, setHostConfirmed] = useState(false);
  const [loading, setLoading] = useState(true);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const [stale, setStale] = useState(false);

  useEffect(() => {
    const element = dialog.current;
    element?.showModal();
    return () => element?.close();
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    void clusterProvisionsApi
      .migrationCandidates(name, controller.signal)
      .then((result) => {
        if (controller.signal.aborted) return;
        if (result.provision_uid !== uid) {
          setError(
            zh
              ? "记录已被替换，请关闭后重新选择。"
              : "This record was replaced. Close and select it again.",
          );
          return;
        }
        setPreview(result);
      })
      .catch((reason: unknown) => {
        if (!controller.signal.aborted)
          setError(reason instanceof Error ? reason.message : String(reason));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [name, uid, reload, zh]);

  function refresh() {
    setPreview(undefined);
    setSelected(new Set());
    setHostUID(undefined);
    setHostConfirmed(false);
    setPassword("");
    setError("");
    setStale(false);
    setLoading(true);
    setReload((value) => value + 1);
  }

  function actionError(reason: unknown) {
    if (
      reason instanceof ApiError &&
      (reason.status === 409 || reason.status === 404)
    ) {
      setStale(true);
      setError(
        zh
          ? "候选已变化，请重新加载并核对。"
          : "The candidates changed. Reload and review them again.",
      );
    } else {
      setError(reason instanceof Error ? reason.message : String(reason));
    }
  }

  const confirmation = preview
    ? migrationConfirmation(
        preview,
        selected,
        evidence,
        hostConfirmed ? hostUID : undefined,
      )
    : undefined;

  async function confirm(event: FormEvent) {
    event.preventDefault();
    if (!confirmation || pending || stale) return;
    setPending(true);
    setError("");
    try {
      await clusterProvisionsApi.migrateOwnership(name, confirmation);
      onRecorded();
    } catch (reason) {
      actionError(reason);
    } finally {
      setPending(false);
    }
  }

  async function inspectHost() {
    if (!preview || !password || pending || stale) return;
    setPending(true);
    setError("");
    setHostUID(undefined);
    setHostConfirmed(false);
    try {
      setHostUID(
        await clusterProvisionsApi.readMigrationHost(name, preview, password),
      );
    } catch (reason) {
      actionError(reason);
    } finally {
      setPassword("");
      setPending(false);
    }
  }

  const locale = zh ? 0 : 1;
  return (
    <dialog
      ref={dialog}
      className="provision-migration-dialog"
      aria-labelledby="provision-migration-title"
      onCancel={(event) => {
        event.preventDefault();
        if (!pending) onClose();
      }}
    >
      <form onSubmit={confirm}>
        <header>
          <div>
            <h3 id="provision-migration-title">
              {zh ? "核对历史归属" : "Review historical ownership"}
            </h3>
            <code>{name}</code>
          </div>
          <button
            type="button"
            className="secondary-button"
            aria-label={zh ? "关闭" : "Close"}
            onClick={onClose}
            disabled={pending}
          >
            <X size={16} />
          </button>
        </header>
        <p>
          {zh
            ? "对照原安装记录、审计或备份，只勾选能确认属于本次开集群的资源。确认仅记录归属。"
            : "Check the original installation record, audit log, or backup. Select only resources you can verify. Confirmation records ownership metadata."}
        </p>
        {loading && (
          <p role="status">{zh ? "正在读取候选…" : "Loading candidates…"}</p>
        )}
        {error && (
          <p role="alert" className="field-error">
            {error}
          </p>
        )}
        {preview && (
          <fieldset disabled={pending || stale}>
            <legend>{zh ? "资源候选" : "Resource candidates"}</legend>
            <ul className="provision-migration-candidates">
              {preview.candidates.map((item) => (
                <li key={item.key}>
                  <label>
                    <input
                      type="checkbox"
                      disabled={item.state !== "review" || !item.identity}
                      checked={
                        item.state === "recorded" || selected.has(item.key)
                      }
                      onChange={(event) => {
                        const next = new Set(selected);
                        if (event.target.checked) next.add(item.key);
                        else next.delete(item.key);
                        setSelected(next);
                      }}
                    />
                    <span>
                      <strong>{labels[item.key][locale]}</strong>
                      <code>
                        {item.namespace ? item.namespace + "/" : ""}
                        {item.name}
                      </code>
                    </span>
                    <span className="provision-migration-status">
                      {candidateStates[item.state][locale]}
                    </span>
                  </label>
                  {item.identity && (
                    <small>
                      UID <code>{item.identity.uid}</code>
                      {item.created_at && (
                        <>
                          {" "}
                          ·{" "}
                          {new Date(item.created_at).toLocaleString(
                            zh ? "zh-CN" : "en-US",
                          )}
                        </>
                      )}
                    </small>
                  )}
                </li>
              ))}
            </ul>
            {preview.candidates.find((item) => item.key === "cluster")
              ?.state === "review" && (
              <small>
                {zh
                  ? "当前仍有同名集群，提交前必须核对并选中该集群。"
                  : "A live Cluster exists. Verify and select it before submitting."}
              </small>
            )}
            {preview.native_state === "review" && (
              <details>
                <summary>
                  {zh
                    ? "核对原控制面主机（可选）"
                    : "Review original control-plane host (optional)"}
                </summary>
                <label>
                  {zh ? "原主机 SSH 口令" : "Original host SSH password"}
                  <input
                    type="password"
                    autoComplete="off"
                    value={password}
                    onChange={(event) => setPassword(event.target.value)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter") {
                        event.preventDefault();
                        void inspectHost();
                      }
                    }}
                  />
                </label>
                <button
                  type="button"
                  className="secondary-button"
                  onClick={() => void inspectHost()}
                  disabled={!password}
                >
                  {zh ? "读取主机身份" : "Read host identity"}
                </button>
                {hostUID && (
                  <label>
                    <input
                      type="checkbox"
                      checked={hostConfirmed}
                      onChange={(event) =>
                        setHostConfirmed(event.target.checked)
                      }
                    />
                    {zh
                      ? "已核对原 kube-system UID"
                      : "Verified the original kube-system UID"}{" "}
                    <code>{hostUID}</code>
                  </label>
                )}
              </details>
            )}
            <label>
              {zh ? "证据引用" : "Evidence reference"}
              <input
                value={evidence}
                maxLength={256}
                onChange={(event) => setEvidence(event.target.value)}
                placeholder={
                  zh
                    ? "例如变更单或备份编号"
                    : "For example, a change ticket or backup ID"
                }
              />
            </label>
          </fieldset>
        )}
        <footer>
          <button
            type="button"
            className="secondary-button"
            onClick={refresh}
            disabled={loading || pending}
          >
            {zh ? "重新加载候选" : "Reload candidates"}
          </button>
          <button
            type="submit"
            className="primary-button"
            disabled={!confirmation || pending || stale}
          >
            {pending
              ? zh
                ? "处理中…"
                : "Working…"
              : zh
                ? "确认并记录归属"
                : "Confirm and record ownership"}
          </button>
        </footer>
      </form>
    </dialog>
  );
}
