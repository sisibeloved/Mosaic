package minimax

import (
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/sisibeloved/Mosaic/internal/agent"
)

type deliverAssetsDocument struct {
	XMLName xml.Name            `xml:"deliver-assets"`
	Media   []deliverAssetMedia `xml:"media"`
}

type deliverAssetMedia struct {
	Type    string `xml:"type,attr"`
	Source  string `xml:"src,attr"`
	Caption string `xml:"caption,attr"`
}

// splitDeliverAssets 把 mcode 原生交付协议从公开正文中分离；标签本身永不入房。
func splitDeliverAssets(body string) (string, []agent.DeliverAsset, error) {
	const (
		openTag  = "<deliver-assets>"
		closeTag = "</deliver-assets>"
	)
	rest := body
	var clean strings.Builder
	var assets []agent.DeliverAsset
	for {
		start := strings.Index(rest, openTag)
		if start < 0 {
			clean.WriteString(rest)
			break
		}
		end := strings.Index(rest[start+len(openTag):], closeTag)
		if end < 0 {
			return "", nil, fmt.Errorf("minimax: deliver-assets 未闭合")
		}
		end += start + len(openTag) + len(closeTag)
		clean.WriteString(rest[:start])
		var doc deliverAssetsDocument
		if err := xml.Unmarshal([]byte(rest[start:end]), &doc); err != nil {
			return "", nil, fmt.Errorf("minimax: deliver-assets XML 无效: %w", err)
		}
		for _, media := range doc.Media {
			assets = append(assets, agent.DeliverAsset{
				Type:    media.Type,
				Source:  strings.TrimSpace(media.Source),
				Caption: agent.RedactSecrets(agent.SanitizeBody(media.Caption)),
			})
		}
		rest = rest[end:]
	}
	if strings.Contains(clean.String(), closeTag) {
		return "", nil, fmt.Errorf("minimax: deliver-assets 闭合标签位置无效")
	}
	normalized, err := agent.NormalizeDeliverAssets(assets)
	if err != nil {
		return "", nil, fmt.Errorf("minimax: %w", err)
	}
	text := strings.TrimSpace(clean.String())
	if text == "" && len(normalized) > 0 {
		captions := make([]string, 0, len(normalized))
		for _, asset := range normalized {
			if asset.Caption != "" {
				captions = append(captions, asset.Caption)
			}
		}
		text = strings.Join(captions, "\n")
		if text == "" {
			return "", nil, fmt.Errorf("minimax: 纯附件消息缺少正文或 caption")
		}
	}
	return text, normalized, nil
}
