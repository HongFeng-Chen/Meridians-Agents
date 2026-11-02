package agentfactory

// 极简 5 段 cron 解析器：分 时 日 月 周。
//
// 支持：
//   *       任意值
//   */n     每 n 个单位
//   a-b     区间
//   a-b/n   区间步长
//   a,b,c   枚举
//
// 周字段：0/7 = 周日，1 = 周一 … 6 = 周六。
// 不引入第三方依赖，够覆盖「每天 9 点」「每 10 分钟」这类后台自动运行场景。

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronSpec 解析后的 cron 规格
type CronSpec struct {
	Minute  []bool // 0-59
	Hour    []bool // 0-23
	Dom     []bool // 1-31
	Month   []bool // 1-12
	Dow     []bool // 0-6
	Expr    string
	DomStar bool // 日字段是否为 *（cron 语义：日与周同时受限时取“或”）
	DowStar bool
}

// ParseCron 解析 5 段表达式
func ParseCron(expr string) (*CronSpec, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 需要 5 段（分 时 日 月 周），当前 %d 段", len(fields))
	}
	spec := &CronSpec{
		Minute:  make([]bool, 60),
		Hour:    make([]bool, 24),
		Dom:     make([]bool, 32),
		Month:   make([]bool, 13),
		Dow:     make([]bool, 8), // 0~7，7 与 0 同为周日
		Expr:    strings.TrimSpace(expr),
		DomStar: fields[2] == "*",
		DowStar: fields[4] == "*",
	}
	if err := fillField(spec.Minute, fields[0], 0, 59, "分"); err != nil {
		return nil, err
	}
	if err := fillField(spec.Hour, fields[1], 0, 23, "时"); err != nil {
		return nil, err
	}
	if err := fillField(spec.Dom, fields[2], 1, 31, "日"); err != nil {
		return nil, err
	}
	if err := fillField(spec.Month, fields[3], 1, 12, "月"); err != nil {
		return nil, err
	}
	if err := fillField(spec.Dow, fields[4], 0, 7, "周"); err != nil {
		return nil, err
	}
	// 7 与 0 等价（周日）
	if spec.Dow[7] {
		spec.Dow[0] = true
	}
	return spec, nil
}

// fillField 解析单个字段到位图
func fillField(dst []bool, field string, min, max int, name string) error {
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("%s段存在空值", name)
		}
		step := 1
		base := part
		if i := strings.Index(part, "/"); i >= 0 {
			base = part[:i]
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return fmt.Errorf("%s段步长不合法：%s", name, part)
			}
			step = n
		}
		lo, hi := min, max
		switch {
		case base == "*":
			// 全区间
		case strings.Contains(base, "-"):
			segs := strings.SplitN(base, "-", 2)
			a, err1 := strconv.Atoi(strings.TrimSpace(segs[0]))
			b, err2 := strconv.Atoi(strings.TrimSpace(segs[1]))
			if err1 != nil || err2 != nil || a > b {
				return fmt.Errorf("%s段区间不合法：%s", name, part)
			}
			lo, hi = a, b
		default:
			v, err := strconv.Atoi(base)
			if err != nil {
				return fmt.Errorf("%s段取值不合法：%s", name, part)
			}
			lo, hi = v, v
		}
		if lo < min || hi > max {
			return fmt.Errorf("%s段超出范围 %d~%d：%s", name, min, max, part)
		}
		for v := lo; v <= hi; v += step {
			dst[v] = true
		}
	}
	return nil
}

// Match 判断给定时刻是否命中
func (s *CronSpec) Match(t time.Time) bool {
	if !s.Minute[t.Minute()] || !s.Hour[t.Hour()] || !s.Month[int(t.Month())] {
		return false
	}
	domOK := s.Dom[t.Day()]
	dowOK := s.Dow[int(t.Weekday())]
	if s.DomStar || s.DowStar {
		return domOK && dowOK
	}
	return domOK || dowOK
}

// Next 返回严格晚于 after 的下一次触发时间（最多向后找 2 年）
func (s *CronSpec) Next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := after.AddDate(2, 0, 0)
	for !t.After(limit) {
		if s.Match(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

// ValidateCron 仅校验表达式合法性
func ValidateCron(expr string) error {
	_, err := ParseCron(expr)
	return err
}
