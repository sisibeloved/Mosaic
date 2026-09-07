// UT 层：附件面——两步上传生命周期（落盘→定稿消费→过期清理）、配额、
// 文件名净化、MIME 探测、注入渲染（文本摘录/图像二进制降级/秘密剔除）。
package attach

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, *timeFake) {
	t.Helper()
	tf := &timeFake{t: time.Unix(1000, 0)}
	return &Store{Root: t.TempDir(), Now: tf.now}, tf
}

type timeFake struct{ t time.Time }

func (f *timeFake) now() time.Time { return f.t }

func TestSaveAndResolveLifecycle(t *testing.T) {
	s, tf := newStore(t)
	content := []byte("hello attachment")
	meta, err := s.SaveIncoming("room_1", "../../etc/passwd", "text/plain", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if meta.Name != "passwd" {
		t.Fatalf("文件名应 basename 化（路径注入防御）: %q", meta.Name)
	}
	if meta.SizeBytes != int64(len(content)) || meta.SHA256 == "" || !tokenRe.MatchString(meta.Token) {
		t.Fatalf("meta 异常: %+v", meta)
	}
	if meta.MIME != "text/plain" {
		t.Fatalf("声明 text/* 应采信: %q", meta.MIME)
	}

	descs, err := s.Resolve("room_1", []string{meta.Token})
	if err != nil || len(descs) != 1 {
		t.Fatalf("resolve: %v %+v", err, descs)
	}
	d := descs[0]
	if d.Name != "passwd" || d.SizeBytes != int64(len(content)) || !attachRe.MatchString(d.AttachmentID) {
		t.Fatalf("descriptor 异常: %+v", d)
	}
	if !strings.HasPrefix(d.StoragePath, "attachments/rooms/room_1/att_") {
		t.Fatalf("storage_path 异常: %q", d.StoragePath)
	}
	// 定稿文件存在且内容一致
	raw, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(d.StoragePath)))
	if err != nil || !bytes.Equal(raw, content) {
		t.Fatalf("定稿文件异常: %v", err)
	}
	// 令牌单次消费：再 resolve 同一令牌 → ErrNotFound
	if _, err := s.Resolve("room_1", []string{meta.Token}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已消费令牌应 ErrNotFound: %v", err)
	}
	_ = tf
}

func TestResolveExpiredToken(t *testing.T) {
	s, tf := newStore(t)
	meta, err := s.SaveIncoming("room_1", "a.txt", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	tf.t = tf.t.Add(TokenTTL + time.Minute)
	if _, err := s.Resolve("room_1", []string{meta.Token}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期令牌应 ErrNotFound: %v", err)
	}
}

func TestSingleFileCap(t *testing.T) {
	s, _ := newStore(t)
	big := bytes.Repeat([]byte("a"), MaxFileBytes+1)
	if _, err := s.SaveIncoming("room_1", "big.bin", "", bytes.NewReader(big)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超单文件上限应 ErrTooLarge: %v", err)
	}
	// 超限不留残留
	entries, _ := os.ReadDir(filepath.Join(s.roomDir("room_1"), "incoming"))
	if len(entries) != 0 {
		t.Fatalf("超限上传不应留残留: %d", len(entries))
	}
}

func TestRoomQuotaCap(t *testing.T) {
	s := &Store{Root: t.TempDir(), Now: func() time.Time { return time.Unix(1000, 0) }}
	// 直接放置一个占满配额的既有附件
	dir := s.roomDir("room_q")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "existing"), bytes.Repeat([]byte("b"), MaxRoomBytes-10), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveIncoming("room_q", "c.txt", "text/plain", strings.NewReader("0123456789")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("房间总量超限应 ErrTooLarge: %v", err)
	}
}

