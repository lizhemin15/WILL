package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NextCronRun 计算 cron 表达式下一次执行时间（5字段标准 cron: min hour dom month dow）
// 支持：* */n a,b,c a-b
func NextCronRun(expr string, from time.Time) (time.Time, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return time.Time{}, fmt.Errorf("cron 表达式需要5个字段（分 时 日 月 周），收到：%q", expr)
	}
	mins, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return time.Time{}, fmt.Errorf("分钟字段错误: %w", err)
	}
	hours, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return time.Time{}, fmt.Errorf("小时字段错误: %w", err)
	}
	doms, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return time.Time{}, fmt.Errorf("日期字段错误: %w", err)
	}
	months, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return time.Time{}, fmt.Errorf("月份字段错误: %w", err)
	}
	dows, err := parseCronField(fields[4], 0, 6)
	if err != nil {
		return time.Time{}, fmt.Errorf("星期字段错误: %w", err)
	}

	// dom 和 dow 均为 * 时只检查 dom；若任一非 * 则满足其一即可
	domStar := strings.TrimSpace(fields[2]) == "*"
	dowStar := strings.TrimSpace(fields[4]) == "*"

	// 从 from+1min 开始搜索，最多向前一年
	t := from.Add(time.Minute).Truncate(time.Minute)
	limit := from.Add(366 * 24 * time.Hour)
	for t.Before(limit) {
		if !months[int(t.Month())] {
			// 跳到下个月1日 00:00
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		domMatch := doms[t.Day()]
		dowMatch := dows[int(t.Weekday())]
		var dayOK bool
		if domStar && dowStar {
			dayOK = true
		} else if domStar {
			dayOK = dowMatch
		} else if dowStar {
			dayOK = domMatch
		} else {
			dayOK = domMatch || dowMatch
		}
		if !dayOK {
			// 跳到明天 00:00
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !hours[t.Hour()] {
			// 跳到下一个小时
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if !mins[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cron %q 在一年内找不到下次运行时间", expr)
}

func parseCronField(field string, min, max int) (map[int]bool, error) {
	result := make(map[int]bool)
	if field == "*" {
		for i := min; i <= max; i++ {
			result[i] = true
		}
		return result, nil
	}
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "*/") {
			n, err := strconv.Atoi(part[2:])
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("无效步长 %q", part)
			}
			for i := min; i <= max; i += n {
				result[i] = true
			}
		} else if idx := strings.Index(part, "-"); idx > 0 {
			a, err1 := strconv.Atoi(part[:idx])
			b, err2 := strconv.Atoi(part[idx+1:])
			if err1 != nil || err2 != nil || a > b || a < min || b > max {
				return nil, fmt.Errorf("无效范围 %q", part)
			}
			for i := a; i <= b; i++ {
				result[i] = true
			}
		} else {
			n, err := strconv.Atoi(part)
			if err != nil || n < min || n > max {
				return nil, fmt.Errorf("无效值 %q（范围 %d-%d）", part, min, max)
			}
			result[n] = true
		}
	}
	return result, nil
}

// CronDescription 将 cron 表达式转为人类可读描述
func CronDescription(expr string) string {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return expr
	}
	min, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]

	if month == "*" && dom == "*" && dow == "*" {
		if min == "0" {
			if strings.HasPrefix(hour, "*/") {
				return "每" + hour[2:] + "小时"
			}
			if !strings.ContainsAny(hour, "*,/-") {
				return "每天 " + hour + ":00"
			}
		}
		if !strings.ContainsAny(min, "*,/-") && !strings.ContainsAny(hour, "*,/-") {
			h, _ := strconv.Atoi(hour)
			m, _ := strconv.Atoi(min)
			return fmt.Sprintf("每天 %02d:%02d", h, m)
		}
		if !strings.ContainsAny(min, "*,/-") && strings.Contains(hour, ",") {
			m, _ := strconv.Atoi(min)
			times := strings.Split(hour, ",")
			var parts []string
			for _, t := range times {
				h, _ := strconv.Atoi(strings.TrimSpace(t))
				parts = append(parts, fmt.Sprintf("%02d:%02d", h, m))
			}
			return "每天 " + strings.Join(parts, "、")
		}
	}
	return expr
}
