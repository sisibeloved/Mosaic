// 表格交换共享日期工具：云端表格普遍采用 Excel 1900 日期系统——序列数整数
// 部分为自 1899-12-30 起的天数，小数部分为当日时间份额（M0 双平台实测）。
// 云端日期无时区信息，统一按 UTC 记（导入报告注记）。
package extdocs

import (
	"math"
	"time"
)

// excelEpoch 1900 日期系统纪元原点。
var excelEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

// excelSerialToTime 序列数 → time（截断到秒）。
func excelSerialToTime(serial float64) time.Time {
	days, frac := math.Modf(serial)
	secs := math.Round(frac * 86400)
	return excelEpoch.AddDate(0, 0, int(days)).Add(time.Duration(secs) * time.Second).UTC()
}

// timeToExcelSerial time → 序列数（秒级精度）。
func timeToExcelSerial(t time.Time) float64 {
	d := t.UTC().Sub(excelEpoch)
	days := math.Floor(d.Hours() / 24)
	frac := d - time.Duration(days)*24*time.Hour
	return days + frac.Seconds()/86400
}

// cloudDateTimeFormats 云端 TIME_STRING / 文本日期的尽力解析格式集。
var cloudDateTimeFormats = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006/01/02 15:04:05",
	"2006-01-02 15:04",
	"2006/01/02 15:04",
	"2006-01-02",
	"2006/01/02",
}

// parseCloudDateTime 云端日期文本尽力解析（失败 false——调用方按文本回退）。
func parseCloudDateTime(s string) (time.Time, bool) {
	for _, layout := range cloudDateTimeFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
