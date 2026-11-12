// forget.go —— 0.7.8 / ADR-0045：记忆 Forget（隐私删除）——墓碑化、删除
// 传播输入与影响报告模型。
//
// 语义与边界（RFC §11.4 / AT-06）：
//   - 墓碑行不删（审计保留不含正文的元数据）：status=deleted、
//     Title/Content 置空、Version+1、EditRecord 追加 cause=delete
//     （0.7.1 留痕机制复用——delete 留痕即删除代次事实源）；
//   - FTS 索引经 v33 memory_fts_au 触发器随正文置空同步（trigram 无 token
//     可命中）+ 召回 status=active 过滤——双保险结构性不可达；
//   - 不跨条目级联（无 derived_from 显式血缘——ADR-0045 §1 边界）；
//   - 防复活查询面 TombstoneHits：key = mem_*（包内原 ID）或
//     bundle:<bdl>:<mem>（激活 origin 标记）——导入预检/激活核对。
package memory

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// StatusDeleted 墓碑态（0.7.8——写入面不可直接造，只经 Forget 到达）。
const StatusDeleted Status = "deleted"

// DeleteCause 留痕 cause 值（0.7.1 EditRecord 枚举扩展）。
const DeleteCause = "delete"

// DeletionReport 删除影响报告（RFC §11.4"不能宣称全域擦除"——编排层
// （app）组合域墓碑事实与托管包扫描）。
type DeletionReport struct {
	MemoryID  string `json:"memory_id"`
	Namespace string `json:"namespace"`
	ScopeID   string `json:"scope_id"`
	DeletedAt string `json:"deleted_at"`
	// Cleared 本实例已清除面（正文 + FTS 索引——结构性不可达）。
	Cleared DeletionCleared `json:"cleared"`
	// ContainedInBundles 托管导出包内仍有正文的清单（zip 是定版快照不重写
	// ——破坏 manifest 摘要完整性；如实列出，可经包删除端点处置）。
	ContainedInBundles []string `json:"contained_in_bundles"`
	// ContainedInImports 本机导入 zip 内仍有正文的清单（审校 #4——受管理
	// 副本不漏报；activated 记录不可删，正文随记录保留直至治理）。
	ContainedInImports []string `json:"contained_in_imports"`
	// OutOfReach 无法保证清除的面（已离开本实例的副本——如实列出）。
	OutOfReach []string `json:"out_of_reach"`
}

// DeletionCleared 已清除面标记。
type DeletionCleared struct {
	Content bool `json:"content"`
	Index   bool `json:"index"`
}

// DeletionReportOf 墓碑事实 + 托管包清单 → 影响报告（编排层组装单源）。
func DeletionReportOf(gone Item, containedInBundles, containedInImports []string) DeletionReport {
	rep := DeletionReport{
		MemoryID:           gone.MemoryID,
		Namespace:          string(gone.Namespace),
		ScopeID:            gone.ScopeID,
		DeletedAt:          gone.UpdatedAt,
		Cleared:            DeletionCleared{Content: true, Index: true},
		ContainedInBundles: append([]string{}, containedInBundles...),
		ContainedInImports: append([]string{}, containedInImports...),
		OutOfReach:         []string{"已在他实例激活的副本无法召回（包已离开本实例）"},
	}
	if len(rep.ContainedInBundles) > 0 {
		rep.OutOfReach = append(rep.OutOfReach, "托管导出包内仍有正文——zip 为定版快照不重写，可经包删除端点处置")
	}
	if len(rep.ContainedInImports) > 0 {
		rep.OutOfReach = append(rep.OutOfReach, "本机导入 zip 内仍有正文——activated 记录不可删（撤销激活另行交付），正文随记录保留直至治理")
	}
	return rep
}

