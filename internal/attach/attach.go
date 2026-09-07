// Package attach 聊天附件（RFC-0013，M4-0 文件上传）。
//
// 两步上传（RFC-0013 §2.3）：
//  1. POST multipart → SaveIncoming 落盘 incoming/<token> + 边车 meta
//     （token 24h 有效、单次消费——发消息时 Resolve 重命名定稿即消费）；
//  2. post_message 携带 tokens → Resolve 校验（存在/未过期/哈希一致）→
//     固化为 Descriptor 嵌入事件载荷。
//
// 上限（RFC-0013 §2.2 建议值）：单附件 8MiB；每消息 ≤4 件（命令面校验）；
// 每房间总量 512MiB（上传前检查，超限拒收）。
//
// 注入策略（§2.4）：文本类注入头部摘录（8k runes，过 RedactSecrets——
// 用户可能上传含密钥的配置文件）；图像/二进制只注入元数据（agent 无头
// 均无图像入参通道——如实降级，不虚构能力）。
//
// 数据面：全部位于数据目录 attachments/ 下（owner-only 边界内）；删除房间
// 级联清理（DeleteRoom）。
package attach

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 上限常量（RFC-0013 建议值）。
const (
	MaxFileBytes  = 8 << 20   // 单附件 8MiB
	MaxRoomBytes  = 512 << 20 // 每房间总量 512MiB
	TokenTTL      = 24 * time.Hour
	ExcerptRunes  = 8000 // 文本注入摘录上限
	nameMaxRunes  = 200
	tokenPattern  = `^upl_[0-9a-z_]+$`
	attachPattern = `^att_[0-9a-z_]+$`
)

var (
	tokenRe  = regexp.MustCompile(tokenPattern)
	attachRe = regexp.MustCompile(attachPattern)
	// ErrNotFound 上传令牌或附件不存在（含过期/已消费）。
	ErrNotFound = errors.New("attach: not found")
	// ErrTooLarge 超单文件或房间总量上限。
	ErrTooLarge = errors.New("attach: too large")
	// ErrChecksum 落盘内容与上传时哈希不符（磁盘损坏显式暴露）。
	ErrChecksum = errors.New("attach: checksum mismatch")
)

// Descriptor 事件载荷中的附件描述子（封闭字段集，RFC-0013 §2.1）。
type Descriptor struct {
	AttachmentID string `json:"attachment_id"`
	Name         string `json:"name"`
	MIME         string `json:"mime"`
	SizeBytes    int64  `json:"size_bytes"`
	StoragePath  string `json:"storage_path"` // 相对数据目录
	SHA256       string `json:"sha256"`
}

