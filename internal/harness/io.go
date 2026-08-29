// 标准库薄封装：registry.go 的可读性辅助（避免实现文件塞满 import）。
package harness

import (
	"encoding/json"
	"os"
)

func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
func osWriteFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}
func osRename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
func osIsNotExist(err error) bool            { return os.IsNotExist(err) }

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}
