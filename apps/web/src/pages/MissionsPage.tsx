// 任务中心（0.6.0 / ADR-0023）：Mission 契约——创建（完备 → ready / 缺失 →
// draft）、列表、详情（当前契约 + 版本历史 + 事件流）、修订（CAS 冲突可见）、
// 取消。0.6.2 增执行上下文清单（装配存档 + 历史 + 层账本）。请求序号守卫与
// 错误态纪律沿用 0.5 系列教训。
import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  api,
  ApiError,
  type BindingView,
  type ContextManifestSummary,
  type ContextManifestView,
  type DeliveryPackageView,
  type IdentityAgent,
  type MissionEvent,
  type MissionRunView,
  type MissionSummary,
  type MissionView,
  type ReopenRequestView,
  type HandoverEventView,
  type ReturnNoticeView,
  type SlotInstanceView,
} from "../api/client";
import { absoluteTime } from "../lib/ui";

const splitLines = (s: string): string[] =>
  s.split("\n").map((x) => x.trim()).filter(Boolean);

// 复审 R6：证据判空统一口径 = Unicode White_Space ∪ U+FEFF ∪ U+0085（与服务端
// blankTrimmed 同集——Go TrimSpace 不含 BOM、JS trim 不含 NEL，取并集）。
const blankTrim = (s: string): string => s.replace(/^[\s\uFEFF\u0085]+|[\s\uFEFF\u0085]+$/g, "");
const hasEvidence = (s: string): boolean => blankTrim(s) !== "";

function statusClass(status: string): string {
  if (status === "accepted") return "text-ok font-semibold";
  if (status === "ready" || status === "running") return "text-ok";
  if (status === "canceled") return "text-danger line-through";
  // paused（阻止新调度）/ waiting（等待核验）/ canceling（停止进行中）与 draft 同暖色系
  if (status === "canceling") return "text-danger";
  return "text-warn";
}

// 0.6.10 回房通知（ADR-0033）：kind/状态徽标文案与配色。
function returnKindLabel(k: string): string {
  return { reviewing: "已提请验收", accepted: "验收通过", returned: "验收退回", canceled: "已取消" }[k] ?? k;
}
function returnKindCls(k: string): string {
  if (k === "accepted") return "text-ok font-semibold";
  if (k === "returned") return "text-warn";
  if (k === "canceled") return "text-faint";
  return "text-dim";
}
function returnStatusLabel(s: string): string {
  return { pending: "待投", held: "保留（房间暂停）", delivered: "已回房", canceled: "已取消（房间删除）", failed: "投递失败" }[s] ?? s;
}
function returnStatusCls(s: string): string {
  if (s === "delivered") return "text-ok";
  if (s === "held" || s === "pending") return "text-warn";
  if (s === "failed") return "text-danger";
  return "text-faint";
}

/** 九审 #1：单卡草稿快照——按任务卡（room|task）记录用户在该卡上编辑的
 * 契约字段与预算；409 合并只用**当前卡**的快照（跨卡污染关闭）。 */
/** 十审 #1/#2：冻结编辑快照——409 时捕获**本卡字段**的当前值（字段级归属
 * 过滤后）；合并只消费这份冻结副本，其后表单变化不影响本次合并。 */
type FrozenDraft = Partial<Record<
  "objective" | "scope" | "acceptance" | "deliverables" | "sources" | "returns" | "reviewer" | "owner", string>> & {
  budget?: Partial<Record<"concurrency" | "max_runs" | "max_wall_clock_seconds" | "max_retries" | "max_delegation_depth", string>>;
};

/** 十一审 #2：提交时快照——发送创建请求前一并冻结来源、字段归属与载荷；
 * 409 返回时（等待期间可能已切到其它任务卡编辑）按这份快照构建续接目标
 * 与冻结编辑——被后续编辑改写的 form/归属不入本次合并。 */
type SubmittedDraft = {
  room: string; task: string;
  fieldOwners: Record<string, string>;
  budgetOwners: Record<string, string>;
  fields: Record<string, string>;
  budget: Record<string, string>;
};

