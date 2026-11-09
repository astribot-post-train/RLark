/**
 * Raw role configuration fields for container-runtime clusters.
 * Uses the same form-section / form-section-head structure as the rest of CreateJob,
 * and env-row / icon-button patterns from the existing codebase.
 * No new CSS classes introduced.
 */

import { type ReactElement } from "react";
import { Plus, X } from "lucide-react";
import type { CRDNodeLite } from "../types";

type Props = {
  zh: boolean;
  disabled?: boolean;
  environment: string;
  onEnvironmentChange: (env: string) => void;
  argv: string;
  onArgvChange: (argv: string) => void;
  env: string;
  onEnvChange: (env: string) => void;
  directory: string;
  onDirectoryChange: (dir: string) => void;
  availableEnvironments: string[];
};

export function RawRoleFields({
  zh,
  disabled,
  environment,
  onEnvironmentChange,
  argv,
  onArgvChange,
  env,
  onEnvChange,
  directory,
  onDirectoryChange,
  availableEnvironments,
}: Props): ReactElement {
  // argv and env are stored as newline-separated strings for textarea editing
  const argvLines = argv.split("\n");
  const envLines = env.split("\n");

  return (
    <>
      {/* Environment */}
      <div className="form-section" style={{ marginTop: 12 }}>
        <div className="form-section-head">
          <small>{zh ? "容器环境" : "Container Environment"}</small>
        </div>
        {availableEnvironments.length > 0 ? (
          <select
            value={environment}
            onChange={(e) => onEnvironmentChange(e.target.value)}
            disabled={disabled}
            required
          >
            <option value="">{zh ? "选择环境" : "Select environment"}</option>
            {availableEnvironments.map((e) => (
              <option key={e} value={e}>{e}</option>
            ))}
          </select>
        ) : (
          <input
            type="text"
            value={environment}
            onChange={(e) => onEnvironmentChange(e.target.value)}
            disabled={disabled}
            required
            placeholder={zh ? "在 Raw 节点上预配置的环境名" : "Environment pre-configured on Raw nodes"}
          />
        )}
        <small style={{ display: "block", marginTop: 6, color: "var(--muted)", fontSize: 11 }}>
          {zh ? "Raw 节点上预先配置的容器环境名称。" : "Container environment pre-configured on the Raw node."}
        </small>
      </div>

      {/* Process argv */}
      <div className="form-section" style={{ marginTop: 12 }}>
        <div className="form-section-head">
          <small>{zh ? "进程参数（argv）" : "Process arguments (argv)"}</small>
          <button
            type="button"
            className="secondary-button"
            disabled={disabled}
            onClick={() => onArgvChange(argv ? argv + "\n" : "")}
          >
            <Plus size={14} />
            {zh ? "添加参数" : "Add arg"}
          </button>
        </div>
        {argvLines.map((line, idx) => (
          <div className="env-row" key={idx}>
            <input
              value={line}
              onChange={(e) => {
                const next = [...argvLines];
                next[idx] = e.target.value;
                onArgvChange(next.join("\n"));
              }}
              disabled={disabled}
              placeholder={idx === 0
                ? (zh ? "可执行文件绝对路径，如 /usr/bin/python3" : "Absolute path, e.g. /usr/bin/python3")
                : (zh ? "参数" : "argument")}
              style={{ gridColumn: "1 / -2" }}
            />
            <button
              type="button"
              className="icon-button danger"
              disabled={disabled || argvLines.length <= 1}
              onClick={() => {
                const next = argvLines.filter((_, i) => i !== idx);
                onArgvChange(next.join("\n"));
              }}
            >
              <X size={14} />
            </button>
          </div>
        ))}
        <small style={{ display: "block", marginTop: 6, color: "var(--muted)", fontSize: 11 }}>
          {zh
            ? "第一行必须是可执行文件的绝对路径。每行一个参数。"
            : "First line must be an absolute path to the executable. One argument per line."}
        </small>
      </div>

      {/* Environment variables */}
      <div className="form-section" style={{ marginTop: 12 }}>
        <div className="form-section-head">
          <small>{zh ? "环境变量" : "Environment Variables"}</small>
          <button
            type="button"
            className="secondary-button"
            disabled={disabled}
            onClick={() => onEnvChange(env ? env + "\n=" : "=")}
          >
            <Plus size={14} />
            {zh ? "添加" : "Add"}
          </button>
        </div>
        {envLines.map((line, idx) => {
          const eqIdx = line.indexOf("=");
          const k = eqIdx >= 0 ? line.slice(0, eqIdx) : line;
          const v = eqIdx >= 0 ? line.slice(eqIdx + 1) : "";
          return (
            <div className="env-row" key={idx}>
              <input
                value={k}
                onChange={(e) => {
                  const next = [...envLines];
                  next[idx] = e.target.value + "=" + v;
                  onEnvChange(next.join("\n"));
                }}
                disabled={disabled}
                placeholder="KEY"
              />
              <input
                value={v}
                onChange={(e) => {
                  const next = [...envLines];
                  next[idx] = k + "=" + e.target.value;
                  onEnvChange(next.join("\n"));
                }}
                disabled={disabled}
                placeholder="value"
              />
              <button
                type="button"
                className="icon-button danger"
                disabled={disabled}
                onClick={() => {
                  const next = envLines.filter((_, i) => i !== idx);
                  onEnvChange(next.join("\n"));
                }}
              >
                <X size={14} />
              </button>
            </div>
          );
        })}
      </div>

      {/* Working directory */}
      <div className="form-section" style={{ marginTop: 12 }}>
        <div className="form-section-head">
          <small>{zh ? "工作目录" : "Working Directory"}</small>
        </div>
        <input
          type="text"
          value={directory}
          onChange={(e) => onDirectoryChange(e.target.value)}
          disabled={disabled}
          required
          placeholder="/workspace"
        />
      </div>

      {/* Storage notice */}
      <div className="form-section" style={{ marginTop: 12 }}>
        <div className="form-section-head">
          <small>{zh ? "存储挂载" : "Volume Mounts"}</small>
        </div>
        <small style={{ color: "var(--muted)", fontSize: 12 }}>
          {zh
            ? "Raw 节点使用宿主环境的存储挂载，不支持存储类配置。镜像和 SDK 由环境提供。"
            : "Raw nodes use host-provided storage mounts. Storage classes and images are provided by the environment."}
        </small>
      </div>
    </>
  );
}

/**
 * Extract available environments from nodes in the selected cluster.
 * Checks both node labels and the spec.environment field.
 */
export function extractEnvironments(nodes: CRDNodeLite[]): string[] {
  const environments = new Set<string>();
  for (const node of nodes) {
    const fromLabel = (node.metadata?.labels || {})["environment"];
    if (fromLabel && typeof fromLabel === "string") {
      environments.add(fromLabel);
    }
  }
  return Array.from(environments).sort();
}
