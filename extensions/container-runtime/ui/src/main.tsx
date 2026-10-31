import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";
import { createRoot } from "react-dom/client";
import {
  APIError,
  Client,
  login,
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
  logText as decodeLogText,
  rawJobIntent,
  nodeRegistrationIntent,
  clusterIntentFromForm,
  canReadDiagnostic,
  diagnosticInventory,
} from "./models";
import { JobsPanel } from "./JobsPanel";
import { NodeRegistrationPanel } from "./NodeRegistrationPanel";
import "./style.css";
const labels = {
  zh: {
    title: "容器集群",
    subtitle: "独立扩展管理面 · Raw 与 Docker 节点预览",
    refresh: "刷新",
    loading: "加载中…",
    unavailable: "数据不可用",
    empty: "没有容器集群",
    select: "选择集群查看节点和升级状态",
    nodes: "节点",
    registerNode: "登记运行时节点",
    nodeName: "节点名称",
    instanceID: "已核验的实例身份（Raw 容器 / Docker Engine）",
    privateAddress: "私网 IPv4 地址",
    networkScope: "网络范围",
    gpuInput: "平台授权 GPU UUID（JSON 字符串数组）",
    registrationHint:
      "登记硬件授权后，在管理员机器使用 container-runtime-admin node-package 制包。安装并收到新鲜心跳后才会就绪；请先核验实例与 GPU 授权。",
    registrationPending:
      "登记结果待确认。保存 UUID 和原始输入，重试相同请求。切换集群已禁用；刷新或退出会丢失内存请求。",
    registrationDone:
      "节点登记已确认；请单独核对安装和心跳。以下为 node-package 的 node 身份字段：",
    invalidNode:
      "请填写有效节点名、实例身份、私网 IPv4、环境、网络范围及不重复的授权 GPU JSON 数组。",
    releases: "Agent 版本",
    allocations: "资源预留",
    upgrades: "升级操作",
    target: "目标版本",
    clear: "暂停新升级（清空目标）",
    apply: "发布目标",
    ready: "就绪",
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
    heartbeat: "心跳距今",
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
  },
  en: {
    title: "Container clusters",
    subtitle: "Independent extension · Raw and Docker node preview",
    refresh: "Refresh",
    loading: "Loading…",
    unavailable: "Data unavailable",
    empty: "No container clusters",
    select: "Select a cluster to inspect nodes and upgrades",
    nodes: "Nodes",
    registerNode: "Register runtime node",
    nodeName: "Node name",
    instanceID: "Verified instance identity (Raw container / Docker Engine)",
    privateAddress: "Private IPv4 address",
    networkScope: "Network scope",
    gpuInput: "Platform-authorized GPU UUIDs (JSON string array)",
    registrationHint:
      "After authorizing inventory, use container-runtime-admin node-package on the administrator machine. Installation and a fresh heartbeat are required for readiness. Verify incarnation and GPU entitlement first.",
    registrationPending:
      "Registration is unconfirmed. Retain the UUID and exact input and retry. Cluster switching is disabled; refresh or sign out loses the in-memory request.",
    registrationDone:
      "Node registration confirmed; verify installation and heartbeat separately. Use these node identity fields in node-package:",
    invalidNode:
      "Provide a valid node name, incarnation, private IPv4, environment, network scope and unique authorized GPU JSON array.",
    releases: "Agent releases",
    allocations: "Allocations",
    upgrades: "Upgrade operations",
    target: "Target release",
    clear: "Pause new upgrades (clear target)",
    apply: "Publish target",
    ready: "Ready",
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
    heartbeat: "Heartbeat age",
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
function App() {
  const [lang, setLang] = useState<Lang>("zh");
  const t = labels[lang];
  const [token, setToken] = useState("");
  const client = useMemo(
    () => (token ? new Client(token) : undefined),
    [token],
  );
  const [clusters, setClusters] = useState<Cluster[]>();
  const [selection, setSelection] = useState<{
    namespace: string;
    name: string;
  }>();
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
  const [nodeIntent, setNodeIntent] = useState<NodeRegistration>();
  const [registeredNode, setRegisteredNode] = useState<RegisteredNode>();
  const logRequest = useRef<AbortController | undefined>(undefined);
  const pending = useRef(new Set<AbortController>());
  const listGate = useRef(new LoadGate());
  const detailGate = useRef(new LoadGate());
  const report = (e: unknown) => {
    if (e instanceof DOMException && e.name === "AbortError") return;
    if (e instanceof APIError && e.status === 401) {
      logout();
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
  };
  const logout = () => {
    pending.current.forEach((c) => c.abort());
    pending.current.clear();
    listGate.current.cancel();
    detailGate.current.cancel();
    setWriting(false);
    setExpandedNodes(new Set());
    setToken("");
    setClusters(undefined);
    setDetail(undefined);
    setJobs(undefined);
    setShowJob(false);
    setJobIntent(undefined);
    setNodeIntent(undefined);
    setShowNodeRegistration(false);
    setRegisteredNode(undefined);
    setSelection(undefined);
    logRequest.current?.abort();
    setLogText(undefined);
    setLogLoading(false);
    statsRequest.current?.abort();
    setStats(undefined);
    setStatsLoading(false);
    setError("");
  };
  useEffect(
    () => () => {
      pending.current.forEach((c) => c.abort());
    },
    [],
  );
  useEffect(() => {
    if (!client) return;
    const controller = new AbortController();
    const valid = listGate.current.begin();
    setLoading(true);
    setError("");
    void client
      .list(controller.signal)
      .then((value) => {
        if (valid()) setClusters(value);
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
      listGate.current.cancel();
    };
  }, [client, revision]);
  useEffect(() => {
    setDetail(undefined);
    setJobs(undefined);
    setShowJob(false);
    setShowNodeRegistration(false);
    setRegisteredNode(undefined);
  }, [client, selection]);
  useEffect(() => {
    if (!client || !selection) return;
    const controller = new AbortController();
    const valid = detailGate.current.begin();
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
      detailGate.current.cancel();
    };
  }, [client, selection, revision]);
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
  }, [client, selection, detail?.jobSubmissionEnabled, revision]);
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
      retained = intent ?? clusterIntentFromForm(crypto.randomUUID(), {
        name: String(data.get("name") || ""),
        agentNamespace: String(data.get("namespace") || ""),
        tenantID: String(data.get("tenant") || ""),
        runtime: String(data.get("runtime") || ""),
      });
    } catch (error) {
      setError(error instanceof Error ? error.message : "Invalid cluster intent");
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
        setShowNodeRegistration(false);
      } catch (e) {
        // Only API validation before provisioning has no possible effects.
        if (!signal.aborted && e instanceof APIError && e.status === 400)
          setNodeIntent(undefined);
        throw e;
      }
    });
  };
  return (
    <main>
      <header>
        <div>
          <div className="eyebrow">RLARK / COMPUTE</div>
          <h1>{t.title}</h1>
          <p className="muted">{t.subtitle}</p>
        </div>
        <div className="actions">
          <button onClick={() => setLang((v) => (v === "zh" ? "en" : "zh"))}>
            {lang === "zh" ? "EN" : "中文"}
          </button>
          {token && (
            <>
              <button
                disabled={writing || loading}
                onClick={() => setRevision((v) => v + 1)}
              >
                {t.refresh}
              </button>
              <button onClick={logout}>{t.logout}</button>
            </>
          )}
        </div>
      </header>
      {error && (
        <div role="alert" className="error">
          {error}
        </div>
      )}
      {!token ? (
        <Login
          key={lang}
          t={t}
          onToken={(v) => {
            setWriting(false);
            setToken(v);
            setError("");
          }}
        />
      ) : (
        <>
          <div className="notice">{t.onboarding}</div>
          <section className="layout">
            <aside>
              <div className="actions">
                <h2>{t.title}</h2>
                <button
                  disabled={writing || !!jobIntent || !!nodeIntent}
                  onClick={() => setShowCreate((v) => !v)}
                >
                  ＋
                </button>
              </div>
              {loading ? (
                <p>{t.loading}</p>
              ) : clusters === undefined ? (
                <p>{t.unavailable}</p>
              ) : clusters.length === 0 ? (
                <p>{t.empty}</p>
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
                    <strong>{c.name}</strong>
                    <span>
                      {c.runtime} · {c.readyNodes}/{c.nodeCount} {t.nodes}
                    </span>
                    <span className={`pill ${clusterState(c)}`}>
                      {t[clusterState(c)]}
                    </span>
                  </button>
                ))
              )}
            </aside>
            <div className="content">
              {showCreate && (
                <section className="panel">
                  <div className="actions">
                    <h2>{t.create}</h2>
                    <button onClick={() => setShowCreate(false)}>
                      {t.close}
                    </button>
                  </div>
                  <p className="muted">
                    {intent ? t.remembered : t.createHint}
                  </p>
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
                </section>
              )}
              {!selection ? (
                <div className="empty">{t.select}</div>
              ) : !detail ? (
                <div className="empty">{error ? t.unavailable : t.loading}</div>
              ) : (
                <>
                  <div className="hero">
                    <div>
                      <h2>{detail.cluster.name}</h2>
                      <p className="muted">
                        {detail.cluster.namespace} · {detail.cluster.tenantID}
                      </p>
                      <code className="muted">UID {detail.cluster.uid}</code>
                    </div>
                    <span className={`pill ${clusterState(detail.cluster)}`}>
                      {t[clusterState(detail.cluster)]}
                    </span>
                  </div>
                  <div className="cards">
                    <Metric
                      title={t.nodes}
                      value={`${detail.cluster.readyNodes}/${detail.cluster.nodeCount}`}
                    />
                    <Metric
                      title={t.gpu}
                      value={`${detail.cluster.gpuCount}`}
                    />
                    <Metric
                      title={t.allocations}
                      value={`${detail.allocations.length}`}
                    />
                    <Metric
                      title={t.upgrades}
                      value={`${detail.upgrades.length}`}
                    />
                  </div>
                  <section className="panel">
                    <h3>{t.target}</h3>
                    <div className="target">
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
                    <p className="muted">{t.targetHint}</p>
                    {detail.cluster.lastAgentUpgrade && (
                      <p className="muted">
                        {t.rollout}:{" "}
                        {detail.cluster.lastAgentUpgrade.nodeRecord.name} →{" "}
                        {detail.cluster.lastAgentUpgrade.release.name} ·{" "}
                        {detail.cluster.lastAgentUpgrade.completedAt}
                      </p>
                    )}
                    <Conditions items={detail.cluster.conditions} />
                  </section>
                  <JobsPanel
                    detail={detail}
                    t={t}
                    writing={writing || !!nodeIntent || !!intent}
                    showJob={showJob}
                    jobIntent={jobIntent}
                    jobsLoading={jobsLoading}
                    jobs={jobs}
                    onToggle={() => setShowJob((v) => !v)}
                    onSubmit={submitJob}
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
                  <NodeRegistrationPanel
                    detail={detail}
                    t={t}
                    writing={writing}
                    blocked={!!jobIntent || !!intent}
                    open={showNodeRegistration}
                    intent={nodeIntent}
                    result={registeredNode}
                    onToggle={() => setShowNodeRegistration((v) => !v)}
                    onSubmit={registerNode}
                  />
                  <section className="panel">
                    <h3>{t.nodes}</h3>
                    {!detail.nodes.length ? (
                      <p className="muted">{t.noRecords}</p>
                    ) : (
                      detail.nodes.map((n) => (
                        <NodeCard
                          key={n.uid}
                          node={n}
                          expanded={expandedNodes.has(n.uid)}
                          onExpand={(open) =>
                            setExpandedNodes((current) => {
                              if (current.has(n.uid) === open) return current;
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
                      ))
                    )}
                  </section>
                  <section className="panel">
                    <h3>{t.releases}</h3>
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
                  </section>
                  <section className="panel">
                    <h3>{t.allocations}</h3>
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
                  </section>
                  <section className="panel">
                    <h3>{t.upgrades}</h3>
                    <Table
                      empty={t.noRecords}
                      rows={detail.upgrades.map((u) => (
                        <tr key={u.uid}>
                          <td>{u.name}</td>
                          <td>
                            {u.nodeName} → {u.releaseName}
                          </td>
                          <td>
                            {u.phase || "Pending"}
                            <Conditions items={u.conditions} />
                          </td>
                        </tr>
                      ))}
                    />
                  </section>
                </>
              )}
            </div>
          </section>
        </>
      )}
    </main>
  );
}
function Login({
  t,
  onToken,
}: {
  t: typeof labels.en;
  onToken: (token: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const current = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => current.current?.abort(), []);
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    const controller = new AbortController();
    current.current?.abort();
    current.current = controller;
    setBusy(true);
    setError("");
    try {
      const token = await login(
        String(data.get("username")),
        String(data.get("password")),
        controller.signal,
      );
      if (!controller.signal.aborted) onToken(token);
    } catch {
      if (!controller.signal.aborted) setError(t.failedLogin);
    } finally {
      if (!controller.signal.aborted) setBusy(false);
    }
  };
  return (
    <form className="panel login" onSubmit={(e) => void submit(e)}>
      <h2>{t.signin}</h2>
      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}
      <label>
        {t.username}
        <input name="username" autoComplete="username" required />
      </label>
      <label>
        {t.password}
        <input
          name="password"
          type="password"
          autoComplete="current-password"
          required
        />
      </label>
      <button className="primary" disabled={busy}>
        {busy ? t.loading : t.signin}
      </button>
    </form>
  );
}
function NodeCard({
  node: n,
  t,
  disabled,
  onScheduling,
  expanded,
  onExpand,
}: {
  node: Node;
  t: typeof labels.en;
  disabled: boolean;
  onScheduling: () => void;
  expanded: boolean;
  onExpand: (open: boolean) => void;
}) {
  return (
    <details
      className="node"
      open={expanded}
      onToggle={(event) => onExpand(event.currentTarget.open)}
    >
      <summary>
        <strong>{n.name}</strong>
        <span>
          {n.privateAddress} · {n.agentVersion || "—"}
        </span>
        <span className={`pill ${nodeState(n)}`}>{t[nodeState(n)]}</span>
        <span>
          {t.schedulable}: {n.schedulable ? t.yes : t.no}
        </span>
        <span>
          {t.heartbeat}: {heartbeatAge(n.lastHeartbeatTime)}
        </span>
      </summary>
      <div className="node-details">
        <p>{n.unschedulable ? t.cordoned : t.schedulable}</p>
        <button
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
        <p className="muted">{t.schedulingHint}</p>
        <p>
          {t.busy}: {n.busy ? t.yes : t.no} · {n.environment} · {n.networkScope}
        </p>
        <p>
          <code>
            UID {n.uid} / {n.instanceID}
          </code>
        </p>
        <p>
          {t.capacity}:{" "}
          {Object.entries(n.capacity ?? {})
            .map(([k, v]) => `${k}: ${v}`)
            .join(" · ") || "—"}
        </p>
        <p>
          {t.gpu}: {(n.authorizedGPUUUIDs ?? []).join(", ") || "—"}
        </p>
        <p>
          {t.capabilities}: {(n.capabilities ?? []).join(", ") || "—"}
        </p>
        <Conditions items={n.conditions} />
      </div>
    </details>
  );
}
function Conditions({ items }: { items: Detail["cluster"]["conditions"] }) {
  return (
    <div className="conditions">
      {(items ?? []).map((c) => (
        <p className="muted" key={c.type}>
          <strong>
            {c.type}: {c.status}
          </strong>{" "}
          · {c.reason} {c.message}{" "}
          <small>generation {c.observedGeneration ?? "—"}</small>
        </p>
      ))}
    </div>
  );
}
function Metric({ title, value }: { title: string; value: string }) {
  return (
    <div className="metric">
      <span className="muted">{title}</span>
      <strong>{value}</strong>
    </div>
  );
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
createRoot(document.getElementById("root")!).render(<App />);
