// 设置页（v1.72 左子导航分区——裁定：Agent/自动化/数据/外观/通用；个人中心并入）：
// 纯全局层，不读取任何"当前房间"状态。Agent（实例管理+运行参数——管谁能入座，
// 核心高频居首）/ 自动化（监控 + 任务执行参数）/ 数据（备份/恢复/自诊断）/
// 外观（主题，v1.72 自个人中心迁入）/ 通用（开发者开关 + 关于，v1.72 并入个人中心）。
// 分层规矩：房间讨论策略在房间内调（右侧信息面板）；本页子导航纯前端态（刷新回
// 默认 Agent 区）。
import { useCallback, useEffect, useRef, useState } from "react";
import { api, ApiError, type AgentEvent, type AgentLifecycleAction, type BackupSummary, type Executable, type IdentityAgent, type MonitorView, type SettingsDoc, type SlotSpecContent, type SlotSpecSummary, type SlotSpecView, type SlotInstanceView, type BindingView } from "../api/client";
import { AppLogo } from "../components/AppLogo";
import { RuntimeOptsEditor } from "../components/RuntimeOptsEditor";
import { UpdateEditor } from "../components/UpdateEditor";
import { useDevMode } from "../state/dev";
import { refreshAgentSeats } from "../state/rooms";
import { useTheme } from "../state/theme";
import { adapterLabel, channelLabel } from "../lib/copy";
import { absoluteTime, truncate } from "../lib/ui";

type Section = "agent" | "slots" | "automation" | "data" | "appearance" | "integration" | "general";

const SECTIONS: { id: Section; label: string }[] = [
  { id: "agent", label: "Agent" },
  { id: "slots", label: "职责" },
  { id: "automation", label: "自动化" },
  { id: "data", label: "数据" },
  { id: "appearance", label: "外观" },
  { id: "integration", label: "集成" },
  { id: "general", label: "通用" },
];

