//go:build windows && it

package harness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestHostRunnerWSLGlobAndRunWithDir_IT(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runner := NewHostRunner()
	distros := runner.WSLDistros(ctx)
	if len(distros) == 0 {
		t.Skip("Windows runner has no WSL distribution")
	}
	distro := distros[0]
	base := fmt.Sprintf("/tmp/mosaic-harness-%d", time.Now().UnixNano())
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _, _ = runner.Run(cleanupCtx, RuntimeWSL, distro, []string{"rm", "-rf", base})
	}()

	if out, code, err := runner.Run(ctx, RuntimeWSL, distro, []string{"mkdir", "-p", base + "/a", base + "/b"}); err != nil || code != 0 {
		t.Fatalf("prepare WSL glob fixture: out=%q code=%d err=%v", out, code, err)
	}
	if out, code, err := runner.Run(ctx, RuntimeWSL, distro, []string{"cp", "/bin/echo", base + "/a/probe"}); err != nil || code != 0 {
		t.Fatalf("prepare WSL executable fixture: out=%q code=%d err=%v", out, code, err)
	}

	got := runner.Glob(ctx, RuntimeWSL, distro, base+"/*")
	want := []string{base + "/a", base + "/b"}
	if len(got) != len(want) {
		t.Fatalf("WSL glob = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("WSL glob[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	out, code, err := runner.RunWithDir(ctx, RuntimeWSL, distro, base+"/a", []string{"probe", "ok"})
	if err != nil || code != 0 {
		t.Fatalf("RunWithDir: out=%q code=%d err=%v", out, code, err)
	}
	if got := strings.TrimSpace(out); got != "ok" {
		t.Fatalf("RunWithDir output = %q, want %q", got, "ok")
	}
}
