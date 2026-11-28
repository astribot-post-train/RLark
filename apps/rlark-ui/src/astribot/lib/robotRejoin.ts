import { joinStageLabel } from "./nodeJoin.js";
import type { NodeAdmission } from "./siteNodeDisplay.js";

// What the console can honestly do for an offline robot. A robot is admitted through a Machine and its NodeClass
// (station-controller starts a NodeOperation with the class's stored credentials); it is never joined through the
// Gateway's Server-only SSH NodeJoin path. Once it has joined, the card may submit
// POST /api/v1/station/machines/{name}/rejoin for that Machine and then follow the returned NodeOperation.
// This module only classifies what the page already sees: joined-offline may submit; every other kind must not.
export type RobotRejoinKind =
  | "no-data"
  | "identity-conflict"
  | "server-record"
  | "no-admission"
  | "joined-offline"
  | "join-stopped"
  | "pending"
  | "expired";

export interface RobotRejoinAssessment {
  kind: RobotRejoinKind;
  message: string;
}

export interface RobotRejoinInput {
  zh: boolean;
  stationPhase: string;
  identityVerified?: string;
  siteDataState: "loading" | "ready" | "unavailable";
  admission?: Pick<
    NodeAdmission,
    | "state"
    | "stuck"
    | "reason"
    | "self_healed"
    | "join_phase"
    | "join_stage"
    | "expires_at"
  >;
}

function note(zh: boolean, zhText: string, enText: string): string {
  return zh ? zhText : enText;
}

function healed(text: string | undefined, zh: boolean): string {
  if (!text) return "";
  return zh
    ? ` RLark 已自动处理过：${text}。`
    : ` RLark already corrected this by itself: ${text}.`;
}

function why(text: string | undefined, zh: boolean): string {
  const value = text?.trim();
  if (!value) return "";
  return zh ? `原因：${value}。` : `Reason: ${value}.`;
}

