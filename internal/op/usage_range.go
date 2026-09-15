package op

import (
	"time"

	"gorm.io/gorm"
)

// ShanghaiLoc 上海时区（无夏令时，固定 +08:00；用 FixedZone 避免依赖系统 tzdata）。
var ShanghaiLoc = time.FixedZone("Asia/Shanghai", 8*3600)

// ParseUsageTime 解析用量区间参数：支持 RFC3339（含时区）或 YYYY-MM-DD（按上海自然日 00:00）。
func ParseUsageTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02", s, ShanghaiLoc)
}

// ApplyUsageRange 区间过滤：from/to 优先；两者均缺省时回退 days（0=全部，滚动窗口）。
// col 需带表前缀（平台侧聚合 join users 时用 usage_logs.created_at）。
func ApplyUsageRange(q *gorm.DB, col string, days int, from, to *time.Time) *gorm.DB {
	if from != nil {
		q = q.Where(col+" >= ?", *from)
	}
	if to != nil {
		q = q.Where(col+" < ?", *to)
	}
	if from == nil && to == nil && days > 0 {
		q = q.Where(col+" > NOW() - ? * INTERVAL '1 day'", days)
	}
	return q
}
