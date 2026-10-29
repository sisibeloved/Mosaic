package personalagent

import "github.com/sisibeloved/Mosaic/internal/harness"

// registryAdapter 把 harness 注册表适配为身份域的 Registry 端口
// （依赖方向：personalagent → harness，单向；harness 不感知身份域）。
type registryAdapter struct {
	reg *harness.Registry
}

// NewRegistryAdapter 生产装配入口。
func NewRegistryAdapter(reg *harness.Registry) Registry {
	return &registryAdapter{reg: reg}
}

func (a *registryAdapter) Snapshot() []ExecutableSnapshot {
	exes := a.reg.List() // 规范序（adapter→priority→path），确定性同步的输入
	out := make([]ExecutableSnapshot, 0, len(exes))
	for _, e := range exes {
		channel := e.Channel
		if channel == "" {
			channel = harness.ChannelCLI
		}
		out = append(out, ExecutableSnapshot{
			ExeID:   e.ID,
			BotID:   e.BotIDOf(),
			Adapter: e.Adapter,
			Runtime: e.Runtime,
			Distro:  e.Distro,
			Channel: channel,
			Enabled: e.Enabled,
		})
	}
	return out
}

func (a *registryAdapter) ApplyAgentIDs(ids map[string]string) (bool, error) {
	return a.reg.ApplyAgentIDs(ids)
}