// The manual entry for every robot case is the Add node page's machine workspace (Scan discovery), where the
// Machine's own problem is shown and the class credentials RLark already stores are used; the join-code path is
// offered there too. The card never opens the Server SSH form for a robot.
export function assessRobotRejoin(
  input: RobotRejoinInput,
): RobotRejoinAssessment {
  const zh = input.zh;
  if (input.siteDataState !== "ready") {
    // Loading is unknown too: judging it as "no admission" would be a confident wrong answer.
    const loading = input.siteDataState === "loading";
    return {
      kind: "no-data",
      message: loading
        ? note(
            zh,
            "场站与入网记录还在加载，暂时无法判断恢复方式；请稍后重新接入或点“重新检测”。机器人离线时，控制台没有可安全触发的远程重连命令。",
            "The site data and admission records are still loading, so the recovery path is not known yet; try Rejoin or Re-check again shortly. While a robot is offline, the console has no safe remote reconnect command.",
          )
        : note(
            zh,
            "读不到场站与入网记录，无法判断恢复方式；请先重新检测或检查 Station API。机器人离线时，控制台没有可安全触发的远程重连命令。",
            "The site data and admission record cannot be read, so the recovery path is unknown; re-check or check Station API first. While a robot is offline, the console has no safe remote reconnect command.",
          ),
    };
  }
  if (input.stationPhase === "Conflict" || input.identityVerified === "False") {
    return {
      kind: "identity-conflict",
      message: note(
        zh,
        "场站记录与当前上报的节点身份不一致。不能按旧配置自动重连；请现场核对这台机器人，确认后在“加入节点 → 扫描发现”重新入网，或删除记录。",
        "The station record and the reporting Node disagree on identity. The old configuration is not used to reconnect automatically; check the robot on site, then rejoin it from Add node → Scan for machines, or delete the record.",
      ),
    };
  }
  const admission = input.admission;
  // A NodeJoin is the Gateway's Server-only SSH join. Seeing one on a robot means the wrong entry was used; the card
  // reports it instead of driving it further.
  if (admission?.join_phase) {
    const stage = admission.join_stage
      ? zh
        ? `第「${joinStageLabel(admission.join_stage, "zh")}」步`
        : `stage ${joinStageLabel(admission.join_stage, "en")}`
      : zh
        ? "未知步骤"
        : "an unknown stage";
    return {
      kind: "server-record",
      message: note(
        zh,
        `这台机器人有一条 SSH 入网记录（${admission.join_phase}，${stage}），但 SSH 入网只支持 Server，不适用于机器人。不要使用该入口；请通过“加入节点 → 扫描发现”或“入网码”入网。`,
        `This robot has an SSH join record (${admission.join_phase}, ${stage}), but SSH join only supports Server machines and does not apply to robots. Do not use that entry; join it from Add node → Scan for machines, or with a join code.`,
      ),
    };
  }
  if (!admission) {
    return {
      kind: "no-admission",
      message: note(
        zh,
        "没有这台机器人的入网记录，控制台没有可复用的自动重连配置。机器人应通过“加入节点 → 扫描发现”或“入网码”重新入网。",
        "There is no admission record for this robot, so the console has no stored configuration to reconnect with. Join it from Add node → Scan for machines, or with a join code.",
      ),
    };
  }
  const selfHealed = healed(admission.self_healed, zh);
  switch (admission.state) {
    case "Joined":
      return {
        kind: "joined-offline",
        message: note(
          zh,
          `这台机器人已经入网。重新接入会按已有 Machine 与 NodeClass 配置提交一次重接，不输入 SSH 口令，也不新建或替换入网记录；相同 request_id 对应同一次任务。卡片显示该 NodeOperation 的真实阶段，失败时显示原因。刷新或返回场站后按集群、节点和任务标识恢复。若长时间不回来，请现场检查电源、网络和站点链路。${selfHealed}`,
          `This robot has already joined. Rejoin submits one retry with the Machine and NodeClass configuration it already has, asks for no SSH password, and does not create or replace an admission; the same request_id is the same task. The card shows that NodeOperation's real phase, and the reason when it fails. A refresh or a return to the station restores it by cluster, node and task id. If it stays away, check power, network and the site link on site.${selfHealed}`,
        ),
      };
    case "Redeemed":
      return {
        kind: "join-stopped",
        message: note(
          zh,
          `机器人的入网已兑换但还没完成。${why(admission.reason, zh)}它走 Machine 与 NodeClass 入网，机型凭据保存在 RLark，不需要在这里输入口令；但重新发起会新建一次接入，不能当成幂等重连盲目触发。请到“加入节点 → 扫描发现”按机器行里的问题处理后重新加入，或在入网记录里重发入网码。${selfHealed}`,
          `The robot's join was redeemed but did not finish. ${why(admission.reason, zh)}It joins through a Machine and its NodeClass, whose credentials RLark already stores, so no password is entered here; but asking again starts a new join and must not be treated as an idempotent reconnect. Fix the problem shown on the machine row in Add node → Scan for machines and join it again, or reissue its join code from the admission records.${selfHealed}`,
        ),
      };
    case "Pending":
      return {
        kind: "pending",
        message: note(
          zh,
          `入网码还没有被机器人执行。请在入网记录里重发入网码，或到“加入节点 → 入网码/扫描发现”重新发起；控制台不能代替机器执行入网命令。${selfHealed}`,
          `The robot has not run its join code yet. Reissue the code from the admission records, or start again from Add node → Join code / Scan for machines; the console cannot run the join command on the machine.${selfHealed}`,
        ),
      };
    case "Expired":
      return {
        kind: "expired",
        message: note(
          zh,
          `入网码已过期或作废。请在入网记录里重发入网码，或到“加入节点 → 扫描发现/入网码”重新发起；控制台不能代替机器执行入网命令。${selfHealed}`,
          `The join code expired or was revoked. Reissue it from the admission records, or start again from Add node → Scan for machines / Join code; the console cannot run the join command on the machine.${selfHealed}`,
        ),
      };
    default:
      return {
        kind: "join-stopped",
        message: note(
          zh,
          `入网记录的状态是 ${admission.state}，没有可直接触发的自动重连。${why(admission.reason, zh)}请到“加入节点 → 扫描发现”按机器行里的问题处理后重新加入。${selfHealed}`,
          `The admission is ${admission.state}, and there is no automatic reconnect to trigger. ${why(admission.reason, zh)}Fix the problem shown on the machine row in Add node → Scan for machines and join it again.${selfHealed}`,
        ),
      };
  }
}
