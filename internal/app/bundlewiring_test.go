// bundlewiring_test.go —— 0.7.5 一审 #4 / 二审 #4：导出面身份标识的
// 路径脱敏投影（ExeID + BotID 派生别名三元组）与"投影后事实进 zip
// 全文本无路径"的整包断言。
package app

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/bundle"
	"github.com/sisibeloved/Mosaic/internal/personalagent"
)

func TestPortableExeID(t *testing.T) {
	cases := []struct {
		adapter, runtime, distro, want string
	}{
		// 注册表实形 exeID = "codex@wsl:openEuler-24.03:/home/u/.local/bin/codex"
		// ——投影只留 adapter@runtime[:distro]，路径段（用户名/目录）不随包。
		{"codex", "wsl", "openEuler-24.03", "codex@wsl:openEuler-24.03"},
		{"kimi", "native", "", "kimi@native"},
		{"zcode", "wsl", "", "zcode@wsl"},
	}
	for _, tc := range cases {
		got := portableExeID(tc.adapter, tc.runtime, tc.distro)
		if got != tc.want {
			t.Fatalf("portableExeID(%q,%q,%q) = %q want %q", tc.adapter, tc.runtime, tc.distro, got, tc.want)
		}
		if strings.ContainsAny(got, `/\`) || strings.Contains(got, ":/") {
			t.Fatalf("投影含路径分隔符: %q", got)
		}
	}
}

// 二审 #4：BotIDOf 回退 = exeID（adapter@runtime[:distro]:path）——含安装
// 路径与用户名；profileKey 仅把 / 换 _（"home_user_" 仍嵌入用户名）。
// portableAliases 须把三元组整体投影且派生自洽（prof/par 由脱敏后 botID
// 经 personalagent 单源派生重算——包内别名体系内部一致）。
func TestPortableAliases(t *testing.T) {
	// 路径型（自动同步实形）。
	in := bundle.AliasFacts{
		BotID:          "codex@wsl:openEuler-24.03:/home/luchen/.local/bin/codex",
		ProfileIDs:     []string{"prof_codex_home_luchen__local_bin_codex"},
		ParticipantIDs: []string{"par_codex_home_luchen__local_bin_codex"},
	}
	got := portableAliases("codex", in)
	if got.BotID != "codex@wsl:openEuler-24.03" {
		t.Fatalf("BotID 投影: %q", got.BotID)
	}
	if len(got.ProfileIDs) != 1 || got.ProfileIDs[0] != personalagent.ProfileIDOf("codex", got.BotID) {
		t.Fatalf("ProfileIDs 应由脱敏后 BotID 单源派生: %v", got.ProfileIDs)
	}
	if len(got.ParticipantIDs) != 1 || got.ParticipantIDs[0] != personalagent.ParticipantIDOf("codex", got.BotID) {
		t.Fatalf("ParticipantIDs 应由脱敏后 BotID 单源派生: %v", got.ParticipantIDs)
	}
	for _, v := range append(append([]string{got.BotID}, got.ProfileIDs...), got.ParticipantIDs...) {
		if strings.Contains(v, "luchen") || strings.Contains(v, "/home/") || strings.Contains(v, `/\`) {
			t.Fatalf("别名仍泄路径/用户名: %q", v)
		}
	}

	// 非路径型 BotID 原样透传（无 ":/" 路径段特征）。
	plain := bundle.AliasFacts{BotID: "bot_stable", ProfileIDs: []string{"prof_x_bot_stable"}, ParticipantIDs: []string{"par_x_bot_stable"}}
	if got := portableAliases("codex", plain); got.BotID != "bot_stable" || got.ProfileIDs[0] != "prof_x_bot_stable" {
		t.Fatalf("非路径 BotID 应原样: %+v", got)
	}

	// 空别名（手建 Agent）保持空。
	if got := portableAliases("codex", bundle.AliasFacts{}); got.BotID != "" || len(got.ProfileIDs) != 0 || len(got.ParticipantIDs) != 0 {
		t.Fatalf("空别名应保持空: %+v", got)
	}
}

// TestPortableIdentityIntoBundle 二审 #4 的整包断言：wiring 投影产出的
// 身份事实（Aliases/Executables 均为 portable 形态）进入导出 zip 后，
// **全包文本**不含路径与用户名特征——投影函数与打包链路（bundle 域原样
// 序列化）一起钉住，回应"完整 ZIP 的脱敏断言"。
func TestPortableIdentityIntoBundle(t *testing.T) {
	dir := t.TempDir()
	svc := bundle.New(bundle.Ports{
		Agent: func(_ context.Context, _ string) (bundle.AgentFacts, bool, error) {
			f := bundle.AgentFacts{AgentID: "ag_ok", DisplayName: "小助", Adapter: "codex"}
			f.Aliases = portableAliases("codex", bundle.AliasFacts{
				BotID:          "codex@wsl:openEuler-24.03:/home/luchen/.local/bin/codex",
				ProfileIDs:     []string{"prof_codex_home_luchen__local_bin_codex"},
				ParticipantIDs: []string{"par_codex_home_luchen__local_bin_codex"},
			})
			f.Executables = append(f.Executables, bundle.ExecutableRef{
				ExecutableID: portableExeID("codex", "wsl", "openEuler-24.03"),
				Runtime:      "wsl", Distro: "openEuler-24.03", Channel: "cli", Enabled: true,
			})
			return f, true, nil
		},
		Slots:    func(_ context.Context, _ string) ([]bundle.SlotFact, error) { return nil, nil },
		Missions: func(_ context.Context, _ string) ([]bundle.MissionFact, error) { return nil, nil },
		Checkpoint: func(_ context.Context, _ string) (bundle.CheckpointFact, bool, error) {
			return bundle.CheckpointFact{}, false, nil
		},
		Memories: func(_ context.Context, _ string, _ string) ([]bundle.MemoryFact, error) {
			return nil, nil
		},
	}, dir, "test", bundle.WithNow(func() string { return "2026-11-09T00:00:00Z" }),
		bundle.WithNewID(func() string { return "bdl_alias1" }))

	view, err := svc.Export(context.Background(), "ag_ok", bundle.ScopeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(filepath.Join(dir, "bundles", view.BundleID, "mosaic-agent-bundle.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(rc)
		rc.Close()
		text := string(raw)
		for _, leak := range []string{"/home/", "luchen", ":/home", `\\`} {
			if strings.Contains(text, leak) {
				t.Fatalf("包内 %s 泄漏路径/用户名特征 %q", f.Name, leak)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "bundles", view.BundleID, "mosaic-agent-bundle.zip")); err != nil {
		t.Fatal(err)
	}
}
