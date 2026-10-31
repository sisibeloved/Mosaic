//go:build it

// IT 层（ADR-0025 / 0.6.2）：context_manifests/context_receipts 经真实
// SQLite——migration v15 新库、v0.6.1 真旧库升级、(mission_id, digest)
// 唯一约束、清单/回执同事务与装配历史读面。
package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/execctx"
)

func sampleManifest(id, missionID, digest string) (execctx.Manifest, execctx.Receipt) {
	m := execctx.Manifest{
		ManifestID: id, MissionID: missionID, ContractVersion: 2,
		BindingID: "bd_it", AgentID: "ag_it",
		State: execctx.StateAssembled, Digest: digest, PolicyVersion: execctx.PolicyVersion,
		Layers: []execctx.Layer{
			{Name: execctx.LayerContract, Role: execctx.RoleRequired, State: execctx.LayerIncluded,
				Bytes: 42, Content: execctx.ContractContent{}},
			{Name: execctx.LayerCheckpoint, Role: execctx.RoleOptional, State: execctx.LayerOmitted,
				ReasonCode: execctx.ReasonCheckpointUnavailable},
		},
		Watermarks: []execctx.Watermark{{Stream: "room:r1", Position: 9}},
		TotalBytes: 42, CreatedAt: "2026-10-30T11:00:00.000Z",
	}
	r := execctx.Receipt{
		ReceiptID: "cxr_" + id, ManifestID: id, Digest: digest,
		PolicyVersion: execctx.PolicyVersion,
		Accounting: []execctx.AccountingEntry{
			{Name: execctx.LayerContract, State: execctx.LayerIncluded, Bytes: 42},
			{Name: execctx.LayerCheckpoint, State: execctx.LayerOmitted,
				ReasonCode: execctx.ReasonCheckpointUnavailable},
		},
		TotalBytes: 42, CreatedAt: "2026-10-30T11:00:00.000Z",
	}
	return m, r
}

// TestExecctxStoreIT v15 建表 + 落库/查面/撞键。
func TestExecctxStoreIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	var v15 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 15`).Scan(&v15)
	if v15 != 1 {
		t.Fatalf("migration v15 未登记: %d", v15)
	}

	m1, r1 := sampleManifest("cmf_a", "mi_it", "d1")
	if err := store.CreateManifest(ctx, m1, r1); err != nil {
		t.Fatalf("清单落库: %v", err)
	}
	// 同 (mission_id, digest) 撞键 → ErrManifestExists
	if err := store.CreateManifest(ctx, m1, r1); !errors.Is(err, execctx.ErrManifestExists) {
		t.Fatalf("撞键期望 ErrManifestExists，得到 %v", err)
	}
	// 异 digest 同 mission → 追加（装配历史）
	m2, r2 := sampleManifest("cmf_b", "mi_it", "d2")
	m2.State, m2.ReasonCode = execctx.StateBlocked, execctx.ReasonReadinessNotMet
	if err := store.CreateManifest(ctx, m2, r2); err != nil {
		t.Fatalf("追加清单: %v", err)
	}

	got, gr, ok, err := store.ManifestByDigest(ctx, "mi_it", "d1")
	if err != nil || !ok {
		t.Fatalf("按摘要查: ok=%v err=%v", ok, err)
	}
	if got.ManifestID != "cmf_a" || gr.ReceiptID != "cxr_cmf_a" || gr.Digest != "d1" {
		t.Fatalf("按摘要查结果错误: %+v %+v", got, gr)
	}
	if len(got.Layers) != 2 || got.Layers[0].Name != execctx.LayerContract ||
		got.Layers[0].Bytes != 42 || got.Layers[0].Content == nil ||
		got.Layers[1].ReasonCode != execctx.ReasonCheckpointUnavailable {
		t.Fatalf("层账本回读错误: %+v", got.Layers)
	}
	if len(got.Watermarks) != 1 || got.Watermarks[0].Position != 9 {
		t.Fatalf("水位回读错误: %+v", got.Watermarks)
	}
	if len(gr.Accounting) != 2 || gr.Accounting[0].Bytes != 42 {
		t.Fatalf("回执账目回读错误: %+v", gr.Accounting)
	}

	byID, _, ok, err := store.ManifestByID(ctx, "cmf_b")
	if err != nil || !ok || byID.State != execctx.StateBlocked || byID.ReasonCode != execctx.ReasonReadinessNotMet {
		t.Fatalf("按 ID 查: ok=%v err=%v state=%s/%s", ok, err, byID.State, byID.ReasonCode)
	}
	if _, _, ok, _ := store.ManifestByID(ctx, "cmf_x"); ok {
		t.Fatal("未知清单应 ok=false")
	}

	list, err := store.ManifestsByMission(ctx, "mi_it")
	if err != nil || len(list) != 2 || list[0].ManifestID != "cmf_b" {
		t.Fatalf("装配历史（创建序倒序）: %v %+v", err, list)
	}
}

// TestExecctxConcurrentSameDigestIT 并发同摘要孪生：撞键者得
// ErrManifestExists——恰一落库（服务层据此转重放）。
func TestExecctxConcurrentSameDigestIT(t *testing.T) {
	store, _ := openTempStore(t)
	defer store.Close()
	ctx := context.Background()

	m, r := sampleManifest("cmf_c", "mi_race", "drace")
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = store.CreateManifest(ctx, m, r)
		}(i)
	}
	wg.Wait()
	okCount := 0
	for _, err := range errs {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, execctx.ErrManifestExists):
		default:
			t.Fatalf("并发落库意外错误: %v", err)
		}
	}
	if okCount != 1 {
		t.Fatalf("并发同摘要应恰一落库，得 %d", okCount)
	}
	if got, _, ok, _ := store.ManifestByDigest(ctx, "mi_race", "drace"); !ok || got.ManifestID != "cmf_c" {
		t.Fatalf("恰一清单应可查: ok=%v", ok)
	}
}

// TestExecctxLegacyUpgradeIT v0.6.1 真旧库升级到 v15（表在席、旧数据不动）。
func TestExecctxLegacyUpgradeIT(t *testing.T) {
	marker := "-- v15（RFC-0018"
	idx := strings.Index(schemaDDL, marker)
	if idx < 0 {
		t.Fatal("schemaDDL 缺 v15 段标记——测试前提失效")
	}
	legacyDDL := schemaDDL[:idx]
	path := filepath.Join(t.TempDir(), "mosaic-legacy-v061.db")
	db, err := sqlOpenRaw(path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := db.Exec(legacyDDL); err != nil {
		db.Close()
		t.Fatalf("legacy ddl: %v", err)
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("旧库升级打开: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	var v15 int
	store.db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 15`).Scan(&v15)
	if v15 != 1 {
		t.Fatalf("升级后 migration v15 应登记: %d", v15)
	}
	m, r := sampleManifest("cmf_l", "mi_legacy", "dl")
	if err := store.CreateManifest(ctx, m, r); err != nil {
		t.Fatalf("升级库落清单: %v", err)
	}
	if got, _, ok, _ := store.ManifestByDigest(ctx, "mi_legacy", "dl"); !ok || got.ManifestID != "cmf_l" {
		t.Fatalf("升级库查清单: ok=%v", ok)
	}
}
