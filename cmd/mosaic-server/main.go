// mosaic-server 是 Mosaic 个人版的 TCP 进程入口（M2 起装配本体在 internal/app，
// 与桌面壳共用；本入口提供监听/信号/退出语义）。
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/sisibeloved/Mosaic/internal/app"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7420", "listen address")
	dataDir := flag.String("data", "./data", "data directory (SQLite + runtime files)")
	// 四轮复审 #2：agent 工作根独立于数据目录（read-only 沙箱不限制读取——
	// 工作目录在 dataDir 内意味着 ../mosaic.db 是邻位可达路径）。
	agentWork := flag.String("agent-work", "", "agent work root (default: user cache dir; kept OUTSIDE -data)")
	// 二轮审校 #17：owner 级 API（命令/SSE/宿主注册表）无认证——非回环监听必须显式豁免
	allowRemote := flag.Bool("allow-remote", false, "allow non-loopback listen (owner APIs are unauthenticated)")
	// 开发者模式（M1 v1.8）：debug 日志级别 + /v1/debug 只读端点 + UI 调试面板。
	dev := flag.Bool("dev", false, "developer mode: debug logs + read-only /v1/debug endpoints + UI debug panel")
	flag.Parse()

	// 信号处理必须先于一切：listening 日志出现后 ST 立即投递 SIGINT，
	// 注册晚于日志会出现"信号走默认动作 → 非零码退出"的竞态（CI 双核实证）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 日志级别：-dev 下放开 debug；常规模式维持 info——埋点零输出。
	// v1.49：TextHandler 人类可读时间（长静默排障；JSON RFC3339 阅读成本高）。
	logLevel := new(slog.LevelVar)
	if *dev {
		logLevel.Set(slog.LevelDebug)
	}
	logger := app.NewLogger(os.Stdout, logLevel)

	if host, _, err := net.SplitHostPort(*addr); err == nil {
		// 复审 #4：空 host（":7420"）与通配地址 = 全接口监听，不是回环。
		ip := net.ParseIP(host)
		loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
		if !loopback && !*allowRemote {
			logger.Error("拒绝非回环监听（owner/harness API 无认证；个人版本地形态）",
				"addr", *addr, "豁免方式", "显式传 -allow-remote 并自行承担暴露面")
			os.Exit(1)
		}
		if !loopback {
			logger.Warn("非回环监听已豁免：API 无认证，请确保处于受信网络", "addr", *addr)
		}
	}

	srv, err := app.Start(ctx, app.Options{
		Addr:      *addr,
		DataDir:   *dataDir,
		AgentWork: *agentWork,
		Logger:    logger,
		Dev:       *dev,
	})
	if err != nil {
		logger.Error("mosaic-server 启动失败", "err", err)
		os.Exit(1)
	}

	<-ctx.Done()
	srv.Shutdown(context.Background())
	logger.Info("mosaic-server stopped")
}