export function MissionsPage() {
  const [missions, setMissions] = useState<MissionSummary[] | null>(null);
  const [listErr, setListErr] = useState<string | null>(null);
  const [agents, setAgents] = useState<{ agent_id: string; display_name: string }[]>([]);
  const [formOpen, setFormOpen] = useState(false);
  const [searchParams] = useSearchParams();
  const [form, setForm] = useState(() => ({
    objective: "", scope: "", acceptance: "", deliverables: "", sources: "", returns: "",
    owner: "", reviewer: "",
    // 交办入口预填（任务卡「交办」跳转 ?room=&task=——ADR-0024 / 0.6.1）
    room: searchParams.get("room") ?? "", task: searchParams.get("task") ?? "",
  }));
  const [budgetOpen, setBudgetOpen] = useState(false);
  // 0.6.13 审校 #6 + 复审 #1/#2/#6：已交办任务卡续接模式（/v1/dispatch/update）
  // ——命中映射的来源不再走 createMission（撞 409 source_already_mapped）。
  // 复审修订：目标 / 版本 / 完整草稿（含 constraints/delegation 基准）在详情
  // 请求**成功后同请求原子生效**（列表可陈旧——版本以预填响应为准）；切换
  // 来源作废旧请求（代次）；加载与失败期间禁用提交并提供重试；冲突提供显式
  // 重新加载（不自动覆盖编辑中草稿）。
  const [updateTarget, setUpdateTarget] = useState<{ missionID: string; version: number; key: string } | null>(null);
  const [baseContract, setBaseContract] = useState<{ constraints: string[]; delegation: { allowed: string[]; max_depth: number } } | null>(null);
  const [detailState, setDetailState] = useState<"idle" | "loading" | "error">("idle");
  const [conflictReload, setConflictReload] = useState(false);
  const [retryTick, setRetryTick] = useState(0);
  const detailSeq = useRef(0);
  // 七审 #5：映射列表未载（null）期间锁定契约字段——列表到达触发详情预填
  // 不得覆盖加载期输入（与详情 loading 同门）。
  const formLocked = detailState !== "idle" || missions === null;
  // 三审/四审前端（映射陈旧切续接）：create 撞 409 后保留用户录入——草稿
  // **绑定引发 409 的 room/task**（四审 #2）；命中同一来源时以详情为底稿合并
  // 用户显式修改的字段（四审 #1：未编辑字段/预算留现行值）。
  // 十二审 #2：待合并记录**按任务卡保留**（Map——键 room|task）：其它任务卡
  // 的详情加载不清除记录；回到引发 409 的卡才消费（消费即删）；提交成功清空。
  const pendingMergesRef = useRef<Map<string, FrozenDraft>>(new Map());
  // 五审 #1：预算按**字段**跟踪用户编辑——合并只覆盖编辑过的字段（展开面板
  // 查看不得放宽其他上限）；面板展开状态不进详情加载依赖（五审 #2）。
  const budgetEditedRef = useRef<Set<string>>(new Set());
  // 六审 #1 + 八审 #1：预算/草稿绑定**已确认卡**（命中映射预填过的来源）——
  // 逐字输入中的来源是草稿不触发清理；仅离开一张已确认卡时清理上一卡预算与草稿。
  const confirmedSrcRef = useRef<{ room: string; task: string } | null>(null);
  // 九审/十审 #1：当前来源（逐字跟踪）+ **字段级任务卡归属**——每个契约/预算
  // 字段记录其最后一次编辑发生在哪张卡；换卡后旧卡字段不再属于新卡（跨卡
  // 污染关闭），首绑来源前的编辑接纳归新卡（十审 #2）。
  const lastSrcRef = useRef<{ room: string; task: string }>({ room: "", task: "" });
  const fieldOwnersRef = useRef<Record<string, string>>({});
  const budgetOwnersRef = useRef<Record<string, string>>({});
  const prevContentRef = useRef<Record<string, string>>({});
  const [budget, setBudget] = useState({ concurrency: "1", max_runs: "32", max_wall_clock_seconds: "86400", max_retries: "0", max_delegation_depth: "0" });
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [rev, setRev] = useState(0);
  const seq = useRef(0);

  useEffect(() => {
    const cur = ++seq.current;
    setListErr(null);
    void Promise.all([api.listMissions(), api.identityAgents()])
      .then(([m, a]) => {
        if (seq.current !== cur) return;
        setMissions(m.missions);
        const active = a.agents.filter((x: IdentityAgent) => x.status === "active");
        setAgents(active.map((x) => ({ agent_id: x.agent_id, display_name: x.display_name })));
      })
      .catch((e) => {
        if (seq.current !== cur) return;
        setListErr(e instanceof Error ? e.message : String(e));
      });
  }, [rev]);

  // 0.6.10 回房卡片跳转（?id= 选中任务）：房间引用卡片直达任务详情。
  useEffect(() => {
    const id = searchParams.get("id");
    if (id) setSelected(id);
  }, [searchParams]);

  // 十审 #1：字段归属跟踪——内容/预算字段值变化时，把该字段记到**当前卡**
  //（lastSrc 键）。合并按"字段属于本卡"取值——A 卡残留（未在新卡编辑过的
  // 字段）不属于 B，回落 B 现行值。
  useEffect(() => {
    const key = lastSrcRef.current.room + "|" + lastSrcRef.current.task;
    const content: Record<string, string> = {
      objective: form.objective, scope: form.scope, acceptance: form.acceptance,
      deliverables: form.deliverables, sources: form.sources, returns: form.returns,
      reviewer: form.reviewer, owner: form.owner,
    };
    const prev = prevContentRef.current;
    for (const [name, val] of Object.entries(content)) {
      if (prev[name] !== val) {
        fieldOwnersRef.current[name] = key;
      }
    }
    prevContentRef.current = content;
  }, [form.objective, form.scope, form.acceptance, form.deliverables, form.sources,
      form.returns, form.reviewer, form.owner]);

  // 复审 #1/#6：来源命中既有映射 → 续接模式——详情成功后目标/版本/预填
  // 原子生效；失败禁提（提交门在按钮与提交函数双重校验）；代次作废切换前
  // 的在途响应（慢请求不得覆盖已切换的表单）。
  useEffect(() => {
    const room = form.room.trim();
    const task = form.task.trim();
    // 十二审 #1：lastSrc 逐字跟踪；**不做任何归属迁移**——未绑定草稿（归属
    // 为空键、部分键或逐字输入的中间键）保持原样，提交时再按最终来源统一
    // 接纳（submitCreate 内——逐字输入 task 的首字符不再把归属固定到中间键）。
    lastSrcRef.current = { room, task };
    // 八审 #1：清理仅在**离开已确认卡**时发生一次（confirmedSrc 置空后逐字
    // 输入不再触发）；命中新卡预填/合并时在详情回调里确认新卡。
    const confirmed = confirmedSrcRef.current;
    if (confirmed && (confirmed.room !== room || confirmed.task !== task)) {
      confirmedSrcRef.current = null; // 离开已确认卡——丢弃其预算与草稿（恰一次）
      budgetEditedRef.current = new Set();
      setBudget({ concurrency: "1", max_runs: "32", max_wall_clock_seconds: "86400", max_retries: "0", max_delegation_depth: "0" });
      setForm((f) => ({
        ...f,
        objective: "", scope: "", acceptance: "", deliverables: "", sources: "", returns: "",
        reviewer: "", owner: "",
      }));
    }
    if (!formOpen || !room || !task || !missions) {
      detailSeq.current++;
      setUpdateTarget(null);
      setBaseContract(null);
      setDetailState("idle");
      return;
    }
    const hit = missions.find(
      (m) => m.room_source?.room_id === room && m.room_source?.task_id === task,
    );
    if (!hit) {
      detailSeq.current++;
      setUpdateTarget(null);
      setBaseContract(null);
      setDetailState("idle");
      return;
    }
    const cur = ++detailSeq.current;
    setDetailState("loading");
    setUpdateTarget(null);
    setBaseContract(null);
    void api
      .getMission(hit.mission_id)
      .then((d) => {
        if (detailSeq.current !== cur) return; // 旧请求（来源已切换）——作废
        confirmedSrcRef.current = { room, task }; // 八审 #1：命中确认（此后离开才清理）
        setUpdateTarget({ missionID: hit.mission_id, version: d.contract_version, key: `web-update-${crypto.randomUUID()}` });
        // 复审 #2：不可编辑字段以同版本契约为基准（全量替换不得清空）。
        setBaseContract({
          constraints: d.constraints ?? [],
          delegation: { allowed: d.delegation?.allowed ?? [], max_depth: d.delegation?.max_depth ?? 0 },
        });
        // 十二审 #2：按任务卡消费（get+delete）——其它任务卡的详情加载不
        // 触碰记录（Map 按键保留）。
        const pmKey = room + "|" + task;
        const fr = pendingMergesRef.current.get(pmKey);
        if (fr) {
          // 409 合并路径（四审 #1）：以现行契约为**底稿**，仅合并用户显式输入
          // 的字段（非空者）；预算未显式编辑（开关未开）留现行值——不清空
          // 验收/交付、不放宽预算。合并结果回填表单供用户核对。
          // 十审 #1：合并只消费**冻结快照**（409 时按字段归属捕获的本卡编辑）
          //——其后表单变化不影响本次合并；未在本卡编辑的字段回落 B 现行值。
          pendingMergesRef.current.delete(pmKey);
          const fv = (v?: string) => (v && v.trim() ? v : "");
          setForm((f) => ({
            ...f,
            objective: fv(fr.objective) || d.objective || "",
            scope: fv(fr.scope) || d.scope || "",
            acceptance: fv(fr.acceptance) || (d.acceptance_criteria ?? []).join("\n"),
            deliverables: fv(fr.deliverables) || (d.deliverable_specs ?? []).join("\n"),
            sources: fv(fr.sources) || (d.source_refs ?? []).join("\n"),
            returns: fv(fr.returns) || (d.return_targets ?? []).join("\n"),
            reviewer: fv(fr.reviewer) || d.reviewer || "",
          }));
          // 预算：现行值为底，仅覆盖冻结快照中**本卡编辑过**的字段。
          const cur = {
            concurrency: String(d.budget?.concurrency ?? 1),
            max_runs: String(d.budget?.max_runs ?? 32),
            max_wall_clock_seconds: String(d.budget?.max_wall_clock_seconds ?? 86400),
            max_retries: String(d.budget?.max_retries ?? 0),
            max_delegation_depth: String(d.budget?.max_delegation_depth ?? 0),
          };
          const fb = fr.budget ?? {};
          setBudget({
            concurrency: fb.concurrency ?? cur.concurrency,
            max_runs: fb.max_runs ?? cur.max_runs,
            max_wall_clock_seconds: fb.max_wall_clock_seconds ?? cur.max_wall_clock_seconds,
            max_retries: fb.max_retries ?? cur.max_retries,
            max_delegation_depth: fb.max_delegation_depth ?? cur.max_delegation_depth,
          });
          setBudgetOpen(true); // 显示合并结果（五审 #2：仅显示——不再触发详情重载）
          setDetailState("idle");
          return;
        }
        setForm((f) => ({
          ...f,
          objective: d.objective ?? f.objective,
          scope: d.scope ?? f.scope,
          acceptance: (d.acceptance_criteria ?? []).join("\n"),
          deliverables: (d.deliverable_specs ?? []).join("\n"),
          sources: (d.source_refs ?? []).join("\n"),
          returns: (d.return_targets ?? []).join("\n"),
          reviewer: d.reviewer ?? "",
          owner: d.owner_agent_id ?? "",
        }));
        setBudget({
          concurrency: String(d.budget?.concurrency ?? 1),
          max_runs: String(d.budget?.max_runs ?? 32),
          max_wall_clock_seconds: String(d.budget?.max_wall_clock_seconds ?? 86400),
          max_retries: String(d.budget?.max_retries ?? 0),
          max_delegation_depth: String(d.budget?.max_delegation_depth ?? 0),
        });
        budgetEditedRef.current = new Set(); // 预算整体换现行——编辑跟踪重置
        setBudgetOpen(true);
        setDetailState("idle");
      })
      .catch((e) => {
        if (detailSeq.current !== cur) return;
        setDetailState("error"); // 复审 #1：读取失败禁用提交（目标不生效）
        setMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
      });
  }, [formOpen, form.room, form.task, missions, retryTick]);

  const act = async (fn: () => Promise<unknown>): Promise<ApiError | null> => {
    setBusy(true);
    setMsg(null);
    try {
      await fn();
      setRev((r) => r + 1);
      return null;
    } catch (e) {
      setMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
      return e instanceof ApiError ? e : null;
    } finally {
      setBusy(false);
    }
  };

  const submitCreate = () => {
    // 来源半填拦截（审校 P2#2）：两项须同时为空或同时非空——半填静默丢
    // 失来源不可事后补（映射为创建事实不可修订）
    const hasRoom = form.room.trim() !== "";
    const hasTask = form.task.trim() !== "";
    if (hasRoom !== hasTask) {
      setMsg("房间来源须同时填写 room_id 与 task_id（或同时留空）——来源创建后不可补");
      return;
    }
    const b = {
      concurrency: Number(budget.concurrency), max_runs: Number(budget.max_runs),
      max_wall_clock_seconds: Number(budget.max_wall_clock_seconds),
      max_retries: Number(budget.max_retries), max_delegation_depth: Number(budget.max_delegation_depth),
    };
    // 十一审 #2：发送前冻结**提交时快照**——来源、字段归属与载荷一并拷贝。
    // 等待响应期间切到其它任务卡编辑（form/归属已被改写）不影响 409 冻结。
    const fk = form.room.trim() + "|" + form.task.trim();
    const snap: SubmittedDraft = {
      room: form.room.trim(), task: form.task.trim(),
      fieldOwners: { ...fieldOwnersRef.current },
      budgetOwners: { ...budgetOwnersRef.current },
      fields: {
        objective: form.objective, scope: form.scope, acceptance: form.acceptance,
        deliverables: form.deliverables, sources: form.sources, returns: form.returns,
        reviewer: form.reviewer, owner: form.owner,
      },
      budget: { ...budget },
    };
    // 十二审 #1：提交时就地接纳**未绑定草稿**——归属为空键、部分键（只填
    // room）或逐字输入的中间键（提交键的输入前缀态：room/task 两段分别是
    // 最终值的前缀）划归最终来源键；其它完整键（旧卡编辑）隔离（跨卡污染
    // 关闭）。只改快照副本——live refs 不受影响。
    const splitKey = (k: string): [string, string] => {
      const i = k.indexOf("|");
      return [k.slice(0, i), k.slice(i + 1)];
    };
    const [fkr, fkt] = splitKey(fk);
    const feedsKey = (owner: string) => {
      if (owner === fk) return true;
      const [orr, ort] = splitKey(owner);
      return fkr.startsWith(orr) && fkt.startsWith(ort);
    };
    for (const name of Object.keys(snap.fieldOwners)) {
      if (feedsKey(snap.fieldOwners[name])) snap.fieldOwners[name] = fk;
    }
    for (const name of Object.keys(snap.budgetOwners)) {
      if (feedsKey(snap.budgetOwners[name])) snap.budgetOwners[name] = fk;
    }
    void (async () => {
      const err = await act(async () => {
      const m = await api.createMission({
        objective: form.objective.trim(),
        scope: form.scope.trim(),
        ...(form.room.trim() && form.task.trim()
          ? { room_source: { room_id: form.room.trim(), task_id: form.task.trim() } }
          : {}),
        ...(form.owner ? { owner_agent_id: form.owner } : {}),
        acceptance_criteria: splitLines(form.acceptance),
        deliverable_specs: splitLines(form.deliverables),
        source_refs: splitLines(form.sources),
        ...(form.returns.trim() ? { return_targets: splitLines(form.returns) } : {}),
        ...(form.reviewer.trim() ? { reviewer: form.reviewer.trim() } : {}),
        // 六审基线：用户编辑过预算（即使收起面板）——显式设置不得被缺省覆盖
        ...(budgetOpen || budgetEditedRef.current.size > 0 ? { budget: b } : {}),
      });
      setSelected(m.mission_id);
      setFormOpen(false);
      setForm({ objective: "", scope: "", acceptance: "", deliverables: "", sources: "", returns: "", owner: "", reviewer: "", room: "", task: "" });
      budgetEditedRef.current = new Set();
      confirmedSrcRef.current = null;
      pendingMergesRef.current.clear();
      });
      // 来源已被他处交办（列表陈旧）——保留录入切续接模式（重取映射与版本）
      if (err?.code === "source_already_mapped") {
        // 十一审 #2：以**提交时快照**（发送前冻结的来源/归属/载荷）构建冻结
        // 编辑——等待期间切换任务卡改写的 form/归属不入本次合并。
        // 十二审 #2：按提交键登记（Map）——其它任务卡的详情加载不清除；回到
        // 引发 409 的卡才消费。
        const frz: FrozenDraft = {};
        for (const [name, val] of Object.entries(snap.fields)) {
          if (snap.fieldOwners[name] === fk && val.trim()) frz[name as keyof FrozenDraft] = val;
        }
        const fb: NonNullable<FrozenDraft["budget"]> = {};
        for (const k of ["concurrency", "max_runs", "max_wall_clock_seconds", "max_retries", "max_delegation_depth"] as const) {
          if (snap.budgetOwners[k] === fk) fb[k] = snap.budget[k];
        }
        if (Object.keys(fb).length) frz.budget = fb;
        pendingMergesRef.current.set(fk, frz);
        setRev((r) => r + 1);
        setMsg("该任务卡已被交办——正切换为续接模式：现行契约将与您已填写的字段合并（未填字段与预算留现行值），请核对后重新提交");
      }
    })();
  };

  // 复审 #1/#2/#6：续接提交——目标就绪才可提交（加载/失败禁用）；基准
  // constraints/delegation 随载荷回传（全量替换不清空不可编辑字段）；版本
  // 冲突提供显式重新加载（不自动覆盖编辑中草稿）；稳定幂等键随目标生成
  //（同键同载荷重试重放，不二次修订）。
  const submitUpdate = () => {
    if (!updateTarget || detailState !== "idle") return;
    const b = {
      concurrency: Number(budget.concurrency), max_runs: Number(budget.max_runs),
      max_wall_clock_seconds: Number(budget.max_wall_clock_seconds),
      max_retries: Number(budget.max_retries), max_delegation_depth: Number(budget.max_delegation_depth),
    };
    void (async () => {
      const err = await act(async () => {
        const rc = await api.updateDispatch({
          mission_id: updateTarget.missionID,
          expected_version: updateTarget.version,
          idempotency_key: updateTarget.key,
          reason: "任务卡追加要求",
          objective: form.objective.trim(),
          scope: form.scope.trim(),
          acceptance_criteria: splitLines(form.acceptance),
          deliverable_specs: splitLines(form.deliverables),
          source_refs: splitLines(form.sources),
          ...(form.returns.trim() ? { return_targets: splitLines(form.returns) } : {}),
          constraints: baseContract?.constraints ?? [],
          delegation: baseContract?.delegation ?? { allowed: [], max_depth: 0 },
          ...(form.reviewer.trim() ? { reviewer: form.reviewer.trim() } : {}),
          budget: b,
        });
        setSelected(rc.domain_command_id.split(":")[0]);
        setFormOpen(false);
        setUpdateTarget(null);
        setBaseContract(null);
        setConflictReload(false);
        setForm({ objective: "", scope: "", acceptance: "", deliverables: "", sources: "", returns: "", owner: "", reviewer: "", room: "", task: "" });
      budgetEditedRef.current = new Set();
      confirmedSrcRef.current = null;
      pendingMergesRef.current.clear();
      });
      // 复审 #6：真并发冲突（他处已修订）→ 显式重新加载入口
      if (err?.code === "version_conflict") setConflictReload(true);
    })();
  };

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-4 px-4 py-6">
      <header className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-medium">任务</h1>
          <p className="mt-0.5 text-xs text-faint">
            Mission 契约（ADR-0023 / 0.6.0）：可验收的一次交办——独立事实源，带版本；执行与验收随 0.6.5+。
          </p>
        </div>
        <button
          type="button"
          disabled={busy}
          onClick={() => setFormOpen((o) => !o)}
          className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
        >
          {formOpen ? "收起" : "新建交办"}
        </button>
      </header>

      {msg && <p className="text-xs text-danger">{msg}</p>}

      {formOpen && (
        <section className="flex flex-col gap-2 rounded-xl border border-border bg-surface p-3">
          {detailState === "loading" && (
            <p className="rounded-lg bg-surface-2 px-2.5 py-1.5 text-xs text-faint">
              正在读取现行契约……契约字段暂不可编辑（防读取返回覆盖刚输入的文字），来源切换不受限。
            </p>
          )}
          {updateTarget && (
            <p className="rounded-lg bg-accent-soft px-2.5 py-1.5 text-xs text-accent">
              该任务卡已交办为 {updateTarget.missionID}（v{updateTarget.version}）——本次为<b>追加要求</b>
              （网关续接 mission_update：全量替换契约、负责人不变，换人走交接；同键重试幂等不二次修订）。
            </p>
          )}
          {formOpen && missions && form.room.trim() && form.task.trim() &&
            missions.some((m) => m.room_source?.room_id === form.room.trim() && m.room_source?.task_id === form.task.trim()) &&
            !updateTarget && detailState === "loading" && (
              <p className="rounded-lg bg-surface-2 px-2.5 py-1.5 text-xs text-faint">读取现行契约…（提交待就绪）</p>
            )}
          {formOpen && detailState === "error" && (
            <p className="flex items-center gap-2 rounded-lg bg-[color-mix(in_srgb,var(--danger)_10%,transparent)] px-2.5 py-1.5 text-xs text-danger">
              契约读取失败——续接提交已禁用（防误写不完整契约）
              <button type="button" onClick={() => setRetryTick((t) => t + 1)} className="underline">
                重试
              </button>
            </p>
          )}
          {conflictReload && (
            <p className="flex items-center gap-2 rounded-lg bg-[color-mix(in_srgb,var(--warn)_12%,transparent)] px-2.5 py-1.5 text-xs text-warn">
              提交与他处修订冲突（version_conflict）——重新加载最新契约后再追加（编辑中草稿将被预填覆盖）
              <button
                type="button"
                onClick={() => {
                  setConflictReload(false);
                  setRetryTick((t) => t + 1);
                }}
                className="underline"
              >
                重新加载
              </button>
            </p>
          )}
          <label className="flex flex-col gap-1 text-xs">
            <span className="text-faint">目标（1–512 字）</span>
            <input
              value={form.objective}
              onChange={(e) => setForm({ ...form, objective: e.target.value })}
              disabled={formLocked}
              className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 text-sm outline-none focus:border-accent"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs">
            <span className="text-faint">范围（1–512 字）</span>
            <input
              value={form.scope}
              onChange={(e) => setForm({ ...form, scope: e.target.value })}
              disabled={formLocked}
              className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 text-sm outline-none focus:border-accent"
            />
          </label>
          <div className="grid gap-2 sm:grid-cols-2">
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">验收标准（每行一项；空 = draft）</span>
              <textarea
                value={form.acceptance}
                onChange={(e) => setForm({ ...form, acceptance: e.target.value })}
              disabled={formLocked}
                rows={3}
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">交付物（每行一项；空 = draft）</span>
              <textarea
                value={form.deliverables}
                onChange={(e) => setForm({ ...form, deliverables: e.target.value })}
              disabled={formLocked}
                rows={3}
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">来源引用（每行一项，如 room:abc:seq1；空 = draft）</span>
              <textarea
                value={form.sources}
                onChange={(e) => setForm({ ...form, sources: e.target.value })}
              disabled={formLocked}
                rows={2}
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">返回位置（每行一项，可空）</span>
              <textarea
                value={form.returns}
                onChange={(e) => setForm({ ...form, returns: e.target.value })}
              disabled={formLocked}
                rows={2}
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
          </div>
          <div className="grid gap-2 sm:grid-cols-2">
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">房间来源 room_id（可空——填写后与任务卡唯一映射，同卡不可重复交办）</span>
              <input
                value={form.room}
                onChange={(e) => setForm({ ...form, room: e.target.value })}
                placeholder="如 rm_xxx"
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span className="text-faint">任务卡 task_id（tsk_ 前缀，可空）</span>
              <input
                value={form.task}
                onChange={(e) => setForm({ ...form, task: e.target.value })}
                placeholder="如 tsk_xxx"
                className="rounded-lg border border-border bg-surface-2 px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
              />
            </label>
          </div>
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <label className="flex items-center gap-1">
              <span className="text-faint">负责人</span>
              <select
                value={form.owner}
                onChange={(e) => setForm({ ...form, owner: e.target.value })}
              disabled={formLocked}
                className="rounded border border-border bg-surface-2 px-1.5 py-1 outline-none"
              >
                <option value="">未指派（draft）</option>
                {agents.map((a) => (
                  <option key={a.agent_id} value={a.agent_id}>
                    {a.display_name}
                  </option>
                ))}
              </select>
            </label>
            <label className="flex items-center gap-1">
              <span className="text-faint">验收人（可空 = 请求人自验）</span>
              <input
                value={form.reviewer}
                onChange={(e) => setForm({ ...form, reviewer: e.target.value })}
              disabled={formLocked}
                placeholder="Agent ID 或留空"
                className="w-44 rounded border border-border bg-surface-2 px-1.5 py-1 outline-none"
              />
            </label>
            <button type="button" disabled={formLocked} onClick={() => setBudgetOpen((o) => !o)} className="underline text-faint disabled:opacity-40">
              {budgetOpen ? "收起预算" : "预算（缺省 1/32/一天/不重试/不委派）"}
            </button>
          </div>
          {budgetOpen && (
            <div className="grid grid-cols-2 gap-2 sm:grid-cols-5">
              {(
                [
                  ["concurrency", "并发"],
                  ["max_runs", "总 Run 数"],
                  ["max_wall_clock_seconds", "墙钟(秒)"],
                  ["max_retries", "重试"],
                  ["max_delegation_depth", "委派深度"],
                ] as const
              ).map(([k, label]) => (
                <label key={k} className="flex flex-col gap-1 text-xs">
                  <span className="text-faint">{label}</span>
                  <input
                    value={budget[k]}
                    disabled={formLocked}
                    onChange={(e) => {
                      budgetEditedRef.current.add(k);
                      budgetOwnersRef.current[k] = lastSrcRef.current.room + "|" + lastSrcRef.current.task;
                      setBudget({ ...budget, [k]: e.target.value });
                    }}
                    className="rounded border border-border bg-surface-2 px-1.5 py-1 font-mono text-xs outline-none"
                  />
                </label>
              ))}
            </div>
          )}
          <div>
            <button
              type="button"
              disabled={busy || !form.objective.trim() || !form.scope.trim() || detailState !== "idle"}
              onClick={updateTarget ? submitUpdate : submitCreate}
              className="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-contrast transition-opacity hover:opacity-90 disabled:opacity-40"
            >
              {busy
                ? "提交中…"
                : detailState === "loading"
                  ? "读取契约…"
                  : detailState === "error"
                    ? "契约未就绪（重试读取）"
                    : updateTarget
                      ? `追加要求（续接 v${updateTarget.version}）`
                      : "创建契约"}
            </button>
          </div>
        </section>
      )}

      {listErr ? (
        <p className="text-xs text-danger">
          {listErr}
          <button type="button" onClick={() => setRev((r) => r + 1)} className="ml-2 underline">
            重试
          </button>
        </p>
      ) : missions === null ? (
        <p className="text-xs text-faint">加载中…</p>
      ) : missions.length === 0 ? (
        <p className="text-xs text-faint">尚无交办——新建后在此管理契约与版本。</p>
      ) : (
        <ul className="divide-y divide-border rounded-xl border border-border">
          {missions.map((m) => (
            <li key={m.mission_id} className="flex items-center gap-3 px-3 py-2.5">
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-1.5 text-sm">
                  <span className={statusClass(m.status)}>{m.status}</span>
                  <span className="font-medium">{m.objective}</span>
                  <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim">v{m.contract_version}</span>
                </div>
                <p className="mt-0.5 truncate font-mono text-[11px] text-faint" title={m.mission_id}>
                  {m.mission_id} · {m.owner_agent_id || "unassigned"} · 修订于 {absoluteTime(m.updated_at)}
                </p>
              </div>
              <button
                type="button"
                onClick={() => setSelected(selected === m.mission_id ? null : m.mission_id)}
                className="shrink-0 rounded-lg bg-surface-3 px-2 py-1 text-xs text-text transition-opacity hover:opacity-85"
              >
                {selected === m.mission_id ? "收起" : "详情"}
              </button>
            </li>
          ))}
        </ul>
      )}

      {selected && (
        <MissionDetail key={selected} missionID={selected} agents={agents} onChanged={() => setRev((r) => r + 1)} />
      )}
    </div>
  );
}

