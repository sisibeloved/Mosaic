// M4-0 系统面：备份/恢复/自诊断（对外契约操作，apigen.ServerInterface）。
// 装配层未注入（测试装配/无存储形态）时 404——与 owner bootstrap 的收敛语义一致。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/sisibeloved/Mosaic/internal/backup"
	"github.com/sisibeloved/Mosaic/internal/settings"
)

// ListBackups GET /v1/system/backups。
func (s *server) ListBackups(w http.ResponseWriter, r *http.Request) {
	if s.deps.Backups == nil {
		writeError(w, http.StatusNotFound, "backups_disabled", "本装配未启用备份面")
		return
	}
	list, err := s.deps.Backups.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "backup_list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": list})
}

// CreateBackup POST /v1/system/backups（写端点三层门）。
func (s *server) CreateBackup(w http.ResponseWriter, r *http.Request) {
	if !s.guardWrite(w, r, false) {
		return
	}
	if s.deps.Backups == nil {
		writeError(w, http.StatusNotFound, "backups_disabled", "本装配未启用备份面")
		return
	}
	// 空 body 允许（无参数操作）；有 body 也只接受空对象，保持解码纪律。
	if body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<10)); err == nil && len(body) > 0 {
		var probe map[string]any
		if json.Unmarshal(body, &probe) != nil || len(probe) != 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "备份创建无参数（空 body 或 {}）")
			return
		}
	}
	sum, err := s.deps.Backups.Create(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "backup_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// RequestRestore POST /v1/system/restore（写端点三层门；两段式的在线段）。
func (s *server) RequestRestore(w http.ResponseWriter, r *http.Request) {
	if !s.guardWrite(w, r, true) {
		return
	}
	if s.deps.Backups == nil {
		writeError(w, http.StatusNotFound, "backups_disabled", "本装配未启用备份面")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		BackupID string `json:"backup_id"`
		Confirm  bool   `json:"confirm"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "载荷须为 {backup_id, confirm}")
		return
	}
	if !req.Confirm {
		writeError(w, http.StatusBadRequest, "restore_not_confirmed", "恢复不可逆，confirm 必须为 true")
		return
	}
	if err := s.deps.Backups.RequestRestore(req.BackupID); err != nil {
		if errors.Is(err, backup.ErrNotFound) {
			writeError(w, http.StatusNotFound, "backup_not_found", "备份不存在或 id 非法")
			return
		}
		writeError(w, http.StatusBadRequest, "restore_rejected", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restart_required": true})
}

// GetDiagnostics GET /v1/system/diagnostics。
func (s *server) GetDiagnostics(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Diagnostics == nil {
		writeError(w, http.StatusNotFound, "diagnostics_disabled", "本装配未启用诊断面")
		return
	}
	bundle, err := s.deps.Diagnostics()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "diagnostics_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, bundle)
}

// GetSystemSettings GET /v1/system/settings（OQ-B 设置族首员，M4-1 切片 B）。
func (s *server) GetSystemSettings(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusNotFound, "settings_disabled", "本装配未启用设置面")
		return
	}
	writeJSON(w, http.StatusOK, settingsDocOf(s.deps.Settings.Snapshot()))
}

// UpdateSystemSettings PUT /v1/system/settings（写端点三层门）。全量替换语义：
// 未识别字段拒绝（解码纪律）；值域校验后原子落盘。引擎活读面即刻对新拉起的
// run 生效（在途 run 不受影响——代次语义与取消一致）。
func (s *server) UpdateSystemSettings(w http.ResponseWriter, r *http.Request) {
	if !s.guardWrite(w, r, true) {
		return
	}
	if s.deps.Settings == nil {
		writeError(w, http.StatusNotFound, "settings_disabled", "本装配未启用设置面")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		RunTimeoutSeconds int `json:"run_timeout_seconds"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "载荷须为 {run_timeout_seconds}")
		return
	}
	if err := settings.ValidateRunTimeoutSeconds(req.RunTimeoutSeconds); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_value", err.Error())
		return
	}
	if err := s.deps.Settings.UpdateRunTimeoutSeconds(req.RunTimeoutSeconds); err != nil {
		writeError(w, http.StatusInternalServerError, "settings_write_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settingsDocOf(s.deps.Settings.Snapshot()))
}

func settingsDocOf(doc settings.Document) map[string]any {
	return map[string]any{"run_timeout_seconds": doc.RunTimeoutSeconds}
}
