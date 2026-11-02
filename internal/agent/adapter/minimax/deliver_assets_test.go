package minimax

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

const sampleDeliverSource = "/home/luchen/.mosaic/agent-work/prof_minimax/session-mvs_147f67d9-bilibili-digest.md"

func TestGenerateParsesDeliverAssets(t *testing.T) {
	body := "报告已完成。\n\n<deliver-assets>\n" +
		`<media type="file" src="` + sampleDeliverSource + `" caption="session digest：5 轮提问 &amp; 最终答案" />` + "\n" +
		"</deliver-assets>"
	content := `{"body":` + mustJSON(t, body) + `,"declared_relations":[]}`
	exec := &fakeExecer{outputs: []string{agentMessageStream(t, content)}}
	adapter := newTestAdapter(exec)
	sess, _ := adapter.Boot(context.Background(), agent.Profile{ProfileID: "p", Adapter: "minimax"})
	defer sess.Close()
	h, err := sess.Run(context.Background(), agent.Task{TaskID: "t", Kind: agent.KindGenerate})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res, err := h.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if got := res.Data["body"]; got != "报告已完成。" {
		t.Fatalf("交付标签应剥离，body=%q", got)
	}
	assets, ok := res.Data["deliver_assets"].([]agent.DeliverAsset)
	if !ok || len(assets) != 1 {
		t.Fatalf("deliver_assets 形状不符：%T %+v", res.Data["deliver_assets"], res.Data["deliver_assets"])
	}
	if assets[0].Type != "file" || assets[0].Source != sampleDeliverSource || assets[0].Caption != "session digest：5 轮提问 & 最终答案" {
		t.Fatalf("附件解析不符：%+v", assets[0])
	}
	if err := agent.ValidateBlock(res.Block, res.Data); err != nil {
		t.Fatalf("交付附件应过 public_draft 校验：%v", err)
	}
}

func TestSplitDeliverAssetsValidation(t *testing.T) {
	t.Run("multiple and attachment only body", func(t *testing.T) {
		clean, assets, err := splitDeliverAssets("<deliver-assets><media type=\"file\" src=\"/work/a.md\" caption=\"甲\"/><media type=\"video\" src=\"/work/b.mp4\" caption=\"乙\"/></deliver-assets>")
		if err != nil {
			t.Fatal(err)
		}
		if clean != "甲\n乙" || len(assets) != 2 || assets[1].Type != "video" {
			t.Fatalf("clean=%q assets=%+v", clean, assets)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		if _, _, err := splitDeliverAssets("正文<deliver-assets><media src=\"/work/a.md\"/>"); err == nil {
			t.Fatal("未闭合交付块必须拒绝，不得原样发布")
		}
	})
	t.Run("too many", func(t *testing.T) {
		block := "<deliver-assets>" + strings.Repeat(`<media type="file" src="/work/a.md"/>`, 5) + "</deliver-assets>"
		if _, _, err := splitDeliverAssets(block); err == nil {
			t.Fatal("超过四件应拒绝")
		}
	})
}

func TestReadDeliverAssetNativeBoundary(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "digest.md")
	if err := os.WriteFile(inside, []byte("digest"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(Config{WorkDir: root})
	r, err := adapter.ReadDeliverAsset(context.Background(), inside)
	if err != nil {
		t.Fatalf("工作区内文件应可读：%v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if string(got) != "digest" {
		t.Fatalf("内容不符：%q", got)
	}
	for _, source := range []string{outside, "../secret.txt", root} {
		if _, err := adapter.ReadDeliverAsset(context.Background(), source); err == nil {
			t.Errorf("越界或目录必须拒绝：%q", source)
		}
	}
	link := filepath.Join(root, "escape.md")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := adapter.ReadDeliverAsset(context.Background(), link); err == nil {
			t.Fatal("符号链接逃逸必须拒绝")
		}
	}
}

type fakeAssetStreamExecer struct {
	fakeExecer
	argv []string
	data []byte
}

func (f *fakeAssetStreamExecer) Stream(_ context.Context, argv []string, _ []string) (io.ReadCloser, error) {
	f.argv = append([]string(nil), argv...)
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

func TestReadDeliverAssetWSLStreamsBinaryAndUsesPositionalArgs(t *testing.T) {
	exec := &fakeAssetStreamExecer{data: []byte{0x00, 0xff, 0x80, 0x5a, 0x0a}}
	adapter := New(Config{WorkDir: "/home/u/work", WSLDistro: "openEuler", Execer: exec})
	r, err := adapter.ReadDeliverAsset(context.Background(), "/home/u/work/result.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, exec.data) {
		t.Fatalf("二进制流被改写：%v", got)
	}
	joined := strings.Join(exec.argv, "\x00")
	if !strings.Contains(joined, "wsl.exe\x00-d\x00openEuler\x00--exec\x00sh\x00-c") {
		t.Fatalf("WSL argv 不符：%v", exec.argv)
	}
	if !strings.Contains(joined, "realpath") || !strings.Contains(joined, "cat") {
		t.Fatalf("WSL 应校验真实路径后流式 cat：%v", exec.argv)
	}
	if exec.argv[len(exec.argv)-2] != "/home/u/work" || exec.argv[len(exec.argv)-1] != "/home/u/work/result.bin" {
		t.Fatalf("root/source 必须是独立位置参数：%v", exec.argv)
	}
}

func TestProcessExecerStreamBinary(t *testing.T) {
	want := []byte{0x00, 0xff, 0x80, 0x5a, 0x0a}
	if os.Getenv("MOSAIC_TEST_ASSET_STREAM") == "1" {
		_, _ = os.Stdout.Write(want)
		os.Exit(0)
	}
	r, err := (&processExecer{}).Stream(context.Background(),
		[]string{os.Args[0], "-test.run=TestProcessExecerStreamBinary"},
		append(os.Environ(), "MOSAIC_TEST_ASSET_STREAM=1"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("真实子进程二进制流被改写：%v", got)
	}
}