/** 契约详情：当前契约 + 修订（CAS）+ 取消 + 版本历史 + 事件流。 */
function MissionDetail({
  missionID,
  agents,
  onChanged,
}: {
  missionID: string;
  agents: { agent_id: string; display_name: string }[];
  onChanged: () => void;
}) {
  const [detail, setDetail] = useState<MissionView | null>(null);
  const [versions, setVersions] = useState<MissionView[] | null>(null);
  const [events, setEvents] = useState<MissionEvent[] | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [reviseOpen, setReviseOpen] = useState(false);
  // 复审 #1：修订草稿基准——打開時固定所见 owner + contract_version；提交锚取
  // 基准值（轮询保鲜的是**显示**，不是用户编辑所依据的事实——草稿携旧 owner 时
  // 锚与当前不符 409，用户重开表单，不得以提交时点轮询值放行旧草稿覆盖新归属）。
  const [reviseBase, setReviseBase] = useState<{ v: number; owner: string } | null>(null);
  const [form, setForm] = useState({ objective: "", scope: "", acceptance: "", deliverables: "", sources: "", owner: "" });
  const [cancelReason, setCancelReason] = useState("");
  const [sendablePkgs, setSendablePkgs] = useState<DeliveryPackageView[]>([]);
  const [pkgTick, setPkgTick] = useState(0); // 复审 R3：交付卡提交后刷新送验列表
  // 0.6.10 回房通知台账（ADR-0033）：详情打开期间 5s 轮询（投递是异步面——
  // pending/held 会随后台 worker 变化）。审校 #4：序号守卫 + 失败保留末次成功
  // 结果（空态与错误态分离，迟到响应不回退已送达）；#9：轮询顺带刷新来源元数据
  // （他页删房后已挂载详情可见墓碑——room_source 局部更新，不打断表单草稿）。
  const [returnNotices, setReturnNotices] = useState<ReturnNoticeView[] | null>(null);
  const [returnErr, setReturnErr] = useState<string | null>(null);
  const [roomSourceNow, setRoomSourceNow] = useState<MissionView["room_source"]>(null);
  const [reviewedPkg, setReviewedPkg] = useState<DeliveryPackageView | null>(null);
  const [reviewPkgErr, setReviewPkgErr] = useState<string | null>(null);
  // 审校 #4：裁定草稿按轮次隔离——新一轮从契约重新初始化（验收人预填 #7、逐项备注、清原因）
  useEffect(() => {
    if (!detail) return;
    setReviewReason("");
    setItemNotes(detail.acceptance_criteria.map(() => ""));
    setReviewReviewer(detail.reviewer || "");
    setReviewOpen(false);
  }, [detail?.review_round, detail?.status, detail?.reviewer, detail?.acceptance_criteria.length]);
  // 审校 #6：ready 时加载当前契约版本可送验包（按选中包送验，不再固定最新）
  useEffect(() => {
    if (!detail || detail.status !== "ready") return;
    // 三审 #1：契约版本切换/列表重载开始即清旧选择与列表——加载慢或失败窗口
    // 内不得持旧包发送（stale_package 由服务端兜底，前端先收口）。
    setSendablePkgs([]);
    setReviewPkgID("");
    let cancelled = false;
    void api
      .listDeliveryPackages(missionID)
      .then(({ packages }) => {
        if (cancelled) return;
        // 复审 R3：只在当前契约版本的包内保留/默认选择
        const cur0 = packages.filter((x) => x.contract_version === detail.contract_version);
        setSendablePkgs(cur0);
        setReviewPkgID(cur0[0]?.package_id ?? "");
      })
      .catch(() => {
        if (!cancelled) {
          setSendablePkgs([]);
          setReviewPkgID("");
        }
      });
    return () => {
      cancelled = true;
    };
  }, [missionID, detail?.status, detail?.updated_at, detail?.contract_version, pkgTick]);
  // 审校 #14：reviewing/accepted 时加载受审包详情（逐项证据可核对）
  // 复审 R4 + 三审 #2：切换受审包先清旧证据与错误；按当前包 ID 记录加载/失败
  // （成功且与本轮包一致才可裁定）；失败后可原地重试——不必退出任务详情。
  const reloadReviewedPkg = () => {
    const pid = detail?.review_package_id;
    setReviewedPkg(null);
    setReviewPkgErr(null);
    if (!pid) return;
    void api
      .getDeliveryPackage(pid)
      .then((pkg) => setReviewedPkg(pkg))
      .catch((e) => setReviewPkgErr(e instanceof Error ? e.message : String(e)));
  };
  useEffect(() => {
    setReviewedPkg(null);
    setReviewPkgErr(null);
    const pid = detail?.review_package_id;
    if (!pid) return;
    let cancelled = false;
    void api
      .getDeliveryPackage(pid)
      .then((pkg) => {
        if (!cancelled) setReviewedPkg(pkg);
      })
      .catch((e) => {
        if (!cancelled) setReviewPkgErr(e instanceof Error ? e.message : String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [detail?.review_package_id]);
  // 裁定可用：本轮包加载成功且与展示一致（R4）
  const reviewEvidenceReady =
    detail?.status === "reviewing" && reviewedPkg !== null && reviewedPkg.package_id === detail?.review_package_id;
  const [reviewOpen, setReviewOpen] = useState(false);
  const [reviewReason, setReviewReason] = useState("");
  const [reviewReviewer, setReviewReviewer] = useState("");
  // 审校 #4/#6/#7/#14：按送验轮次隔离的裁定草稿 + 送验选包 + 受审包详情
  const [itemNotes, setItemNotes] = useState<string[]>([]);
  const [reviewPkgID, setReviewPkgID] = useState("");
  const seq = useRef(0);

  const load = () => {
    const cur = ++seq.current;
    setLoadErr(null);
    void Promise.all([api.getMission(missionID), api.listMissionVersions(missionID), api.listMissionEvents(missionID)])
      .then(([d, v, e]) => {
        if (seq.current !== cur) return; // 只被更新的 load 作废（轮询不动 seq）
        // 三审 P2：detail 应用代次在**响应时**取——载入的详情/版本/事件原子落地，
        // 且必覆盖此前已应用的轮询快照（载入是用户触发的权威读取）。
        const gen = ++detailGenRef.current;
        if (gen < appliedSrcRef.current) return;
        appliedSrcRef.current = gen;
        setDetail(d);
        setVersions(v.versions);
        setEvents(e.events);
      })
      .catch((e) => {
        if (seq.current !== cur) return;
        setLoadErr(e instanceof Error ? e.message : String(e));
      });
  };

  useEffect(load, [missionID]); // 父层按 missionID key 重挂载；onChanged 后手动 load

  // 三审 P2：detail 应用代次——load 与轮询共用单调计数（detailGenRef 取号 /
  // appliedSrcRef 记已应用），**取号源各自独立**（load 守卫只看 seq——轮询取号
  // 不再作废在途载入；复审 #3 的"迟到旧轮询盖回新详情"由应用层代次拦截）。
  const appliedSrcRef = useRef(0);
  const detailGenRef = useRef(0);
  useEffect(() => {
    setRoomSourceNow(detail?.room_source ?? null);
  }, [detail?.room_source]);

  // 回房台账：挂载即取 + 5s 轮询（worker 投递异步推进；详情重挂载/换任务即重置）
  useEffect(() => {
    let alive = true;
    let seqRN = 0;
    let appliedRN = 0;
    const loadRN = () => {
      const rn = ++seqRN;
      // 复审 #3：两请求各自独立更新——台账不被来源挂起挡住、删房标记不被台账
      // 失败挡住；#4：只丢弃**比已显示更旧**的响应——轮询间隔短于单轮耗时的
      // 连续慢响应不再被发起序号全部丢弃（每轮到达的最新结果总会显示）。
      void api
        .listMissionReturnNotices(missionID)
        .then((r) => {
          if (!alive || rn < appliedRN) return;
          appliedRN = rn;
          setReturnErr(null);
          setReturnNotices(r.notices);
        })
        .catch(() => {
          // 四审 #5：忽略早于最新已显示成功的失败（旧失败不得在 delivered 之上
          // 再提示"暂不可用"）；失败保留末次成功结果。
          if (!alive || rn < appliedRN) return;
          setReturnErr("回房台账暂不可用（显示上次结果）");
        });
      // 三审 P2：轮询取号自 detailGenRef（不动 seq——在途载入不被作废）；应用条件
      // ticket >= 已应用代次。0.6.12 审校 #6：同步刷新交接元数据（owner / pending /
      // 状态 / 版本）——他处发起/承接/婉拒/撤回后本页可见待承接入口；表单草稿独立
      // state 不受影响（裁定草稿按轮次隔离既有守卫）。
      const ticket = ++detailGenRef.current;
      void api
        .getMission(missionID)
        .then((m) => {
          if (!alive || ticket < appliedSrcRef.current) return;
          appliedSrcRef.current = ticket;
          setDetail(m);
          setRoomSourceNow(m.room_source ?? null);
        })
        .catch(() => {
          /* 来源元数据刷新失败：保留现有值（下轮再试） */
        });
    };
    loadRN();
    const timer = setInterval(loadRN, 5000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [missionID]);

  const srcNow = roomSourceNow ?? detail?.room_source ?? null;

  // 写入归属守卫（审校 P1#1）：详情加载完成且归属当前任务才允许修订/取消——
  // 迟到响应或异常状态下绝不把写请求发给错误目标
  const owned = detail !== null && detail.mission_id === missionID;

  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setMsg(null);
    try {
      await fn();
      load();
      onChanged();
    } catch (e) {
      setMsg(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  if (loadErr) {
    return (
      <section className="rounded-xl border border-border p-3 text-xs text-danger">
        {loadErr}
        <button type="button" onClick={load} className="ml-2 underline">
          重试
        </button>
      </section>
    );
  }
  if (!detail) {
    return <p className="text-xs text-faint">契约加载中…</p>;
  }

  return (
    <section className="flex flex-col gap-2 rounded-xl border border-border bg-surface p-3 text-xs">
      <div className="flex flex-wrap items-center gap-2">
        <span className={statusClass(detail.status)}>{detail.status}</span>
        <span className="font-mono text-faint">{detail.mission_id}</span>
        <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim">v{detail.contract_version}</span>
        <span className="text-faint">
          负责人：{detail.owner_agent_id || "unassigned"} · 验收人：{detail.reviewer || "请求人自验"}
          {detail.pending_owner_agent_id && (
            <span className="ml-1 rounded bg-surface-3 px-1.5 py-px text-[10px] text-warn"
              title="在途交接（ADR-0035）：owner 不变直至承接；修订/送验/取消冻结，先了结交接">
              ⇄ 待承接 → {detail.pending_owner_agent_id}
            </span>
          )}
          {srcNow && (
            <span
              className="ml-1 rounded bg-surface-3 px-1.5 py-px font-mono text-[10px]"
              title={
                srcNow.deleted_at
                  ? "来源房间已删除（返回目标取消，结果保留——ADR-0033）"
                  : "房间交办来源（创建事实，不可修订）"
              }
            >
              {srcNow.room_id}/{srcNow.task_id}
              {srcNow.deleted_at ? "（房间已删除）" : ""}
            </span>
          )}
        </span>
      </div>
      <p className="text-sm">{detail.objective}</p>
      <p className="text-dim">范围：{detail.scope}</p>
      <div className="grid gap-1 sm:grid-cols-2">
        <p className="text-dim">验收：{detail.acceptance_criteria.join("；") || "（缺失 → draft）"}</p>
        <p className="text-dim">交付：{detail.deliverable_specs.join("；") || "（缺失）"}</p>
        <p className="font-mono text-faint">来源：{detail.source_refs.join("；") || "（缺失）"}</p>
        <p className="font-mono text-faint">
          预算：{detail.budget.concurrency} 并发 / {detail.budget.max_runs} Run / {detail.budget.max_wall_clock_seconds}s / 重试 {detail.budget.max_retries} / 委派 {detail.budget.max_delegation_depth}
        </p>
      </div>
      {msg && <p className="text-danger">{msg}</p>}
      {detail.status === "canceling" && (
        <p className="text-danger">停止进行中——在途 Run 终止后自动完成取消；若长时间停留可再点「续驱停止」，重启后由恢复分派兜底。</p>
      )}
      {detail.status === "paused" && (
        <p className="text-warn">
          已暂停——不进入调度；在途执行已在安全边界停止（Run 记 canceled）。
          {detail.waiting_reason ? "恢复后回到等待核验（有 unknown Run 待处置）。" : "点「恢复」回到 ready 自动调度。"}
        </p>
      )}
      {detail.status === "reviewing" && (
        <p className="text-warn">
          等待验收——受审包 <span className="font-mono">{detail.review_package_id}</span>
          {detail.reviewer ? `（验收人：${detail.reviewer}）` : "（请求人自验）"}。验收期间契约冻结（不可修订/暂停/再交付）。
        </p>
      )}
      {detail.status === "accepted" && (
        <div className="text-ok">
          <p>已验收（终态）——按交付包 <span className="font-mono">{detail.review_package_id}</span> 验收通过；不可修订、取消、再交付或再执行。</p>
          {reviewedPkg && (
            <details className="mt-0.5">
              <summary className="cursor-pointer select-none text-faint text-[11px]">验收所依据的交付包（Run #{reviewedPkg.attempt}）</summary>
              <ul className="mt-0.5 flex flex-col gap-0.5">
                {reviewedPkg.acceptance_mapping.map((m, i) => (
                  <li key={i} className="text-[11px]">
                    <span className="text-dim">{m.criterion}</span> — <span className="text-faint">{blankTrim(m.evidence) || "（无证据）"}</span>
                  </li>
                ))}
              </ul>
            </details>
          )}
        </div>
      )}
      {detail.status === "waiting" && (
        <p className="text-warn">
          等待核验（{detail.waiting_reason || "reconciliation"}）——进程重启时有执行在途、结果未知（Run 记 unknown）。
          不会自动重跑：请在下方执行区核对工作区副作用后处置（已达成 / 未达成 / 放弃），处置后回到调度列。
        </p>
      )}

      {detail.status !== "canceled" && detail.status !== "accepted" && (
        <div className="flex flex-wrap items-center gap-2">
          {detail.status !== "reviewing" && (
          <button
            type="button"
            disabled={busy || !owned}
            onClick={() => {
              setReviseOpen((o) => !o);
              setReviseBase({ v: detail.contract_version, owner: detail.owner_agent_id ?? "" });
              setForm({
                objective: detail.objective,
                scope: detail.scope,
                acceptance: detail.acceptance_criteria.join("\n"),
                deliverables: detail.deliverable_specs.join("\n"),
                sources: detail.source_refs.join("\n"),
                owner: detail.owner_agent_id,
              });
            }}
            className="rounded-lg bg-surface-3 px-2 py-1 disabled:opacity-40"
          >
            {reviseOpen ? "收起修订" : "修订契约"}
          </button>
          )}
          {(detail.status === "ready" || detail.status === "running" || detail.status === "waiting") && (
            <button
              type="button"
              disabled={busy || !owned}
              onClick={() =>
                void act(async () => {
                  if (detail.mission_id !== missionID) throw new Error("详情未就绪，已阻止写入");
                  await api.pauseMission(missionID, detail.contract_version, cancelReason.trim() || undefined);
                })
              }
              className="rounded-lg bg-surface-3 px-2 py-1 disabled:opacity-40"
            >
              暂停
            </button>
          )}
          {detail.status === "ready" && (
            <div className="flex items-center gap-1">
              <select
                value={reviewPkgID}
                onChange={(e) => setReviewPkgID(e.target.value)}
                disabled={sendablePkgs.length === 0}
                className="max-w-56 rounded border border-border bg-surface px-1.5 py-1 text-xs"
                title="选择送验的交付包（当前契约版本）"
              >
                {sendablePkgs.length === 0 && <option value="">（无当前版本交付包）</option>}
                {sendablePkgs.map((x) => (
                  <option key={x.package_id} value={x.package_id}>
                    {x.package_id.slice(0, 12)}… · Run #{x.attempt} · 证据 {x.acceptance_mapping.filter((m) => hasEvidence(m.evidence)).length}/{detail.acceptance_criteria.length}
                  </option>
                ))}
              </select>
              <button
                type="button"
                disabled={busy || !owned || !reviewPkgID}
                onClick={() =>
                  void act(async () => {
                    if (detail.mission_id !== missionID) throw new Error("详情未就绪，已阻止写入");
                    const sel = sendablePkgs.find((x) => x.package_id === reviewPkgID);
                    if (!sel) throw new Error("所选包不在当前契约版本列表——重新选择");
                    if (sel.contract_version !== detail.contract_version) throw new Error("所选包对应旧契约版本——重新选择");
                    await api.requestMissionReview(missionID, detail.contract_version, reviewPkgID);
                  })
                }
                className="rounded-lg bg-surface-3 px-2 py-1 disabled:opacity-40"
                title="按选中交付包提请验收（必需项：成果引用非空 + 每条验收标准有证据——缺项 409 携清单）"
              >
                提请验收
              </button>
            </div>
          )}
          {detail.status === "reviewing" && (
            <button
              type="button"
              onClick={() => setReviewOpen((o) => !o)}
              className="rounded-lg bg-accent px-2 py-1 text-accent-contrast"
            >
              {reviewOpen ? "收起验收" : "验收"}
            </button>
          )}
          {detail.status === "paused" && (
            <button
              type="button"
              disabled={busy || !owned}
              onClick={() =>
                void act(async () => {
                  if (detail.mission_id !== missionID) throw new Error("详情未就绪，已阻止写入");
                  await api.resumeMission(missionID, detail.contract_version);
                })
              }
              className="rounded-lg bg-accent px-2 py-1 text-accent-contrast disabled:opacity-40"
            >
              恢复
            </button>
          )}
          <input
            value={cancelReason}
            onChange={(e) => setCancelReason(e.target.value)}
            placeholder="取消/暂停原因（可选，≤200 字）"
            className="min-w-0 flex-1 rounded border border-border bg-surface-2 px-1.5 py-1 outline-none"
          />
          <button
            type="button"
            disabled={busy || !owned}
            onClick={() =>
              void act(async () => {
                if (detail.mission_id !== missionID) throw new Error("详情未就绪，已阻止写入");
                await api.cancelMission(missionID, detail.contract_version, cancelReason.trim() || undefined);
              })
            }
            className="rounded-lg bg-surface-3 px-2 py-1 text-danger disabled:opacity-40"
          >
            {detail.status === "canceling" ? "续驱停止" : "取消交办"}
          </button>
        </div>
      )}

      {detail.status === "reviewing" && reviewOpen && (
        <div className="flex flex-col gap-2 rounded-lg bg-surface-2 p-2">
          {reviewPkgErr && (
            <p className="text-danger">
              受审包加载失败：{reviewPkgErr}（裁定需本轮证据加载成功）
              <button type="button" onClick={reloadReviewedPkg} className="ml-2 underline text-[11px]">
                重试
              </button>
            </p>
          )}
          {!reviewPkgErr && !reviewEvidenceReady && <p className="text-warn">正在加载本轮受审包证据……</p>}
          {reviewedPkg && (
            <div className="rounded border border-border bg-surface p-2">
              <p className="text-faint">受审包 <span className="font-mono">{reviewedPkg.package_id}</span>（第 {detail.review_round} 轮 · Run #{reviewedPkg.attempt} · gen {reviewedPkg.generation}）</p>
              <p className="font-mono text-[10px] text-faint">{reviewedPkg.artifact_refs.join("；") || "（无成果引用）"}</p>
              <ul className="mt-1 flex flex-col gap-0.5">
                {reviewedPkg.acceptance_mapping.map((m, i) => (
                  <li key={i} className="text-[11px]">
                    <span className="text-dim">{m.criterion}</span>
                    <span className={hasEvidence(m.evidence) ? "text-ok" : "text-warn"}> — {blankTrim(m.evidence) || "（无证据）"}</span>
                  </li>
                ))}
              </ul>
              {reviewedPkg.open_items.length > 0 && <p className="text-warn text-[11px]">未解决：{reviewedPkg.open_items.join("；")}</p>}
            </div>
          )}
          <div className="flex flex-wrap items-center gap-2">
            <input
              value={reviewReviewer}
              onChange={(e) => setReviewReviewer(e.target.value)}
              placeholder={`验收人${detail.reviewer ? `（契约指定：${detail.reviewer}）` : "（契约未指定 = 请求人自验）"}`}
              className="min-w-0 flex-1 rounded border border-border bg-surface px-1.5 py-1 outline-none"
            />
          </div>
          <div className="flex flex-col gap-1">
            <span className="text-faint">逐项依据（可选，随裁定留痕）：</span>
            {detail.acceptance_criteria.map((c, i) => (
              <div key={i} className="flex items-center gap-1.5">
                <span className="w-1/3 truncate text-dim" title={c}>{c}</span>
                <input value={itemNotes[i] ?? ""} onChange={(e) => setItemNotes(itemNotes.map((v, j) => (j === i ? e.target.value : v)))} placeholder="备注（≤500 字）" className="min-w-0 flex-1 rounded border border-border bg-surface px-1.5 py-0.5 outline-none" />
              </div>
            ))}
          </div>
          <textarea
            value={reviewReason}
            onChange={(e) => setReviewReason(e.target.value)}
            placeholder="裁定备注 / 退回原因（退回必填，≤500 字）"
            rows={2}
            className="rounded border border-border bg-surface px-1.5 py-1 outline-none"
          />
          <div className="flex items-center gap-2">
            <button
              type="button"
              disabled={busy || !reviewEvidenceReady}
              onClick={() =>
                void act(async () => {
                  if (detail.mission_id !== missionID) throw new Error("详情未就绪，已阻止写入");
                  await api.reviewMission(missionID, detail.contract_version, {
                    decision: "accepted",
                    reviewRound: detail.review_round,
                    ...(reviewReviewer.trim() ? { reviewer: reviewReviewer.trim() } : {}),
                    ...(reviewReason.trim() ? { reason: reviewReason.trim() } : {}),
                    item_notes: detail.acceptance_criteria
                      .map((c, i) => ({ criterion: c, note: (itemNotes[i] ?? "").trim() }))
                      .filter((x) => x.note),
                  });
                })
              }
              className="rounded-lg bg-surface-3 px-2 py-1 text-ok disabled:opacity-40"
            >
              接受（终态）
            </button>
            <button
              type="button"
              disabled={busy || !reviewEvidenceReady}
              onClick={() =>
                void act(async () => {
                  if (detail.mission_id !== missionID) throw new Error("详情未就绪，已阻止写入");
                  if (!reviewReason.trim()) throw new Error("退回须填写原因");
                  await api.reviewMission(missionID, detail.contract_version, {
                    decision: "returned",
                    reviewRound: detail.review_round,
                    reason: reviewReason.trim(),
                    ...(reviewReviewer.trim() ? { reviewer: reviewReviewer.trim() } : {}),
                    item_notes: detail.acceptance_criteria
                      .map((c, i) => ({ criterion: c, note: (itemNotes[i] ?? "").trim() }))
                      .filter((x) => x.note),
                  });
                })
              }
              className="rounded-lg bg-surface-3 px-2 py-1 text-danger disabled:opacity-40"
            >
              退回（回 ready）
            </button>
            <span className="text-faint">接受 = 不可逆终态；退回后可补证再交付、再送验。</span>
          </div>
        </div>
      )}

      <HandoverSection missionID={missionID} detail={detail} agents={agents} onChanged={() => load()} />
      <ReopenSection missionID={missionID} contractVersion={detail?.contract_version ?? 0} onChanged={() => load()} />
      {(srcNow || (returnNotices ?? []).length > 0) && (
        <div className="flex flex-col gap-1 rounded-lg bg-surface-2 p-2">
          <p className="text-faint">回房（执行结果 → 来源房间 · ADR-0033）</p>
          {srcNow?.deleted_at && (
            <p className="text-warn">来源房间已删除——返回目标已取消；任务结果与来源映射保留（不重跑任务）。</p>
          )}
          {returnErr && <p className="text-warn">{returnErr}</p>}
          {returnNotices === null ? (
            <p className="text-faint">回房台账载入中…</p>
          ) : returnNotices.length === 0 ? (
            <p className="text-faint">暂无回房通知（尚未送验/裁定）。</p>
          ) : (
            <ul className="flex flex-col gap-1">
              {returnNotices.map((n) => (
                <li key={n.notice_id} className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px]">
                  <span className={returnKindCls(n.kind)}>{returnKindLabel(n.kind)}</span>
                  <span className={returnStatusCls(n.status)}>{returnStatusLabel(n.status)}</span>
                  <span className="font-mono text-faint">第 {n.review_round || "-"} 轮</span>
                  {n.room_event_id ? <span className="font-mono text-faint">{n.room_event_id}</span> : null}
                  {n.status === "held" && <span className="text-warn">房间暂停——恢复后投递</span>}
                  {n.status === "failed" && <span className="text-danger">重试耗尽：{n.last_error ?? ""}</span>}
                  {n.status === "canceled" && !srcNow?.deleted_at && (
                    <span className="text-faint">房间已删除</span>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      {reviseOpen && detail.status !== "canceled" && detail.status !== "reviewing" && detail.status !== "accepted" && (
        <div className="flex flex-col gap-2 rounded-lg bg-surface-2 p-2">
          <p className="text-faint">
            全量替换 + 版本 v{detail.contract_version + 1}；基于 v{detail.contract_version}（CAS——并发修订冲突 409 可见）。
          </p>
          <div className="grid gap-2 sm:grid-cols-2">
            <input
              value={form.objective}
              onChange={(e) => setForm({ ...form, objective: e.target.value })}
              placeholder="目标"
              className="rounded border border-border bg-surface px-1.5 py-1 outline-none"
            />
            <input
              value={form.scope}
              onChange={(e) => setForm({ ...form, scope: e.target.value })}
              placeholder="范围"
              className="rounded border border-border bg-surface px-1.5 py-1 outline-none"
            />
            <textarea
              value={form.acceptance}
              onChange={(e) => setForm({ ...form, acceptance: e.target.value })}
              rows={2}
              placeholder="验收标准（每行一项）"
              className="rounded border border-border bg-surface px-1.5 py-1 font-mono outline-none"
            />
            <textarea
              value={form.deliverables}
              onChange={(e) => setForm({ ...form, deliverables: e.target.value })}
              rows={2}
              placeholder="交付物（每行一项）"
              className="rounded border border-border bg-surface px-1.5 py-1 font-mono outline-none"
            />
            <textarea
              value={form.sources}
              onChange={(e) => setForm({ ...form, sources: e.target.value })}
              rows={2}
              placeholder="来源引用（每行一项）"
              className="rounded border border-border bg-surface px-1.5 py-1 font-mono outline-none"
            />
            <select
              value={form.owner}
              onChange={(e) => setForm({ ...form, owner: e.target.value })}
              className="rounded border border-border bg-surface px-1.5 py-1 outline-none"
            >
              <option value="">未指派（draft）</option>
              {agents.map((a) => (
                <option key={a.agent_id} value={a.agent_id}>
                  {a.display_name}
                </option>
              ))}
            </select>
          </div>
          <div>
            <button
              type="button"
              disabled={busy || !owned || !form.objective.trim() || !form.scope.trim() || !reviseBase}
              onClick={() =>
                void act(async () => {
                  if (detail.mission_id !== missionID) throw new Error("详情未就绪，已阻止写入");
                  // 全量替换语义：未编辑字段从当前契约快照保留（审校 P1#2——
                  // 漏传即清空，预算/约束/返回位/验收人/委派不得被缺省重置）
                  await api.reviseMission(missionID, {
                    // 复审 #1：版本与归属锚取**草稿基准**（打开表单时固定）——
                    // 轮询刷新的是显示；旧草稿对不上当前基准时 409 可见，不放行
                    expected_version: reviseBase?.v ?? detail.contract_version,
                    expected_owner_agent_id: reviseBase?.owner ?? detail.owner_agent_id ?? '',
                    ...(form.owner ? { owner_agent_id: form.owner } : {}),
                    objective: form.objective.trim(),
                    scope: form.scope.trim(),
                    acceptance_criteria: splitLines(form.acceptance),
                    deliverable_specs: splitLines(form.deliverables),
                    source_refs: splitLines(form.sources),
                    constraints: detail.constraints,
                    return_targets: detail.return_targets,
                    budget: detail.budget,
                    delegation: detail.delegation,
                    ...(detail.reviewer ? { reviewer: detail.reviewer } : {}),
                  });
                })
              }
              className="rounded-lg bg-accent px-3 py-1.5 text-accent-contrast disabled:opacity-40"
            >
              {busy ? "提交中…" : `提交修订（v${detail.contract_version} → v${detail.contract_version + 1}）`}
            </button>
          </div>
        </div>
      )}

      <details className="mt-1">
        <summary className="cursor-pointer select-none text-faint">
          版本历史{versions === null ? "" : `（${versions.length} 版）`}
        </summary>
        <ul className="mt-1 flex flex-col gap-0.5">
          {(versions ?? []).map((v) => (
            <li key={v.contract_version} className="flex items-center gap-2 text-faint">
              <span className="rounded bg-surface-3 px-1.5 py-px text-[10px]">v{v.contract_version}</span>
              <span className={statusClass(v.status)}>{v.status}</span>
              <span className="truncate">{v.objective}</span>
              <span className="ml-auto shrink-0">{absoluteTime(v.created_at)}</span>
            </li>
          ))}
        </ul>
      </details>

      <details className="mt-1">
        <summary className="cursor-pointer select-none text-faint">
          事件流{events === null ? "" : `（${events.length} 条）`}
        </summary>
        <ul className="mt-1 flex flex-col gap-0.5 font-mono text-[11px]">
          {(events ?? []).map((e) => (
            <li key={e.seq} className="flex items-center gap-2 text-faint">
              <span className="shrink-0">#{e.seq}</span>
              <span className="shrink-0 text-dim">{e.event_type}</span>
              <span className="truncate">{JSON.stringify(e.payload)}</span>
              <span className="ml-auto shrink-0">{absoluteTime(e.occurred_at)}</span>
            </li>
          ))}
        </ul>
      </details>

      {/* 审校 P7：取消后仍渲染（只读——canceled 时仅禁新装配，历史/层内容可查） */}
      <ExecContextCard missionID={missionID} ownerAgentID={detail.owner_agent_id} canceled={detail.status === "canceled" || detail.status === "accepted"} />

      {/* 0.6.5 执行区：手动调度 + Run 历史（ADR-0028） */}
      <MissionRunCard missionID={missionID} status={detail.status} onChanged={() => load()} />

      {/* 0.6.8 交付区：submit_result 不自动送验（ADR-0031） */}
      <MissionDeliveryCard missionID={missionID} detail={detail} onChanged={() => load()} onPackagesChanged={() => setPkgTick((x) => x + 1)} />
    </section>
  );
}

/** 交付区（0.6.8 / ADR-0031）：当前交付包摘要 + 历史 + 提交表单。提交对
 *  Mission 状态零副作用——"执行成功 / 成果提交 / 验收通过"三事实分离。 */
function MissionDeliveryCard({ missionID, detail, onChanged, onPackagesChanged }: { missionID: string; detail: MissionView; onChanged: () => void; onPackagesChanged: () => void }) {
  const [packages, setPackages] = useState<DeliveryPackageView[] | null>(null);
  const [runs, setRuns] = useState<MissionRunView[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [formOpen, setFormOpen] = useState(false);
  const [runID, setRunID] = useState("");
  const [refs, setRefs] = useState("");
  const [evidence, setEvidence] = useState<string[]>([]);
  const [verification, setVerification] = useState("");
  const [openItems, setOpenItems] = useState("");
  const [risks, setRisks] = useState("");
  const [notes, setNotes] = useState("");
  const seq = useRef(0);

  const load = () => {
    const cur = ++seq.current;
    setErr(null);
    void Promise.all([api.listDeliveryPackages(missionID), api.listMissionRuns(missionID)])
      .then(([p, r]) => {
        if (seq.current !== cur) return;
        setPackages(p.packages);
        setRuns(r.runs);
      })
      .catch((e) => {
        if (seq.current !== cur) return;
        setErr(e instanceof Error ? e.message : String(e));
      });
  };
  useEffect(load, [missionID]);
  // 审校 #4：可交付 Run 资格列表须随执行卡进度刷新——10s 轮询（后台调度器完成/
  // 核验处置落地）+ 详情变化即刻重载（手动执行/核验经父层 onChanged 更新
  // detail）。只刷新 packages/runs，不触碰表单草稿（refs/evidence/… 独立 state）。
  useEffect(() => {
    const timer = setInterval(load, 10_000);
    return () => clearInterval(timer);
  }, [missionID]);
  useEffect(load, [detail.updated_at, detail.status]);
  useEffect(() => {
    setEvidence(detail.acceptance_criteria.map(() => ""));
  }, [detail.contract_version, detail.acceptance_criteria.length]);

  // 可交付 Run：completed，或 unknown 且处置 verified_completed（ADR-0030）
  const deliverable = runs.filter((r) => r.state === "completed" || (r.state === "unknown" && r.resolution === "verified_completed"));
  const latest = packages && packages.length > 0 ? packages[0] : null;
  const canceled = detail.status === "canceled";
  // 0.6.9：验收期间受审包固定、accepted 终态——提交入口关闭（摘要/历史照常）
  const submitLocked = canceled || detail.status === "reviewing" || detail.status === "accepted";

  const submit = async () => {
    const run = deliverable.find((r) => r.run_id === runID);
    if (!run) {
      setErr("请选择可交付的 Run");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await api.submitDeliveryPackage(missionID, {
        run_id: run.run_id,
        generation: run.generation ?? 1,
        expected_version: detail.contract_version,
        artifact_refs: splitLines(refs),
        acceptance_mapping: detail.acceptance_criteria.map((c, i) => ({ criterion: c, evidence: evidence[i] ?? "" })),
        verification_evidence: verification.trim() || undefined,
        open_items: splitLines(openItems),
        risks: splitLines(risks),
        notes: notes.trim() || undefined,
      });
      setFormOpen(false);
      setRefs("");
      setVerification("");
      setOpenItems("");
      setRisks("");
      setNotes("");
      setEvidence(detail.acceptance_criteria.map(() => ""));
      load();
      onChanged();
      onPackagesChanged(); // 复审 R3：新包立即可送验（刷新父层包列表）
    } catch (e) {
      setErr(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const coverage = (p: DeliveryPackageView) => `${p.acceptance_mapping.filter((m) => hasEvidence(m.evidence)).length}/${p.acceptance_mapping.length}`;

  return (
    <details className="mt-1">
      <summary className="cursor-pointer select-none text-faint">
        交付{packages === null ? "" : packages.length === 0 ? "（未提交）" : `（${packages.length} 次提交）`}
      </summary>
      <div className="mt-1 flex flex-col gap-2 text-xs">
        {err && <p className="text-danger">{err}</p>}
        {latest ? (
          <div className="rounded-lg bg-surface-2 p-2">
            <p>
              <span className="text-ok">当前交付包</span> <span className="font-mono text-faint">{latest.package_id}</span>
              <span className="ml-2 text-faint">来源 Run #{latest.attempt}（gen {latest.generation}）· 契约 v{latest.contract_version} · {absoluteTime(latest.submitted_at)}</span>
            </p>
            <p className="text-dim">
              成果引用 {latest.artifact_refs.length} 项 · 验收对应 {coverage(latest)} 有证据 · 未解决 {latest.open_items.length} · 风险 {latest.risks.length}
            </p>
            {latest.artifact_refs.length > 0 && <p className="font-mono text-faint">{latest.artifact_refs.join("；")}</p>}
            <p className="text-warn">已提交成果——尚未送验（request_review / 验收随 0.6.9）。</p>
          </div>
        ) : (
          packages !== null && <p className="text-faint">尚未提交交付包——执行成功 ≠ 成果提交 ≠ 验收通过（三个独立事实）。</p>
        )}
        {!submitLocked && (
          <button type="button" onClick={() => setFormOpen((o) => !o)} className="self-start rounded-lg bg-surface-3 px-2 py-1 disabled:opacity-40" disabled={deliverable.length === 0}>
            {formOpen ? "收起提交" : latest ? "补证 / 再提交" : "提交交付包"}
          </button>
        )}
        {!submitLocked && deliverable.length === 0 && <span className="text-faint">无可交付 Run（需 completed 或 unknown 已核验达成）。</span>}
        {submitLocked && detail.status !== "canceled" && (
          <span className="text-faint">{detail.status === "reviewing" ? "验收中——受审包已固定，退回后再补证。" : "已验收（终态）——不再受理交付。"}</span>
        )}
        {formOpen && !submitLocked && (
          <div className="flex flex-col gap-1.5 rounded-lg bg-surface-2 p-2">
            <select value={runID} onChange={(e) => setRunID(e.target.value)} className="rounded border border-border bg-surface px-1.5 py-1">
              <option value="">选择来源 Run</option>
              {deliverable.map((r) => (
                <option key={r.run_id} value={r.run_id}>
                  #{r.attempt} {r.state}{r.resolution ? `（${r.resolution}）` : ""} · gen {r.generation ?? "?"}
                </option>
              ))}
            </select>
            <textarea value={refs} onChange={(e) => setRefs(e.target.value)} placeholder="成果引用（每行一项：工作区路径 / 文档引用 / URL；≤16 项）" rows={2} className="rounded border border-border bg-surface px-1.5 py-1 outline-none" />
            <p className="text-faint">验收逐项对应（证据可留空——先交付再补证；缺项由送验时拒绝）：</p>
            {detail.acceptance_criteria.map((c, i) => (
              <div key={i} className="flex items-center gap-1.5">
                <span className="w-1/3 truncate text-dim" title={c}>{c}</span>
                <input value={evidence[i] ?? ""} onChange={(e) => setEvidence(evidence.map((v, j) => (j === i ? e.target.value : v)))} placeholder="证据 / 说明（≤1000 字）" className="min-w-0 flex-1 rounded border border-border bg-surface px-1.5 py-0.5 outline-none" />
              </div>
            ))}
            <textarea value={verification} onChange={(e) => setVerification(e.target.value)} placeholder="验证证据（负责人验证过程——不能仅拼接执行回复；≤4000 字）" rows={2} className="rounded border border-border bg-surface px-1.5 py-1 outline-none" />
            <div className="grid gap-1.5 sm:grid-cols-2">
              <textarea value={openItems} onChange={(e) => setOpenItems(e.target.value)} placeholder="未解决项（每行一项，≤16）" rows={2} className="rounded border border-border bg-surface px-1.5 py-1 outline-none" />
              <textarea value={risks} onChange={(e) => setRisks(e.target.value)} placeholder="风险（每行一项，≤8）" rows={2} className="rounded border border-border bg-surface px-1.5 py-1 outline-none" />
            </div>
            <input value={notes} onChange={(e) => setNotes(e.target.value)} placeholder="备注（可选，≤1000 字）" className="rounded border border-border bg-surface px-1.5 py-1 outline-none" />
            <div className="flex items-center gap-2">
              <button type="button" disabled={busy || !runID} onClick={() => void submit()} className="rounded-lg bg-accent px-3 py-1.5 text-accent-contrast disabled:opacity-40">
                {busy ? "提交中…" : "提交（不自动送验）"}
              </button>
              <span className="text-faint">基于契约 v{detail.contract_version}；提交不改变任务状态。</span>
            </div>
          </div>
        )}
        {packages !== null && packages.length > 1 && (
          <details>
            <summary className="cursor-pointer select-none text-faint">历史提交（{packages.length - 1}）</summary>
            <ul className="mt-1 flex flex-col gap-0.5">
              {packages.slice(1).map((p) => (
                <li key={p.package_id} className="font-mono text-[10px] text-faint">
                  {p.package_id} · Run #{p.attempt} · 验收对应 {coverage(p)} · {absoluteTime(p.submitted_at)}
                </li>
              ))}
            </ul>
          </details>
        )}
      </div>
    </details>
  );
}

/** 执行区（0.6.5 / ADR-0028）：手动调度一次执行 + Run 历史。 */
function MissionRunCard({ missionID, status, onChanged }: { missionID: string; status: string; onChanged: () => void }) {
  const [runs, setRuns] = useState<MissionRunView[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);
  const seq = useRef(0);

  const load = () => {
    const cur = ++seq.current;
    setErr(null);
    void api
      .listMissionRuns(missionID)
      .then((r) => {
        if (seq.current !== cur) return;
        setRuns(r.runs);
      })
      .catch((e) => {
        if (seq.current !== cur) return;
        setErr(e instanceof Error ? e.message : String(e));
      });
  };
  useEffect(load, [missionID]);
  // #11：后台（调度器）创建/完成 Run 后同步刷新——10s 轮询
  useEffect(() => {
    const timer = setInterval(load, 10_000);
    return () => clearInterval(timer);
  }, [missionID]);

  const execute = async () => {
    setBusy(true);
    setErr(null);
    try {
      await api.createMissionRun(missionID);
      load();
      onChanged();
    } catch (e) {
      setErr(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const stateClass = (s: string) =>
    s === "completed" ? "text-ok" : s === "failed" ? "text-danger" : s === "canceled" ? "text-dim line-through" : s === "unknown" ? "text-warn font-semibold" : "text-warn";

  // 0.6.7 / ADR-0030：unknown 核验处置（状态保持 unknown；处置后 Mission 回调度列）
  const [resolveNote, setResolveNote] = useState("");
  const resolve = async (runID: string, outcome: "verified_completed" | "verified_failed" | "discarded") => {
    setBusy(true);
    setErr(null);
    try {
      await api.resolveMissionRun(runID, outcome, resolveNote.trim() || undefined);
      setResolveNote("");
      load();
      onChanged();
    } catch (e) {
      setErr(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <details className="mt-1">
      <summary className="cursor-pointer select-none text-faint">
        执行{runs === null ? "" : `（${runs.length} 次尝试）`}
      </summary>
      <div className="mt-1 flex flex-col gap-2">
        {err && <p className="text-danger">{err}</p>}
        {status === "ready" && (
          <div className="flex items-center gap-2 text-xs">
            <button
              type="button"
              disabled={busy}
              onClick={() => void execute()}
              className="rounded-lg bg-accent px-3 py-1.5 text-accent-contrast disabled:opacity-40"
            >
              {busy ? "执行中…" : "执行一次"}
            </button>
            <span className="text-faint">领取租约 → 装配清单 → 提交执行 → 围栏核对 → 终态释放（周期调度也在后台运行）</span>
          </div>
        )}
        {runs !== null && runs.length > 0 && (
          <ul className="flex flex-col gap-0.5">
            {runs.map((r) => (
              <li key={r.run_id} className="flex flex-wrap items-center gap-2 text-xs text-faint">
                <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim">#{r.attempt}</span>
                <span className={stateClass(r.state)}>{r.state}</span>
                {r.result_block && <span className="truncate" title={r.result_block}>{r.result_block.slice(0, 80)}…</span>}
                {r.error_text && <span className="text-danger truncate" title={r.error_text}>{r.error_text.slice(0, 60)}</span>}
                {r.state === "unknown" && r.resolution && (
                  <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim" title={r.resolution_note ?? ""}>
                    已处置：{r.resolution}
                  </span>
                )}
                {r.state === "unknown" && !r.resolution && (
                  <div className="flex w-full flex-wrap items-center gap-1.5">
                    <span className="text-warn">结果未知——核对工作区副作用后处置：</span>
                    <input
                      value={resolveNote}
                      onChange={(e) => setResolveNote(e.target.value)}
                      placeholder="备注（可选，≤500 字）"
                      className="min-w-0 flex-1 rounded border border-border bg-surface-2 px-1.5 py-0.5 outline-none"
                    />
                    <button type="button" disabled={busy} onClick={() => void resolve(r.run_id, "verified_completed")} className="rounded-lg bg-surface-3 px-2 py-0.5 text-ok disabled:opacity-40">
                      已达成
                    </button>
                    <button type="button" disabled={busy} onClick={() => void resolve(r.run_id, "verified_failed")} className="rounded-lg bg-surface-3 px-2 py-0.5 text-danger disabled:opacity-40">
                      未达成
                    </button>
                    <button type="button" disabled={busy} onClick={() => void resolve(r.run_id, "discarded")} className="rounded-lg bg-surface-3 px-2 py-0.5 disabled:opacity-40">
                      放弃
                    </button>
                  </div>
                )}
                <button
                  type="button"
                  onClick={() => setExpanded(expanded === r.run_id ? null : r.run_id)}
                  className="ml-auto shrink-0 underline text-[10px]"
                >
                  {expanded === r.run_id ? "收起" : "详情"}
                </button>
                {expanded === r.run_id && (
                  <div className="w-full font-mono text-[10px] text-dim">
                    <p>run: {r.run_id} · 租约 {r.lease_id} · 清单 {r.manifest_id}</p>
                    <p>围栏 gen {r.generation ?? "?"} · {r.fence_token.slice(0, 16)}… · {absoluteTime(r.requested_at)}</p>
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
        {runs !== null && runs.length === 0 && <p className="text-faint">尚无执行——点「执行一次」调度（或等待周期扫描）。</p>}
      </div>
    </details>
  );
}

/** 执行上下文清单（0.6.2 / ADR-0025）：选任职装配存档、层账本、装配历史。 */
function ExecContextCard({ missionID, ownerAgentID, canceled }: { missionID: string; ownerAgentID: string; canceled: boolean }) {
  const [manifests, setManifests] = useState<ContextManifestSummary[] | null>(null);
  const [instances, setInstances] = useState<SlotInstanceView[]>([]);
  const [bindings, setBindings] = useState<BindingView[]>([]);
  const [bindingID, setBindingID] = useState("");
  const [result, setResult] = useState<ContextManifestView | null>(null);
  const [replayed, setReplayed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const seq = useRef(0);
  const resultSeq = useRef(0); // 结果请求序号（审校 P3）：装配/历史查看共用——只接纳最新响应

  const load = () => {
    const cur = ++seq.current;
    setErr(null);
    void Promise.all([api.listContextManifests(missionID), api.listSlotInstances(), api.listSlotBindings()])
      .then(([m, i, b]) => {
        if (seq.current !== cur) return;
        setManifests(m.manifests);
        setInstances(i.instances);
        setBindings(b.bindings);
      })
      .catch((e) => {
        if (seq.current !== cur) return;
        setErr(e instanceof Error ? e.message : String(e));
      });
  };
  useEffect(load, [missionID]); // 父层按 missionID key 重挂载
  // 负责人变化清空任职选择（审校 P2）：旧 owner 的选择残留不可提交
  useEffect(() => {
    setBindingID("");
  }, [ownerAgentID]);

  // 执行者须为契约负责人（owner_mismatch 只是 blocked 预演）——默认只列
  // owner 的任职，便于直接选对；无 owner 时提示先指派。
  const owned = bindings.filter((b) => ownerAgentID !== "" && b.agent_id === ownerAgentID);
  const instTitle = (id: string) => instances.find((x) => x.slot_instance_id === id)?.title ?? id;

  const build = async (id: string) => {
    // 提交前核对任职仍属于当前负责人（审校 P2）：迟到列表/并发修订防护
    if (!owned.some((b) => b.binding_id === id)) {
      setErr("所选任职已不属于当前负责人——请重新选择");
      setBindingID("");
      return;
    }
    setBusy(true);
    setErr(null);
    const cur = ++resultSeq.current; // 出发取号（审校 P3）
    try {
      const r = await api.buildContextManifest(missionID, id);
      if (resultSeq.current !== cur) return; // 过期响应弃用
      setResult(r.manifest);
      setReplayed(r.replayed);
      load();
    } catch (e) {
      if (resultSeq.current !== cur) return;
      setErr(e instanceof ApiError ? `${e.code}：${e.message}` : e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const viewHistory = (manifestID: string) => {
    const cur = ++resultSeq.current; // 与装配共用结果序号（审校 P3）
    void api
      .getContextManifest(manifestID)
      .then((v) => {
        if (resultSeq.current !== cur) return; // 迟到响应不覆盖最新查看/装配结果
        setResult(v);
        setReplayed(false);
      })
      .catch((e) => {
        if (resultSeq.current !== cur) return;
        setErr(e instanceof Error ? e.message : String(e));
      });
  };

  return (
    <details className="mt-1">
      <summary className="cursor-pointer select-none text-faint">
        执行上下文{manifests === null ? "" : `（${manifests.length} 份清单）`}
      </summary>
      <div className="mt-1 flex flex-col gap-2">
        {err && <p className="text-danger">{err}</p>}
        {canceled ? (
          // 审校 P7：取消后保留只读卡——既有层内容与历史仍可查（数据保留，
          // API 仍可读），仅不能再装配（后端 409 mission_canceled）
          <p className="text-faint">任务已取消——不能再装配；既有清单与层内容仍可查看。</p>
        ) : ownerAgentID === "" ? (
          <p className="text-faint">未指派负责人——先修订契约指派 owner 再装配。</p>
        ) : (
          <div className="flex flex-wrap items-center gap-2">
            <select
              value={bindingID}
              onChange={(e) => setBindingID(e.target.value)}
              className="min-w-0 max-w-full rounded border border-border bg-surface-2 px-1.5 py-1 outline-none"
            >
              <option value="">选择负责人任职（{owned.length}）</option>
              {owned.map((b) => (
                <option key={b.binding_id} value={b.binding_id}>
                  {instTitle(b.slot_instance_id)} · {b.status} · v{b.slot_spec_version}
                </option>
              ))}
            </select>
            <button
              type="button"
              disabled={busy || bindingID === ""}
              onClick={() => void build(bindingID)}
              className="rounded-lg bg-accent px-2.5 py-1 text-accent-contrast disabled:opacity-40"
            >
              {busy ? "装配中…" : "装配清单"}
            </button>
            <span className="text-faint">
              确定性装配存档——同状态重建重放原清单（digest 即身份）；执行随 0.6.5。
            </span>
          </div>
        )}

        {result && (
          <div className="flex flex-col gap-1 rounded-lg bg-surface-2 p-2">
            <div className="flex flex-wrap items-center gap-2">
              <span className={result.state === "assembled" ? "text-ok" : "text-warn"}>{result.state}</span>
              {result.reason_code && <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-warn">{result.reason_code}</span>}
              {replayed && <span className="rounded bg-surface-3 px-1.5 py-px text-[10px] text-dim">重放（同状态重建）</span>}
              <span className="font-mono text-[10px] text-faint" title={result.digest}>
                {result.digest.slice(0, 16)}…
              </span>
              <span className="text-faint">
                契约 v{result.contract_version} · {(result.total_bytes / 1024).toFixed(1)} KiB · {result.policy_version}
              </span>
              {result.watermarks.map((w) => (
                <span key={w.stream} className="rounded bg-surface-3 px-1.5 py-px font-mono text-[10px] text-dim">
                  {w.stream}@{w.position}
                </span>
              ))}
            </div>
            <ul className="flex flex-col gap-0.5 font-mono text-[11px]">
              {result.layers.map((l) => (
                <li key={l.name} className="flex flex-wrap items-center gap-2 text-faint">
                  <span className="w-36 shrink-0 text-dim">{l.name}</span>
                  <span className={l.state === "included" ? "text-ok" : "text-warn"}>{l.state}</span>
                  <span>{l.role}</span>
                  {l.state === "included" && <span>{l.bytes} B</span>}
                  {l.reason_code && <span className="text-warn">{l.reason_code}</span>}
                  {l.dropped ? <span>裁 {l.dropped} 条</span> : null}
                  {l.truncated_messages ? <span>截 {l.truncated_messages} 条</span> : null}
                  {l.content && (
                    <details className="ml-auto">
                      <summary className="cursor-pointer select-none text-[10px]">内容块</summary>
                      <pre className="mt-1 max-h-48 max-w-full overflow-auto rounded bg-surface-3 p-1.5 text-[10px]">
                        {JSON.stringify(l.content, null, 2)}
                      </pre>
                    </details>
                  )}
                </li>
              ))}
            </ul>
          </div>
        )}

        {manifests !== null && manifests.length > 0 && (
          <ul className="flex flex-col gap-0.5">
            {manifests.map((m) => (
              <li key={m.manifest_id} className="flex items-center gap-2 text-faint">
                <button
                  type="button"
                  onClick={() => viewHistory(m.manifest_id)}
                  className="shrink-0 rounded bg-surface-3 px-1.5 py-px text-[10px] hover:opacity-85"
                >
                  查看
                </button>
                <span className={m.state === "assembled" ? "text-ok" : "text-warn"}>{m.state}</span>
                {m.reason_code && <span className="text-warn">{m.reason_code}</span>}
                <span className="font-mono text-[10px]" title={m.digest}>
                  {m.digest.slice(0, 12)}…
                </span>
                <span>v{m.contract_version}</span>
                <span className="ml-auto shrink-0">{absoluteTime(m.created_at)}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </details>
  );
}


// 0.6.11 决策重开请求区（ADR-0034）：提交表单 + 历史列表（任何状态可发起——
// accepted 终态的新证据重开正是典型场景；不翻转状态）。
function ReopenSection({ missionID, contractVersion, onChanged }: {
  missionID: string; contractVersion: number; onChanged: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [decisionRef, setDecisionRef] = useState("");
  const [evidence, setEvidence] = useState("");
  const [affected, setAffected] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [history, setHistory] = useState<ReopenRequestView[] | null>(null);

  const [histErr, setHistErr] = useState(false); // 二审 #4：首次/重试失败可见可重试
  const [histTick, setHistTick] = useState(0); // 0.6.12 审校 #5：重试须独立刷新计数（msg 已空时 setMsg(null) 不触发重查）
  useEffect(() => {
    let alive = true;
    let seq = 0;
    let applied = 0;
    const load = () => {
      const cur = ++seq;
      void api
        .listReopenRequests(missionID)
        .then((r) => {
          if (!alive || cur < applied) return; // 审校 #5：只丢比已显示更旧的响应
          applied = cur;
          setHistErr(false);
          setHistory(r.requests);
        })
        .catch(() => {
          if (!alive || cur < applied) return;
          setHistErr(true); // 二审 #4：错误可见（不静默清空成功结果）
        });
    };
    load();
    return () => {
      alive = false;
    };
  }, [missionID, msg, histTick]);

  const submit = async () => {
    setBusy(true); setMsg(null);
    try {
      const out = await api.submitReopenRequest(missionID, {
        expected_version: contractVersion,
        decision_ref: decisionRef.trim(),
        evidence_refs: evidence.split(/\n/).map((x) => x.trim()).filter(Boolean),
        ...(affected.trim() ? { affected_mission_ids: affected.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean) } : {}),
        ...(reason.trim() ? { reason: reason.trim() } : {}),
      });
      const parts = (out.affected ?? []).map((a) => `${a.mission_id}: ${a.result}${a.detail ? `（${a.detail}）` : ""}`).join("；");
      setMsg(`已记录 ${out.request_id}${parts ? `（${parts}）` : ""}`);
      setDecisionRef(""); setEvidence(""); setAffected(""); setReason(""); setOpen(false);
      onChanged();
    } catch (e) {
      setMsg(e instanceof ApiError ? `${e.code}：${e.message}` : String(e));
    } finally { setBusy(false); }
  };

  return (
    <div className="flex flex-col gap-1 rounded-lg bg-surface-2 p-2">
      <div className="flex items-center gap-2">
        <p className="text-faint">决策重开（reopen_request）</p>
        {(history?.length ?? 0) > 0 && <span className="text-faint text-[11px]">{history!.length} 条历史</span>}
        <button onClick={() => setOpen(!open)} disabled={busy}
          className="ml-auto rounded-lg bg-surface-3 px-2 py-1 text-xs text-warn disabled:opacity-40">
          {open ? "收起" : "提请重开"}
        </button>
      </div>
      {open && (
        <div className="flex flex-col gap-1.5">
          <input value={decisionRef} onChange={(e) => setDecisionRef(e.target.value)}
            placeholder="受影响决定引用（如 closure:room:seq42——必填）"
            className="rounded border border-border bg-surface px-1.5 py-1 text-sm outline-none" />
          <textarea value={evidence} onChange={(e) => setEvidence(e.target.value)} rows={2}
            placeholder="证据引用（每行一项——至少 1 条）"
            className="rounded border border-border bg-surface px-1.5 py-1 font-mono text-xs outline-none" />
          <textarea value={affected} onChange={(e) => setAffected(e.target.value)} rows={2}
            placeholder="需暂停的工作项任务 ID（空格/逗号/换行分隔——可空）"
            className="rounded border border-border bg-surface px-1.5 py-1 font-mono text-xs outline-none" />
          <textarea value={reason} onChange={(e) => setReason(e.target.value)} rows={2}
            placeholder="原因（≤280 字）"
            className="rounded border border-border bg-surface px-1.5 py-1 text-xs outline-none" />
          <button onClick={submit} disabled={busy || !decisionRef.trim() || !evidence.trim()}
            className="rounded-lg bg-surface-3 px-2 py-1 text-xs text-warn disabled:opacity-40">
            {busy ? "提交中…" : `提交重开（契约 v${contractVersion}）`}
          </button>
        </div>
      )}
      {msg && <p className="text-[11px] text-dim">{msg}</p>}
      {histErr && (
        <button onClick={() => setHistTick((t) => t + 1)} className="self-start text-[11px] text-warn underline">
          重开历史加载失败——点击重试
        </button>
      )}
      {(history?.length ?? 0) > 0 && (
        <details>
          <summary className="cursor-pointer select-none text-faint text-[11px]">重开历史</summary>
          <ul className="mt-0.5 flex flex-col gap-0.5">
            {history!.map((r) => (
              <li key={r.request_id} className="flex flex-wrap items-center gap-x-2 text-[11px]">
                <span className="font-mono text-faint">{r.request_id}</span>
                <span className="text-warn">{r.decision_ref}</span>
                <span className="text-faint">v{r.contract_version} · {new Date(r.occurred_at).toLocaleString()}</span>
                {r.affected_mission_ids.length > 0 && (
                  <span className="text-faint">受影响 {r.affected_mission_ids.length} 项</span>
                )}
                {r.reason && <span className="text-dim truncate">{r.reason}</span>}
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>
  );
}

// HandoverSection 责任交接（0.6.12 / ADR-0035）：发起（承接人 + 交接说明；owner 不变、
// pending 置位）→ 承接（owner 翻转 + 回执）| 婉拒 | 撤回；历史读面（含承接回执）。
// 个人版 owner 代行双方动作（身份域无 per-agent 凭证）。
const HANDOVER_OFFERABLE = new Set(["draft", "ready", "running", "waiting", "paused"]);
const HANDOVER_KIND_LABEL: Record<string, string> = { offered: "发起", accepted: "承接回执", declined: "婉拒", revoked: "撤回" };

function HandoverSection({ missionID, detail, agents, onChanged }: {
  missionID: string;
  detail: MissionView;
  agents: { agent_id: string; display_name: string }[];
  onChanged: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [newOwner, setNewOwner] = useState("");
  const [notes, setNotes] = useState("");
  const [text, setText] = useState(""); // 承接备注 / 婉拒·撤回原因（≤280）
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [history, setHistory] = useState<HandoverEventView[] | null>(null);
  const [histErr, setHistErr] = useState(false);
  const [histTick, setHistTick] = useState(0); // 审校 #5：重试按钮独立刷新计数
  // 三审 P3-1：历史资料对应 offerID——刷新中/失败窗口不显示旧 offer 说明，明确
  // 标识当前资料状态（同人所换 offer 的慢请求不冒充新资料）。
  const [histFor, setHistFor] = useState<string | null>(null);
  const pending = detail.pending_owner_agent_id;
  const offerID = detail.pending_offer_id ?? "";
  const version = detail.contract_version;
  // 复审 #4：备注草稿按发起隔离——同人重新邀请（pending 不变、offer_id 已换）时
  // 旧备注不带进新 offer。
  useEffect(() => {
    setText("");
  }, [offerID]);

  useEffect(() => {
    let alive = true;
    let seq = 0;
    let applied = 0;
    const load = () => {
      const cur = ++seq;
      void api
        .listMissionHandovers(missionID)
        .then((r) => {
          if (!alive || cur < applied) return; // 只丢比已显示更旧的响应
          applied = cur;
          setHistErr(false);
          setHistory(r.events);
          setHistFor(offerID); // 三审 P3-1：资料就绪代次——与当前在途发起对齐才显示
        })
        .catch(() => {
          if (!alive || cur < applied) return;
          setHistErr(true); // 错误可见（不静默清空成功结果）
        });
    };
    load();
    return () => {
      alive = false;
    };
  }, [missionID, msg, offerID, histTick]); // 复审 #4：按 offer_id 刷新（pending 不变而 offer 已换时不显示旧说明）

  const run = async (label: string, fn: () => Promise<MissionView>) => {
    setBusy(true); setMsg(null);
    try {
      const m = await fn();
      setMsg(`${label}完成（负责人 ${m.owner_agent_id || "unassigned"}${m.pending_owner_agent_id ? `，待承接 → ${m.pending_owner_agent_id}` : ""}）`);
      setNewOwner(""); setNotes(""); setText(""); setOpen(false);
      onChanged();
    } catch (e) {
      setMsg(e instanceof ApiError ? `${e.code}：${e.message}` : String(e));
    } finally { setBusy(false); }
  };

  const canOffer = HANDOVER_OFFERABLE.has(detail.status) && !pending;
  const candidates = agents.filter((a) => a.agent_id !== detail.owner_agent_id);
  const nameOf = (id: string) => agents.find((a) => a.agent_id === id)?.display_name ?? id;

  return (
    <div className="flex flex-col gap-1 rounded-lg bg-surface-2 p-2">
      <div className="flex items-center gap-2">
        <p className="text-faint">责任交接（handover）</p>
        {(history?.length ?? 0) > 0 && <span className="text-faint text-[11px]">{history!.length} 条历史</span>}
        {!pending && (
          <button onClick={() => setOpen(!open)} disabled={busy || !canOffer}
            title={canOffer ? "" : "仅 draft/ready/running/waiting/paused 可发起（reviewing 先退回或裁定；canceling 先收尾；终态无物可交）"}
            className="ml-auto rounded-lg bg-surface-3 px-2 py-1 text-xs text-accent disabled:opacity-40">
            {open ? "收起" : "发起交接"}
          </button>
        )}
      </div>
      {pending && (
        <div className="flex flex-col gap-1 rounded border border-warn/40 bg-surface p-1.5">
          <p className="text-[11px] text-warn">
            待承接 → {nameOf(pending)}（{pending}）· 发起人 {detail.owner_agent_id || "unassigned"} 仍为负责人；修订/送验/取消已冻结，先了结交接。
          </p>
          {histFor !== offerID && (
            <p className="text-[11px] text-dim">
              {histErr ? "交接资料加载失败——下方说明可能过期，重试后再操作。" : "交接资料刷新中——下方说明可能过期，操作已绑定当前发起（" + offerID + "）。"}
            </p>
          )}
          <input value={text} onChange={(e) => setText(e.target.value)} maxLength={280}
            placeholder="承接备注 / 婉拒或撤回原因（≤280 字，可空）"
            className="rounded border border-border bg-surface px-1.5 py-1 text-xs outline-none" />
          <div className="flex flex-wrap gap-1">
            <button onClick={() => run("承接", () => api.acceptMissionHandover(missionID, { expected_version: version, offer_id: offerID, ...(text.trim() ? { note: text.trim() } : {}) }))}
              disabled={busy} className="rounded-lg bg-surface-3 px-2 py-1 text-xs text-ok disabled:opacity-40">
              承接（负责人 → {nameOf(pending)}）
            </button>
            <button onClick={() => run("婉拒", () => api.declineMissionHandover(missionID, { expected_version: version, offer_id: offerID, ...(text.trim() ? { reason: text.trim() } : {}) }))}
              disabled={busy} className="rounded-lg bg-surface-3 px-2 py-1 text-xs text-warn disabled:opacity-40">
              婉拒
            </button>
            <button onClick={() => run("撤回", () => api.revokeMissionHandover(missionID, { expected_version: version, offer_id: offerID, ...(text.trim() ? { reason: text.trim() } : {}) }))}
              disabled={busy} className="rounded-lg bg-surface-3 px-2 py-1 text-xs text-dim disabled:opacity-40">
              撤回
            </button>
          </div>
        </div>
      )}
      {open && !pending && (
        <div className="flex flex-col gap-1.5">
          <select value={newOwner} onChange={(e) => setNewOwner(e.target.value)}
            className="rounded border border-border bg-surface px-1.5 py-1 text-sm outline-none">
            <option value="">选择承接人（须存在且 active；{candidates.length} 候选）</option>
            {candidates.map((a) => (
              <option key={a.agent_id} value={a.agent_id}>{a.display_name}（{a.agent_id}）</option>
            ))}
          </select>
          <textarea value={notes} onChange={(e) => setNotes(e.target.value)} rows={3} maxLength={2000}
            placeholder="交接说明——在途事项 / 未决动作 / 证据指引（≤2000 字，可空）"
            className="rounded border border-border bg-surface px-1.5 py-1 text-xs outline-none" />
          <button onClick={() => run("发起", () => api.offerMissionHandover(missionID, { expected_version: version, new_owner_agent_id: newOwner, ...(notes.trim() ? { notes: notes.trim() } : {}) }))}
            disabled={busy || !newOwner}
            className="rounded-lg bg-surface-3 px-2 py-1 text-xs text-accent disabled:opacity-40">
            {busy ? "提交中…" : `发起交接（契约 v${version}；owner 不变直至承接）`}
          </button>
        </div>
      )}
      {msg && <p className="text-[11px] text-dim">{msg}</p>}
      {histErr && (
        <button onClick={() => setHistTick((t) => t + 1)} className="self-start text-[11px] text-warn underline">
          交接历史加载失败——点击重试
        </button>
      )}
      {histFor !== offerID && !histErr && (history?.length ?? 0) > 0 && (
        <p className="text-[11px] text-faint">交接历史刷新中（当前资料对应新发起）…</p>
      )}
      {histFor === offerID && (history?.length ?? 0) > 0 && (
        <details>
          <summary className="cursor-pointer select-none text-faint text-[11px]">交接历史</summary>
          <ul className="mt-0.5 flex flex-col gap-0.5">
            {history!.map((h, i) => (
              <li key={`${h.kind}-${h.occurred_at}-${i}`} className="flex flex-wrap items-center gap-x-2 text-[11px]">
                <span className={h.kind === "accepted" ? "text-ok" : h.kind === "offered" ? "text-accent" : "text-warn"}>
                  {HANDOVER_KIND_LABEL[h.kind] ?? h.kind}
                </span>
                <span className="font-mono text-faint text-[10px]">{h.offer_id}</span>
                <span className="font-mono text-faint">{h.from_owner_agent_id || "unassigned"} → {h.to_owner_agent_id}</span>
                <span className="text-faint">v{h.contract_version} · {new Date(h.occurred_at).toLocaleString()}</span>
                {h.status_at_accept && <span className="text-faint">承接时 {h.status_at_accept}</span>}
                {(h.notes || h.note || h.reason) && <span className="text-dim truncate">{h.notes || h.note || h.reason}</span>}
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>
  );
}
