// 结构投影最小版（RFC-0006 降级路径，M3-4）：推断边（echoes——不同作者间的
// 近重复发言）、重复风险（发言者近期与既有发言的撞车率）、漂移签名（近期窗口
// 规范化哈希——同流同签名、变体即漂移）。全部确定性纯函数，自事件流重建。
// 向量/聚类等完整结构投影仍归 RFC-0006 全量面；本最小版只供波内排序特征与
// 图谱视图 inferred 边（双视图"显式 vs 推断"语义收口）。
package room

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// epistemicWindow 参与推断计算的最大近期消息数（O(n²) 界：个人版房间规模护栏）。
const epistemicWindow = 50

// echoThreshold 判定近重复的规范化 bigram Jaccard 下限。
const echoThreshold = 0.6

// normalizeBody 规范化正文（去空白/标点、小写）——比较口径与展示无关。
func normalizeBody(body string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(body) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r >= unicodeStart && r <= unicodeEnd:
			b.WriteRune(r)
		}
	}
	return b.String()
}

const (
	unicodeStart = 0x4e00 // CJK 统一表意文字起点（中文正文保留）
	unicodeEnd   = 0x9fff
)

// bigrams 字符 bigram 集合。
func bigrams(s string) map[string]bool {
	out := map[string]bool{}
	rs := []rune(s)
	for i := 0; i+1 < len(rs); i++ {
		out[string(rs[i:i+2])] = true
	}
	return out
}

// echoSimilar 规范化 bigram Jaccard ≥ 阈值。
func echoSimilar(a, b string) bool {
	ga, gb := bigrams(a), bigrams(b)
	if len(ga) == 0 || len(gb) == 0 {
		return false
	}
	inter := 0
	for k := range ga {
		if gb[k] {
			inter++
		}
	}
	union := len(ga) + len(gb) - inter
	return float64(inter)/float64(union) >= echoThreshold
}

// typedMessage 消息最小投影（结构计算用）。
type typedMessage struct {
	EventID string
	Author  string
	Body    string
}

func recentMessages(envs []protocol.Envelope, limit int) []typedMessage {
	var msgs []typedMessage
	for _, env := range envs {
		if env.Type != protocol.EventMessagePosted {
			continue
		}
		var p struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(env.Payload, &p)
		msgs = append(msgs, typedMessage{EventID: env.EventID, Author: env.Actor.ParticipantID, Body: p.Body})
	}
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	return msgs
}

// InferredEchoEdges 推断边（echoes）：不同作者的近重复发言 → 后发言者 echoes
// 先发言者（Inferred=true——图谱双视图的"推断"语义收口，v1.19 登记）。
func InferredEchoEdges(envs []protocol.Envelope) []GraphEdge {
	msgs := recentMessages(envs, epistemicWindow)
	edges := []GraphEdge{}
	for i := 1; i < len(msgs); i++ {
		for j := i - 1; j >= 0; j-- {
			if msgs[i].Author == msgs[j].Author {
				continue
			}
			if echoSimilar(normalizeBody(msgs[i].Body), normalizeBody(msgs[j].Body)) {
				edges = append(edges, GraphEdge{
					Kind: "echoes", From: msgs[i].EventID, To: msgs[j].EventID, Inferred: true,
				})
				break // 每条消息至多一条 echoes 边（最近的撞车源）
			}
		}
	}
	return edges
}

// RepetitionRiskOf 发言者重复风险（RFC-0003 结构特征，M3-4 接入）：该作者近期
// （最后 5 条）与更早不同作者发言的撞车占比——高值提示"别再说一遍"。
func RepetitionRiskOf(envs []protocol.Envelope, participantID string) float64 {
	msgs := recentMessages(envs, epistemicWindow)
	var mine []typedMessage
	for _, m := range msgs {
		if m.Author == participantID {
			mine = append(mine, m)
		}
	}
	if len(mine) == 0 {
		return 0
	}
	if len(mine) > 5 {
		mine = mine[len(mine)-5:]
	}
	echoes := 0
	for _, m := range mine {
		for _, other := range msgs {
			if other.Author == participantID || other.EventID == m.EventID {
				continue
			}
			if echoSimilar(normalizeBody(m.Body), normalizeBody(other.Body)) {
				echoes++
				break
			}
		}
	}
	return float64(echoes) / float64(len(mine))
}

// ViewpointDiversityOf 近期发言作者多样性（1 - 最大作者占比；多来源高、独白低）。
func ViewpointDiversityOf(envs []protocol.Envelope) float64 {
	msgs := recentMessages(envs, 10)
	if len(msgs) == 0 {
		return 0.5 // 中性（无信息）
	}
	counts := map[string]int{}
	for _, m := range msgs {
		counts[m.Author]++
	}
	max := 0
	for _, c := range counts {
		if c > max {
			max = c
		}
	}
	return 1 - float64(max)/float64(len(msgs))
}

// DriftSignature 漂移签名：近期窗口（默认 20 条）规范化正文序列的 sha256——
// 同流同签名（稳定），内容变体即漂移；重聚焦/收束信号的确定性底座。
func DriftSignature(envs []protocol.Envelope, window int) string {
	if window <= 0 {
		window = 20
	}
	msgs := recentMessages(envs, window)
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, normalizeBody(m.Body))
	}
	sort.Strings(parts) // 作者内语序扰动容忍：按内容集合签名
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// envelopesOfStored StoredEvent 列表 → Envelope 列表（结构特征计算桥）。
func envelopesOfStored(events []StoredEvent) []protocol.Envelope {
	out := make([]protocol.Envelope, len(events))
	for i := range events {
		out[i] = events[i].Envelope
	}
	return out
}
