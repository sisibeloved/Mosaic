// opaque cursor 编解码：全局位（存储内部序号）的对外不透明编码。
// 属协议层关注点：外部只见 v1 前缀 base64，不得泄露 seq 语义（RFC-0001 P0）。
package protocol

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// EncodeCursor 把全局位编码为对外不透明游标（v1 前缀留作格式演进）。
func EncodeCursor(globalPos int64) string {
	return base64.URLEncoding.EncodeToString([]byte("v1:" + strconv.FormatInt(globalPos, 10)))
}

// DecodeCursor 解析不透明游标；空串返回 0（从头开始）。非法格式报错（不静默从头）。
func DecodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("protocol: 非法游标: %w", err)
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 || parts[0] != "v1" {
		return 0, fmt.Errorf("protocol: 无法识别的游标版本: %q", cursor)
	}
	pos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || pos < 0 {
		return 0, fmt.Errorf("protocol: 游标位非法: %q", cursor)
	}
	return pos, nil
}
