// M4-0 系统面：备份/恢复/自诊断（对外契约操作，apigen.ServerInterface）。
// 装配层未注入（测试装配/无存储形态）时 404——与 owner bootstrap 的收敛语义一致。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/sisibeloved/Mosaic/internal/backup"
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