// UploadMeta 上传响应（第一步产物；token 进 post_message.attachments）。
type UploadMeta struct {
	Token     string `json:"token"`
	Name      string `json:"name"`
	MIME      string `json:"mime"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// incomingMeta 边车（incoming/<token>.json）。
type incomingMeta struct {
	UploadMeta
	UploadedAt time.Time `json:"uploaded_at"`
}

// Store 附件存储（Root = 数据目录）。
type Store struct {
	Root string
	Now  func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) roomDir(roomID string) string {
	return filepath.Join(s.Root, "attachments", "rooms", filepath.ToSlash(safeRoomID(roomID)))
}

// safeRoomID 房间 ID 进目录名的白名单（房间 ID 本服务生成，防御性收口）。
func safeRoomID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return "x"
	}
	return out
}

// SaveIncoming 第一步：流式落盘 + 边车。r 由调用方限制读量（MaxFileBytes）；
// 房间总量超限返回 ErrTooLarge。
func (s *Store) SaveIncoming(roomID, declaredName string, mimeHint string, r io.Reader) (UploadMeta, error) {
	s.sweepExpired(roomID)
	if err := s.checkRoomQuota(roomID, 0); err != nil {
		return UploadMeta{}, err
	}
	token := newID("upl")
	dir := filepath.Join(s.roomDir(roomID), "incoming")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return UploadMeta{}, fmt.Errorf("attach: mkdir: %w", err)
	}
	name := SanitizeName(declaredName)
	tmp := filepath.Join(dir, token+".part")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return UploadMeta{}, fmt.Errorf("attach: create: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, MaxFileBytes+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return UploadMeta{}, fmt.Errorf("attach: write: %v/%v", err, closeErr)
	}
	if n > MaxFileBytes {
		_ = os.Remove(tmp)
		return UploadMeta{}, fmt.Errorf("%w: 单附件上限 %d 字节", ErrTooLarge, MaxFileBytes)
	}
	if err := s.checkRoomQuota(roomID, n); err != nil {
		_ = os.Remove(tmp)
		return UploadMeta{}, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, token)); err != nil {
		_ = os.Remove(tmp)
		return UploadMeta{}, fmt.Errorf("attach: finalize incoming: %w", err)
	}
	meta := UploadMeta{
		Token: token, Name: name,
		MIME:      DetectMIME(mimeHint, s.sampleHead(roomID, token)),
		SizeBytes: n, SHA256: hex.EncodeToString(h.Sum(nil)),
	}
	if err := writeSidecar(filepath.Join(dir, token+".json"), incomingMeta{UploadMeta: meta, UploadedAt: s.now()}); err != nil {
		_ = os.Remove(filepath.Join(dir, token))
		return UploadMeta{}, fmt.Errorf("attach: sidecar: %w", err)
	}
	return meta, nil
}

// sampleHead 读入盘文件头部做 MIME 探测（SaveIncoming 落盘后调用）。
func (s *Store) sampleHead(roomID, token string) []byte {
	f, err := os.Open(filepath.Join(s.roomDir(roomID), "incoming", token))
	if err != nil {
		return nil
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	return head[:n]
}

// Resolve 第二步：tokens → 定稿 Descriptor（重命名即消费；并发同名令牌
// 后到者得 ErrNotFound）。两段实现保证原子性：先校验全部令牌（形状/存在/
// 未过期/哈希一致），任一无效整体拒绝且不消费任何令牌；全部通过后统一定稿。
func (s *Store) Resolve(roomID string, tokens []string) ([]Descriptor, error) {
	dir := filepath.Join(s.roomDir(roomID), "incoming")
	type staged struct {
		token string
		meta  UploadMeta
		src   string
	}
	pending := make([]staged, 0, len(tokens))
	for _, tk := range tokens {
		if !tokenRe.MatchString(tk) {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, tk)
		}
		meta, err := readSidecar(filepath.Join(dir, tk+".json"))
		if err != nil {
			return nil, fmt.Errorf("%w: %q（不存在或已消费）", ErrNotFound, tk)
		}
		if s.now().Sub(meta.UploadedAt) > TokenTTL {
			_ = os.Remove(filepath.Join(dir, tk))
			_ = os.Remove(filepath.Join(dir, tk+".json"))
			return nil, fmt.Errorf("%w: %q（已过期）", ErrNotFound, tk)
		}
		src := filepath.Join(dir, tk)
		raw, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, tk)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != meta.SHA256 || int64(len(raw)) != meta.SizeBytes {
			return nil, fmt.Errorf("%w: %q 落盘内容与上传时不符", ErrChecksum, tk)
		}
		pending = append(pending, staged{token: tk, meta: meta.UploadMeta, src: src})
	}
	out := make([]Descriptor, 0, len(pending))
	for _, p := range pending {
		attID := newID("att")
		storage := path.Join("attachments", "rooms", safeRoomID(roomID), attID)
		if err := os.Rename(p.src, filepath.Join(s.Root, filepath.FromSlash(storage))); err != nil {
			return nil, fmt.Errorf("attach: finalize: %w", err)
		}
		_ = os.Remove(filepath.Join(dir, p.token+".json")) // 消费边车
		out = append(out, Descriptor{
			AttachmentID: attID, Name: p.meta.Name, MIME: p.meta.MIME,
			SizeBytes: p.meta.SizeBytes, StoragePath: storage, SHA256: p.meta.SHA256,
		})
	}
	return out, nil
}

// Open 下载面：读回附件内容与描述子。
func (s *Store) Open(roomID, attachmentID string) ([]byte, Descriptor, error) {
	if !attachRe.MatchString(attachmentID) {
		return nil, Descriptor{}, fmt.Errorf("%w: %q", ErrNotFound, attachmentID)
	}
	storage := path.Join("attachments", "rooms", safeRoomID(roomID), attachmentID)
	raw, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(storage)))
	if err != nil {
		return nil, Descriptor{}, fmt.Errorf("%w: %q", ErrNotFound, attachmentID)
	}
	sum := sha256.Sum256(raw)
	// 描述子其余字段无法从文件恢复——调用方（httpapi）从事件载荷查；此处供
	// 下载响应做长度校验。
	return raw, Descriptor{AttachmentID: attachmentID, SizeBytes: int64(len(raw)),
		SHA256: hex.EncodeToString(sum[:]), MIME: "application/octet-stream"}, nil
}

// DeleteRoom 删除房间级联清理（M3-6 语义延伸：墓碑不保留内容）。
func (s *Store) DeleteRoom(roomID string) error {
	if err := os.RemoveAll(s.roomDir(roomID)); err != nil {
		return fmt.Errorf("attach: delete room: %w", err)
	}
	return nil
}

// checkRoomQuota 房间总量（final + incoming）+ extra 不超 MaxRoomBytes。
func (s *Store) checkRoomQuota(roomID string, extra int64) error {
	var total int64
	_ = filepath.WalkDir(s.roomDir(roomID), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, ie := d.Info(); ie == nil {
				total += info.Size()
			}
		}
		return nil // best-effort 遍历
	})
	if total+extra > MaxRoomBytes {
		return fmt.Errorf("%w: 房间附件总量上限 %d 字节（现 %d + 新 %d）", ErrTooLarge, MaxRoomBytes, total, extra)
	}
	return nil
}

// sweepExpired 清理过期未消费的上传（SaveIncoming 顺带执行，best-effort）。
func (s *Store) sweepExpired(roomID string) {
	dir := filepath.Join(s.roomDir(roomID), "incoming")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		meta, err := readSidecar(filepath.Join(dir, e.Name()+".json"))
		if err != nil || s.now().Sub(meta.UploadedAt) <= TokenTTL {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
		_ = os.Remove(filepath.Join(dir, e.Name()+".json"))
	}
}

func writeSidecar(path string, m incomingMeta) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func readSidecar(path string) (incomingMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return incomingMeta{}, err
	}
	var m incomingMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return incomingMeta{}, err
	}
	return m, nil
}

func newID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + hex.EncodeToString(b[:])
}

// ---- 文件名/MIME/注入渲染（纯函数） ----

// SanitizeName 文件名防御：basename 化、控制字符剔除、不得以 . 开头、
// ≤200 runes（RFC-0013 §3——路径注入防御：storage_path 由服务端生成）。
func SanitizeName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimLeft(out, ".")
	if out == "" || out == "/" {
		out = "file"
	}
	if len([]rune(out)) > nameMaxRunes {
		out = string([]rune(out)[:nameMaxRunes])
	}
	return out
}

// textMIMEs 显式文本族（http.DetectContentType 之外的应用层文本类型）。
var textMIMEs = map[string]bool{
	"application/json": true, "application/xml": true, "application/yaml": true,
	"application/toml": true, "application/csv": true, "application/javascript": true,
	"application/x-yaml": true, "application/sql": true,
}

// IsTextLike 文本类判定：显式文本 MIME，或字节样本为合法 UTF-8 且不含 NUL。
func IsTextLike(mime string, sample []byte) bool {
	if strings.HasPrefix(mime, "text/") || textMIMEs[mime] {
		return true
	}
	return utf8.Valid(sample) && !bytesContainsNUL(sample)
}

func bytesContainsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

// DetectMIME 探测（客户端声明 + 内容嗅探折中：声明属 text/* 或已知应用层
// 类型则采信，否则内容嗅探，兜底 octet-stream）。
func DetectMIME(declared string, head []byte) string {
	if declared != "" && (strings.HasPrefix(declared, "text/") || textMIMEs[declared] ||
		strings.HasPrefix(declared, "image/") || declared == "application/pdf") {
		return declared
	}
	return http.DetectContentType(head)
}

// RenderForContext 语境注入渲染（RFC-0013 §2.4）：文本类 → 头部摘录
// （ExcerptRunes 上限 + 截断标注 + redact 过秘密形状——用户可能上传含密钥
// 的配置文件）；图像/二进制 → 元数据降级（agent 无头无图像通道——如实降级
// 不虚构能力）。head 为文件头部字节（调用方读有界前缀）。
func RenderForContext(d Descriptor, redact func(string) string, head []byte) string {
	label := fmt.Sprintf("[附件 %s · %s · %d 字节", d.Name, d.MIME, d.SizeBytes)
	if !IsTextLike(d.MIME, head) {
		if strings.HasPrefix(d.MIME, "image/") {
			return label + " · 图像——你无法查看原图，仅知元数据]"
		}
		return label + " · 二进制——仅元数据]"
	}
	text := string(head)
	if redact != nil {
		text = redact(text)
	}
	runes := []rune(text)
	suffix := "]"
	if len(runes) > ExcerptRunes {
		runes = runes[:ExcerptRunes]
		suffix = fmt.Sprintf("（全文 %d 字节，摘录截断）]", d.SizeBytes)
	}
	return label + "]\n" + string(runes) + suffix
}
