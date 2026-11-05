import type { FormEvent } from "react";
import type { Detail, NodeRegistration, RegisteredNode } from "./api";

type Labels = {
  registerNode: string;
  nodeName: string;
  instanceID: string;
  privateAddress: string;
  environment: string;
  networkScope: string;
  role: string;
  gpuInput: string;
  gpuAutoHint: string;
  registrationHint: string;
  registrationPending: string;
  registrationDone: string;
  retry: string;
  close: string;
};

// Package signing stays on the administrator machine. This panel only declares
// authorized inventory and displays the exact identity needed by node-package.
export function NodeRegistrationPanel({
  detail,
  t,
  writing,
  blocked,
  intent,
  result,
  onSubmit,
}: {
  detail: Detail;
  t: Labels;
  writing: boolean;
  blocked: boolean;
  intent?: NodeRegistration;
  result?: RegisteredNode;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
}) {
  if (!detail.nodeRegistrationEnabled && !intent && !result) return null;
  const inputs = [
    ["name", t.nodeName],
    ["instanceID", t.instanceID],
    ["privateAddress", t.privateAddress],
    ["environment", t.environment],
    ["networkScope", t.networkScope],
  ] as const;
  return (
    <div className="registration-panel">
      {intent && (
        <p role="status" className="notice">
          {t.registrationPending} <code>{intent.operationID}</code>
        </p>
      )}
        <form
          className="create"
          onSubmit={onSubmit}
          key={intent?.operationID || "new"}
        >
          {inputs.map(([name, label]) => (
            <label key={name}>
              {label}
              <input
                name={name}
                required
                disabled={writing || !!intent}
                defaultValue={intent?.[name] || ""}
              />
            </label>
          ))}
          <label>
            {t.role}
            <select name="role" required disabled={writing || !!intent} defaultValue={intent?.role || "Worker"}>
              <option value="Gateway">Gateway</option>
              <option value="Worker">Worker</option>
            </select>
          </label>
          <label>
            {t.gpuInput}
            <textarea
              name="gpus"
              rows={3}
              disabled={writing || !!intent}
              defaultValue={JSON.stringify(
                intent?.authorizedGPUUUIDs || [],
                null,
                2,
              )}
            />
            <small className="muted">{t.gpuAutoHint}</small>
          </label>
          <button
            className="primary"
            disabled={
              writing ||
              blocked ||
              !detail.nodeRegistrationEnabled ||
              (!!intent && intent.clusterUID !== detail.cluster.uid)
            }
          >
            {intent ? t.retry : t.registerNode}
          </button>
        </form>
      {result && result.clusterUID === detail.cluster.uid && (
        <div role="status">
          <p>{t.registrationDone}</p>
          <pre>
            {JSON.stringify(
              {
                namespace: result.namespace,
                name: result.name,
                uid: result.uid,
                tenantID: detail.cluster.tenantID,
                credentialID: result.operationID,
              },
              null,
              2,
            )}
          </pre>
        </div>
      )}
    </div>
  );
}