export function SettingsPage() {
  const [section, setSection] = useState<Section>("agent");
  const [devMode, setDevMode] = useDevMode();
  const [theme, setTheme] = useTheme();
  const [executables, setExecutables] = useState<Executable[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  // 0.5.1 身份生命周期（ADR-0018）
  const [identAgents, setIdentAgents] = useState<IdentityAgent[] | null>(null);
  const [identMsg, setIdentMsg] = useState<string | null>(null);
  const [identBusy, setIdentBusy] = useState<string | null>(null);
  // 生命周期事件缓存修订号：操作成功后递增，明细组件据此失效缓存（审校 P2#2）
  const [identRev, setIdentRev] = useState(0);
  // 0.5.2 职责模板（ADR-0019）
  const [slotSpecs, setSlotSpecs] = useState<SlotSpecSummary[] | null>(null);
  const [slotMsg, setSlotMsg] = useState<string | null>(null);
  const [slotBusy, setSlotBusy] = useState(false);
  const [slotFormOpen, setSlotFormOpen] = useState(false);
  // mode: create（建谱系）| append（追加版本，携带谱系 id）
  const [slotForm, setSlotForm] = useState<{ mode: "create" | "append"; id: string; name: string; responsibility: string; deliverables: string; reqTools: string; optTools: string }>(
    { mode: "create", id: "", name: "", responsibility: "", deliverables: "", reqTools: "", optTools: "" },
  );
  const slotSeq = useRef(0);
  // 修订基线（审校 P1）：startRevise 抓取的完整最新模板——追加提交时未编辑段
  // （skills/context/delegation/runtime/handoff）原样带入，表单仅覆盖可编辑字段
  const [slotBaseline, setSlotBaseline] = useState<SlotSpecView | null>(null);
  // 修订加载请求序号（审校 P2#6）：新建/取消/切换修订使在途响应失效，不覆盖草稿
  const reviseSeq = useRef(0);
  const [slotVersionsRev, setSlotVersionsRev] = useState(0);
  // 0.5.3 职责实例（ADR-0020）；instError 读取失败态（保留已有列表，审校 P2#4）
  const [slotInstances, setSlotInstances] = useState<SlotInstanceView[] | null>(null);
  const [instError, setInstError] = useState<string | null>(null);
  const [instRetry, setInstRetry] = useState(0);
  const instSeq = useRef(0);
  const [instFormOpen, setInstFormOpen] = useState(false);
  const [instForm, setInstForm] = useState({ specID: "", title: "" });
  const [form, setForm] = useState({ adapter: "", runtime: "native", distro: "", path: "", version: "", channel: "" });
  const [formMsg, setFormMsg] = useState<string | null>(null);
  // M4-0 数据与诊断区
  const [backups, setBackups] = useState<BackupSummary[] | null>(null);
  const [dataMsg, setDataMsg] = useState<string | null>(null);
  const [backupBusy, setBackupBusy] = useState(false);
  const [restoreConfirmID, setRestoreConfirmID] = useState<string | null>(null);
  const [restoreBusy, setRestoreBusy] = useState(false);
  // M4-5 监控面
  const [monitors, setMonitors] = useState<MonitorView[] | null>(null);
  const [monitorMsg, setMonitorMsg] = useState<string | null>(null);
  const [monitorForm, setMonitorForm] = useState<{
    kind: "script" | "url";
    target: string;
    args: string;
    roomID: string;
    assignee: string;
    intervalMin: number;
  }>({ kind: "script", target: "", args: "", roomID: "", assignee: "", intervalMin: 5 });
  const [rooms, setRooms] = useState<{ room_id: string; display_name: string }[]>([]);
  const [agentSeats, setAgentSeats] = useState<{ participant_id: string; display_name: string }[]>([]);
  // RFC-0016 外部云文档面（多 provider）
  const [extUnavailable, setExtUnavailable] = useState(false); // 面未装配
  const [extProviders, setExtProviders] = useState<{ provider: string; display_name: string; configured: boolean }[]>([]);
  // 腾讯文档
  const [txToken, setTxToken] = useState("");
  const [txMsg, setTxMsg] = useState<string | null>(null);
  const [txBusy, setTxBusy] = useState(false);
  // 飞书
  const [fsAppID, setFsAppID] = useState("");
  const [fsAppSecret, setFsAppSecret] = useState("");
  const [fsCode, setFsCode] = useState("");
  const [fsMsg, setFsMsg] = useState<string | null>(null);
  const [fsBusy, setFsBusy] = useState(false);
  const [fsAuthorized, setFsAuthorized] = useState(false);
  // 自动绑定轮询序号：新一次「登录飞书」或组件卸载令旧轮询失效
  const fsPollSeq = useRef(0);

  const refreshExtDocs = useCallback(async () => {
    try {
      const { providers } = await api.listExtDocProviders();
      setExtProviders(providers);
      setExtUnavailable(false);
      const fs = providers.find((p) => p.provider === "feishu_docs");
      setFsAuthorized(fs?.configured ?? false);
    } catch {
      setExtUnavailable(true);
    }
  }, []);

  const saveTxToken = useCallback(
    async (token: string) => {
      setTxBusy(true);
      setTxMsg(null);
      try {
        await api.updateExtDocSettings("tencent_docs", { token });
        setTxToken("");
        setTxMsg(token === "" ? "已清除 token" : "已保存 token（0600 本地落盘，不回传）");
        await refreshExtDocs();
      } catch (e) {
        setTxMsg(e instanceof Error ? e.message : String(e));
      } finally {
        setTxBusy(false);
      }
    },
    [refreshExtDocs],
  );

  const verifyExtDoc = useCallback(
    async (provider: "tencent_docs" | "feishu_docs") => {
      setTxBusy(true);
      setTxMsg(null);
      try {
        const r = await api.verifyExtDoc(provider);
        setTxMsg(r.ok ? "连接成功" : `上游拒绝（${r.upstream_code ?? "?"}）：${r.message ?? ""}`);
        if (r.ok) await refreshExtDocs();
      } catch (e) {
        setTxMsg(e instanceof Error ? e.message : String(e));
      } finally {
        setTxBusy(false);
      }
    },
    [refreshExtDocs],
  );

  const saveFeishuApp = useCallback(async () => {
    setFsBusy(true);
    setFsMsg(null);
    try {
      await api.updateExtDocSettings("feishu_docs", { app_id: fsAppID, app_secret: fsAppSecret });
      setFsAppSecret("");
      setFsMsg("应用凭据已保存——点「登录飞书」完成授权");
      await refreshExtDocs();
    } catch (e) {
      setFsMsg(e instanceof Error ? e.message : String(e));
    } finally {
      setFsBusy(false);
    }
  }, [fsAppID, fsAppSecret, refreshExtDocs]);

  // pollFeishuBind 自动绑定轮询：回调监听收到 code 换 token 后 authorized 变
  // true——每 2s 查一次，10 分钟（回调监听空闲上限）后放弃。
  const pollFeishuBind = useCallback(async () => {
    const seq = ++fsPollSeq.current;
    const startedAt = Date.now();
    const tick = async () => {
      if (fsPollSeq.current !== seq || Date.now() - startedAt > 10 * 60_000) return;
      let authorized = false;
      try {
        const st = await api.getExtDocSettings("feishu_docs");
        authorized = st.authorized ?? false;
      } catch {
        return void setTimeout(() => void tick(), 2000); // 轮询瞬断不放弃
      }
      if (authorized) {
        fsPollSeq.current = 0;
        setFsAuthorized(true);
        setFsMsg("已自动绑定飞书账号（token 自动刷新）——浏览器授权页可以关闭了。");
        await refreshExtDocs();
        return;
      }
      setTimeout(() => void tick(), 2000);
    };
    void tick();
  }, [refreshExtDocs]);

  const loginFeishu = useCallback(async () => {
    try {
      const info = await api.feishuAuthorizeURL();
      window.open(info.url, "_blank", "noreferrer");
      if (info.auto_captured) {
        setFsMsg("已打开授权页——在浏览器完成授权后这里会自动绑定，无需复制 code。");
        void pollFeishuBind();
      } else {
        setFsMsg(
          `回调端口（localhost:3000）被占用，自动绑定不可用。手动路径：确认飞书后台安全设置的重定向 URL 登记为 ${info.redirect_uri}（须完全一致），授权后从地址栏复制 code 粘贴到下方。`,
        );
      }
    } catch (e) {
      setFsMsg(e instanceof Error ? e.message : String(e));
    }
  }, [pollFeishuBind]);

  // 卸载时终止轮询
  useEffect(() => {
    return () => {
      fsPollSeq.current++;
    };
  }, []);

  const submitFeishuCode = useCallback(async () => {
    setFsBusy(true);
    setFsMsg(null);
    try {
      await api.feishuOAuthCallback(fsCode.trim());
      setFsCode("");
      setFsMsg("已绑定飞书账号（token 自动刷新）");
      await refreshExtDocs();
    } catch (e) {
      setFsMsg(e instanceof Error ? e.message : String(e));
    } finally {
      setFsBusy(false);
    }
  }, [fsCode, refreshExtDocs]);

  const refreshMonitors = useCallback(async () => {
    try {
      const { monitors: list } = await api.monitors();
      setMonitors(list);
    } catch {
      setMonitors(null);
    }
  }, []);

  useEffect(() => {
    void refreshMonitors();
    void api.listRooms().then(({ rooms: rs }) => setRooms(rs)).catch(() => {});
    void api.agents().then(({ agents }) => setAgentSeats(agents.filter((a) => a.participant_id !== "par_echo"))).catch(() => {});
  }, [refreshMonitors]);

  const onAddMonitor = async () => {
    setMonitorMsg(null);
    try {
      await api.createMonitor({
        kind: monitorForm.kind,
        target: monitorForm.target.trim(),
        ...(monitorForm.kind === "script" && monitorForm.args.trim() ? { args: monitorForm.args.trim().split(/\s+/) } : {}),
        room_id: monitorForm.roomID,
        assignee: monitorForm.assignee,
        interval_sec: Math.max(10, Math.round(monitorForm.intervalMin * 60)),
      });
      setMonitorForm({ ...monitorForm, target: "", args: "" });
      setMonitorMsg("已登记——首次检查即建立基线并触发分析");
      await refreshMonitors();
    } catch (e) {
      setMonitorMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    }
  };

  // M4-1 任务执行设置（OQ-B 设置族首员：分钟粒度输入，秒粒度存储）+
  // M4-3 回复路径开关（reply_or_pass_mode——A/B 控制组）+
  // 附录 K 发言资格闸（阈值/冷却强度，"必回"病理的确定性兜底）
  const [runTimeoutMin, setRunTimeoutMin] = useState<number | null>(null);
  const [ropMode, setRopMode] = useState<"auto" | "off" | null>(null);
  const [gateOff, setGateOff] = useState<boolean | null>(null);
  const [gateThreshold, setGateThreshold] = useState<number | null>(null);
  const [gateCooldown, setGateCooldown] = useState<number | null>(null);
  const [runTimeoutMsg, setRunTimeoutMsg] = useState<string | null>(null);
  const [runTimeoutBusy, setRunTimeoutBusy] = useState(false);

  // OQ-B 设置族：加载（404 = 装配未启用，如实提示）
  useEffect(() => {
    let alive = true;
    void api
      .settings()
      .then((doc: SettingsDoc) => {
        if (alive) {
          setRunTimeoutMin(Math.round(doc.run_timeout_seconds / 60));
          setRopMode(doc.reply_or_pass_mode === "off" ? "off" : "auto");
          setGateOff(doc.speak_gate_off === true);
          setGateThreshold(doc.speak_gate_threshold ?? 0.3);
          setGateCooldown(doc.speak_gate_cooldown_penalty ?? 0.2);
        }
      })
      .catch((e) => {
        if (alive && e instanceof ApiError && e.status === 404) {
          setRunTimeoutMsg("本装配未启用设置面");
        }
      });
    return () => {
      alive = false;
    };
  }, []);

  const onSaveRunTimeout = async () => {
    if (runTimeoutMin == null) return;
    setRunTimeoutBusy(true);
    setRunTimeoutMsg(null);
    try {
      const doc = await api.updateSettings({
        run_timeout_seconds: runTimeoutMin * 60,
        reply_or_pass_mode: ropMode === "off" ? "off" : "auto",
        speak_gate_off: gateOff === true,
        speak_gate_threshold: gateThreshold ?? 0.3,
        speak_gate_cooldown_penalty: gateCooldown ?? 0.2,
      });
      setRunTimeoutMin(Math.round(doc.run_timeout_seconds / 60));
      setRopMode(doc.reply_or_pass_mode === "off" ? "off" : "auto");
      setGateOff(doc.speak_gate_off === true);
      setGateThreshold(doc.speak_gate_threshold ?? 0.3);
      setGateCooldown(doc.speak_gate_cooldown_penalty ?? 0.2);
      setRunTimeoutMsg("已保存——任务时限对下次执行生效；发言闸门对下一波生效");
    } catch (e) {
      setRunTimeoutMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setRunTimeoutBusy(false);
    }
  };

  const refreshExecutables = useCallback(async () => {
    try {
      const { executables } = await api.executables();
      setExecutables(executables);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    void refreshExecutables();
  }, [refreshExecutables]);

  // 0.5.1 身份生命周期（ADR-0018）：身份列表加载（读取触发注册表→身份表惰性
  // 同步——与可执行项列表同口径）；转移后整表刷新。
  // identSeq 请求序号（审校 P2#5）：并行读取时只放行最新一次响应落地——旧响应
  // 到达即丢弃，杜绝把已显示的新状态覆盖回旧状态。
  const identSeq = useRef(0);
  const refreshIdentAgents = useCallback(async () => {
    const seq = ++identSeq.current;
    try {
      const { agents } = await api.identityAgents();
      if (identSeq.current !== seq) return;
      setIdentAgents(agents);
    } catch {
      if (identSeq.current === seq) setIdentAgents(null);
    }
  }, []);

  useEffect(() => {
    void refreshIdentAgents();
  }, [refreshIdentAgents]);

  const transitionAgent = async (id: string, action: AgentLifecycleAction) => {
    if (action === "retire" && !window.confirm("退役为终态：该 Agent 不再参与后续委派（身份、历史与可执行项均保留）。确认退役？")) {
      return;
    }
    setIdentBusy(`${id}:${action}`);
    setIdentMsg(null);
    try {
      const res = await api.transitionAgent(id, action);
      if (res.outstanding_commitments.length > 0) {
        setIdentMsg(`退役暂缓——未处置承诺：${res.outstanding_commitments.join("；")}`);
      }
      // 操作成功：递增修订号，使已展开的事件明细缓存失效并重读（审校 P2#2）
      setIdentRev((r) => r + 1);
    } catch (e) {
      setIdentMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      // 操作锁保持到刷新完成（审校 P2#5）：不在刷新中途放开按钮，连续操作不交错；
      // 配合请求序号，旧慢响应无法覆盖新状态。
      await refreshIdentAgents();
      setIdentBusy(null);
    }
  };


  // 0.5.2 职责模板（ADR-0019）：列表加载（请求序号拒旧响应——与身份区同纪律）
  // 与建谱系/追加版本。
  const refreshSlotSpecs = useCallback(async () => {
    const seq = ++slotSeq.current;
    try {
      const { specs } = await api.listSlotSpecs();
      if (slotSeq.current !== seq) return;
      setSlotSpecs(specs);
    } catch {
      if (slotSeq.current === seq) setSlotSpecs(null);
    }
  }, []);

  useEffect(() => {
    void refreshSlotSpecs();
  }, [refreshSlotSpecs]);

  const splitList = (s: string): string[] =>
    s.split(/[,，\n]/).map((x) => x.trim()).filter(Boolean);

  // 交付规范仅按行拆（审校 P2#2）：逗号是条目内容的一部分——按逗号拆会把
  // 一项拆成多项（超限失败或改变条目含义）；与表单"每行一项"说明一致
  const splitLines = (s: string): string[] =>
    s.split("\n").map((x) => x.trim()).filter(Boolean);

  const slotContentOf = (): SlotSpecContent => {
    const edited: SlotSpecContent = {
      name: slotForm.name.trim(),
      responsibility: slotForm.responsibility.trim(),
      deliverables: splitLines(slotForm.deliverables),
      ...(slotForm.reqTools.trim() || slotForm.optTools.trim()
        ? { tools: { required: splitList(slotForm.reqTools), optional: splitList(slotForm.optTools) } }
        : {}),
    };
    if (slotForm.mode !== "append" || !slotBaseline) return edited; // 新建：仅表单字段
    // 修订（审校 P1）：完整基线兜底——表单未覆盖的段（skills/context/delegation/
    // runtime/handoff）原样带入，仅改名称时其余配置逐字保留；工具段表单为空 =
    // 未编辑，带基线（清空工具请走 API——最小表单不支持该编辑）
    return {
      ...edited,
      ...(slotBaseline.skills?.length ? { skills: slotBaseline.skills } : {}),
      ...(slotBaseline.context ? { context: slotBaseline.context } : {}),
      ...(slotBaseline.delegation ? { delegation: slotBaseline.delegation } : {}),
      ...(slotBaseline.runtime ? { runtime: slotBaseline.runtime } : {}),
      ...(slotBaseline.handoff ? { handoff: slotBaseline.handoff } : {}),
      tools: edited.tools ?? slotBaseline.tools ?? undefined,
    };
  };

  const submitSlot = async () => {
    setSlotBusy(true);
    setSlotMsg(null);
    try {
      if (slotForm.mode === "create") {
        await api.createSlotSpec({ slot_spec_id: slotForm.id.trim(), ...slotContentOf() });
        setSlotMsg(`已建谱系 ${slotForm.id.trim()}（version=1）`);
      } else {
        const v = await api.appendSlotSpecVersion(slotForm.id, slotContentOf());
        setSlotMsg(`已追加 ${slotForm.id} version=${v.version}`);
      }
      setSlotFormOpen(false);
      reviseSeq.current++; // 提交后使在途修订加载失效（审校 P2#6）
      setSlotBaseline(null);
      setSlotVersionsRev((r) => r + 1); // 版本明细失效重读（审校 P2#4）
      await refreshSlotSpecs();
      await refreshSlotInstances(); // 实例现势连接同步刷新（审校 P2#5：存档字段不变，spec 现势跟进）
    } catch (e) {
      setSlotMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setSlotBusy(false);
    }
  };

  // startRevise 以最新版本预填表单，进入追加模式（修订即追加不可变版本）。
  // 请求序号守卫（审校 P2#6）：等待期间用户若新建/取消/切换模板，本响应作废——
  // 不覆盖草稿、不切回追加模式。
  const startRevise = async (id: string) => {
    setSlotMsg(null);
    const cur = ++reviseSeq.current;
    try {
      const latest = await api.getSlotSpec(id);
      if (reviseSeq.current !== cur) return;
      setSlotBaseline(latest); // 完整基线（审校 P1）
      setSlotForm({
        mode: "append",
        id,
        name: latest.name,
        responsibility: latest.responsibility,
        deliverables: latest.deliverables.join("\n"),
        reqTools: latest.tools?.required?.join(", ") ?? "",
        optTools: latest.tools?.optional?.join(", ") ?? "",
      });
      setSlotFormOpen(true);
    } catch (e) {
      if (reviseSeq.current === cur) setSlotMsg(e instanceof Error ? e.message : String(e));
    }
  };


  // 0.5.3 职责实例（ADR-0020）：列表加载（请求序号拒旧响应）与实例化。
  const refreshSlotInstances = useCallback(async () => {
    const seq = ++instSeq.current;
    try {
      const { instances } = await api.listSlotInstances();
      if (instSeq.current !== seq) return;
      setSlotInstances(instances);
      setInstError(null);
    } catch (e) {
      if (instSeq.current !== seq) return;
      // 失败不清空已有列表（审校 P2#4）——单列错误态，服务恢复后可重试
      setInstError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    void refreshSlotInstances();
  }, [refreshSlotInstances, instRetry]);

  const submitInstance = async () => {
    setSlotBusy(true);
    setSlotMsg(null);
    try {
      const inst = await api.createSlotInstance({
        slot_spec_id: instForm.specID,
        ...(instForm.title.trim() ? { title: instForm.title.trim() } : {}),
      });
      setSlotMsg(`已实例化 ${inst.title}（${inst.slot_instance_id}）`);
      setInstFormOpen(false);
      setInstForm({ specID: "", title: "" });
      await refreshSlotInstances();
    } catch (e) {
      setSlotMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setSlotBusy(false);
    }
  };


  // RFC-0016 外部云文档面：provider 状态加载（404 = 未装配）
  useEffect(() => {
    void refreshExtDocs();
  }, [refreshExtDocs]);

  // M4-0 备份面：列表加载（404 = 装配未启用，如实提示）
  const refreshBackups = useCallback(async () => {
    try {
      const { backups } = await api.listBackups();
      setBackups(backups);
    } catch (e) {
      setBackups(null);
      if (e instanceof ApiError && e.status === 404) {
        setDataMsg("本装配未启用备份面");
      } else {
        setDataMsg(e instanceof Error ? e.message : String(e));
      }
    }
  }, []);

  useEffect(() => {
    void refreshBackups();
  }, [refreshBackups]);

  const onCreateBackup = async () => {
    setBackupBusy(true);
    setDataMsg(null);
    try {
      const sum = await api.createBackup();
      setDataMsg(`备份完成：${sum.backup_id}（${(sum.size_bytes / 1024).toFixed(0)} KiB）`);
      await refreshBackups();
    } catch (e) {
      setDataMsg(`备份失败：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setBackupBusy(false);
    }
  };

  // 恢复两段式的在线段：落标记后需重启应用（实际换库在下次启动、开库之前）
  const onRequestRestore = async (backupID: string) => {
    setRestoreBusy(true);
    setDataMsg(null);
    try {
      await api.requestRestore(backupID);
      setDataMsg("恢复已排程：重启应用后生效（被换下的数据保留在安全副本目录）");
      setRestoreConfirmID(null);
    } catch (e) {
      setDataMsg(`恢复请求失败：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setRestoreBusy(false);
    }
  };

  // 自诊断：拉取 bundle 存本地文件（浏览器下载面）
  const onDownloadDiagnostics = async () => {
    try {
      const bundle = await api.diagnostics();
      const blob = new Blob([JSON.stringify(bundle, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `mosaic-diagnostics-${new Date().toISOString().replace(/[:.]/g, "-")}.json`;
      a.click();
      URL.revokeObjectURL(url);
    } catch (e) {
      setDataMsg(`诊断报告获取失败：${e instanceof Error ? e.message : String(e)}`);
    }
  };

  const toggle = async (exe: Executable) => {
    setBusy(exe.id);
    setError(null);
    try {
      await api.setEnabled(exe.id, !exe.enabled);
      await refreshExecutables();
      await refreshAgentSeats(); // 侧栏私聊组随启停即时增减（v1.74 联系人收敛）
      // 启停改变身份视图的可执行引用（enabled）：同步刷新生命周期列表
      await refreshIdentAgents();
    } catch (e) {
      setError(
        e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e),
      );
    } finally {
      setBusy(null);
    }
  };

  const register = async () => {
    const adapter = form.adapter.trim();
    const path = form.path.trim();
    if (!adapter || !path) {
      setFormMsg("适配器与路径必填");
      return;
    }
    setBusy("register");
    setFormMsg(null);
    try {
      await api.registerExecutable({
        adapter,
        runtime: form.runtime,
        path,
        ...(form.runtime === "wsl" && form.distro.trim() ? { distro: form.distro.trim() } : {}),
        ...(form.version.trim() ? { version: form.version.trim() } : {}),
        ...(form.channel.trim() ? { channel: form.channel.trim() } : {}),
      });
      setForm({ adapter: "", runtime: "native", distro: "", path: "", version: "", channel: "" });
      setFormMsg("已登记");
      await refreshExecutables();
      await refreshAgentSeats();
      // 登记即建身份（惰性同步在身份列表读取内发生）：刷新生命周期列表，
      // 新身份立即可暂停/退役（审校 P2#3）
      await refreshIdentAgents();
    } catch (e) {
      setFormMsg(
        e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e),
      );
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="flex h-full">
      {/* 左子导航（v1.72 分区）：窄列常驻，核心高频的 Agent 居首 */}
      <nav className="flex w-40 shrink-0 flex-col gap-0.5 border-r border-border px-2 py-4" aria-label="设置分区">
        <h1 className="px-2 pb-2 text-sm font-semibold tracking-tight">设置</h1>
        {SECTIONS.map((s) => (
          <button
            key={s.id}
            type="button"
            onClick={() => setSection(s.id)}
            aria-current={section === s.id ? "true" : undefined}
            className={`rounded-lg px-2.5 py-1.5 text-left text-sm transition-colors ${
              section === s.id ? "bg-surface-3 text-text" : "text-dim hover:bg-surface-2 hover:text-text"
            }`}
          >
            {s.label}
          </button>
        ))}
      </nav>

      <div className="min-w-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex max-w-2xl flex-col gap-8 px-6 py-8">
          {section === "agent" && (
            <>
            <section>
              <h2 className="mb-1 text-sm font-medium">Agent 实例</h2>
              <p className="mb-3 text-xs text-faint">
                自动扫描本机已安装的 CLI（Codex / Kimi 等）；未登录的项不可启用。启用后 ≤10 秒自动入座，无需重启。
                同一 Agent 的多个实例按家族优先级排序（Codex 桌面 App 优先、Kimi Code 优先，ADR-0012）——想用哪个就启用哪个。
              </p>
              {error && <p className="mb-2 text-xs text-danger">{error}</p>}
              {executables === null ? (
                <p className="text-xs text-faint">加载中…</p>
              ) : executables.length === 0 ? (
                <p className="text-xs text-faint">未发现任何可执行项。</p>
              ) : (
                <ul className="divide-y divide-border rounded-xl border border-border">
                  {executables.map((exe) => {
                    const loginBlocked = exe.login_state !== "logged_in" && !exe.enabled;
                    return (
                      <li key={exe.id} className="flex flex-col gap-1.5 px-3 py-2.5">
                        <div className="flex items-center gap-3">
                          <div className="min-w-0 flex-1">
                            <div className="flex flex-wrap items-center gap-1.5 text-sm">
                              <span className="font-medium">{adapterLabel(exe.adapter)}</span>
                              <ExeBadge>{channelLabel(exe.channel)}</ExeBadge>
                              <ExeBadge>{exe.runtime}{exe.distro ? `（${exe.distro}）` : ""}</ExeBadge>
                              {exe.version && <span className="text-xs text-faint">{exe.version}</span>}
                              <ExeBadge tone={exe.login_state === "logged_in" ? "ok" : "warn"}>
                                {exe.login_state ?? "unknown"}
                              </ExeBadge>
                              {exe.agent_id && (
                                <span
                                  className="font-mono text-[10px] text-faint"
                                  title={`Agent 身份（跨房间/部署位置稳定，ADR-0017）：${exe.agent_id}`}
                                >
                                  {truncate(exe.agent_id, 14)}
                                </span>
                              )}
                              {exe.source === "manual" && <ExeBadge>手动登记</ExeBadge>}
                            </div>
                            <p className="mt-0.5 truncate font-mono text-[11px] text-faint" title={exe.path}>
                              {truncate(exe.path, 64)}
                            </p>
                          </div>
                          <button
                            type="button"
                            disabled={busy === exe.id || loginBlocked}
                            title={loginBlocked ? "未登录——请先在对应 CLI 登录" : undefined}
                            onClick={() => void toggle(exe)}
                            className={`shrink-0 rounded-lg px-2.5 py-1 text-xs transition-opacity disabled:opacity-40 ${
                              exe.enabled
                                ? "bg-accent-soft text-accent hover:opacity-85"
                                : "bg-surface-3 text-text hover:opacity-85"
                            }`}
                          >
                            {exe.enabled ? "已启用" : loginBlocked ? "未登录" : "启用"}
                          </button>
                        </div>
                        {/* v1.48 运行参数：模型覆盖与思考强度（kimi 实查候选；codex 五档强度） */}
                        <RuntimeOptsEditor exe={exe} />
                        {/* 0.4.1 版本检测与更新：npm 分发面实查最新版；claude 官方自更新；
                            kimi/zcode manual 指引（更新后版本回填注册表） */}
                        <UpdateEditor exe={exe} onUpdated={() => void refreshExecutables()} />
                      </li>
                    );
                  })}
                </ul>
              )}
              <div className="mt-3">
                <button
                  type="button"
                  onClick={() => void refreshExecutables().then(refreshAgentSeats)}
                  className="rounded-lg bg-surface-3 px-2.5 py-1 text-xs text-text transition-opacity hover:opacity-85"
                >
                  重新扫描
                </button>
              </div>
              <details className="mt-3 rounded-xl border border-border px-3 py-2">
                <summary className="cursor-pointer text-xs text-dim">手动登记可执行项</summary>
                <div className="mt-3 grid grid-cols-2 gap-2 text-xs">
                  <label className="flex flex-col gap-1">
                    <span className="text-faint">适配器（如 codex / kimi）</span>
                    <input
                      value={form.adapter}
                      onChange={(e) => setForm({ ...form, adapter: e.target.value })}
                      className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                    />
                  </label>
                  <label className="flex flex-col gap-1">
                    <span className="text-faint">运行面</span>
                    <select
                      value={form.runtime}
                      onChange={(e) => setForm({ ...form, runtime: e.target.value })}
                      className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                    >
                      <option value="native">native</option>
                      <option value="wsl">wsl</option>
                    </select>
                  </label>
                  {form.runtime === "wsl" && (
                    <label className="flex flex-col gap-1">
                      <span className="text-faint">WSL 发行版</span>
                      <input
                        value={form.distro}
                        onChange={(e) => setForm({ ...form, distro: e.target.value })}
                        className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                      />
                    </label>
                  )}
                  <label className="col-span-2 flex flex-col gap-1">
                    <span className="text-faint">可执行文件绝对路径</span>
                    <input
                      value={form.path}
                      onChange={(e) => setForm({ ...form, path: e.target.value })}
                      placeholder="/usr/local/bin/codex"
                      className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono outline-none focus:border-accent"
                    />
                  </label>
                  <label className="flex flex-col gap-1">
                    <span className="text-faint">版本（可选，留空自动探测）</span>
                    <input
                      value={form.version}
                      onChange={(e) => setForm({ ...form, version: e.target.value })}
                      className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                    />
                  </label>
                  <label className="flex flex-col gap-1">
                    <span className="text-faint">渠道（可选，如 app:kimi-work；留空按 CLI）</span>
                    <input
                      value={form.channel}
                      onChange={(e) => setForm({ ...form, channel: e.target.value })}
                      className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                    />
                  </label>
                </div>
                <div className="mt-3 flex items-center gap-2">
                  <button
                    type="button"
                    disabled={busy === "register"}
                    onClick={() => void register()}
                    className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
                  >
                    {busy === "register" ? "登记中…" : "登记"}
                  </button>
                  {formMsg && <span className="text-xs text-dim">{formMsg}</span>}
                </div>
                <p className="mt-2 text-[11px] text-faint">登记时服务端会探测该路径（探测失败拒收）。</p>
              </details>
            </section>

            <section className="mt-8">
              <h2 className="mb-1 text-sm font-medium">Agent 身份与生命周期</h2>
              <p className="mb-3 text-xs text-faint">
                长期 Agent 的稳定身份（跨房间、跨部署位置，ADR-0017）。暂停 = 暂离后续委派；
                退役 = 终态（身份与历史保留，可执行项照常）。每次转移记入事件流（ADR-0018）。
              </p>
              {identMsg && <p className="mb-2 text-xs text-danger">{identMsg}</p>}
              {identAgents === null ? (
                <p className="text-xs text-faint">加载中…</p>
              ) : identAgents.length === 0 ? (
                <p className="text-xs text-faint">尚无 Agent 身份——发现或手动登记可执行项后自动建立。</p>
              ) : (
                <ul className="divide-y divide-border rounded-xl border border-border">
                  {identAgents.map((ag) => (
                    <li key={ag.agent_id} className="flex flex-col gap-1 px-3 py-2.5">
                      <div className="flex items-center gap-3">
                        <div className="min-w-0 flex-1">
                          <div className="flex flex-wrap items-center gap-1.5 text-sm">
                            <span className={`font-medium ${ag.status === "retired" ? "text-dim" : ""}`}>{ag.display_name}</span>
                            {ag.adapter && <ExeBadge>{adapterLabel(ag.adapter)}</ExeBadge>}
                            <ExeBadge tone={ag.status === "active" ? "ok" : ag.status === "retired" ? undefined : "warn"}>
                              {agentStatusLabel(ag.status)}
                            </ExeBadge>
                            {ag.source === "manual" && <ExeBadge>手动建</ExeBadge>}
                            <span
                              className="font-mono text-[10px] text-faint"
                              title={`稳定身份（ADR-0017）：${ag.agent_id}`}
                            >
                              {truncate(ag.agent_id, 16)}
                            </span>
                          </div>
                          <p className="mt-0.5 text-[11px] text-faint">
                            {ag.executables.length} 个可执行项挂靠 · 建立于 {absoluteTime(ag.created_at)}
                          </p>
                        </div>
                        <div className="flex shrink-0 items-center gap-1.5">
                          {ag.status === "active" && (
                            <>
                              <LifecycleBtn
                                disabled={identBusy !== null}
                                busy={identBusy === `${ag.agent_id}:suspend`}
                                onClick={() => void transitionAgent(ag.agent_id, "suspend")}
                              >
                                暂停
                              </LifecycleBtn>
                              <LifecycleBtn
                                danger
                                disabled={identBusy !== null}
                                busy={identBusy === `${ag.agent_id}:retire`}
                                onClick={() => void transitionAgent(ag.agent_id, "retire")}
                              >
                                退役
                              </LifecycleBtn>
                            </>
                          )}
                          {ag.status === "suspended" && (
                            <>
                              <LifecycleBtn
                                disabled={identBusy !== null}
                                busy={identBusy === `${ag.agent_id}:resume`}
                                onClick={() => void transitionAgent(ag.agent_id, "resume")}
                              >
                                恢复
                              </LifecycleBtn>
                              <LifecycleBtn
                                danger
                                disabled={identBusy !== null}
                                busy={identBusy === `${ag.agent_id}:retire`}
                                onClick={() => void transitionAgent(ag.agent_id, "retire")}
                              >
                                退役
                              </LifecycleBtn>
                            </>
                          )}
                          {ag.status === "retiring" && (
                            <LifecycleBtn
                              danger
                              disabled={identBusy !== null}
                              busy={identBusy === `${ag.agent_id}:retire`}
                              onClick={() => void transitionAgent(ag.agent_id, "retire")}
                            >
                              重试退役
                            </LifecycleBtn>
                          )}
                        </div>
                      </div>
                      <AgentEventsDetails agentID={ag.agent_id} rev={identRev} />
                    </li>
                  ))}
                </ul>
              )}
            </section>
            </>
          )}


          {section === "slots" && (
            <section>
              <h2 className="mb-1 text-sm font-medium">职责模板</h2>
              <p className="mb-3 text-xs text-faint">
                职责的版本化模板（RFC-0018 §5 / ADR-0019）：承担什么职责、需要什么能力、如何交付。
                模板版本不可变——修订即追加新版本，历史永不清除；工具/上下文分必选与可选
                （必选缺失将 blocked、可选缺失按声明降级，准入随 0.5.5 交付）。0.5.2 仅模板本身，
                实例与任职随 0.5.3/0.5.4。
              </p>
              {slotMsg && <p className="mb-2 text-xs text-danger">{slotMsg}</p>}
              <div className="mb-3">
                <button
                  type="button"
                  onClick={() => {
                    reviseSeq.current++; // 在途修订加载作废（审校 P2#6）
                    setSlotBaseline(null);
                    setSlotForm({ mode: "create", id: "", name: "", responsibility: "", deliverables: "", reqTools: "", optTools: "" });
                    setSlotFormOpen((o) => !o);
                    setSlotMsg(null);
                  }}
                  className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90"
                >
                  {slotFormOpen && slotForm.mode === "create" ? "收起" : "新建模板"}
                </button>
              </div>
              {slotFormOpen && (
                <div className="mb-4 rounded-xl border border-border p-3">
                  <p className="mb-2 text-xs text-dim">
                    {slotForm.mode === "create" ? "建谱系（首版 version=1）" : `修订 ${slotForm.id}（追加下一不可变版本）`}
                  </p>
                  <div className="flex flex-col gap-2">
                    {slotForm.mode === "create" && (
                      <label className="flex flex-col gap-1">
                        <span className="text-faint">谱系标识（slot. 前缀小写，跨版本稳定，如 slot.research-lead）</span>
                        <input
                          value={slotForm.id}
                          onChange={(e) => setSlotForm({ ...slotForm, id: e.target.value })}
                          placeholder="slot.research-lead"
                          className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
                        />
                      </label>
                    )}
                    <label className="flex flex-col gap-1">
                      <span className="text-faint">名称</span>
                      <input
                        value={slotForm.name}
                        onChange={(e) => setSlotForm({ ...slotForm, name: e.target.value })}
                        className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 text-sm outline-none focus:border-accent"
                      />
                    </label>
                    <label className="flex flex-col gap-1">
                      <span className="text-faint">职责陈述</span>
                      <textarea
                        value={slotForm.responsibility}
                        onChange={(e) => setSlotForm({ ...slotForm, responsibility: e.target.value })}
                        rows={2}
                        className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 text-sm outline-none focus:border-accent"
                      />
                    </label>
                    <label className="flex flex-col gap-1">
                      <span className="text-faint">交付规范（每行一项）</span>
                      <textarea
                        value={slotForm.deliverables}
                        onChange={(e) => setSlotForm({ ...slotForm, deliverables: e.target.value })}
                        rows={2}
                        placeholder={"结论、来源与具名异议\n可验收产物清单"}
                        className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 text-sm outline-none focus:border-accent"
                      />
                    </label>
                    <div className="grid grid-cols-2 gap-2">
                      <label className="flex flex-col gap-1">
                        <span className="text-faint">必选工具（逗号分隔，如 documents.read）</span>
                        <input
                          value={slotForm.reqTools}
                          onChange={(e) => setSlotForm({ ...slotForm, reqTools: e.target.value })}
                          className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
                        />
                      </label>
                      <label className="flex flex-col gap-1">
                        <span className="text-faint">可选工具（缺失按声明降级）</span>
                        <input
                          value={slotForm.optTools}
                          onChange={(e) => setSlotForm({ ...slotForm, optTools: e.target.value })}
                          className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
                        />
                      </label>
                    </div>
                    <div className="mt-1 flex items-center gap-2">
                      <button
                        type="button"
                        disabled={slotBusy}
                        onClick={() => void submitSlot()}
                        className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
                      >
                        {slotBusy ? "提交中…" : slotForm.mode === "create" ? "创建" : "追加版本"}
                      </button>
                      {slotForm.mode === "append" && (
                        <button
                          type="button"
                          onClick={() => {
                            reviseSeq.current++;
                            setSlotBaseline(null);
                            setSlotFormOpen(false);
                            setSlotForm({ mode: "create", id: "", name: "", responsibility: "", deliverables: "", reqTools: "", optTools: "" });
                          }}
                          className="rounded-lg bg-surface-3 px-3 py-1.5 text-xs text-text"
                        >
                          取消修订
                        </button>
                      )}
                    </div>
                  </div>
                </div>
              )}
              {slotSpecs === null ? (
                <p className="text-xs text-faint">加载中…</p>
              ) : slotSpecs.length === 0 ? (
                <p className="text-xs text-faint">尚无职责模板——新建后供 0.5.3+ 的职责实例与任职引用。</p>
              ) : (
                <ul className="divide-y divide-border rounded-xl border border-border">
                  {slotSpecs.map((spec) => (
                    <li key={spec.slot_spec_id} className="flex flex-col gap-1 px-3 py-2.5">
                      <div className="flex items-center gap-3">
                        <div className="min-w-0 flex-1">
                          <div className="flex flex-wrap items-center gap-1.5 text-sm">
                            <span className="font-medium">{spec.name}</span>
                            <ExeBadge>v{spec.latest_version}</ExeBadge>
                            {spec.version_count > 1 && <ExeBadge>{spec.version_count} 版</ExeBadge>}
                          </div>
                          <p className="mt-0.5 truncate font-mono text-[11px] text-faint" title={spec.slot_spec_id}>
                            {spec.slot_spec_id} · 修订于 {absoluteTime(spec.updated_at)}
                          </p>
                        </div>
                        <button
                          type="button"
                          onClick={() => void startRevise(spec.slot_spec_id)}
                          className="shrink-0 rounded-lg bg-surface-3 px-2 py-1 text-xs text-text transition-opacity hover:opacity-85"
                        >
                          修订
                        </button>
                      </div>
                      <SlotVersionsDetails slotSpecID={spec.slot_spec_id} rev={slotVersionsRev} />
                    </li>
                  ))}
                </ul>
              )}

              <h3 className="mb-1 mt-6 text-sm font-medium">职责实例</h3>
              <p className="mb-3 text-xs text-faint">
                某作用域内实际承担的一份具体职责（ADR-0020）：同一模板可实例化多份，换人后实例保留
                （任职随 0.5.4）。每个实例有独立的知识命名空间（slotns_）——职责知识挂实例，不挂任职者。
              </p>
              <div className="mb-3">
                <button
                  type="button"
                  onClick={() => {
                    setInstForm({ specID: slotSpecs?.[0]?.slot_spec_id ?? "", title: "" });
                    setInstFormOpen((o) => !o);
                    setSlotMsg(null);
                  }}
                  className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90"
                >
                  {instFormOpen ? "收起" : "实例化职责"}
                </button>
              </div>
              {instFormOpen && (
                <div className="mb-4 flex flex-wrap items-end gap-2 rounded-xl border border-border p-3">
                  <label className="flex flex-col gap-1">
                    <span className="text-faint">模板</span>
                    <select
                      value={instForm.specID}
                      onChange={(e) => setInstForm({ ...instForm, specID: e.target.value })}
                      className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 text-sm outline-none focus:border-accent"
                    >
                      {(slotSpecs ?? []).map((s) => (
                        <option key={s.slot_spec_id} value={s.slot_spec_id}>
                          {s.name}（{s.slot_spec_id}）
                        </option>
                      ))}
                    </select>
                  </label>
                  <label className="flex flex-1 flex-col gap-1">
                    <span className="text-faint">标题（可选，缺省取模板名）</span>
                    <input
                      value={instForm.title}
                      onChange={(e) => setInstForm({ ...instForm, title: e.target.value })}
                      className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 text-sm outline-none focus:border-accent"
                    />
                  </label>
                  <button
                    type="button"
                    disabled={slotBusy || !instForm.specID}
                    onClick={() => void submitInstance()}
                    className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
                  >
                    {slotBusy ? "提交中…" : "创建实例"}
                  </button>
                </div>
              )}
              {instError !== null && (
                <div className="mb-2 flex items-center gap-2">
                  <p className="text-xs text-danger">实例列表读取失败：{instError}{slotInstances !== null ? "（下方为最近一次成功列表）" : ""}</p>
                  <button
                    type="button"
                    onClick={() => setInstRetry((a) => a + 1)}
                    className="rounded-lg bg-surface-3 px-2 py-1 text-xs text-dim"
                  >
                    重试
                  </button>
                </div>
              )}
              {slotInstances === null && instError === null ? (
                <p className="text-xs text-faint">加载中…</p>
              ) : slotInstances === null ? (
                <p className="text-xs text-faint">列表暂不可用——重试成功后显示。</p>
              ) : slotInstances.length === 0 ? (
                <p className="text-xs text-faint">尚无职责实例——实例化后供 0.5.4 任职引用。</p>
              ) : (
                <ul className="divide-y divide-border rounded-xl border border-border">
                  {slotInstances.map((inst) => (
                    <li key={inst.slot_instance_id} className="flex flex-col gap-1 px-3 py-2.5">
                      <div className="flex flex-wrap items-center gap-1.5 text-sm">
                        <span className="font-medium">{inst.title}</span>
                        <ExeBadge>{inst.spec.name} v{inst.spec.latest_version}</ExeBadge>
                        <ExeBadge>自 v{inst.created_from_version} 实例化</ExeBadge>
                      </div>
                      <p
                        className="truncate font-mono text-[11px] text-faint"
                        title={`知识命名空间（职责知识挂此，不挂任职者）：${inst.knowledge_namespace}\n实例 ID：${inst.slot_instance_id}`}
                      >
                        {inst.knowledge_namespace} · {absoluteTime(inst.created_at)}
                      </p>
                      <InstanceBindings instanceID={inst.slot_instance_id} specLatest={inst.spec.latest_version} />
                    </li>
                  ))}
                </ul>
              )}
            </section>
          )}

          {section === "automation" && (
            <>
              <section>
                <h2 className="mb-1 text-sm font-medium">监控</h2>
                <p className="mb-2 text-xs text-faint">
                  登记脚本或 URL 作为监控源：周期取快照、哈希比对——无变化不调用模型；有变化把差异交给
                  指派 Agent 分析（独立任务通道），结果投回所选房间。水位（观察/处理/送达）与失败分类
                  分开记录，处理失败自动重试、源失败恢复后续跑。
                </p>
                {monitors === null ? (
                  <p className="text-xs text-faint">监控面未装配（旧装配或测试形态）。</p>
                ) : (
                  <>
                    <ul className="divide-y divide-border rounded-xl border border-border">
                      {monitors.map((m) => (
                        <li key={m.monitor_id} className="px-3 py-2.5 text-xs">
                          <div className="flex flex-wrap items-center gap-2">
                            <ExeBadge>{m.kind}</ExeBadge>
                            <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-dim" title={m.target}>
                              {m.target}
                            </span>
                            <ExeBadge tone={m.enabled ? "ok" : "dim"}>{m.enabled ? "已启用" : "已停用"}</ExeBadge>
                            <button
                              type="button"
                              onClick={() => void api.monitorAction(m.monitor_id, m.enabled ? "disable" : "enable").then(refreshMonitors).catch(() => {})}
                              className="rounded-lg px-2 py-0.5 text-dim hover:text-text"
                            >
                              {m.enabled ? "停用" : "启用"}
                            </button>
                            <button
                              type="button"
                              onClick={() => void api.monitorAction(m.monitor_id, "check").then(refreshMonitors).catch(() => {})}
                              className="rounded-lg bg-surface-3 px-2 py-0.5 text-text hover:opacity-85"
                            >
                              立即检查
                            </button>
                            <button
                              type="button"
                              onClick={() => void api.deleteMonitor(m.monitor_id).then(refreshMonitors).catch(() => {})}
                              className="rounded-lg px-2 py-0.5 text-dim hover:text-danger"
                            >
                              删除
                            </button>
                          </div>
                          <p className="mt-1 text-[11px] text-faint">
                            {m.interval_sec >= 60 ? `${Math.round(m.interval_sec / 60)} 分钟` : `${m.interval_sec} 秒`} · 投递到 {m.room_id.slice(0, 14)}… ·
                            无变化 {m.state?.no_change_count ?? 0} 次
                            {m.state?.in_flight_run ? " · 分析中" : ""}
                            {m.state?.pending_hash ? " · 有待处理变化" : ""}
                            {m.state?.last_error ? ` · 源失败：${m.state.last_error.slice(0, 60)}` : ""}
                            {m.state?.process_error ? ` · ${m.state.process_error.slice(0, 60)}` : ""}
                            {m.state?.last_delivered_at ? ` · 上次送达 ${relativeTimeOf(m.state.last_delivered_at)}` : " · 尚未送达"}
                          </p>
                        </li>
                      ))}
                    </ul>
                    <details className="mt-3 rounded-xl border border-border px-3 py-2">
                      <summary className="cursor-pointer text-xs text-dim">登记监控源</summary>
                      <div className="mt-3 grid grid-cols-2 gap-2 text-xs">
                        <label className="flex flex-col gap-1">
                          <span className="text-faint">类型</span>
                          <select
                            value={monitorForm.kind}
                            onChange={(e) => setMonitorForm({ ...monitorForm, kind: e.target.value === "url" ? "url" : "script" })}
                            className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                          >
                            <option value="script">脚本（本机）</option>
                            <option value="url">URL</option>
                          </select>
                        </label>
                        <label className="flex flex-col gap-1">
                          <span className="text-faint">检查间隔（分钟，≥1）</span>
                          <input
                            type="number"
                            min={1}
                            value={monitorForm.intervalMin}
                            onChange={(e) => setMonitorForm({ ...monitorForm, intervalMin: Number(e.target.value) || 1 })}
                            className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                          />
                        </label>
                        <label className="col-span-2 flex flex-col gap-1">
                          <span className="text-faint">{monitorForm.kind === "script" ? "脚本绝对路径（无 shell）" : "URL（http/https）"}</span>
                          <input
                            value={monitorForm.target}
                            onChange={(e) => setMonitorForm({ ...monitorForm, target: e.target.value })}
                            placeholder={monitorForm.kind === "script" ? "/usr/local/bin/check-feed.sh" : "https://example.com/status.json"}
                            className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono outline-none focus:border-accent"
                          />
                        </label>
                        {monitorForm.kind === "script" && (
                          <label className="col-span-2 flex flex-col gap-1">
                            <span className="text-faint">参数（空格分隔，可选）</span>
                            <input
                              value={monitorForm.args}
                              onChange={(e) => setMonitorForm({ ...monitorForm, args: e.target.value })}
                              className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono outline-none focus:border-accent"
                            />
                          </label>
                        )}
                        <label className="flex flex-col gap-1">
                          <span className="text-faint">投递房间</span>
                          <select
                            value={monitorForm.roomID}
                            onChange={(e) => setMonitorForm({ ...monitorForm, roomID: e.target.value })}
                            className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                          >
                            <option value="">选择房间…</option>
                            {rooms.map((r) => (
                              <option key={r.room_id} value={r.room_id}>
                                {r.display_name}
                              </option>
                            ))}
                          </select>
                        </label>
                        <label className="flex flex-col gap-1">
                          <span className="text-faint">指派 Agent</span>
                          <select
                            value={monitorForm.assignee}
                            onChange={(e) => setMonitorForm({ ...monitorForm, assignee: e.target.value })}
                            className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                          >
                            <option value="">选择 Agent…</option>
                            {agentSeats.map((a) => (
                              <option key={a.participant_id} value={a.participant_id}>
                                {a.display_name}
                              </option>
                            ))}
                          </select>
                        </label>
                      </div>
                      <div className="mt-3 flex items-center gap-2">
                        <button
                          type="button"
                          disabled={!monitorForm.target.trim() || !monitorForm.roomID || !monitorForm.assignee}
                          onClick={() => void onAddMonitor()}
                          className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
                        >
                          登记
                        </button>
                        {monitorMsg && <span className="text-xs text-dim">{monitorMsg}</span>}
                      </div>
                    </details>
                  </>
                )}
              </section>

              <section>
                <h2 className="mb-1 text-sm font-medium">任务执行与发言</h2>
                <div className="flex flex-wrap items-center gap-2 text-xs">
                  <label className="flex items-center gap-2">
                    <span className="text-faint">独立任务执行时长上限</span>
                    <input
                      type="number"
                      min={1}
                      max={120}
                      value={runTimeoutMin ?? ""}
                      disabled={runTimeoutMin == null}
                      onChange={(e) => setRunTimeoutMin(e.target.value ? Number(e.target.value) : null)}
                      className="w-20 rounded-lg border border-border bg-surface-2 px-2 py-1.5 outline-none focus:border-accent"
                    />
                    <span className="text-faint">分钟</span>
                  </label>
                </div>
                <div className="mt-2 flex flex-wrap items-center gap-2">
                  <span className="text-faint">点名回复路径（reply-or-pass）</span>
                  {(["auto", "off"] as const).map((m) => (
                    <button
                      key={m}
                      type="button"
                      aria-pressed={ropMode === m}
                      disabled={ropMode === null}
                      onClick={() => {
                        setRopMode(m);
                      }}
                      className={`rounded-lg border px-2.5 py-1 text-xs transition-colors ${
                        ropMode === m
                          ? "border-accent bg-accent-soft text-accent"
                          : "border-border bg-surface-2 text-dim hover:text-text"
                      }`}
                    >
                      {m === "auto" ? "启用（点名单人/任务交付走单次路径）" : "关闭（A/B 对照：回退两阶段）"}
                    </button>
                  ))}
                </div>
                <div className="mt-2 flex flex-wrap items-center gap-2">
                  <span className="text-faint">发言资格闸</span>
                  {([false, true] as const).map((off) => (
                    <button
                      key={String(off)}
                      type="button"
                      aria-pressed={gateOff === off}
                      disabled={gateOff === null}
                      onClick={() => {
                        setGateOff(off);
                      }}
                      className={`rounded-lg border px-2.5 py-1 text-xs transition-colors ${
                        gateOff === off
                          ? "border-accent bg-accent-soft text-accent"
                          : "border-border bg-surface-2 text-dim hover:text-text"
                      }`}
                    >
                      {off ? "关闭（有意愿即发言）" : "启用（低相关被闸，@点名豁免）"}
                    </button>
                  ))}
                  <label className="flex items-center gap-1.5">
                    <span className="text-faint">阈值</span>
                    <input
                      type="number"
                      step={0.05}
                      min={0.05}
                      max={0.95}
                      value={gateThreshold ?? ""}
                      disabled={gateThreshold == null}
                      onChange={(e) => setGateThreshold(e.target.value ? Number(e.target.value) : null)}
                      className="w-16 rounded-lg border border-border bg-surface-2 px-2 py-1 outline-none focus:border-accent"
                    />
                  </label>
                  <label className="flex items-center gap-1.5">
                    <span className="text-faint">连续发言冷却</span>
                    <input
                      type="number"
                      step={0.05}
                      min={0.05}
                      max={0.9}
                      value={gateCooldown ?? ""}
                      disabled={gateCooldown == null}
                      onChange={(e) => setGateCooldown(e.target.value ? Number(e.target.value) : null)}
                      className="w-16 rounded-lg border border-border bg-surface-2 px-2 py-1 outline-none focus:border-accent"
                    />
                  </label>
                  <button
                    type="button"
                    disabled={
                      runTimeoutBusy ||
                      runTimeoutMin == null ||
                      runTimeoutMin < 1 ||
                      runTimeoutMin > 120 ||
                      gateThreshold == null ||
                      gateThreshold < 0.05 ||
                      gateThreshold > 0.95 ||
                      gateCooldown == null ||
                      gateCooldown < 0.05 ||
                      gateCooldown > 0.9
                    }
                    onClick={() => void onSaveRunTimeout()}
                    className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
                  >
                    {runTimeoutBusy ? "保存中…" : "保存"}
                  </button>
                  {runTimeoutMsg && <span className="text-dim">{runTimeoutMsg}</span>}
                </div>
                <p className="mt-1.5 text-[11px] leading-4 text-faint">
                  任务面板"执行"发起的独立任务在此时限内运行（缺省 10 分钟，可调 1..120）；到点未完成按
                  执行失败收口，错误信息明示超时。群聊单轮回复不受此设置影响。点名回复路径仅在
                  明确点名单人或任务交付追问两类场景替换两阶段流程；关闭即回退（A/B 测量对照）。
                  发言资格闸（缺省启用）：Agent 意图自报的相关性/紧急度加权分低于阈值（缺省 0.30）
                  时不发言——被闸意图在讨论区留痕、可保送翻转；@点名/定向直通不受限；上一波发过言的
                  座位额外扣冷却分（缺省 0.20），避免刷屏。
                </p>
              </section>
            </>
          )}

          {section === "data" && (
            <section>
              <h2 className="mb-1 text-sm font-medium">数据与诊断</h2>
              <div className="mb-2 flex flex-wrap items-center gap-2">
                <button
                  type="button"
                  disabled={backupBusy || backups === null}
                  onClick={() => void onCreateBackup()}
                  className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
                >
                  {backupBusy ? "备份中…" : "立即备份"}
                </button>
                <button
                  type="button"
                  onClick={() => void onDownloadDiagnostics()}
                  className="rounded-lg border border-border bg-surface-2 px-3 py-1.5 text-xs text-text transition-colors hover:border-faint"
                >
                  下载诊断报告
                </button>
                {dataMsg && <span className="text-xs text-dim">{dataMsg}</span>}
              </div>
              {backups === null ? (
                <p className="text-xs text-faint">备份面未装配（旧装配或测试形态）。</p>
              ) : backups.length === 0 ? (
                <p className="text-xs text-faint">还没有备份——"立即备份"生成当前数据的完整快照（含校验清单）。</p>
              ) : (
                <ul className="divide-y divide-border overflow-hidden rounded-xl border border-border">
                  {backups.map((b) => (
                    <li key={b.backup_id} className="flex items-center gap-3 px-3 py-2 text-xs">
                      <span className="min-w-0 flex-1">
                        <span className="block truncate text-text" title={b.backup_id}>
                          {absoluteTime(b.created_at)}
                        </span>
                        <span className="block text-faint">
                          {b.backup_id} · {(b.size_bytes / 1024).toFixed(0)} KiB
                        </span>
                      </span>
                      {restoreConfirmID === b.backup_id ? (
                        <>
                          <button
                            type="button"
                            disabled={restoreBusy}
                            onClick={() => void onRequestRestore(b.backup_id)}
                            className="rounded-lg bg-danger px-2.5 py-1 font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-40"
                          >
                            {restoreBusy ? "排程中…" : "确认恢复"}
                          </button>
                          <button
                            type="button"
                            onClick={() => setRestoreConfirmID(null)}
                            className="rounded-lg px-2 py-1 text-dim hover:text-text"
                          >
                            取消
                          </button>
                        </>
                      ) : (
                        <button
                          type="button"
                          onClick={() => setRestoreConfirmID(b.backup_id)}
                          title="恢复此备份（重启应用后生效；当前数据保留在安全副本目录）"
                          className="rounded-lg px-2.5 py-1 text-dim transition-colors hover:bg-[color-mix(in_srgb,var(--danger)_12%,transparent)] hover:text-danger"
                        >
                          恢复
                        </button>
                      )}
                    </li>
                  ))}
                </ul>
              )}
              <p className="mt-1.5 text-[11px] leading-4 text-faint">
                备份是 SQLite 一致快照（VACUUM INTO，不停写）+ 逐文件校验清单；恢复在重启时
                应用——被换下的数据自动保留在数据目录 restore-safety/（可手工回滚）。
                诊断报告含版本/运行时/数据面统计/Agent 注册表状态/日志尾，不含任何凭据。
              </p>
            </section>
          )}

          {section === "appearance" && (
            <section>
              <h2 className="mb-1 text-sm font-medium">外观</h2>
              <p className="mb-3 text-xs text-faint">主题即时生效，本地持久化（侧栏底部也保留了快捷切换）。</p>
              <div className="flex gap-2">
                {(
                  [
                    { id: "dark", label: "暗色（默认）" },
                    { id: "light", label: "亮色" },
                  ] as const
                ).map((t) => (
                  <button
                    key={t.id}
                    type="button"
                    onClick={() => setTheme(t.id)}
                    className={`rounded-xl border px-3.5 py-2 text-sm transition-colors ${
                      theme === t.id
                        ? "border-accent bg-accent-soft text-accent"
                        : "border-border bg-surface-2 text-text hover:border-faint"
                    }`}
                  >
                    {t.label}
                  </button>
                ))}
              </div>
            </section>
          )}

          {section === "integration" && (
            extUnavailable ? (
              <p className="text-xs text-faint">本装配未启用外部文档面。</p>
            ) : (
              <>
                <p className="mb-4 text-xs text-faint">
                  文档级映射：Mosaic 文档一键共享到外部云文档（markdown 快照）；聊天里粘贴白名单链接
                  自动渲染成卡片。凭据仅存本机（0600），不进日志、不经 API 回传。
                </p>

                <section className="mb-6">
                  <h2 className="mb-1 text-sm font-medium">腾讯文档</h2>
                  <p className="mb-2 text-xs text-faint">
                    token 经官方授权页 docs.qq.com/scenario/open-claw.html 获取（老地址
                    open/auth/mcp.html 已跳转至此）——登录腾讯文档 → 个人身份 → 获取/重置 token。
                    频控：免费 2000 次/天，超级会员 20000 次/天。
                  </p>
                  <div className="mb-2 text-xs">
                    <span className={extProviders.find((p) => p.provider === "tencent_docs")?.configured ? "text-accent" : "text-faint"}>
                      {extProviders.find((p) => p.provider === "tencent_docs")?.configured ? "● 已配置" : "○ 未配置"}
                    </span>
                  </div>
                  <div className="flex flex-wrap items-center gap-2">
                    <input
                      type="password"
                      value={txToken}
                      onChange={(e) => setTxToken(e.target.value)}
                      placeholder="粘贴个人 MCP token"
                      autoComplete="off"
                      className="w-72 rounded-xl border border-border bg-surface-2 px-3 py-2 font-mono text-xs text-text placeholder:text-faint focus:border-accent focus:outline-none"
                    />
                    <button
                      type="button"
                      disabled={txBusy || txToken.trim() === ""}
                      onClick={() => void saveTxToken(txToken.trim())}
                      className="rounded-xl border border-accent bg-accent-soft px-3 py-2 text-xs text-accent transition-colors hover:bg-accent/20 disabled:opacity-40"
                    >
                      保存
                    </button>
                    {extProviders.find((p) => p.provider === "tencent_docs")?.configured && (
                      <>
                        <button
                          type="button"
                          disabled={txBusy}
                          onClick={() => void verifyExtDoc("tencent_docs")}
                          className="rounded-xl border border-border bg-surface-2 px-3 py-2 text-xs text-text transition-colors hover:border-faint disabled:opacity-40"
                        >
                          测试连接
                        </button>
                        <button
                          type="button"
                          disabled={txBusy}
                          onClick={() => void saveTxToken("")}
                          className="rounded-xl border border-border bg-surface-2 px-3 py-2 text-xs text-dim transition-colors hover:border-faint hover:text-text disabled:opacity-40"
                        >
                          清除
                        </button>
                      </>
                    )}
                  </div>
                  {txMsg && <p className="mt-2 text-xs text-dim">{txMsg}</p>}
                </section>

                <section>
                  <h2 className="mb-1 text-sm font-medium">飞书云文档</h2>
                  <p className="mb-2 text-xs text-faint">
                    用户身份 OAuth（文档落你自己的"我的云文档"）。一次性准备：open.feishu.cn 创建
                    <b>自建应用</b> → 权限管理开通 <code className="rounded bg-surface-3 px-1">drive:drive</code>、
                    <code className="mx-1 rounded bg-surface-3 px-1">offline_access</code> 与
                    <code className="mx-1 rounded bg-surface-3 px-1">bitable:app:readonly</code>（多维表格导入）→
                    安全设置登记重定向 URL <code className="rounded bg-surface-3 px-1">http://localhost:3000/callback</code>
                    （完全一致）→ 发布应用版本。「登录飞书」自动绑定：授权页跳转落回本地
                    回调即自动换取 token（端口被占时降级手动粘 code）。
                  </p>
                  <div className="mb-2 text-xs">
                    <span className={fsAuthorized ? "text-accent" : "text-faint"}>
                      {fsAuthorized ? "● 已授权（token 自动刷新）" : "○ 未授权"}
                    </span>
                  </div>
                  <div className="flex flex-wrap items-center gap-2">
                    <input
                      value={fsAppID}
                      onChange={(e) => setFsAppID(e.target.value)}
                      placeholder="App ID（cli_ 开头）"
                      autoComplete="off"
                      className="w-44 rounded-xl border border-border bg-surface-2 px-3 py-2 font-mono text-xs text-text placeholder:text-faint focus:border-accent focus:outline-none"
                    />
                    <input
                      type="password"
                      value={fsAppSecret}
                      onChange={(e) => setFsAppSecret(e.target.value)}
                      placeholder="App Secret"
                      autoComplete="off"
                      className="w-56 rounded-xl border border-border bg-surface-2 px-3 py-2 font-mono text-xs text-text placeholder:text-faint focus:border-accent focus:outline-none"
                    />
                    <button
                      type="button"
                      disabled={fsBusy || fsAppID.trim() === "" || fsAppSecret.trim() === ""}
                      onClick={() => void saveFeishuApp()}
                      className="rounded-xl border border-accent bg-accent-soft px-3 py-2 text-xs text-accent transition-colors hover:bg-accent/20 disabled:opacity-40"
                    >
                      保存应用凭据
                    </button>
                    <button
                      type="button"
                      disabled={fsBusy || fsAppID.trim() === ""}
                      onClick={() => void loginFeishu()}
                      className="rounded-xl border border-border bg-surface-2 px-3 py-2 text-xs text-text transition-colors hover:border-faint disabled:opacity-40"
                    >
                      登录飞书
                    </button>
                  </div>
                  <div className="mt-2 flex flex-wrap items-center gap-2">
                    <input
                      value={fsCode}
                      onChange={(e) => setFsCode(e.target.value)}
                      placeholder="手动备用通道：从地址栏复制 code 粘贴（自动绑定时无需）"
                      autoComplete="off"
                      className="w-72 rounded-xl border border-border bg-surface-2 px-3 py-2 font-mono text-xs text-text placeholder:text-faint focus:border-accent focus:outline-none"
                    />
                    <button
                      type="button"
                      disabled={fsBusy || fsCode.trim() === ""}
                      onClick={() => void submitFeishuCode()}
                      className="rounded-xl border border-border bg-surface-2 px-3 py-2 text-xs text-text transition-colors hover:border-faint disabled:opacity-40"
                    >
                      绑定
                    </button>
                  </div>
                  {fsMsg && <p className="mt-2 max-w-2xl break-all text-xs text-dim">{fsMsg}</p>}
                </section>
              </>
            )
          )}

          {section === "general" && (
            <>
              <section>
                <h2 className="mb-1 text-sm font-medium">开发者</h2>
                <div className="flex items-start justify-between gap-4">
                  <p className="text-xs text-faint">
                    全局开关：开启后每个房间信息面板出现"调试"分段（trace、事件日志、预算水位——
                    调试数据是房间局部信息，在房间上下文查看）。调试端点需服务端以 -dev
                    启动才装配（桌面端默认启用）；开关只控制界面展示，本地持久化。
                  </p>
                  <button
                    type="button"
                    aria-pressed={devMode}
                    onClick={() => setDevMode(!devMode)}
                    className={`shrink-0 rounded-xl border px-3.5 py-2 text-sm transition-colors ${
                      devMode
                        ? "border-accent bg-accent-soft text-accent"
                        : "border-border bg-surface-2 text-text hover:border-faint"
                    }`}
                  >
                    {devMode ? "已开启" : "已关闭"}
                  </button>
                </div>
              </section>

              <section>
                <h2 className="mb-3 text-sm font-medium">关于</h2>
                <div className="flex items-center gap-4 rounded-xl border border-border px-4 py-4">
                  <AppLogo size={40} />
                  <div className="min-w-0 flex-1">
                    <p className="text-sm font-medium">Mosaic v0.5.4</p>
                    <p className="text-xs text-faint">多智能体讨论室 · 本地单用户</p>
                  </div>
                  <a
                    href="https://github.com/sisibeloved/Mosaic"
                    target="_blank"
                    rel="noreferrer"
                    className="shrink-0 text-xs text-accent hover:underline"
                  >
                    GitHub 仓库
                  </a>
                </div>
                <p className="mt-2 text-[11px] text-faint">
                  显示名当前由服务端分配；自定义昵称与头像随后续版本开放（届时恢复个人中心入口）。
                </p>
              </section>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

function ExeBadge({
  children,
  tone = "dim",
}: {
  children: React.ReactNode;
  tone?: "ok" | "warn" | "dim";
}) {
  const cls =
    tone === "ok"
      ? "bg-accent-soft text-accent"
      : tone === "warn"
        ? "bg-[color-mix(in_srgb,var(--warn)_14%,transparent)] text-warn"
        : "bg-surface-3 text-dim";
  return <span className={`rounded px-1.5 py-px text-[10px] leading-4 ${cls}`}>{children}</span>;
}

function relativeTimeOf(iso: string): string {
  const d = new Date(iso).getTime();
  if (!Number.isFinite(d)) return iso;
  const diff = Date.now() - d;
  if (diff < 60_000) return "刚刚";
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`;
  return `${Math.floor(diff / 86_400_000)} 天前`;
}

/** 生命周期状态徽标文案（ADR-0018 状态机）。 */
function agentStatusLabel(status: string): string {
  switch (status) {
    case "active":
      return "活跃";
    case "suspended":
      return "已暂停";
    case "retiring":
      return "退役中";
    case "retired":
      return "已退役";
    default:
      return status;
  }
}

/** 生命周期动作按钮（暂停/恢复/退役）。 */
function LifecycleBtn({
  children,
  onClick,
  disabled,
  busy,
  danger,
}: {
  children: React.ReactNode;
  onClick: () => void;
  disabled?: boolean;
  busy?: boolean;
  danger?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className={`rounded-lg px-2 py-1 text-xs transition-opacity hover:opacity-85 disabled:opacity-40 ${
        danger ? "bg-surface-3 text-danger" : "bg-surface-3 text-text"
      }`}
    >
      {busy ? "…" : children}
    </button>
  );
}

/** 生命周期事件流（懒加载：首次展开才拉取，ADR-0018 只读审计面；rev 为生命
 * 周期操作修订号——操作成功后递增使缓存失效，已展开明细立即重读，连续操作
 * 每次都拿到最新事件史（审校 P2#2）。 */
function AgentEventsDetails({ agentID, rev }: { agentID: string; rev: number }) {
  const [events, setEvents] = useState<AgentEvent[] | null>(null);
  const [open, setOpen] = useState(false);
  useEffect(() => {
    setEvents(null); // 缓存失效：下次展开或（已展开时）下方读取效应立即重拉
  }, [rev]);
  useEffect(() => {
    if (!open || events !== null) return;
    void api
      .identityAgentEvents(agentID)
      .then(({ events: evs }) => setEvents(evs))
      .catch(() => setEvents([]));
  }, [open, events, agentID]);
  return (
    <details onToggle={(e) => setOpen((e.target as HTMLDetailsElement).open)}>
      <summary className="cursor-pointer select-none text-[11px] text-faint">生命周期事件</summary>
      {events === null ? (
        <p className="mt-1 text-[11px] text-faint">加载中…</p>
      ) : events.length === 0 ? (
        <p className="mt-1 text-[11px] text-faint">暂无事件。</p>
      ) : (
        <ul className="mt-1 flex flex-col gap-0.5 font-mono text-[10px] text-faint">
          {events.map((ev) => (
            <li key={ev.seq}>
              #{ev.seq} {absoluteTime(ev.occurred_at)} {eventSummary(ev)}
            </li>
          ))}
        </ul>
      )}
    </details>
  );
}

function eventSummary(ev: AgentEvent): string {
  const p = (ev.payload ?? {}) as Record<string, string>;
  if (ev.event_type === "created") return `created（来源 ${p.source ?? "?"}）`;
  return `status_changed ${p.from ?? "?"} → ${p.to ?? "?"}${p.reason ? `（${p.reason}）` : ""}`;
}

/** 职责模板版本明细（ADR-0019）：展开或修订号变化即读取——读取直接依赖
 * (open, rev)（审校 P2#4：刷新标记变化必触发重读，不依赖缓存空值比较）；
 * 请求序号拒迟到响应；失败只记错误态不缓存空结果，重试入口显式（审校 P2#5）。 */
function SlotVersionsDetails({ slotSpecID, rev }: { slotSpecID: string; rev: number }) {
  const [versions, setVersions] = useState<SlotSpecView[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const seq = useRef(0);
  useEffect(() => {
    if (!open) return;
    const cur = ++seq.current;
    setError(null);
    void api
      .listSlotSpecVersions(slotSpecID)
      .then(({ versions: vs }) => {
        if (seq.current !== cur) return; // 迟到响应丢弃（审校 P2#4）
        setVersions(vs);
      })
      .catch((e) => {
        if (seq.current !== cur) return;
        // 失败不缓存空结果（审校 P2#5）——保持 versions 现值，显式错误态 + 重试
        setError(e instanceof Error ? e.message : String(e));
      });
  }, [open, rev, slotSpecID, attempt]);
  return (
    <details onToggle={(e) => setOpen((e.target as HTMLDetailsElement).open)}>
      <summary className="cursor-pointer select-none text-[11px] text-faint">版本历史</summary>
      {error !== null ? (
        <div className="mt-1 flex items-center gap-2">
          <p className="text-[11px] text-danger">读取失败：{error}</p>
          <button
            type="button"
            onClick={() => setAttempt((a) => a + 1)}
            className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim"
          >
            重试
          </button>
        </div>
      ) : versions === null ? (
        <p className="mt-1 text-[11px] text-faint">加载中…</p>
      ) : versions.length === 0 ? (
        <p className="mt-1 text-[11px] text-faint">暂无版本。</p>
      ) : (
        <ul className="mt-1 flex flex-col gap-1">
          {[...versions].reverse().map((v) => (
            <li key={v.version} className="rounded-lg bg-surface-2 px-2 py-1.5 text-[11px]">
              <div className="flex items-center gap-1.5">
                <span className="font-medium">v{v.version}</span>
                <span className="text-faint">{v.name}</span>
                <span className="text-faint">{absoluteTime(v.created_at)}</span>
              </div>
              <p className="mt-0.5 text-dim">{v.responsibility}</p>
              <p className="mt-0.5 text-faint">交付：{v.deliverables.join("；")}</p>
              {v.tools && (v.tools.required.length > 0 || v.tools.optional.length > 0) && (
                <p className="mt-0.5 font-mono text-[10px] text-faint">
                  必选 {v.tools.required.join(", ") || "—"} · 可选 {v.tools.optional.join(", ") || "—"}
                </p>
              )}
            </li>
          ))}
        </ul>
      )}
    </details>
  );
}

/** 实例任职管理（ADR-0021 / 0.5.4）：展开即拉取；任职（Agent 下拉 + 钉版本）、
 * 激活（单 active——冲突可见 409）、释放（交接备注输入）。 */
function InstanceBindings({ instanceID, specLatest }: { instanceID: string; specLatest: number }) {
  const [open, setOpen] = useState(false);
  const [bindings, setBindings] = useState<BindingView[] | null>(null);
  const [agents, setAgents] = useState<{ agent_id: string; display_name: string }[]>([]);
  const [agentID, setAgentID] = useState("");
  const [version, setVersion] = useState<string>("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [rev, setRev] = useState(0);
  const seq = useRef(0);

  useEffect(() => {
    if (!open) return;
    const cur = ++seq.current;
    void Promise.all([api.listSlotBindings(instanceID), api.identityAgents()])
      .then(([b, a]) => {
        if (seq.current !== cur) return;
        setBindings(b.bindings);
        const active = a.agents.filter((x) => x.status === "active");
        setAgents(active.map((x) => ({ agent_id: x.agent_id, display_name: x.display_name })));
        setAgentID((prev) => prev || active[0]?.agent_id || "");
      })
      .catch((e) => {
        if (seq.current !== cur) return;
        setMsg(e instanceof Error ? e.message : String(e));
      });
  }, [open, instanceID, rev]);

  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setMsg(null);
    try {
      await fn();
      setRev((r) => r + 1);
    } catch (e) {
      setMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const activeBinding = (bindings ?? []).find((b) => b.status === "active");
  const readyOnes = (bindings ?? []).filter((b) => b.status === "ready");
  const draining = (bindings ?? []).filter((b) => b.status === "draining");
  // 最近交接按释放时间（updated_at）取最新（审校 P2#1）——非创建序末项；
  // 接口保持创建序不变
  const released = (bindings ?? [])
    .filter((b) => b.status === "released")
    .sort((a, b) => (a.updated_at < b.updated_at ? 1 : -1))
    .slice(0, 1);

  return (
    <details onToggle={(e) => setOpen((e.target as HTMLDetailsElement).open)}>
      <summary className="cursor-pointer select-none text-[11px] text-faint">
        任职{bindings === null ? "" : activeBinding ? `：${agentNameOf(agents, activeBinding.agent_id)}（在任）` : "：空缺"}
      </summary>
      <div className="mt-1 flex flex-col gap-1.5">
        {msg && <p className="text-[11px] text-danger">{msg}</p>}
        {bindings === null ? (
          <p className="text-[11px] text-faint">加载中…</p>
        ) : (
          <>
            {activeBinding && (
              <div className="flex flex-wrap items-center gap-1.5 rounded-lg bg-surface-2 px-2 py-1.5 text-[11px]">
                <span className="font-medium">在任：{agentNameOf(agents, activeBinding.agent_id)}</span>
                <ExeBadge>v{activeBinding.slot_spec_version}（钉住）</ExeBadge>
                <input
                  value={note}
                  onChange={(e) => setNote(e.target.value)}
                  placeholder="交接备注（可选，≤4096 字）"
                  className="min-w-0 flex-1 rounded border border-border bg-surface px-1.5 py-px text-[11px] outline-none focus:border-accent"
                />
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void act(() => api.releaseSlotBinding(activeBinding.binding_id, note.trim() || undefined))}
                  className="rounded bg-surface-3 px-2 py-px text-[11px] text-danger disabled:opacity-40"
                >
                  释放并排空
                </button>
              </div>
            )}
            {draining.map((b) => (
              <p key={b.binding_id} className="text-[11px] text-warn">
                排空中（{agentNameOf(agents, b.agent_id)}）——在途工作清空后重试释放
              </p>
            ))}
            {readyOnes.length > 0 && (
              <div className="flex flex-col gap-1">
                {readyOnes.map((b) => (
                  <div key={b.binding_id} className="flex items-center gap-1.5 text-[11px]">
                    <span>就绪：{agentNameOf(agents, b.agent_id)}</span>
                    <ExeBadge>v{b.slot_spec_version}</ExeBadge>
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => void act(() => api.activateSlotBinding(b.binding_id))}
                      className="rounded bg-surface-3 px-2 py-px text-[11px] disabled:opacity-40"
                    >
                      激活{activeBinding ? "（需先释放现任）" : ""}
                    </button>
                  </div>
                ))}
              </div>
            )}
            {released.length > 0 && released[0].handoff_note && (
              <p className="rounded bg-surface-2 px-2 py-1 text-[11px] text-dim" title={released[0].binding_id}>
                最近交接（{agentNameOf(agents, released[0].agent_id)}）：{released[0].handoff_note}
              </p>
            )}
            <div className="flex flex-wrap items-center gap-1.5 text-[11px]">
              <select
                value={agentID}
                onChange={(e) => setAgentID(e.target.value)}
                className="rounded border border-border bg-surface-2 px-1.5 py-px outline-none"
              >
                {agents.map((a) => (
                  <option key={a.agent_id} value={a.agent_id}>
                    {a.display_name}
                  </option>
                ))}
              </select>
              <input
                value={version}
                onChange={(e) => setVersion(e.target.value)}
                placeholder={`钉版本（空 = 最新 v${specLatest}）`}
                className="w-32 rounded border border-border bg-surface-2 px-1.5 py-px outline-none"
              />
              <button
                type="button"
                disabled={busy || !agentID}
                onClick={() =>
                  void act(() =>
                    api.prepareSlotBinding({
                      slot_instance_id: instanceID,
                      agent_id: agentID,
                      ...(version.trim() ? { slot_spec_version: Number(version.trim()) } : {}),
                    }),
                  )
                }
                className="rounded bg-surface-3 px-2 py-px disabled:opacity-40"
              >
                建任职
              </button>
            </div>
          </>
        )}
      </div>
    </details>
  );
}

function agentNameOf(agents: { agent_id: string; display_name: string }[], id: string): string {
  return agents.find((a) => a.agent_id === id)?.display_name ?? id;
}