// Forget 隐私删除（ADR-0045 §1）：任意非墓碑态（active/proposed/superseded）
// 皆可删；幂等（已删 no-op 返回当前墓碑——版本不再递增）。
func (s *Service) Forget(ctx context.Context, memoryID, note string) (Item, error) {
	if utf8.RuneCountInString(note) > 500 {
		return Item{}, invalidErr("note ≤500 字")
	}
	// 复审 #1：读取与提交全程持 forgetMu——与 CreateGuarded 同一同步边界
	//（CreateGuarded 持锁期间 Forget 不得插入提交，反之亦然——
	// "核对未命中→Forget 提交→仍写出 active"的交错结构性消除）。
	s.forgetMu.Lock()
	defer s.forgetMu.Unlock()
	it, err := s.Get(ctx, memoryID)
	if err != nil {
		return Item{}, err
	}
	if it.Status == StatusDeleted {
		return it, nil // 幂等：墓碑已成——不追留痕不递增版本
	}
	now := s.now()
	next := it
	next.Status = StatusDeleted
	next.Title = ""
	next.Content = "" // "审计仅保留不含正文的墓碑"——归属/来源引用/敏感度/留痕保留
	next.Version = it.Version + 1
	next.EditHistory = append(append([]EditRecord{}, it.EditHistory...), EditRecord{
		Version: next.Version, Cause: DeleteCause, Note: note, EditedBy: "owner", OccurredAt: now,
	})
	next.UpdatedAt = now
	out, ok, err := s.store.UpdateItem(ctx, memoryID, it.Version, it.Status, next)
	if err != nil {
		return Item{}, err
	}
	if !ok {
		// 并发策展/修订抢先——重读重试一次（状态机竞态窗口收窄）。
		cur, err := s.Get(ctx, memoryID)
		if err != nil {
			return Item{}, err
		}
		if cur.Status == StatusDeleted {
			return cur, nil // 并发删除已遂
		}
		return Item{}, fmt.Errorf("%w: 版本竞态——请重试（当前 v%d/%s）", ErrInvalid, cur.Version, cur.Status)
	}
	return out, nil
}

// TombstoneHits 防复活核对（ADR-0045 §3；审校 #2 归一）：键归一到**稳定
// 记忆身份**——裸 ID 或 bundle 标记（bundle:<bdl>:<mem>）的尾段记忆 ID
// （跨导出/跨迁移跳不变：同一源记忆的两份不同 bundle_id 的包同键）；墓碑
// 侧同归一，返回交集（无 = 空）。
func (s *Service) TombstoneHits(ctx context.Context, keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	// 审校 #2 归一：键 = 稳定记忆身份（裸 ID 或 bundle 标记尾段）。
	keySet := normalizeMemoryKeys(keys)
	// 墓碑逐域扫（0.7.0 分域隔离纪律：List 按命名空间必填——五域逐一聚合；
	// 个人实例删除量小；ID 与 SourceRefs 双匹配；命中是键级事实不泄域内容）。
	var out []string
	for _, ns := range []Namespace{NSAgent, NSRoom, NSWorkspace, NSMission, NSSlot} {
		before := ""
		for {
			res, err := s.List(ctx, ListFilter{Namespace: ns, Status: StatusDeleted, Limit: 100, Before: before, IncludeExpired: true})
			if err != nil {
				return nil, err
			}
			for _, it := range res.Items {
				// 墓碑侧键集同归一（ID + 标记尾段）——交集即命中。
				for k := range normalizeMemoryKeys(append([]string{it.MemoryID}, it.SourceRefs...)) {
					if keySet[k] {
						out = append(out, k)
					}
				}
			}
			if res.NextCursor == "" {
				break
			}
			before = res.NextCursor
		}
	}
	return out, nil
}

// normalizeMemoryKeys 键归一（复审 #2 收紧）：**只认记忆身份**——mem_ 裸 ID
// 原样 + bundle:<bdl>:<mem> 标记取尾段 <mem>（跨导出/跨跳稳定）；普通证据
// 引用（ev_1 等非身份键）一律丢弃——两条记忆共享同一证据不等于同一记忆
// （误入键集会把同源不同事实的记忆在激活时静默跳过）。
func normalizeMemoryKeys(keys []string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		if strings.HasPrefix(k, "bundle:") {
			if i := strings.LastIndex(k, ":"); i > 0 && i < len(k)-1 {
				out[k[i+1:]] = true
			}
			continue
		}
		if strings.HasPrefix(k, "mem_") {
			out[k] = true
		}
	}
	return out
}

// CreateGuarded 墓碑守卫创建（0.7.8 审校 #3）：tombstoneKeys 核对与创建
// 在**同一 forgetMu 边界**内完成——Forget 提交后，在途激活的核对必然命中
// （不存在"查完未命中→Forget 插入→仍写出 active 副本"的交错窗口）；
// 命中返回 created=false（调用方按跳过计），未命中正常创建。
func (s *Service) CreateGuarded(ctx context.Context, tombstoneKeys []string, cmd CreateCmd) (bool, error) {
	if len(tombstoneKeys) == 0 {
		_, err := s.Create(ctx, cmd)
		return err == nil, err
	}
	s.forgetMu.Lock()
	defer s.forgetMu.Unlock()
	hits, err := s.TombstoneHits(ctx, tombstoneKeys)
	if err != nil {
		return false, err
	}
	if len(hits) > 0 {
		return false, nil
	}
	if _, err := s.Create(ctx, cmd); err != nil {
		return false, err
	}
	return true, nil
}
