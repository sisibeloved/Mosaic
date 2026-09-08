// 按需检索平面（RFC-0007 §3.1.8 v0.2 修订 / v1.45 裁定 2）：SQLite FTS5
// (trigram) 起步、pgvector 无限期推迟。本文件是**语义基准**（线性子串匹配，
// 大小写不敏感——FTS5 实现的对照真理；trigram 对 <3 字查询不可用，FTS5 侧
// 回退 LIKE 子串，与线性版同为子串语义）。前置验证已实证（v1.46 spike，
// modernc v1.57.0 / SQLite 3.53.3）：unicode61 对中文整串成单 token 不可用；
// trigram 对 CJK 子串（≥3 字）与英文均正确命中。
package room

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"github.com/sisibeloved/Mosaic/internal/protocol"
)

// SearchHit 房内消息检索命中（对外视图：无 seq/tenant；position 供 UI 跳转定位）。
type SearchHit struct {
	EventID    string  `json:"event_id"`
	Actor      string  `json:"actor"`
	ActorKind  string  `json:"actor_kind"`
	ThreadID   *string `json:"thread_id"`
	Body       string  `json:"body"`
	OccurredAt string  `json:"occurred_at"`
	Position   string  `json:"position"`
}

// SearchMessages 线性语义基准：全量事件 → 子串命中（最新在前；actor/threadID
// 可选过滤；limit 1..100 默认 20）。个人版房间规模毫秒级；FTS5 实现服务同语义。
func SearchMessages(events []StoredEvent, query, actor, threadID string, limit int) []SearchHit {
	query = strings.TrimSpace(query)
	if query == "" {
		return []SearchHit{}
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	needle := strings.ToLower(query)
	hits := []SearchHit{}
	for i := len(events) - 1; i >= 0; i-- { // 最新在前
		env := events[i].Envelope
		if env.Type != protocol.EventMessagePosted {
			continue
		}
		if actor != "" && env.Actor.ParticipantID != actor {
			continue
		}
		if threadID != "" && (env.ThreadID == nil || *env.ThreadID != threadID) {
			continue
		}
		var p struct {
			Body string `json:"body"`
		}
		if json.Unmarshal(env.Payload, &p) != nil {
			continue
		}
		if strings.Contains(strings.ToLower(p.Body), needle) {
			hits = append(hits, SearchHit{
				EventID:    env.EventID,
				Actor:      env.Actor.ParticipantID,
				ActorKind:  env.Actor.Kind,
				ThreadID:   env.ThreadID,
				Body:       p.Body,
				OccurredAt: env.OccurredAt,
				Position:   events[i].Cursor,
			})
			if len(hits) >= limit {
				break
			}
		}
	}
	return hits
}

// ExtractKeywords 正文 → 检索关键词：CJK 连续段（≥2 字）与 ASCII 词（≥3 字母）
// 各自成词，按长度降序取前 3（长词更特异；同长按首现序）——确定性、零模型。
func ExtractKeywords(body string) []string {
	type span struct {
		text  string
		start int
	}
	var spans []span
	var buf strings.Builder
	var bufStart int
	flush := func(at int) {
		s := strings.TrimSpace(buf.String())
		if s != "" {
			spans = append(spans, span{text: s, start: bufStart})
		}
		buf.Reset()
		bufStart = at + 1
	}
	for i, r := range body {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if buf.Len() == 0 {
				bufStart = i
			}
			buf.WriteRune(r)
			continue
		}
		flush(i)
	}
	flush(len(body))
	// CJK 连续段与 ASCII 词分桶（CJK 段内无空格，整段即词）
	type kw struct {
		text  string
		order int
		cjk   bool
	}
	var kws []kw
	for i, sp := range spans {
		if containsCJK(sp.text) {
			if len([]rune(sp.text)) >= 2 {
				kws = append(kws, kw{text: sp.text, order: i, cjk: true})
			}
			continue
		}
		if len(sp.text) >= 3 {
			kws = append(kws, kw{text: strings.ToLower(sp.text), order: i})
		}
	}
	sort.SliceStable(kws, func(a, b int) bool {
		ra, rb := []rune(kws[a].text), []rune(kws[b].text)
		if len(ra) != len(rb) {
			return len(ra) > len(rb)
		}
		return kws[a].order < kws[b].order
	})
	out := []string{}
	for _, k := range kws {
		if len(out) >= 3 {
			break
		}
		out = append(out, k.text)
	}
	return out
}

// RetrieveRelated 组装时检索（按需平面的记忆接入）：近期窗口之外的旧消息按
// 关键词召回（limit 条）。排除集含 recent 窗口 event_id 与刺激本身（重复注入
// 无信息量）；命中即 provenance（event_id/actor/body 入上下文，Receipt 层摘要
// 可验证）。匹配语义（v1.70 升级，对齐 FTS5 trigram 口径的纯函数实现——组装
// 面保持事件流纯函数，回放重建不变）：ASCII 词子串命中；CJK bigram 重叠计数；
// 按**匹配强度降序**排列（多关键词/bigram 命中者优先于仅靠新近度撞上的弱关联），
// 同分按新近度——召回质量从"最新优先"升级为"最相关优先"，仍确定性零模型。
func RetrieveRelated(envs []protocol.Envelope, keywords []string, exclude map[string]bool, limit int) []protocol.Envelope {
	if len(keywords) == 0 || limit <= 0 {
		return nil
	}
	kws := make([]string, 0, len(keywords))
	for _, k := range keywords {
		if containsCJK(k) {
			kws = append(kws, k)
		}
	}
	ascii := make([]string, 0, len(keywords))
	for _, k := range keywords {
		if !containsCJK(k) {
			ascii = append(ascii, k)
		}
	}
	type scored struct {
		env   protocol.Envelope
		score int
	}
	var hits []scored
	for i := len(envs) - 1; i >= 0; i-- { // 新近序入列（同分稳定 tiebreak）
		env := envs[i]
		if env.Type != protocol.EventMessagePosted || exclude[env.EventID] {
			continue
		}
		var p struct {
			Body string `json:"body"`
		}
		if json.Unmarshal(env.Payload, &p) != nil {
			continue
		}
		if s := relatedScore(p.Body, kws, ascii); s > 0 {
			hits = append(hits, scored{env: env, score: s})
		}
	}
	// 稳定排序：分数降序（插入序即新近序——同分保新近优先）。
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].score > hits[j-1].score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	out := make([]protocol.Envelope, 0, limit)
	for _, h := range hits {
		if len(out) >= limit {
			break
		}
		out = append(out, h.env)
	}
	return out
}

// relatedScore 正文与关键词的匹配强度：ASCII 子串各计 1 分 ×词长权重；
// CJK 每个 bigram 命中计 1 分。
func relatedScore(body string, cjkSpans, asciiWords []string) int {
	score := 0
	lower := strings.ToLower(body)
	for _, w := range asciiWords {
		if strings.Contains(lower, w) {
			score += 1 + len(w)/8
		}
	}
	if len(cjkSpans) == 0 {
		return score
	}
	bodyGrams := bigramsOf(body)
	for _, span := range cjkSpans {
		for g := range bigramsOf(span) {
			if bodyGrams[g] {
				score++
			}
		}
	}
	return score
}

func bigramsOf(s string) map[string]bool {
	out := map[string]bool{}
	rs := []rune(s)
	for i := 0; i+1 < len(rs); i++ {
		if containsCJK(string(rs[i : i+2])) {
			out[string(rs[i:i+2])] = true
		}
	}
	return out
}
