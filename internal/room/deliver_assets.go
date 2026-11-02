package room

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/agent"
	"github.com/sisibeloved/Mosaic/internal/attach"
)

// AgentAttachmentStore 复用人工附件的配额、MIME、hash 与定稿语义。
type AgentAttachmentStore interface {
	SaveIncoming(roomID, declaredName, mimeHint string, r io.Reader) (attach.UploadMeta, error)
	Resolve(roomID string, tokens []string) ([]AttachmentDescriptor, error)
}

func (e *Engine) materializeDeliverAssets(ctx context.Context, roomID, participantID string, draft *agent.Result) {
	raw, ok := draft.Data["deliver_assets"]
	if !ok {
		return
	}
	delete(draft.Data, "deliver_assets")
	assets, err := agent.NormalizeDeliverAssets(raw)
	if err != nil {
		e.deliverAssetFailed(roomID, participantID, "交付文件", err, draft)
		return
	}
	if e.cfg.AgentAttachments == nil || e.cfg.Agents == nil {
		e.deliverAssetFailed(roomID, participantID, "交付文件", fmt.Errorf("附件面未装配"), draft)
		return
	}
	var profile agent.Profile
	found := false
	for _, seat := range e.cfg.Seats {
		if seat.ParticipantID == participantID {
			profile, found = seat.Profile, true
			break
		}
	}
	if !found {
		e.deliverAssetFailed(roomID, participantID, "交付文件", fmt.Errorf("座位已离席"), draft)
		return
	}
	descriptors := make([]AttachmentDescriptor, 0, len(assets))
	for _, asset := range assets {
		name := attach.SanitizeName(asset.Source)
		reader, err := e.cfg.Agents.ReadDeliverAsset(ctx, profile, asset.Source)
		if err != nil {
			e.deliverAssetFailed(roomID, participantID, name, err, draft)
			continue
		}
		meta, saveErr := e.cfg.AgentAttachments.SaveIncoming(roomID, name, "", reader)
		closeErr := reader.Close()
		if saveErr == nil && closeErr != nil {
			saveErr = closeErr
		}
		if saveErr != nil {
			e.deliverAssetFailed(roomID, participantID, name, saveErr, draft)
			continue
		}
		resolved, err := e.cfg.AgentAttachments.Resolve(roomID, []string{meta.Token})
		if err != nil || len(resolved) != 1 {
			if err == nil {
				err = fmt.Errorf("附件定稿数量异常")
			}
			e.deliverAssetFailed(roomID, participantID, name, err, draft)
			continue
		}
		caption := agent.RedactSecrets(agent.SanitizeBody(asset.Caption))
		if runes := []rune(caption); len(runes) > 500 {
			caption = string(runes[:500])
		}
		resolved[0].Caption = caption
		descriptors = append(descriptors, resolved[0])
	}
	if len(descriptors) > 0 {
		draft.Data["attachments"] = descriptors
	}
}

func (e *Engine) deliverAssetFailed(roomID, participantID, name string, err error, draft *agent.Result) {
	name = attach.SanitizeName(name)
	e.warn(roomID, "Agent 附件导入失败", "participant", participantID, "attachment", name, "err", err)
	body, _ := draft.Data["body"].(string)
	draft.Data["body"] = strings.TrimSpace(body + fmt.Sprintf("\n\n附件 %s 未能导入。", name))
}
