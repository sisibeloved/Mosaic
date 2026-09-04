// v1.48 运行参数端点 UT：PUT 更新（模型/强度校验与清除）+ GET 模型候选
// （codex 五档静态空候选；kimi 实查路径的解析在 harness 包 UT 覆盖——
// miniRunner 场景此处只验端点接线）。
package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sisibeloved/Mosaic/internal/harness"
)

func runtimeTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ts, reg := newHarnessTestServer(t)
	// 手动登记一个 codex 实例（同 TestHarnessEndpoints 路径：miniRunner 探测通过）
	resp, err := http.Post(ts.URL+"/v1/harness/executables", "application/json",
		strings.NewReader(`{"adapter":"codex","runtime":"native","path":"/opt/tools/codex"}`))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d", resp.StatusCode)
	}
	_ = reg
	list, _ := http.Get(ts.URL + "/v1/harness/executables")
	var doc struct {
		Executables []harness.Executable `json:"executables"`
	}
	_ = json.NewDecoder(list.Body).Decode(&doc)
	list.Body.Close()
	if len(doc.Executables) == 0 {
		t.Fatal("登记后列表为空")
	}
	return ts, doc.Executables[0].ID
}

func TestRuntimeUpdateEndpoint(t *testing.T) {
	ts, id := runtimeTestServer(t)

	put := func(target, body string) int {
		req, _ := http.NewRequest(http.MethodPut, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	base := ts.URL + "/v1/harness/executables/" + url.PathEscape(id)

	// 合法更新
	if code := put(base, `{"model":"gpt-5.6-sol","reasoning_effort":"xhigh"}`); code != http.StatusOK {
		t.Fatalf("合法更新 = %d", code)
	}
	// 非法档位 → 400
	if code := put(base, `{"model":"m","reasoning_effort":"ultra"}`); code != http.StatusBadRequest {
		t.Fatalf("非法档位 = %d", code)
	}
	// 未知字段 → 400（严格解码纪律）
	if code := put(base, `{"model":"m","reasoning_effort":"","extra":1}`); code != http.StatusBadRequest {
		t.Fatalf("未知字段 = %d", code)
	}
	// 清除覆盖 → 200
	if code := put(base, `{"model":"","reasoning_effort":""}`); code != http.StatusOK {
		t.Fatalf("清除 = %d", code)
	}
	// 不存在 → 404
	if code := put(ts.URL+"/v1/harness/executables/nope", `{}`); code != http.StatusNotFound {
		t.Fatalf("不存在 = %d", code)
	}
	// 列表透出当前值（更新 → 列表）
	if code := put(base, `{"model":"gpt-5.6-sol","reasoning_effort":"high"}`); code != http.StatusOK {
		t.Fatal(code)
	}
	list, _ := http.Get(ts.URL + "/v1/harness/executables")
	var doc struct {
		Executables []harness.Executable `json:"executables"`
	}
	_ = json.NewDecoder(list.Body).Decode(&doc)
	list.Body.Close()
	if doc.Executables[0].Model != "gpt-5.6-sol" || doc.Executables[0].ReasoningEffort != "high" {
		t.Fatalf("列表应透出运行参数: %+v", doc.Executables[0])
	}
}

func TestRuntimeModelsEndpoint(t *testing.T) {
	ts, id := runtimeTestServer(t)

	resp, err := http.Get(ts.URL + "/v1/harness/executables/" + url.PathEscape(id) + "/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models 状态 = %d", resp.StatusCode)
	}
	var opts harness.RuntimeOptions
	_ = json.NewDecoder(resp.Body).Decode(&opts)
	if opts.Dynamic || len(opts.Models) != 0 {
		t.Fatalf("codex 应空候选非动态: %+v", opts)
	}
	if len(opts.EffortLevels) != 5 || opts.EffortLevels[4] != "xhigh" {
		t.Fatalf("codex 五档: %+v", opts.EffortLevels)
	}
	// v1.49 确定量默认：miniRunner 无配置文件 → 官方回退（强度 medium；模型为
	// CLI 内置预设，不虚构）
	if opts.DefaultModel != "" || opts.DefaultEffort != "medium" || opts.DefaultSource != "builtin" {
		t.Fatalf("codex 默认值应官方回退: %+v", opts)
	}
	// 不存在 → 404
	resp404, _ := http.Get(ts.URL + "/v1/harness/executables/nope/models")
	resp404.Body.Close()
	if resp404.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在 = %d", resp404.StatusCode)
	}
}
