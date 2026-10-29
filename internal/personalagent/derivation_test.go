package personalagent

import "testing"

// TestDerivationMatchesLegacyAppFormula 钉住"派生单源化不改输出"（ADR-0017 §3）：
// 本测试原样重建 app.go v1.66 起的内联式（sanitizeProfileKey + 前缀拼接），
// personalagent 的单源函数必须与它逐字节相等——存量房间事件里的
// participantID / profileID 才能经别名表解析到同一 Agent，不漂移。
func TestDerivationMatchesLegacyAppFormula(t *testing.T) {
	legacySanitize := func(id string) string {
		var b []rune
		for _, r := range id {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
				b = append(b, r)
			default:
				b = append(b, '_')
			}
		}
		if len(b) != 0 {
			return string(b)
		}
		return "x"
	}
	legacyProfile := func(adapter, botID string) string {
		return "prof_" + adapter + "_" + legacySanitize(botID)
	}
	legacyParticipant := func(adapter, botID string) string {
		return "par_" + adapter + "_" + legacySanitize(botID)
	}

	cases := []struct{ adapter, botID string }{
		{"codex", "codex@native:/usr/local/bin/codex"},
		{"kimi", "kimi@wsl:Ubuntu:/home/u/.nvm/versions/node/v22.0.0/bin/kimi"},
		{"zcode", "zcode@native:C:\\Users\\陆尘\\AppData\\Local\\Programs\\zcode\\zcode.exe"},
		{"minimax", "mcode@native:/opt/minimax/bin/mcode"},
		{"claude", "claude@wsl:openEuler-24.03:/home/u/.local/bin/claude"},
		{"echo", "@@@@"}, // 全非法字符 → "x" 分支
	}
	for _, c := range cases {
		if got, want := ProfileIDOf(c.adapter, c.botID), legacyProfile(c.adapter, c.botID); got != want {
			t.Errorf("ProfileIDOf(%q,%q) = %q, 旧式 = %q", c.adapter, c.botID, got, want)
		}
		if got, want := ParticipantIDOf(c.adapter, c.botID), legacyParticipant(c.adapter, c.botID); got != want {
			t.Errorf("ParticipantIDOf(%q,%q) = %q, 旧式 = %q", c.adapter, c.botID, got, want)
		}
	}
}
