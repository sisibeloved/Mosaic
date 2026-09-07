// M4-0 附件面（RFC-0013）：上传（multipart，第一步）与下载。
// 上传是写端点（Origin + OwnerToken；Content-Type 是 multipart 非 JSON——
// 走 guardWrite 的 requireJSON=false 变体）；下载走读路径（与其余读一致）。
package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/sisibeloved/Mosaic/internal/attach"
)

// maxUploadOverhead multipart 包装开销余量（8MiB 体上限 + 边界/头部）。
const maxUploadOverhead = 64 << 10

// UploadRoomAttachment POST /v1/rooms/{room_id}/attachments（apigen.ServerInterface）。
func (s *server) UploadRoomAttachment(w http.ResponseWriter, r *http.Request, roomID string) {
	if !s.guardWrite(w, r, false) {
		return
	}
	if s.deps.Attachments == nil {
		writeError(w, http.StatusNotFound, "attachments_disabled", "本装配未启用附件面")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, attach.MaxFileBytes+maxUploadOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "multipart_required", "上传须为 multipart/form-data（字段 file）")
		return
	}
	var file io.Reader
	var declaredName, declaredMIME string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "multipart_invalid", err.Error())
			return
		}
		if part.FormName() != "file" {
			continue // 其余字段忽略（字段面保持封闭——元数据以服务端探测为准）
		}
		file = part
		declaredName = part.FileName()
		declaredMIME = part.Header.Get("Content-Type")
		break
	}
	if file == nil {
		writeError(w, http.StatusBadRequest, "file_field_required", "缺少 file 字段")
		return
	}
	meta, err := s.deps.Attachments.SaveIncoming(roomID, declaredName, declaredMIME, file)
	if err != nil {
		if errors.Is(err, attach.ErrTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "attachment_too_large", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "attachment_upload_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// DownloadRoomAttachment GET /v1/rooms/{room_id}/attachments/{attachment_id}。
// octet-stream 附件形态下发（存在性与哈希由 Store 校验；UI 卡片展示载荷中的
// 真实文件名，下载文件名用 id——描述子字段不在下载面重复信任）。
func (s *server) DownloadRoomAttachment(w http.ResponseWriter, _ *http.Request, roomID string, attachmentID string) {
	if s.deps.Attachments == nil {
		writeError(w, http.StatusNotFound, "attachments_disabled", "本装配未启用附件面")
		return
	}
	raw, _, err := s.deps.Attachments.Open(roomID, attachmentID)
	if err != nil {
		if errors.Is(err, attach.ErrNotFound) {
			writeError(w, http.StatusNotFound, "attachment_not_found", "附件不存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "attachment_read_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, attachmentID))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(raw)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
