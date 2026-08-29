//go:build it

package codexadapter

import "os"

func osGetenv(key string) string              { return os.Getenv(key) }
func osStat(path string) (os.FileInfo, error) { return os.Stat(path) }
