package slot

import "time"

// nowUTC 缺省时钟（RFC3339Nano UTC）。
func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }
