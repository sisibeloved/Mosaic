// UT 层：opaque cursor 编解码纯函数（无数据库依赖）。
package sqlite

import (
	"strings"
	"testing"
)

func TestCursorCodecRoundTrip(t *testing.T) {
	for _, pos := range []int64{0, 1, 42, 148, 1 << 40} {
		cursor := EncodeCursor(pos)
		if strings.Contains(cursor, "seq") {
			t.Errorf("游标不得泄露内部语义（%d → %q）", pos, cursor)
		}
		got, err := DecodeCursor(cursor)
		if err != nil {
			t.Fatalf("decode %q: %v", cursor, err)
		}
		if got != pos {
			t.Fatalf("round-trip = %d（期望 %d）", got, pos)
		}
	}
}

func TestCursorDecodeRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"v1:148", "!!!not-base64!!!", "djE6eHg", "v2:148"} {
		// v1:148 是未编码明文；djE6eHg 是 "v1:xx" 的合法 base64 但位非数字；v2 前缀未知版本
		if _, err := DecodeCursor(bad); err == nil {
			t.Errorf("非法游标 %q 必须报错", bad)
		}
	}
	if pos, err := DecodeCursor(""); err != nil || pos != 0 {
		t.Fatalf("空游标语义为从头开始：pos=%d err=%v", pos, err)
	}
}
