// A1 坐标工具：1-based 行列整数是权威表达；A1 语法只出现在读端点参数与公式
// 引用里（列标 A..GR = 1..MaxCols）。
package sheet

import (
	"fmt"
	"strings"
)

// Coord 单元格坐标（1-based）。
type Coord struct {
	R int `json:"r"`
	C int `json:"c"`
}

// ColName 列号 → 列标（1→A、26→Z、27→AA、200→GR）。
func ColName(c int) string {
	if c < 1 || c > MaxCols {
		return ""
	}
	var buf [4]byte
	n := len(buf)
	for c > 0 {
		c--
		n--
		buf[n] = byte('A' + c%26)
		c /= 26
	}
	return string(buf[n:])
}

// ColIndex 列标 → 列号（非法返回 0）。
func ColIndex(name string) int {
	if name == "" || len(name) > 3 {
		return 0
	}
	n := 0
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if ch >= 'a' && ch <= 'z' {
			ch -= 'a' - 'A'
		}
		if ch < 'A' || ch > 'Z' {
			return 0
		}
		n = n*26 + int(ch-'A'+1)
	}
	if n > MaxCols {
		return 0
	}
	return n
}

// A1Of 坐标 → "B7" 形态（越界返回空串）。
func A1Of(r, c int) string {
	name := ColName(c)
	if name == "" || r < 1 || r > MaxRows {
		return ""
	}
	return fmt.Sprintf("%s%d", name, r)
}

// ParseA1 "B7" → 坐标（行列越界或语法非法返回 ok=false）。
func ParseA1(s string) (Coord, bool) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) {
		ch := s[i]
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' {
			i++
			continue
		}
		break
	}
	col := ColIndex(s[:i])
	row := 0
	for _, ch := range s[i:] {
		if ch < '0' || ch > '9' {
			return Coord{}, false
		}
		row = row*10 + int(ch-'0')
		if row > MaxRows {
			return Coord{}, false
		}
	}
	if col == 0 || row == 0 || i == 0 || i == len(s) {
		return Coord{}, false
	}
	return Coord{R: row, C: col}, true
}

// ParseRange "A1:C10" → 四角（端点自动规整为左上/右下；任一端非法失败）。
func ParseRange(s string) (r1, c1, r2, c2 int, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, 0, 0, 0, false
	}
	a, ok1 := ParseA1(parts[0])
	b, ok2 := ParseA1(parts[1])
	if !ok1 || !ok2 {
		return 0, 0, 0, 0, false
	}
	r1, c1, r2, c2 = a.R, a.C, b.R, b.C
	if r1 > r2 {
		r1, r2 = r2, r1
	}
	if c1 > c2 {
		c1, c2 = c2, c1
	}
	return r1, c1, r2, c2, true
}
