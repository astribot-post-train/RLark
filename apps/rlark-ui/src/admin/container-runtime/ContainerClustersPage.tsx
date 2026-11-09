import {
  useEffect,
  useCallback,
  useMemo,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";

import {
  APIError,
  Client,
  type Cluster,
  type ClusterIntent,
  type Detail,
  type JobSummary,
  type RawJobRequest,
  type Node,
  type NodeRegistration,
  type RegisteredNode,
  type StatsSnapshot,
} from "./api";
import {
  clusterState,
  heartbeatAge,
  LoadGate,
  nodeState,
  nativeNodeState,
  logText as decodeLogText,
  rawJobIntent,
  nodeRegistrationIntent,
  clusterIntentFromForm,
  canReadDiagnostic,
  diagnosticInventory,
} from "./models";
import { JobForm, JobsPanel } from "./JobsPanel";
import { Drawer } from "./Drawer";
import { NodeRegistrationPanel } from "./NodeRegistrationPanel";
import { EnrollmentOperations, EnrollmentPanel } from "./EnrollmentPanel";
import { activeEnrollments, nodeProbe, probeState } from "./enrollment";
import type { EnrollmentNode } from "./api";
import { Donut, Hint, SegmentBar, StatusDot, type Tone } from "./Charts";
import "./style.css";
const labels = {
  zh: {
    title: "容器集群",
    subtitle: "Raw 与 Docker 节点预览",
    refresh: "刷新",
    loading: "加载中…",
    unavailable: "数据不可用",
    empty: "没有容器集群",
    select: "选择集群查看节点和升级状态",
    nodes: "节点",
    nodeColumn: "节点名称",
    roleColumn: "角色",
    reachability: "A 到节点网络探测",
    reachabilityUnknown: "未探测",
    registerNode: "登记运行时节点",
    nodeName: "节点名称",
    instanceID: "已核验的实例身份（Raw 容器 / Docker Engine）",
    privateAddress: "私网 IPv4 地址",
    networkScope: "网络范围",
    role: "集群角色",
    gpuInput: "可选 GPU UUID 白名单（JSON 字符串数组）",
    gpuAutoHint: "留空数组 [] 时使用 Agent 最近 45 秒内认证上报的 GPU；心跳过期后不参与调度。",
    registrationHint:
      "登记后仍需在管理员机器导出并安装 Agent 包。节点收到新鲜心跳后才会就绪；GPU UUID 可留空以启用 Agent 自动发现。",
    registrationPending:
      "登记结果待确认。保存 UUID 和原始输入，重试相同请求。切换集群已禁用；刷新或退出会丢失内存请求。",
    registrationDone:
      "节点登记已确认；请单独核对安装和心跳。以下为 node-package 的 node 身份字段：",
    invalidNode:
      "请填写有效节点名、实例身份、私网 IPv4、环境、网络范围和 Gateway/Worker 角色；GPU 白名单必须是有效且不重复的 JSON 数组。",
    releases: "Agent 版本",
    allocations: "资源预留",
    upgrades: "升级操作",
    target: "目标版本",
    clear: "暂停新升级（清空目标）",
    apply: "发布目标",
    ready: "就绪",
    nativeOnline: "原生节点在线",
    nativeOffline: "原生节点离线",
    nativeUnknown: "原生状态未知",
    stale: "过期 / 未就绪",
    held: "已有预留",
    invalid: "绑定无效",
    degraded: "等待节点",
    deleting: "删除中",
    schedulable: "可调度",
    busy: "忙",
    cordon: "禁止新任务",
    uncordon: "允许新任务",
    cordoned: "已禁止新任务",
    schedulingHint: "只调整调度意图，不停止现有任务。有预留时不能恢复调度。",
    yes: "是",
    no: "否",
    gpu: "授权 GPU",
    discoveredGPU: "Agent 发现 GPU",
    heartbeat: "心跳距今",
    privateIP: "私网 IP",
    nativeAddresses: "原生地址",
    instance: "实例身份",
    agentReady: "容器 Agent",
    nativeState: "原生状态",
    agentState: "Agent 状态",
    version: "Agent 版本",
    actions: "操作",
    identity: "身份",
    nodeInfo: "节点信息",
    scheduling: "调度",
    resources: "资源与能力",
    nativeUID: "原生 UID",
    nativeCapacity: "原生容量",
    nativeAllocatable: "原生可分配量",
    nativePhase: "原生阶段",
    nativeObserved: "原生观测时间",
    conflict: "资源已变化，请刷新后重试。",
    create: "创建集群",
    name: "集群名",
    namespace: "Agent namespace",
    tenant: "租户",
    runtime: "运行时",
    rawRuntime: "Raw（已有进程）",
    dockerRuntime: "Docker（Docker Engine）",
    retry: "重试同一操作",
    close: "收起",
    logout: "退出",
    signin: "管理员登录",
    username: "用户名",
    password: "密码",
    capacity: "观测容量",
    capabilities: "能力",
    conditions: "条件",
    noRecords: "暂无记录",
    targetHint:
      "目标是持续期望状态。清空目标只暂停新升级，不取消正在应用的操作。",
    onboarding:
      "Docker 集群独立于 Kubernetes 集群；节点登记、Agent 更新和诊断需单独启用，安装包通过管理员 CLI 导出。Docker Ray 多节点仍是预览能力。",
    logs: "读取日志",
    stats: "读取资源快照",
    statsLoading: "读取中…",
    statsUnavailable: "资源快照不可用",
    statsAt: "采样时间",
    cpuUsage: "CPU 累计纳秒",
    memory: "内存",
    pids: "PID 数",
    blockIO: "块设备 I/O",
    read: "读",
    write: "写",
    bytes: "字节",
    gpuStatsHint:
      "单次采样不提供 CPU 百分比、GPU 利用率和 host network 流量归属。",
    unavailableValue: "不可用",
    logLoading: "读取中…",
    logUnavailable: "日志不可用",
    logClose: "关闭日志",
    logBounded: "输出已达到读取或保留上限",
    invalidJob:
      "请填写有效的环境、绝对路径和 JSON 字符串数组；不能覆盖 GPU 可见性。",
    createHint:
      "创建时自动生成操作 UUID。提交后保留相同请求，响应丢失可安全重试。",
    remembered:
      "已保存未确认的创建请求；重试沿用原 UUID，不能修改或接管已有资源。",
    failedLogin: "登录失败或账号不是管理员",
    rollout: "最近成功升级",
    session: "会话已过期，请重新登录",
    jobsTitle: "Raw 作业",
    submitJob: "提交作业",
    environment: "环境",
    argv: "命令参数（JSON 字符串数组）",
    directory: "工作目录",
    env: "环境变量（KEY=VALUE 的 JSON 数组）",
    suspend: "暂停",
    suspended: "已暂停",
    active: "未暂停",
    unobserved: "观测待更新",
    retainedJob:
      "提交结果待确认，请保存操作 ID 和原始输入并重试相同请求。切换集群已禁用；刷新或退出会丢失内存中的请求，需先核对资源。",
    jobHint: "首版只提交单进程 Raw 作业；GPU 由节点授权和调度器分配。",
    noJobs: "暂无 Raw 作业",
    retryJob: "重试同一作业",
    enroll: "部署 / 修改节点",
    redeploy: "修改 IP / 重新部署",
    redeployHint: "打开部署表单并定位到该节点。已接入节点的地址变化由控制器校验，不会静默接管。",
    enrollments: "接入操作",
    jobName: "作业名称",
    phase: "状态",
    chartNodes: "节点就绪",
    chartReach: "网络可达",
    chartJobs: "作业结果",
    chartRollout: "升级状态",
    reachable: "可达",
    unreachable: "不可达",
    notReady: "未就绪",
    succeeded: "成功",
    failed: "失败",
    running: "运行中",
    other: "其他",
    noTarget: "无目标",
  },
  en: {
    title: "Container clusters",
    subtitle: "Raw and Docker node preview",
    refresh: "Refresh",
    loading: "Loading…",
    unavailable: "Data unavailable",
    empty: "No container clusters",
    select: "Select a cluster to inspect nodes and upgrades",
    nodes: "Nodes",
    nodeColumn: "Node",
    roleColumn: "Role",
    reachability: "A-to-node network probe",
    reachabilityUnknown: "Not probed",
    registerNode: "Register runtime node",
    nodeName: "Node name",
    instanceID: "Verified instance identity (Raw container / Docker Engine)",
    privateAddress: "Private IPv4 address",
    networkScope: "Network scope",
    role: "Cluster role",
    gpuInput: "Optional GPU UUID allowlist (JSON string array)",
    gpuAutoHint: "With an empty array [], use GPUs reported by an authenticated Agent within the last 45 seconds; stale heartbeats are not schedulable.",
    registrationHint:
      "After registration, export and install the Agent package from an administrator machine. A fresh heartbeat is required for readiness; leave the GPU list empty to enable Agent discovery.",
    registrationPending:
      "Registration is unconfirmed. Retain the UUID and exact input and retry. Cluster switching is disabled; refresh or sign out loses the in-memory request.",
    registrationDone:
      "Node registration confirmed; verify installation and heartbeat separately. Use these node identity fields in node-package:",
    invalidNode:
      "Provide a valid node name, incarnation, private IPv4, environment, network scope and Gateway/Worker role; the optional GPU allowlist must be valid unique JSON.",
    releases: "Agent releases",
    allocations: "Allocations",
    upgrades: "Upgrade operations",
    target: "Target release",
    clear: "Pause new upgrades (clear target)",
    apply: "Publish target",
    ready: "Ready",
    nativeOnline: "Native node online",
    nativeOffline: "Native node offline",
    nativeUnknown: "Native status unknown",
    stale: "Stale / not ready",
    held: "Allocation held",
    invalid: "Invalid binding",
    degraded: "Waiting for nodes",
    deleting: "Deleting",
    schedulable: "Schedulable",
    busy: "Busy",
    cordon: "Cordon",
    uncordon: "Uncordon",
    cordoned: "Cordoned",
    schedulingHint:
      "Changes scheduling intent only. Existing workloads continue; held reservations block uncordon.",
    yes: "Yes",
    no: "No",
    gpu: "Authorized GPUs",
    discoveredGPU: "Agent-discovered GPUs",
    heartbeat: "Heartbeat age",
    privateIP: "Private IP",
    nativeAddresses: "Native addresses",
    instance: "Instance identity",
    agentReady: "Container Agent",
    nativeState: "Native status",
    agentState: "Agent status",
    version: "Agent version",
    actions: "Actions",
    identity: "Identity",
    nodeInfo: "Node information",
    scheduling: "Scheduling",
    resources: "Resources and capabilities",
    nativeUID: "Native UID",
    nativeCapacity: "Native capacity",
    nativeAllocatable: "Native allocatable",
    nativePhase: "Native phase",
    nativeObserved: "Native observed time",
    conflict: "Resource changed. Refresh and retry.",
    create: "Create cluster",
    name: "Cluster name",
    namespace: "Agent namespace",
    tenant: "Tenant",
    runtime: "Runtime",
    rawRuntime: "Raw (existing process)",
    dockerRuntime: "Docker (Docker Engine)",
    retry: "Retry same operation",
    close: "Collapse",
    logout: "Sign out",
    signin: "Administrator sign in",
    username: "Username",
    password: "Password",
    capacity: "Observed capacity",
    capabilities: "Capabilities",
    conditions: "Conditions",
    noRecords: "No records",
    targetHint:
      "The target is durable intent. Clearing it pauses new upgrades; it does not cancel an applying operation.",
    onboarding:
      "Docker clusters are separate from Kubernetes clusters. Registration, Agent updates and diagnostics require explicit enablement and administrator package export. Multi-node Docker Ray remains preview.",
    logs: "Read logs",
    stats: "Read resource snapshot",
    statsLoading: "Reading…",
    statsUnavailable: "Resource snapshot unavailable",
    statsAt: "Sample time",
    cpuUsage: "Cumulative CPU ns",
    memory: "Memory",
    pids: "PIDs",
    blockIO: "Block I/O",
    read: "read",
    write: "write",
    bytes: "bytes",
    gpuStatsHint:
      "One sample does not provide CPU percentage, GPU utilization or host-network traffic attribution.",
    unavailableValue: "Unavailable",
    logLoading: "Reading…",
    logUnavailable: "Logs unavailable",
    logClose: "Close logs",
    logBounded: "Output reached the read or retention limit",
    invalidJob:
      "Provide a valid environment, absolute paths and JSON string arrays. GPU visibility overrides are forbidden.",
    createHint:
      "An operation UUID is generated at submission. Retain the identical request to recover a lost response.",
    remembered:
      "An unconfirmed create request is saved. Retry retains its UUID and cannot adopt existing resources.",
    failedLogin: "Login failed or administrator access is missing",
    rollout: "Last successful upgrade",
    session: "Session expired. Sign in again",
    jobsTitle: "Raw jobs",
    submitJob: "Submit job",
    environment: "Environment",
    argv: "Arguments (JSON string array)",
    directory: "Working directory",
    env: "Environment (JSON array of KEY=VALUE)",
    suspend: "Suspend",
    suspended: "Suspended",
    active: "Not suspended",
    unobserved: "Observation pending",
    retainedJob:
      "Submission is unconfirmed. Retain the operation ID and exact input and retry the same request. Cluster switching is disabled. Refresh or sign out loses the in-memory request; inspect resources first.",
    jobHint:
      "The preview submits one Raw process; GPU placement remains controlled by node authorization and the scheduler.",
    noJobs: "No Raw jobs",
    retryJob: "Retry same job",
    enroll: "Deploy / edit nodes",
    redeploy: "Edit IP / redeploy",
    redeployHint: "Opens the deploy form at this node. Address changes on joined nodes are validated by the controller and never silently adopted.",
    enrollments: "Enrollments",
    jobName: "Job",
    phase: "Phase",
    chartNodes: "Node readiness",
    chartReach: "Reachability",
    chartJobs: "Job outcomes",
    chartRollout: "Rollout",
    reachable: "Reachable",
    unreachable: "Unreachable",
    notReady: "Not ready",
    succeeded: "Succeeded",
    failed: "Failed",
    running: "Running",
    other: "Other",
    noTarget: "No target",
  },
};
type Lang = keyof typeof labels;
const intentKey = "rlark-container-cluster-create-intent";
function readIntent(): ClusterIntent | undefined {
  try {
    const value = sessionStorage.getItem(intentKey);
    if (!value) return;
    const v = JSON.parse(value) as ClusterIntent;
    if (
      typeof v.operationID === "string" &&
      typeof v.name === "string" &&
      typeof v.tenantID === "string" &&
      typeof v.agentNamespace === "string" &&
      (v.runtime === "Raw" || v.runtime === "Docker")
    )
      return v;
  } catch {
    /* Storage unavailable: keep in-memory recovery for this session. */
  }
}
export function ContainerClustersPage({
  lang,
  selectedCluster,
  onSelect,
}: {
  lang: Lang;
  selectedCluster: string;
  onSelect: (
    selection: { namespace: string; name: string } | undefined,
  ) => void;
}) {
  const t = labels[lang];
  const client = useMemo(() => new Client(), []);
  const [clusters, setClusters] = useState<Cluster[]>();
  const selection = useMemo(() => {
    const parts = selectedCluster.split("/");
    return parts.length === 2 && parts.every(Boolean)
      ? { namespace: parts[0], name: parts[1] }
      : undefined;
  }, [selectedCluster]);
  const setSelection = onSelect;
  const [detail, setDetail] = useState<Detail>();
  const [jobs, setJobs] = useState<JobSummary[]>();
  const [jobsLoading, setJobsLoading] = useState(false);
  const [revision, setRevision] = useState(0);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [writing, setWriting] = useState(false);
  const [expandedNodes, setExpandedNodes] = useState<Set<string>>(
    () => new Set(),
  );
  const [target, setTarget] = useState("");
  const observedTarget = useRef<string | undefined>(undefined);
  const [showCreate, setShowCreate] = useState(false);
  const [intent, setIntent] = useState<ClusterIntent | undefined>(readIntent);
  const [logText, setLogText] = useState<{
    key: string;
    text: string;
    note: string;
  }>();
  const [logLoading, setLogLoading] = useState(false);
  const [stats, setStats] = useState<{ key: string; value: StatsSnapshot }>();
  const [statsLoading, setStatsLoading] = useState(false);
  const statsRequest = useRef<AbortController | undefined>(undefined);
  const [showJob, setShowJob] = useState(false);
  const [jobIntent, setJobIntent] = useState<RawJobRequest>();
  const [showNodeRegistration, setShowNodeRegistration] = useState(false);
  const [showEnroll, setShowEnroll] = useState(false);
  const [enrollFocus, setEnrollFocus] = useState<{ name: string; seq: number }>();
  const [tab, setTab] = useState<
    "nodes" | "jobs" | "enrollments" | "allocations" | "releases" | "upgrades"
  >("nodes");
  const [nodeIntent, setNodeIntent] = useState<NodeRegistration>();
  const [registeredNode, setRegisteredNode] = useState<RegisteredNode>();
  const logRequest = useRef<AbortController | undefined>(undefined);

  const pending = useRef(new Set<AbortController>());
  const listGate = useRef(new LoadGate());
  const detailGate = useRef(new LoadGate());
  const report = useCallback(
    (e: unknown) => {
      if (e instanceof DOMException && e.name === "AbortError") return;
      if (e instanceof APIError && e.status === 401) {
        setError(t.session);
        return;
      }
      setError(
        e instanceof APIError && e.status === 409
          ? t.conflict
          : e instanceof Error
            ? e.message
            : t.unavailable,
      );
    },
    [t],
  );
  useEffect(
    () => () => {
      pending.current.forEach((c) => c.abort());
    },
    [],
  );
  useEffect(() => {
    if (!client) return;
    const controller = new AbortController();
    const gate = listGate.current;
    const valid = gate.begin();
    setLoading(true);
    void client
      .list(controller.signal)
      .then((value) => {
        if (valid()) {
          setClusters(value);
          // Clear a prior error only once the refresh succeeds, so a banner
          // does not blink away and back on every periodic refresh.
          setError("");
        }
      })
      .catch((e) => {
        if (valid() && !controller.signal.aborted) {
          setClusters(undefined);
          report(e);
        }
      })
      .finally(() => {
        if (valid()) setLoading(false);
      });
    return () => {
      controller.abort();
      gate.cancel();
    };
  }, [client, revision, report]);
  useEffect(() => {
    setDetail(undefined);
    setJobs(undefined);
    setShowJob(false);
    setShowNodeRegistration(false);
    setShowEnroll(false);
    setRegisteredNode(undefined);
    setTab("nodes");
    observedTarget.current = undefined;
    setTarget("");
  }, [client, selection]);
  useEffect(() => {
    if (!client || !selection) return;
    const controller = new AbortController();
    const gate = detailGate.current;
    const valid = gate.begin();
    void client
      .detail(selection.namespace, selection.name, controller.signal)
      .then((value) => {
        if (valid()) {
          setDetail(value);
          const prior = observedTarget.current;
          setTarget((previous) =>
            prior !== undefined && previous !== prior
              ? previous
              : value.cluster.targetAgentRelease,
          );
          observedTarget.current = value.cluster.targetAgentRelease;
        }
      })
      .catch((e) => {
        if (valid() && !controller.signal.aborted) {
          setDetail(undefined);
          report(e);
        }
      });
    return () => {
      controller.abort();
      gate.cancel();
    };
  }, [client, selection, revision, report]);
  useEffect(() => {
    if (!client || !selection || !detail?.jobSubmissionEnabled) {
      setJobs(undefined);
      setJobsLoading(false);
      return;
    }
    const controller = new AbortController();
    setJobsLoading(true);
    void client
      .jobs(selection.namespace, selection.name, controller.signal)
      .then((value) => {
        if (!controller.signal.aborted) setJobs(value);
      })
      .catch((e) => {
        if (!controller.signal.aborted) {
          setJobs(undefined);
          report(e);
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setJobsLoading(false);
      });
    return () => controller.abort();
  }, [client, selection, detail?.jobSubmissionEnabled, revision, report]);
  async function mutate(action: (signal: AbortSignal) => Promise<void>) {
    const controller = new AbortController();
    pending.current.add(controller);
    setWriting(true);
    setError("");
    try {
      await action(controller.signal);
      if (!controller.signal.aborted) setRevision((v) => v + 1);
    } catch (e) {
      if (!controller.signal.aborted) report(e);
    } finally {
      pending.current.delete(controller);
      if (!controller.signal.aborted) setWriting(false);
    }
  }
  const readLogs = async (allocation: Detail["allocations"][number]) => {
    if (
      !client ||
      !selection ||
      !detail ||
      !canReadDiagnostic(detail, allocation)
    )
      return;
    const controller = new AbortController();
    logRequest.current?.abort();
    logRequest.current = controller;
    setLogText(undefined);
    pending.current.add(controller);
    setLogLoading(true);
    try {
      const snapshot = await client.logs(
        selection.namespace,
        allocation,
        controller.signal,
      );
      if (controller.signal.aborted) return;
      const text = decodeLogText(snapshot.data);
      setLogText({
        key: allocation.uid,
        text,
        note:
          snapshot.truncated || snapshot.retentionLimitReached
            ? t.logBounded
            : "",
      });
    } catch (e) {
      if (!controller.signal.aborted) {
        setLogText(undefined);
        report(e);
      }
    } finally {
      pending.current.delete(controller);
      if (!controller.signal.aborted) setLogLoading(false);
    }
  };
  const readStats = async (allocation: Detail["allocations"][number]) => {
    if (
      !client ||
      !selection ||
      !detail ||
      !canReadDiagnostic(detail, allocation)
    )
      return;
    const node = detail?.nodes.find((n) => n.name === allocation.nodeName);
    if (
      detail?.cluster.runtime !== "Docker" ||
      !node?.capabilities?.includes("docker-diagnostics-v1") ||
      allocation.deleting ||
      allocation.cleanupConfirmed
    )
      return;
    const controller = new AbortController();
    statsRequest.current?.abort();
    statsRequest.current = controller;
    setStats(undefined);
    setStatsLoading(true);
    try {
      const value = await client.stats(
        selection.namespace,
        allocation,
        controller.signal,
      );
      if (!controller.signal.aborted) setStats({ key: allocation.uid, value });
    } catch (e) {
      if (!controller.signal.aborted) {
        setStats(undefined);
        report(e);
      }
    } finally {
      if (!controller.signal.aborted) setStatsLoading(false);
    }
  };
  const diagnosticScope = diagnosticInventory(detail);
  useEffect(() => {
    logRequest.current?.abort();
    statsRequest.current?.abort();
    setLogText(undefined);
    setStats(undefined);
    setLogLoading(false);
    setStatsLoading(false);
    return () => {
      logRequest.current?.abort();
      statsRequest.current?.abort();
    };
  }, [client, selection, diagnosticScope]);
  useEffect(() => {
    if (!client || writing) return;
    const timer = setInterval(() => setRevision((v) => v + 1), 15000);
    return () => clearInterval(timer);
  }, [client, writing]);
  const publish = () => {
    if (!client || !detail) return;
    const snapshot = detail.cluster;
    const release = detail.releases.find((v) => v.name === target);
    void mutate(async (signal) => {
      await client.target(snapshot, target, release?.uid ?? "", signal);
      observedTarget.current = undefined;
    });
  };
  const create = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!client || jobIntent || nodeIntent) return;
    const data = new FormData(event.currentTarget);
    let retained: ClusterIntent;
    try {
      retained =
        intent ??
        clusterIntentFromForm(crypto.randomUUID(), {
          name: String(data.get("name") || ""),
          agentNamespace: String(data.get("namespace") || ""),
          tenantID: String(data.get("tenant") || ""),
          runtime: String(data.get("runtime") || ""),
        });
    } catch (error) {
      setError(
        error instanceof Error ? error.message : "Invalid cluster intent",
      );
      return;
    }
    try {
      sessionStorage.setItem(intentKey, JSON.stringify(retained));
    } catch {
      /* The same mounted form still retains the exact in-memory intent. */
    }
    setIntent(retained);
    void mutate(async (signal) => {
      const created = await client.create(retained, signal);
      if (signal.aborted) return;
      setIntent(undefined);
      try {
        sessionStorage.removeItem(intentKey);
      } catch {
        /* No credentials were persisted. */
      }
      setShowCreate(false);
      if (!jobIntent && !nodeIntent) setSelection(created);
    });
  };
  const submitJob = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!client || !selection || !detail || nodeIntent || intent) return;
    const data = new FormData(event.currentTarget);
    let request: RawJobRequest;
    try {
      request =
        jobIntent ??
        rawJobIntent(
          crypto.randomUUID(),
          detail.cluster.uid,
          String(data.get("environment") || ""),
          String(data.get("argv") || ""),
          String(data.get("env") || "[]"),
          String(data.get("directory") || ""),
        );
      if (
        request.clusterUID !== detail.cluster.uid ||
        !detail.jobSubmissionEnabled
      )
        throw new Error();
    } catch {
      setError(t.invalidJob);
      return;
    }
    setJobIntent(request);
    void mutate(async (signal) => {
      try {
        await client.submitJob(
          selection.namespace,
          selection.name,
          request,
          signal,
        );
      } catch (e) {
        // Validation failures precede effects. Other failures retain the exact
        // request because the server may already have committed it.
        if (!signal.aborted && e instanceof APIError && e.status === 400) {
          setJobIntent(undefined);
        }
        throw e;
      }
      if (signal.aborted) return;
      setJobIntent(undefined);
      setShowJob(false);
    });
  };
  const registerNode = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!client || !selection || !detail || jobIntent || intent) return;
    const data = new FormData(event.currentTarget);
    let request: NodeRegistration;
    try {
      request =
        nodeIntent ??
        nodeRegistrationIntent(crypto.randomUUID(), detail.cluster.uid, {
          name: String(data.get("name") || ""),
          instanceID: String(data.get("instanceID") || ""),
          privateAddress: String(data.get("privateAddress") || ""),
          environment: String(data.get("environment") || ""),
          networkScope: String(data.get("networkScope") || ""),
          role: String(data.get("role") || "Worker") as "Gateway" | "Worker",
          gpuText: String(data.get("gpus") || "[]"),
        });
      if (
        request.clusterUID !== detail.cluster.uid ||
        !detail.nodeRegistrationEnabled
      )
        throw new Error();
    } catch {
      setError(t.invalidNode);
      return;
    }
    setNodeIntent(request);
    setRegisteredNode(undefined);
    void mutate(async (signal) => {
      try {
        const result = await client.registerNode(
          selection.namespace,
          selection.name,
          request,
          signal,
        );
        if (signal.aborted) return;
        setRegisteredNode(result);
        setNodeIntent(undefined);
      } catch (e) {
        // Only API validation before provisioning has no possible effects.
        if (!signal.aborted && e instanceof APIError && e.status === 400)
          setNodeIntent(undefined);
        throw e;
      }
    });
  };
  const tabs: [typeof tab, string, number][] = detail
    ? [
        ["nodes", t.nodes, detail.nodes.length],
        ...(detail.jobSubmissionEnabled || jobIntent
          ? [["jobs", t.jobsTitle, jobs?.length ?? 0] as [typeof tab, string, number]]
          : []),
        ["allocations", t.allocations, detail.allocations.length],
        ["releases", t.releases, detail.releases.length],
        ["upgrades", t.upgrades, detail.upgrades.length],
      ]
    : [];
  if (detail && (detail.enrollmentEnabled || detail.enrollments.length))
    tabs.splice(tabs[1]?.[0] === "jobs" ? 2 : 1, 0, ["enrollments", t.enrollments, detail.enrollments.length]);
  const pendingEnrollments = detail ? activeEnrollments(detail) : 0;
  return (
    <div className="page-content resource-page container-runtime-page">
      <header className="section-heading">
        <div>
          <h1>
            {t.title}
            <Hint text={t.onboarding} />
          </h1>
          <p className="muted">{t.subtitle}</p>
        </div>
        <div className="actions">
          <button
            disabled={writing || (loading && clusters === undefined)}
            onClick={() => setRevision((v) => v + 1)}
          >
            {t.refresh}
          </button>
          <button
            className="primary"
            disabled={writing || !!jobIntent || !!nodeIntent}
            onClick={() => setShowCreate(true)}
          >
            ＋ {t.create}
          </button>
        </div>
      </header>
      {error && (
        <div role="alert" className="error">
          {error}
        </div>
      )}
      <>
        <section className="layout">
          <nav className="container-cluster-picker" aria-label={t.title}>
            <div className="cluster-options">
              {/* Keep the loaded list during background refreshes; the
                  placeholder is only for the first load. */}
              {loading && clusters === undefined ? (
                <p className="muted">{t.loading}</p>
              ) : clusters === undefined ? (
                <p className="muted">{t.unavailable}</p>
              ) : clusters.length === 0 ? (
                <p className="muted">{t.empty}</p>
              ) : (
                clusters.map((c) => (
                  <button
                    disabled={writing || !!jobIntent || !!nodeIntent}
                    className={`cluster ${selection?.name === c.name && selection.namespace === c.namespace ? "selected" : ""}`}
                    key={c.uid}
                    onClick={() => {
                      setError("");
                      observedTarget.current = undefined;
                      setSelection({ namespace: c.namespace, name: c.name });
                    }}
                  >
                    <StatusDot
                      label={c.name}
                      tone={clusterTone(clusterState(c))}
                      detail={t[clusterState(c)]}
                    />
                    <span>
                      {c.runtime} · {c.readyNodes}/{c.nodeCount}
                    </span>
                  </button>
                ))
              )}
            </div>
          </nav>
          <div className="content">
            <Drawer
              open={showCreate}
              title={t.create}
              hint={intent ? t.remembered : t.createHint}
              closeLabel={t.close}
              onClose={() => setShowCreate(false)}
            >
                <form className="create" onSubmit={create}>
                  <label>
                    {t.name}
                    <input
                      name="name"
                      required
                      disabled={!!intent || writing}
                      defaultValue={intent?.name}
                    />
                  </label>
                  <label>
                    {t.namespace}
                    <input
                      name="namespace"
                      required
                      disabled={!!intent || writing}
                      defaultValue={intent?.agentNamespace}
                    />
                  </label>
                  <label>
                    {t.tenant}
                    <input
                      name="tenant"
                      required
                      disabled={!!intent || writing}
                      defaultValue={intent?.tenantID}
                    />
                  </label>
                  <label>
                    {t.runtime}
                    <select
                      name="runtime"
                      required
                      disabled={!!intent || writing}
                      defaultValue={intent?.runtime || "Docker"}
                    >
                      <option value="Docker">{t.dockerRuntime}</option>
                      <option value="Raw">{t.rawRuntime}</option>
                    </select>
                  </label>
                  {intent && <code>{intent.operationID}</code>}
                  <button className="primary" disabled={writing}>
                    {intent ? t.retry : t.create}
                  </button>
                </form>
            </Drawer>
            {!selection ? (
              <div className="empty">{t.select}</div>
            ) : !detail ? (
              <div className="empty">{error ? t.unavailable : t.loading}</div>
            ) : (
              <>
                <section className="hero">
                  <div className="hero-identity">
                    <h2>
                      {detail.cluster.name}
                      <span className={`pill ${clusterState(detail.cluster)}`}>
                        {t[clusterState(detail.cluster)]}
                      </span>
                    </h2>
                    <p className="muted" title={`UID ${detail.cluster.uid}`}>
                      {detail.cluster.runtime} · {detail.cluster.namespace} ·{" "}
                      {detail.cluster.tenantID} · {t.gpu}{" "}
                      {detail.cluster.gpuCount}
                    </p>
                  </div>
                  <div className="target">
                    <span className="target-label">
                      {t.target}
                      <Hint text={t.targetHint} />
                    </span>
                    <select
                      aria-label={t.target}
                      value={target}
                      disabled={writing}
                      onChange={(e) => setTarget(e.target.value)}
                    >
                      <option value="">{t.clear}</option>
                      {detail.cluster.targetAgentRelease &&
                        !detail.releases.some(
                          (v) => v.name === detail.cluster.targetAgentRelease,
                        ) && (
                          <option
                            disabled
                            value={detail.cluster.targetAgentRelease}
                          >
                            {detail.cluster.targetAgentRelease} · unavailable
                          </option>
                        )}
                      {detail.releases.map((r) => (
                        <option
                          key={r.uid}
                          value={r.name}
                          disabled={!r.available}
                        >
                          {r.name} · {r.version}
                          {r.available ? "" : " · unavailable"}
                        </option>
                      ))}
                    </select>
                    <button
                      className="primary"
                      onClick={publish}
                      disabled={
                        writing ||
                        !detail.cluster.bindingValid ||
                        detail.cluster.deleting ||
                        target === detail.cluster.targetAgentRelease ||
                        (!!target &&
                          !detail.releases.some(
                            (v) => v.name === target && v.available,
                          ))
                      }
                    >
                      {t.apply}
                    </button>
                  </div>
                </section>
                <OverviewCharts detail={detail} jobs={jobs} t={t} />
                <section className="table-card">
                  <div className="table-card-head">
                    <div className="tabs" role="tablist">
                      {tabs.map(([key, label, count]) => (
                        <button
                          key={key}
                          type="button"
                          role="tab"
                          aria-selected={tab === key}
                          className={tab === key ? "active" : ""}
                          onClick={() => setTab(key)}
                        >
                          {label}
                          <b>{count}</b>
                          {key === "enrollments" && pendingEnrollments > 0 && (
                            <i
                              className="tab-pulse tone-info"
                              aria-label={`${pendingEnrollments} ${t.running}`}
                            />
                          )}
                        </button>
                      ))}
                    </div>
                    {tab === "nodes" && (
                      <div className="actions">
                        {detail.nodeRegistrationEnabled && (
                          <button
                            type="button"
                            disabled={writing || !!jobIntent || !!intent}
                            onClick={() => setShowNodeRegistration(true)}
                          >
                            {t.registerNode}
                          </button>
                        )}
                        <button
                          type="button"
                          className="primary"
                          onClick={() => {
                            setEnrollFocus(undefined);
                            setShowEnroll(true);
                          }}
                        >
                          ＋ {t.enroll}
                        </button>
                      </div>
                    )}
                  </div>
                  <div hidden={tab !== "jobs"}>
                <JobsPanel
                  detail={detail}
                  t={t}
                  writing={writing || !!nodeIntent || !!intent}
                  jobIntent={jobIntent}
                  jobsLoading={jobsLoading && jobs === undefined}
                  jobs={jobs}
                  onOpen={() => setShowJob(true)}
                  onSuspend={(job) => {
                    if (client && selection)
                      void mutate(async (signal) => {
                        await client.suspendJob(
                          selection.namespace,
                          selection.name,
                          job,
                          signal,
                        );
                      });
                  }}
                />
                  </div>
                <Drawer
                  open={showJob}
                  title={t.submitJob}
                  hint={t.jobHint}
                  closeLabel={t.close}
                  onClose={() => setShowJob(false)}
                >
                  <JobForm
                    detail={detail}
                    t={t}
                    writing={writing || !!nodeIntent || !!intent}
                    jobIntent={jobIntent}
                    onSubmit={submitJob}
                  />
                </Drawer>
                <Drawer
                  open={showEnroll}
                  title={t.enroll}
                  closeLabel={t.close}
                  onClose={() => setShowEnroll(false)}
                >
                  <EnrollmentPanel key={detail.cluster.uid} focusNode={showEnroll ? enrollFocus : undefined} detail={detail} client={client} lang={lang} blocked={writing || !!jobIntent || !!nodeIntent || !!intent} onChanged={()=>setRevision(v=>v+1)} />
                </Drawer>
                <Drawer
                  open={showNodeRegistration}
                  title={t.registerNode}
                  hint={t.registrationHint}
                  closeLabel={t.close}
                  onClose={() => setShowNodeRegistration(false)}
                >
                  <NodeRegistrationPanel
                    detail={detail}
                    t={t}
                    writing={writing}
                    blocked={!!jobIntent || !!intent}
                    intent={nodeIntent}
                    result={registeredNode}
                    onSubmit={registerNode}
                  />
                </Drawer>
                <div hidden={tab !== "nodes"}>
                  {!detail.nodes.length ? (
                    <p className="muted">{t.noRecords}</p>
                  ) : (
                    <div className="table-scroll node-table-scroll">
                      <table className="node-table">
                        <thead>
                          <tr>
                            <th>{t.nodeColumn}</th>
                            <th>{t.roleColumn}</th>
                            <th>{t.reachability}</th>
                            <th>{t.privateIP}</th>
                            <th>{t.nativeState}</th>
                            <th>{t.agentState}</th>
                            <th>{t.version}</th>
                            <th>{t.schedulable}</th>
                            <th>{t.heartbeat}</th>
                            <th>{t.actions}</th>
                          </tr>
                        </thead>
                        <tbody>
                          {detail.nodes.map((n) => (
                            <NodeCard
                              key={n.uid}
                              node={n}
                              probe={nodeProbe(detail,n)}
                              runtime={detail.cluster.runtime}
                              expanded={expandedNodes.has(n.uid)}
                              onExpand={(open) =>
                                setExpandedNodes((current) => {
                                  if (current.has(n.uid) === open)
                                    return current;
                                  const next = new Set(current);
                                  if (open) next.add(n.uid);
                                  else next.delete(n.uid);
                                  return next;
                                })
                              }
                              t={t}
                              disabled={
                                writing ||
                                detail.cluster.deleting ||
                                !detail.cluster.bindingValid
                              }
                              onRedeploy={() => {
                                setEnrollFocus({ name: n.name, seq: Date.now() });
                                setShowEnroll(true);
                              }}
                              onScheduling={() => {
                                if (client)
                                  void mutate((signal) =>
                                    client.nodeScheduling(
                                      detail.cluster,
                                      n,
                                      !n.unschedulable,
                                      signal,
                                    ),
                                  );
                              }}
                            />
                          ))}
                        </tbody>
                      </table>
                    </div>
                  )}
                </div>
                <div hidden={tab !== "releases"}>
                  <Table
                    empty={t.noRecords}
                    rows={detail.releases.map((r) => (
                      <tr key={r.uid}>
                        <td>
                          <strong>{r.name}</strong>
                          <small>{r.version}</small>
                        </td>
                        <td>
                          {r.os}/{r.architecture}
                        </td>
                        <td>
                          <span
                            className={`pill ${r.available ? "ready" : "stale"}`}
                          >
                            {r.available ? t.ready : t.stale}
                          </span>
                        </td>
                        <td>
                          <code>{r.sha256.slice(0, 16)}…</code>
                        </td>
                      </tr>
                    ))}
                  />
                </div>
                <div hidden={tab !== "allocations"}>
                  <Table
                    empty={t.noRecords}
                    rows={detail.allocations.map((a) => (
                      <tr key={a.uid}>
                        <td>
                          {a.name}
                          <small>{a.kind}</small>
                        </td>
                        <td>{a.nodeName}</td>
                        <td>
                          {a.jobName} / {a.taskName}
                        </td>
                        <td>
                          {a.phase || "Pending"}
                          {a.deleting ? " · deleting" : ""}
                          <small>
                            cleanup: {a.cleanupConfirmed ? t.yes : t.no}
                          </small>
                          <button
                            type="button"
                            disabled={
                              logLoading ||
                              statsLoading ||
                              !canReadDiagnostic(detail, a)
                            }
                            onClick={() => void readLogs(a)}
                          >
                            {logLoading ? t.logLoading : t.logs}
                          </button>
                          {detail.cluster.runtime === "Docker" && (
                            <button
                              type="button"
                              disabled={
                                statsLoading ||
                                logLoading ||
                                !canReadDiagnostic(detail, a)
                              }
                              onClick={() => void readStats(a)}
                            >
                              {statsLoading ? t.statsLoading : t.stats}
                            </button>
                          )}
                        </td>
                      </tr>
                    ))}
                  />
                  {stats && (
                    <div className="log-viewer">
                      <div className="actions">
                        <strong>{stats.key}</strong>
                        <button
                          type="button"
                          onClick={() => setStats(undefined)}
                        >
                          {t.close}
                        </button>
                      </div>
                      <small>
                        {t.statsAt}: {stats.value.sampledAt}
                      </small>
                      <pre>
                        {[
                          `${t.cpuUsage}: ${stats.value.cpuUsageNS ?? t.unavailableValue}`,
                          `${t.memory}: ${stats.value.memoryUsageBytes ?? t.unavailableValue} / ${stats.value.memoryLimitBytes ?? t.unavailableValue} ${t.bytes}`,
                          `${t.pids}: ${stats.value.pids ?? t.unavailableValue}`,
                          `${t.blockIO}: ${stats.value.blockReadBytes ?? t.unavailableValue} ${t.read} / ${stats.value.blockWriteBytes ?? t.unavailableValue} ${t.write} ${t.bytes}`,
                        ].join("\n")}
                      </pre>
                      <small>{t.gpuStatsHint}</small>
                    </div>
                  )}
                  {logText && (
                    <div className="log-viewer">
                      <div className="actions">
                        <strong>{logText.key}</strong>
                        <button
                          type="button"
                          onClick={() => setLogText(undefined)}
                        >
                          {t.logClose}
                        </button>
                      </div>
                      <pre>{logText.text}</pre>
                      {logText.note && <small>{logText.note}</small>}
                    </div>
                  )}
                </div>
                <div hidden={tab !== "enrollments"}>
                  <EnrollmentOperations
                    detail={detail}
                    client={client}
                    lang={lang}
                    blocked={writing || !!jobIntent || !!nodeIntent || !!intent}
                    onChanged={() => setRevision((v) => v + 1)}
                  />
                </div>
                <div hidden={tab !== "upgrades"}>
                  <Table
                    empty={t.noRecords}
                    rows={detail.upgrades.map((u) => (
                      <tr key={u.uid}>
                        <td>{u.name}</td>
                        <td>
                          {u.nodeName} → {u.releaseName}
                        </td>
                        <td>
                          <Conditions
                            items={u.conditions}
                            fallback={u.phase || "Pending"}
                          />
                        </td>
                      </tr>
                    ))}
                  />
                </div>
                </section>
              </>
            )}
          </div>
        </section>
      </>
    </div>
  );
}
function NodeCard({
  node: n,
  probe,
  runtime,
  t,
  disabled,
  onScheduling,
  onRedeploy,
  expanded,
  onExpand,
}: {
  node: Node;
  probe?: EnrollmentNode;
  runtime: "Raw" | "Docker";
  t: typeof labels.en;
  disabled: boolean;
  onScheduling: () => void;
  onRedeploy: () => void;
  expanded: boolean;
  onExpand: (open: boolean) => void;
}) {
  const nativeStatus = nativeNodeState(n);
  const agentStatus = nodeState(n);
  const addressList = (n.resources?.addresses ?? [])
    .map((address) => `${address.type}: ${address.address}`)
    .join(" · ");
  const capacity = Object.entries(n.capacity ?? {})
    .map(([key, value]) => `${key}: ${value}`)
    .join(" · ");
  const nativeCapacity = Object.entries(n.resources?.capacity ?? {})
    .map(([key, value]) => `${key}: ${value}`)
    .join(" · ");
  const nativeAllocatable = Object.entries(n.resources?.allocatable ?? {})
    .map(([key, value]) => `${key}: ${value}`)
    .join(" · ");
  const nativeInfo = n.resources?.nodeInfo;
  const nativeStatusLabel =
    nativeStatus === "online"
      ? t.nativeOnline
      : nativeStatus === "offline"
        ? t.nativeOffline
        : t.nativeUnknown;
  const fields = [
    [t.privateIP, n.privateAddress || "—"],
    [t.nativeAddresses, addressList || "—"],
    [t.runtime, `${runtime} · ${n.agentVersion || "—"}`],
    [t.instance, n.instanceID || "—"],
    [t.roleColumn, n.role || "Worker"],
    ["UID", n.uid],
    [t.nativeUID, n.nativeUID || "—"],
    [t.environment, n.environment || "—"],
    [t.networkScope, n.networkScope || "—"],
    [t.busy, n.busy ? t.yes : t.no],
    [
      t.heartbeat,
      `${heartbeatAge(n.lastHeartbeatTime)} · ${n.lastHeartbeatTime || "—"}`,
    ],
    [t.scheduling, n.unschedulable ? t.cordoned : t.schedulable],
    [t.capacity, capacity || "—"],
    [t.nativeCapacity, nativeCapacity || "—"],
    [t.nativeAllocatable, nativeAllocatable || "—"],
    [t.gpu, (n.authorizedGPUUUIDs ?? []).join(", ") || "—"],
    [t.discoveredGPU, (n.discoveredGPUUUIDs ?? []).join(", ") || "—"],
    [t.capabilities, (n.capabilities ?? []).join(", ") || "—"],
    [t.nativePhase, n.resources?.phase || "—"],
    [t.nativeObserved, n.resources?.observedAt || "—"],
    [
      t.nodeInfo,
      [
        nativeInfo?.operatingSystem,
        nativeInfo?.architecture,
        nativeInfo?.kernelVersion,
        nativeInfo?.agentVersion,
        nativeInfo?.incarnation,
      ]
        .filter(Boolean)
        .join(" · ") || "—",
    ],
  ];
  return (
    <>
      <tr className="node-summary-row">
        <td>
          <button
            type="button"
            className="node-expand-button"
            aria-expanded={expanded}
            onClick={() => onExpand(!expanded)}
          >
            {expanded ? "▾" : "▸"} {n.name}
          </button>
        </td>
        <td>{n.role || "Worker"}</td>
        <td>
          <span className={`pill ${probeState(probe)==="Reachable"?"ready":"stale"}`} title={probe?.probedAt}>{probeState(probe)==="Unknown"?t.reachabilityUnknown:probeState(probe)}</span>
        </td>
        <td>{n.privateAddress || "—"}</td>
        <td>
          <span className={`pill ${nativeStatus}`}>{nativeStatusLabel}</span>
        </td>
        <td>
          <span className={`pill ${agentStatus}`}>{t[agentStatus]}</span>
        </td>
        <td>{n.agentVersion || "—"}</td>
        <td>{n.schedulable ? t.yes : t.no}</td>
        <td>{heartbeatAge(n.lastHeartbeatTime)}</td>
        <td className="col-actions">
          <button
            type="button"
            title={t.redeployHint}
            onClick={onRedeploy}
          >
            {t.redeploy}
          </button>
          <button
            type="button"
            disabled={
              disabled ||
              !n.bindingValid ||
              !n.resourceVersion ||
              (n.unschedulable && n.held)
            }
            onClick={onScheduling}
          >
            {n.unschedulable ? t.uncordon : t.cordon}
          </button>
        </td>
      </tr>
      {expanded && (
        <tr className="node-detail-row">
          <td colSpan={10}>
            <table className="node-detail-table">
              <tbody>
                {fields.map(([label, value]) => (
                  <tr key={label}>
                    <th scope="row">{label}</th>
                    <td>{value}</td>
                  </tr>
                ))}
                {(n.conditions ?? []).map((condition) => (
                  <tr key={condition.type}>
                    <th scope="row">{condition.type}</th>
                    <td>
                      <strong>{condition.status}</strong> · {condition.reason}{" "}
                      {condition.message} · generation{" "}
                      {condition.observedGeneration ?? "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            <p className="muted node-scheduling-hint">{t.schedulingHint}</p>
          </td>
        </tr>
      )}
    </>
  );
}
// Conditions render as labelled dots; reason, message and generation move
// into the focusable tooltip so the table stays scannable.
function Conditions({
  items,
  fallback,
}: {
  items: Detail["cluster"]["conditions"];
  fallback?: string;
}) {
  const list = items ?? [];
  if (!list.length && fallback)
    return <StatusDot label={fallback} tone={phaseTone(fallback)} />;
  return (
    <div className="conditions">
      {fallback && <StatusDot label={fallback} tone={phaseTone(fallback)} />}
      {list.map((c) => (
        <StatusDot
          key={c.type}
          label={c.type}
          tone={c.status === "True" ? "good" : c.status === "False" ? "bad" : "neutral"}
          detail={`${c.status} · ${c.reason || "—"}${c.message ? ` · ${c.message}` : ""} · generation ${c.observedGeneration ?? "—"}`}
        />
      ))}
    </div>
  );
}
function OverviewCharts({
  detail,
  jobs,
  t,
}: {
  detail: Detail;
  jobs?: JobSummary[];
  t: typeof labels.en;
}) {
  const ready = detail.nodes.filter((n) => nodeState(n) === "ready").length;
  const held = detail.nodes.filter((n) => nodeState(n) === "held").length;
  const reach = { Reachable: 0, Unreachable: 0, Unknown: 0 };
  for (const n of detail.nodes) {
    const s = probeState(nodeProbe(detail, n));
    if (s === "Reachable") reach.Reachable++;
    else if (s === "Unreachable") reach.Unreachable++;
    else reach.Unknown++;
  }
  const jobCount = (phases: string[]) =>
    (jobs ?? []).filter((j) => phases.includes(j.phase)).length;
  const succeeded = jobCount(["Succeeded"]);
  const failed = jobCount(["Failed", "Stopped"]);
  const running = jobCount(["Running"]);
  const otherJobs = (jobs?.length ?? 0) - succeeded - failed - running;
  const last = detail.cluster.lastAgentUpgrade;
  return (
    <div className="chart-strip">
      <Donut
        title={t.chartNodes}
        center={`${ready}/${detail.nodes.length}`}
        segments={[
          { label: t.ready, value: ready, tone: "good" },
          { label: t.held, value: held, tone: "info" },
          { label: t.notReady, value: detail.nodes.length - ready - held, tone: "bad" },
        ]}
      />
      <SegmentBar
        title={t.chartReach}
        center={`${reach.Reachable}/${detail.nodes.length}`}
        segments={[
          { label: t.reachable, value: reach.Reachable, tone: "good" },
          { label: t.unreachable, value: reach.Unreachable, tone: "bad" },
          { label: t.reachabilityUnknown, value: reach.Unknown, tone: "neutral" },
        ]}
      />
      <Donut
        title={t.chartJobs}
        center={`${jobs?.length ?? 0}`}
        segments={[
          { label: t.succeeded, value: succeeded, tone: "good" },
          { label: t.failed, value: failed, tone: "bad" },
          { label: t.running, value: running, tone: "info" },
          { label: t.other, value: otherJobs, tone: "neutral" },
        ]}
      />
      <figure className="chart-card">
        <figcaption>
          {t.chartRollout}
          <strong className="chart-headline">
            {detail.cluster.targetAgentRelease || t.noTarget}
          </strong>
        </figcaption>
        <Conditions items={detail.cluster.conditions} />
        {last && (
          <p className="muted chart-foot" title={last.completedAt}>
            {t.rollout}: {last.nodeRecord.name} → {last.release.name}
          </p>
        )}
      </figure>
    </div>
  );
}
function clusterTone(state: ReturnType<typeof clusterState>): Tone {
  return state === "ready" ? "good" : state === "degraded" ? "warn" : "bad";
}
function phaseTone(phase: string): Tone {
  if (["Succeeded", "Completed", "Ready"].includes(phase)) return "good";
  if (["Failed", "Stopped", "Cancelled"].includes(phase)) return "bad";
  if (["Running", "Applying", "Active"].includes(phase)) return "info";
  return "neutral";
}
function Table({ rows, empty }: { rows: ReactNode[]; empty: string }) {
  return rows.length ? (
    <div className="table-scroll">
      <table>
        <tbody>{rows}</tbody>
      </table>
    </div>
  ) : (
    <p className="muted">{empty}</p>
  );
}
