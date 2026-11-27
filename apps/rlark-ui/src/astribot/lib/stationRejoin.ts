import { joinStageLabel, joinStageNeedsPassword, type NodeJoin } from "./nodeJoin.js";
import type { NodeAdmission } from "./siteNodeDisplay.js";

// Automatic rejoin continues an existing SSH join record from the stage it stopped at. It never creates a record:
// the Gateway already refuses a retry of a join that is not Failed, and the record's machine address, account and
// pinned host key are reused as they are. The password lives only while one run lasts and is never stored, so a
// failure before `install` (the last stage that logs in to the machine) cannot be continued without an operator
// typing it again. This module decides that boundary from the admission's view of its NodeJoin; the page only acts
// on the answer.
export type StationRejoinPlan =
  | { kind: "retry"; name: string; stage: string }
  | { kind: "running"; stage?: string }
  | { kind: "succeeded" }
  | { kind: "needs-password"; stage: string }
  | { kind: "no-stage" }
  | { kind: "no-record" }
  | { kind: "no-cluster" };

export type StationRejoinPhase = "retrying" | "running" | "succeeded" | "failed" | "timed-out" | "unavailable";

export interface StationRejoinState {
  phase: StationRejoinPhase;
  message: string;
  stage?: string;
  at: number;
}

// A join's name is <cluster>.<node> (astribot_node_join.go). The admission is the console's view of the join's
// phase and stage, so a station without a live join record (never joined over SSH, or its record aged out) is
// reported as no-record rather than given a new one.
export function planStationRejoin(input: {
  clusterID?: string;
  nodeName?: string;
  admission?: Pick<NodeAdmission, "join_phase" | "join_stage">;
}): StationRejoinPlan {
  const clusterID = input.clusterID?.trim();
  const nodeName = input.nodeName?.trim();
  if (!clusterID || !nodeName) return { kind: "no-cluster" };
  const phase = input.admission?.join_phase;
  if (!phase) return { kind: "no-record" };
  if (phase === "Running") return { kind: "running", stage: input.admission?.join_stage };
  if (phase === "Succeeded") return { kind: "succeeded" };
  if (phase !== "Failed") return { kind: "no-record" };
  const stage = input.admission?.join_stage ?? "";
  // Without a stage the console cannot prove the retry needs no password; fail closed to the manual flow.
  if (!stage) return { kind: "no-stage" };
  if (joinStageNeedsPassword(stage)) return { kind: "needs-password", stage };
  return { kind: "retry", name: `${clusterID}.${nodeName}`, stage };
}

// Why a click cannot start an automatic rejoin. Empty for a retry, which is the one plan that proceeds.
export function rejoinPlanReason(plan: StationRejoinPlan, zh: boolean): string {
  switch (plan.kind) {
    case "retry":
      return "";
    case "no-cluster":
      return zh
        ? "无法确定这个场站所属的集群，自动接入需要一个能对应到集群的场站记录；请手动重新接入。"
        : "This station does not resolve to a cluster, so no join record can be matched automatically; rejoin manually.";
    case "no-record":
      return zh
        ? "没有可重试的 SSH 接入记录（从未通过 SSH 接入，或记录已过期）。自动接入不会新建记录；请手动重新接入。"
        : "There is no retryable SSH join record (the node never joined over SSH, or the record has aged out). Automatic rejoin never creates one; rejoin manually.";
    case "running":
      return zh
        ? "这个节点的接入正在进行中，不能重复提交；请等待它结束，或用“重新检测”查看结果。"
        : "This node's join is already running and a second submission is refused; wait for it to finish, or use Re-check.";
    case "succeeded":
      return zh
        ? "接入记录已经成功完成；当前离线不是接入流程失败，自动重试不会为它新建记录。等机器恢复上报后会自动显示在线，或手动重新接入。"
        : "The join record already completed; being offline now is not a join failure, and automatic rejoin will not create a second record. The tile updates when the machine reports again, or rejoin manually.";
    case "needs-password":
      return zh
        ? `接入停在第「${joinStageLabel(plan.stage, "zh")}」步；从这一步重试要登录机器，而 SSH 口令不会保存，无法自动继续。请手动重新接入并输入口令。`
        : `The join stopped at ${joinStageLabel(plan.stage, "en")}; retrying from there logs in to the machine, and its SSH password is never stored, so it cannot continue automatically. Rejoin manually and enter it.`;
    case "no-stage":
      return zh
        ? "接入记录没有写明停在哪一步，无法确认自动重试不需要登录机器；请手动重新接入。"
        : "The join record does not say which stage it stopped at, so the console cannot prove the retry needs no machine login; rejoin manually.";
  }
}

// The line the card shows while a retry is followed: the stage the record is on and its latest progress, if any.
export function stationRejoinProgress(join: NodeJoin, zh: boolean): string {
  if (join.status?.phase === "Succeeded") {
    return zh ? "接入已恢复：所有阶段完成。" : "Rejoin complete: every stage finished.";
  }
  const stage = join.status?.stage ?? "";
  const running = join.status?.stages?.find((entry) => entry.name === stage && (entry.state === "Running" || entry.state === "Failed"));
  const last = running?.message?.trim();
  const where = stage ? (zh ? `第「${joinStageLabel(stage, "zh")}」步` : `stage ${joinStageLabel(stage, "en")}`) : "";
  const base = where
    ? (zh ? `正在${where}…` : `Running ${where}…`)
    : (zh ? "接入进行中…" : "Rejoining…");
  return last ? `${base} ${last}` : base;
}

// The failed stage's own message, which the Gateway wrote when it stopped. It is shown verbatim (the management
// plane writes it in the operator's language) with the stage named; a record without a reason says so.
export function stationRejoinFailure(join: NodeJoin | undefined, zh: boolean): string {
  if (!join) {
    return zh
      ? "接入失败，但读不到这条接入记录的原因；请重新检测，或手动重新接入。"
      : "The rejoin failed and its record could not be read; re-check, or rejoin manually.";
  }
  const failed = join.status?.stages?.find((entry) => entry.state === "Failed");
  if (!failed) {
    return zh
      ? `接入失败（${join.status?.phase ?? "未知"}），记录没有写明失败原因；请重新检测，或手动重新接入。`
      : `The rejoin failed (${join.status?.phase ?? "unknown"}) and the record gives no reason; re-check, or rejoin manually.`;
  }
  const label = joinStageLabel(failed.name, zh ? "zh" : "en");
  const why = (failed.message || failed.readback || "").trim();
  return zh
    ? `接入停在「${label}」：${why || "记录没有写明原因"}。`
    : `The rejoin stopped at ${label}: ${why || "the record gives no reason."}`;
}
