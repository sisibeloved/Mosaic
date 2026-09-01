// 用户语言映射层（唯一权威处）：内部枚举/标识符 → 界面文案。
// 各函数注释标注来源枚举（api/http-api/openapi.yaml 与领域模型约定）。

/** 参与者类别（ParticipantView.kind / EventView.actor.kind：human | agent | system）。 */
export function kindLabel(kind: string): string {
  switch (kind) {
    case "human":
      return "人类";
    case "agent":
      return "智能体";
    case "system":
      return "系统";
    default:
      return kind;
  }
}

/** 适配器名（Executable.adapter / ParticipantView.adapter：codex | kimi | echo…）。 */
export function adapterLabel(adapter: string): string {
  switch (adapter) {
    case "codex":
      return "Codex";
    case "kimi":
      return "Kimi";
    case "echo":
      return "Echo（测试桩）";
    default:
      return adapter;
  }
}

/** 接入渠道（Executable.channel / ParticipantView.channel：cli | app:* 家族，ADR-0012）。 */
export function channelLabel(channel?: string): string {
  if (!channel || channel === "cli") return "CLI";
  if (channel === "app:codex-desktop") return "桌面 App";
  if (channel === "app:kimi-work") return "Kimi Work";
  return channel;
}

/** 座位状态（ParticipantView.seat_status：seated | …）。 */
export function seatStatusLabel(status: string): string {
  return status === "seated" ? "在座" : status;
}

/** 评估档位（ScorecardItem.score_band：high | low | unranked）。 */
export function scoreBandLabel(band: string): string {
  switch (band) {
    case "high":
      return "高";
    case "low":
      return "低";
    case "unranked":
      return "未参评";
    default:
      return band;
  }
}

/** 发言意向类型（ScorecardItem.type：answer | extend | challenge | support | question | redirect | synthesize）。 */
export function intentTypeLabel(type: string): string {
  switch (type) {
    case "answer":
      return "回答";
    case "extend":
      return "补充";
    case "challenge":
      return "质疑";
    case "support":
      return "支持";
    case "question":
      return "提问";
    case "redirect":
      return "转进";
    case "synthesize":
      return "总结";
    default:
      return type;
  }
}

/** 观点关系类型（GraphEdge.kind：显式关系 supports | challenges | extends | questions；结构边 forked_from | responds_to | merged_into）。 */
export function relationKindLabel(kind: string): string {
  switch (kind) {
    case "supports":
      return "支持";
    case "challenges":
      return "质疑";
    case "extends":
      return "延伸";
    case "questions":
      return "追问";
    case "forked_from":
      return "分支自";
    case "responds_to":
      return "回应";
    case "merged_into":
      return "合并入";
    default:
      return kind;
  }
}

/** 发言意向动作兜底（ScorecardItem.action：speak | silent；silent 在组件侧单独文案，此处是 type 缺失时的极小概率兜底）。 */
export function intentActionLabel(action: string): string {
  switch (action) {
    case "speak":
      return "发言";
    case "silent":
      return "本轮不发言";
    default:
      return action;
  }
}

/** 话题线状态（ThreadItem.state：active | paused | closed | merged）。 */
export function threadStateLabel(state: string): string {
  switch (state) {
    case "active":
      return "进行中";
    case "paused":
      return "已暂停";
    case "closed":
      return "已结束";
    case "merged":
      return "已合并";
    default:
      return state;
  }
}
