// 确定性选择回归门禁（RFC-0003 §3.4：固定 Intent fixture → 选择结果必须逐位一致）。
// fixtures/*.json 改动即视为裁决基线变更，须附推导说明。
package attention

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type fixtureScores struct {
	Relevance  float64 `json:"relevance"`
	Novelty    float64 `json:"novelty"`
	Urgency    float64 `json:"urgency"`
	Confidence float64 `json:"confidence"`
}

type fixtureIntent struct {
	IntentID      string         `json:"intent_id"`
	ParticipantID string         `json:"participant_id"`
	Action        string         `json:"action"`
	Type          string         `json:"type"`
	Scores        fixtureScores  `json:"scores"`
	AddressedTo   []string       `json:"addressed_to"`
}

type fixtureFile struct {
	Policy struct {
		Mode        string  `json:"mode"`
		MaxSpeakers int     `json:"max_speakers"`
		Lambda      float64 `json:"lambda"`
		Weights     struct {
			Relevance     float64 `json:"relevance"`
			Novelty       float64 `json:"novelty"`
			Diversity     float64 `json:"diversity"`
			Urgency       float64 `json:"urgency"`
			DirectAddress float64 `json:"direct_address"`
			FloorShare    float64 `json:"floor_share"`
			Repetition    float64 `json:"repetition"`
		} `json:"weights"`
	} `json:"policy"`
	Candidates []struct {
		Intent      fixtureIntent `json:"intent"`
		Context     ContextFeatures `json:"context"`
		Eligibility struct {
			Enabled        bool `json:"enabled"`
			CooldownOK     bool `json:"cooldown_ok"`
			ThreadWritable bool `json:"thread_writable"`
			BudgetOK       bool `json:"budget_ok"`
		} `json:"eligibility"`
	} `json:"candidates"`
	Expected struct {
		Selected      []string          `json:"selected"`
		SelectedBands []string          `json:"selected_bands"`
		Rejected      map[string]string `json:"rejected"`
		Quiescent     bool              `json:"quiescent"`
		SilentCount   int               `json:"silent_count"`
	} `json:"expected"`
}

func TestDeterministicSelectionFixtures(t *testing.T) {
	dir := "fixtures"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	ran := 0
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var fx fixtureFile
		if err := json.Unmarshal(raw, &fx); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		policy := Policy{
			Mode:        fx.Policy.Mode,
			MaxSpeakers: fx.Policy.MaxSpeakers,
			Lambda:      fx.Policy.Lambda,
			Weights: Weights{
				Relevance:     fx.Policy.Weights.Relevance,
				Novelty:       fx.Policy.Weights.Novelty,
				Diversity:     fx.Policy.Weights.Diversity,
				Urgency:       fx.Policy.Weights.Urgency,
				DirectAddress: fx.Policy.Weights.DirectAddress,
				FloorShare:    fx.Policy.Weights.FloorShare,
				Repetition:    fx.Policy.Weights.Repetition,
			},
		}
		if err := policy.Validate(); err != nil {
			t.Fatalf("%s: fixture policy 非法: %v", name, err)
		}
		var candidates []Candidate
		for _, c := range fx.Candidates {
			candidates = append(candidates, Candidate{
				Intent: Intent{
					IntentID:      c.Intent.IntentID,
					ParticipantID: c.Intent.ParticipantID,
					Action:        c.Intent.Action,
					Type:          c.Intent.Type,
					AddressedTo:   c.Intent.AddressedTo,
					Scores: Scores{
						Relevance:  c.Intent.Scores.Relevance,
						Novelty:    c.Intent.Scores.Novelty,
						Urgency:    c.Intent.Scores.Urgency,
						Confidence: c.Intent.Scores.Confidence,
					},
				},
				Ctx: c.Context,
				Eligibility: Eligibility{
					Enabled:        c.Eligibility.Enabled,
					CooldownOK:     c.Eligibility.CooldownOK,
					ThreadWritable: c.Eligibility.ThreadWritable,
					BudgetOK:       c.Eligibility.BudgetOK,
				},
			})
		}

		// 跑两遍：先与期望比，再互相逐位比（确定性双保险）
		first := Select(candidates, policy)
		second := Select(candidates, policy)
		if !resultsEqual(first, second) {
			t.Errorf("%s: 同输入两次选择不一致", name)
		}

		if len(first.Selected) != len(fx.Expected.Selected) {
			t.Errorf("%s: 选择数 = %d（期望 %d）：%+v", name, len(first.Selected), len(fx.Expected.Selected), first.Selected)
			continue
		}
		for i, want := range fx.Expected.Selected {
			if first.Selected[i].IntentID != want {
				t.Errorf("%s: rank %d = %s（期望 %s）", name, i+1, first.Selected[i].IntentID, want)
			}
			if band := fx.Expected.SelectedBands[i]; first.Selected[i].Band != band {
				t.Errorf("%s: %s band = %s（期望 %s）", name, want, first.Selected[i].Band, band)
			}
		}
		gotRejected := map[string]string{}
		for _, r := range first.Rejected {
			gotRejected[r.IntentID] = r.Reason
		}
		for id, wantReason := range fx.Expected.Rejected {
			if gotRejected[id] != wantReason {
				t.Errorf("%s: %s 拒绝原因 = %q（期望 %q）", name, id, gotRejected[id], wantReason)
			}
		}
		if first.Quiescent != fx.Expected.Quiescent {
			t.Errorf("%s: quiescent = %v（期望 %v）", name, first.Quiescent, fx.Expected.Quiescent)
		}
		if first.SilentCount != fx.Expected.SilentCount {
			t.Errorf("%s: silent_count = %d（期望 %d）", name, first.SilentCount, fx.Expected.SilentCount)
		}
		ran++
	}
	if ran == 0 {
		t.Fatal("fixtures/ 下没有可执行的确定性基线")
	}
}
