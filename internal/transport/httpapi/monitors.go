// M4-5 监控面：监控源 CRUD + 启停 + 手动检查（对外契约操作，apigen.ServerInterface）。
// 未装配（测试装配）时 404——与备份/设置面收敛语义一致。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sisibeloved/Mosaic/internal/monitor"
)

type monitorCreateRequest struct {
	Kind        string   `json:"kind"`
	Target      string   `json:"target"`
	Args        []string `json:"args"`
	RoomID      string   `json:"room_id"`
	Assignee    string   `json:"assignee"`
	IntervalSec int      `json:"interval_sec"`
	Instruction string   `json:"instruction"`
	Enabled     *bool    `json:"enabled"`
}

// ListMonitors GET /v1/monitors。
func (s *server) ListMonitors(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Monitors == nil {
		writeError(w, http.StatusNotFound, "monitors_disabled", "本装配未启用监控面")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"monitors": s.deps.Monitors.List()})
}

// CreateMonitor POST /v1/monitors（写端点三层门）。
func (s *server) CreateMonitor(w http.ResponseWriter, r *http.Request) {
	if !s.guardWrite(w, r, true) {
		return
	}
	if s.deps.Monitors == nil {
		writeError(w, http.StatusNotFound, "monitors_disabled", "本装配未启用监控面")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req monitorCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "载荷不合法："+err.Error())
		return
	}
	src := monitor.Source{
		Kind: req.Kind, Target: req.Target, Args: req.Args,
		RoomID: req.RoomID, Assignee: req.Assignee,
		IntervalSec: req.IntervalSec, Instruction: req.Instruction,
		Enabled: req.Enabled == nil || *req.Enabled,
	}
	view, err := s.deps.Monitors.Add(src)
	if err != nil {
		if errors.Is(err, monitor.ErrInvalidSource) {
			writeError(w, http.StatusBadRequest, "invalid_monitor", err.Error())
			return
		}
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *server) deleteOrToggleMonitor(w http.ResponseWriter, r *http.Request, monitorID string, toggle *bool) {
	if !s.guardWrite(w, r, false) { // 空 body：只做跨源门
		return
	}
	if s.deps.Monitors == nil {
		writeError(w, http.StatusNotFound, "monitors_disabled", "本装配未启用监控面")
		return
	}
	var err error
	if toggle == nil {
		err = s.deps.Monitors.Remove(monitorID)
	} else {
		err = s.deps.Monitors.SetEnabled(monitorID, *toggle)
	}
	if err != nil {
		if errors.Is(err, monitor.ErrInvalidSource) {
			writeError(w, http.StatusNotFound, "monitor_not_found", err.Error())
			return
		}
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// DeleteMonitor DELETE /v1/monitors/{monitor_id}。
func (s *server) DeleteMonitor(w http.ResponseWriter, r *http.Request, monitorID string) {
	s.deleteOrToggleMonitor(w, r, monitorID, nil)
}

// EnableMonitor POST /v1/monitors/{monitor_id}/enable。
func (s *server) EnableMonitor(w http.ResponseWriter, r *http.Request, monitorID string) {
	t := true
	s.deleteOrToggleMonitor(w, r, monitorID, &t)
}

// DisableMonitor POST /v1/monitors/{monitor_id}/disable。
func (s *server) DisableMonitor(w http.ResponseWriter, r *http.Request, monitorID string) {
	f := false
	s.deleteOrToggleMonitor(w, r, monitorID, &f)
}

// CheckMonitor POST /v1/monitors/{monitor_id}/check：手动触发一次检查（忽略间隔）。
func (s *server) CheckMonitor(w http.ResponseWriter, r *http.Request, monitorID string) {
	if !s.guardWrite(w, r, false) {
		return
	}
	if s.deps.Monitors == nil {
		writeError(w, http.StatusNotFound, "monitors_disabled", "本装配未启用监控面")
		return
	}
	if err := s.deps.Monitors.CheckNow(r.Context(), monitorID); err != nil {
		if errors.Is(err, monitor.ErrInvalidSource) {
			writeError(w, http.StatusNotFound, "monitor_not_found", err.Error())
			return
		}
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "checked"})
}
