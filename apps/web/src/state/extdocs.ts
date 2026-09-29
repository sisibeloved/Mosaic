// 外部云文档共享映射 store（RFC-0016 多 provider；零依赖 useSyncExternalStore
// + 模块级状态，state/docs.ts 先例）：全量映射小表，一处拉取多处消费
//（DocsTab 徽标/编辑页共享态）；按 doc_id → provider 双键索引。无推送——
// 动作后显式刷新。
import { useSyncExternalStore } from "react";
import { api, ApiError, type ExtDocProviderID, type ExtDocShare } from "../api/client";

export interface ExtSharesState {
  /** doc_id → (provider → 映射)；null = 尚未加载 */
  byDoc: Map<string, Map<string, ExtDocShare>> | null;
  /** 面未装配（extdocs 端点 404）——徽标整体隐藏。 */
  unavailable: boolean;
  /** 已配置的 provider（共享菜单只列可用的）。 */
  configuredProviders: Set<ExtDocProviderID>;
}

let state: ExtSharesState = { byDoc: null, unavailable: false, configuredProviders: new Set() };
const listeners = new Set<() => void>();
let inflight: Promise<void> | null = null;

function emit(next: ExtSharesState): void {
  state = next;
  listeners.forEach((l) => l());
}

/** 拉取全量映射 + provider 配置态（单飞去重；404 = 未装配面）。 */
export async function refreshExtShares(): Promise<void> {
  if (inflight) return inflight;
  inflight = (async () => {
    try {
      const [{ shares }, { providers }] = await Promise.all([
        api.listExtDocShares(),
        api.listExtDocProviders().catch(() => ({ providers: [] })),
      ]);
      const byDoc = new Map<string, Map<string, ExtDocShare>>();
      for (const s of shares) {
        let m = byDoc.get(s.doc_id);
        if (!m) {
          m = new Map();
          byDoc.set(s.doc_id, m);
        }
        m.set(s.provider, s);
      }
      const configured = new Set<ExtDocProviderID>();
      for (const p of providers) if (p.configured) configured.add(p.provider as ExtDocProviderID);
      emit({ byDoc, unavailable: false, configuredProviders: configured });
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) {
        emit({ byDoc: new Map(), unavailable: true, configuredProviders: new Set() });
        return;
      }
      // 其他失败保持现状（下次动作/挂载重试）
    } finally {
      inflight = null;
    }
  })();
  return inflight;
}

/** 共享动作（provider 各自创建 + 刷新映射表）。409 extdocs_already_shared
 * 不抛——视为"已共享"直接刷新展示既有映射。 */
export async function shareDocToExt(docID: string, provider: ExtDocProviderID, force = false): Promise<ExtDocShare | null> {
  try {
    const share = await api.shareDocToExt(docID, provider, force);
    await refreshExtShares();
    return share;
  } catch (e) {
    if (e instanceof ApiError && e.status === 409) {
      await refreshExtShares();
      return state.byDoc?.get(docID)?.get(provider) ?? null;
    }
    throw e;
  }
}

export function useExtShares(): ExtSharesState {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => state,
  );
}
