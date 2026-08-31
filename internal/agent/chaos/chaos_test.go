// conformance 反例门禁：chaos 适配器的每个违规旋钮必须被套件抓获；
// 零旋钮（clean）必须通过——套件不得误伤合规适配器。
package chaos

import (
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent/conformance"
)

// TestCleanPassesSuite：clean chaos（行为完全合规）必须全绿。
func TestCleanPassesSuite(t *testing.T) {
	conformance.Suite(t, Adapter{})
}

// TestEachKnobCaught：每个违规旋钮注入后，套件必须报出对应检查项的失败——
// 反例存在即证明套件"有牙"（不是永远放行的橡皮图章）。
func TestEachKnobCaught(t *testing.T) {
	cases := []struct {
		name      string
		knobs     Knobs
		wantCheck string
	}{
		{"张冠李戴", Knobs{WrongBlock: true}, "kind_mapping"},
		{"畸形块", Knobs{MalformedData: true}, "block_structure"},
		{"忽视取消", Knobs{IgnoreCancel: true}, "cancel_stale"},
		{"取消后挂死", Knobs{CancelHangs: true}, "cancel_stale"},
		{"启动即崩溃", Knobs{BootFails: true}, "boot"},
		{"流式撒谎", Knobs{StreamingLie: true}, "streaming_honesty"},
		{"虚构用量", Knobs{UsageFabrication: true}, "usage_honesty"},
		{"observe 撒谎", Knobs{ObserveLie: true}, "observe_honesty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fails := conformance.Run(Adapter{Knobs: tc.knobs})
			for _, f := range fails {
				if f.Check == tc.wantCheck {
					return // 抓获
				}
			}
			t.Fatalf("旋钮 %s 未被抓获（期望检查项 %q）；实际失败清单：%+v", tc.name, tc.wantCheck, fails)
		})
	}
}