func TestResolveAnyInvalidTokenRejectsWhole(t *testing.T) {
	s, _ := newStore(t)
	meta, _ := s.SaveIncoming("room_1", "a.txt", "text/plain", strings.NewReader("x"))
	if _, err := s.Resolve("room_1", []string{meta.Token, "upl_nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("任一令牌无效应整体拒绝: %v", err)
	}
	// 原有效令牌未被消费（原子性）
	if _, err := s.Resolve("room_1", []string{meta.Token}); err != nil {
		t.Fatalf("整体拒绝不应消费有效令牌: %v", err)
	}
}

func TestOpenAndDeleteRoom(t *testing.T) {
	s, _ := newStore(t)
	meta, _ := s.SaveIncoming("room_1", "a.txt", "text/plain", strings.NewReader("content"))
	descs, _ := s.Resolve("room_1", []string{meta.Token})
	raw, d, err := s.Open("room_1", descs[0].AttachmentID)
	if err != nil || string(raw) != "content" || d.SizeBytes != 7 {
		t.Fatalf("open 异常: %v %+v", err, d)
	}
	if _, _, err := s.Open("room_1", "att_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知附件应 ErrNotFound: %v", err)
	}
	if err := s.DeleteRoom("room_1"); err != nil {
		t.Fatalf("delete room: %v", err)
	}
	if _, _, err := s.Open("room_1", descs[0].AttachmentID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除房间后附件应不可达: %v", err)
	}
}

func TestSweepExpired(t *testing.T) {
	s, tf := newStore(t)
	old, _ := s.SaveIncoming("room_1", "old.txt", "text/plain", strings.NewReader("x"))
	tf.t = tf.t.Add(TokenTTL + time.Hour)
	fresh, _ := s.SaveIncoming("room_1", "fresh.txt", "text/plain", strings.NewReader("y"))
	if _, err := s.Resolve("room_1", []string{old.Token}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期令牌应已被清理: %v", err)
	}
	if _, err := s.Resolve("room_1", []string{fresh.Token}); err != nil {
		t.Fatalf("新鲜令牌应可用: %v", err)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":       "passwd",
		"..\\..\\win\\cmd":       "cmd",
		".hidden":                "hidden",
		"...":                    "file",
		"":                       "file",
		"a\x00b\x1fc":            "abc",
		"  spaced  ":             "spaced",
		strings.Repeat("x", 300): strings.Repeat("x", nameMaxRunes),
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q（want %q）", in, got, want)
		}
	}
}

func TestDetectMIME(t *testing.T) {
	if got := DetectMIME("application/json", nil); got != "application/json" {
		t.Errorf("声明 JSON 应采信: %q", got)
	}
	if got := DetectMIME("", []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}); got != "image/png" {
		t.Errorf("PNG 嗅探: %q", got)
	}
	if got := DetectMIME("", []byte("just text")); got != "text/plain; charset=utf-8" {
		t.Errorf("文本嗅探: %q", got)
	}
}

func TestRenderForContext(t *testing.T) {
	redact := func(s string) string { return strings.ReplaceAll(s, "sk-SECRET", "[REDACTED]") }

	// 文本类：摘录 + 秘密剔除 + 截断标注
	textD := Descriptor{Name: "conf.txt", MIME: "text/plain", SizeBytes: 100}
	out := RenderForContext(textD, redact, []byte("api key = sk-SECRET end"))
	if !strings.Contains(out, "[REDACTED]") || strings.Contains(out, "sk-SECRET") {
		t.Fatalf("摘录应过秘密剔除: %q", out)
	}
	if !strings.Contains(out, "conf.txt") || !strings.Contains(out, "api key") {
		t.Fatalf("摘录应含标签与内容: %q", out)
	}

	// 截断：超 8k runes 的头部 → 截断标注
	bigD := Descriptor{Name: "big.log", MIME: "text/plain", SizeBytes: 1 << 20}
	out = RenderForContext(bigD, nil, bytes.Repeat([]byte("字"), ExcerptRunes+100))
	if !strings.Contains(out, "摘录截断") {
		t.Fatalf("超限应带截断标注: %.40s…", out)
	}

	// 图像/二进制：只元数据，如实降级
	imgD := Descriptor{Name: "shot.png", MIME: "image/png", SizeBytes: 5}
	out = RenderForContext(imgD, nil, []byte{0x89, 'P', 'N', 'G'})
	if !strings.Contains(out, "无法查看原图") || strings.Contains(out, "PNG") {
		t.Fatalf("图像应元数据降级: %q", out)
	}
	binD := Descriptor{Name: "data.bin", MIME: "application/octet-stream", SizeBytes: 9}
	out = RenderForContext(binD, nil, []byte{0, 1, 2, 0, 3})
	if !strings.Contains(out, "仅元数据") {
		t.Fatalf("二进制应元数据降级: %q", out)
	}
}

var _ = context.Background // 保留 context 以备流式用例扩展
