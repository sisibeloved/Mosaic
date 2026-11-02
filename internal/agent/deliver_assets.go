package agent

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	MaxDeliverAssets       = 4
	maxDeliverAssetSource  = 4096
	maxDeliverAssetCaption = 500
)

// DeliverAsset 是适配器返回的工作区产物引用；只有域层物化后才成为消息附件。
type DeliverAsset struct {
	Type    string `json:"type"`
	Source  string `json:"src"`
	Caption string `json:"caption,omitempty"`
}

// NormalizeDeliverAssets 校验并统一 Go 原生与 JSON 解码两种侧车形状。
func NormalizeDeliverAssets(raw any) ([]DeliverAsset, error) {
	var assets []DeliverAsset
	switch values := raw.(type) {
	case []DeliverAsset:
		assets = append(assets, values...)
	case []any:
		for i, value := range values {
			switch item := value.(type) {
			case DeliverAsset:
				assets = append(assets, item)
			case map[string]any:
				asset := DeliverAsset{}
				asset.Type, _ = item["type"].(string)
				asset.Source, _ = item["src"].(string)
				asset.Caption, _ = item["caption"].(string)
				assets = append(assets, asset)
			default:
				return nil, fmt.Errorf("agent: deliver_assets[%d] 非对象", i)
			}
		}
	default:
		return nil, fmt.Errorf("agent: public_draft.deliver_assets 必须为数组")
	}
	if len(assets) > MaxDeliverAssets {
		return nil, fmt.Errorf("agent: deliver_assets 超单消息上限 %d", MaxDeliverAssets)
	}
	for i, asset := range assets {
		switch asset.Type {
		case "file", "image", "video", "audio":
		default:
			return nil, fmt.Errorf("agent: deliver_assets[%d].type 非法 %q", i, asset.Type)
		}
		if strings.TrimSpace(asset.Source) == "" || len([]rune(asset.Source)) > maxDeliverAssetSource {
			return nil, fmt.Errorf("agent: deliver_assets[%d].src 缺失或超长", i)
		}
		for _, r := range asset.Source {
			if unicode.IsControl(r) {
				return nil, fmt.Errorf("agent: deliver_assets[%d].src 含控制字符", i)
			}
		}
		if len([]rune(asset.Caption)) > maxDeliverAssetCaption {
			return nil, fmt.Errorf("agent: deliver_assets[%d].caption 超 %d 字", i, maxDeliverAssetCaption)
		}
	}
	return assets, nil
}
