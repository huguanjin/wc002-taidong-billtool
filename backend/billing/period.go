package billing

import (
	"fmt"
	"time"
)

// 账期按月计算。账单模板里的「账期月份」是月粒度，所以任务链路只接受单个月，
// 不做跨月区间——跨月时一张账单标哪个账期都会有歧义，按月统计也会含糊。

// MonthRange 返回某账期的**闭区间**起止时刻：首日 00:00:00 到末日 23:59:59。
//
// 一律用 cstLocation（+08:00）构造，不用 time.Local：
// 容器通常跑 UTC，用本地时区算会把账期整体偏 8 小时，月初/月末那一两个小时的
// 消费会被划进相邻月份，而账单上的账期还写着原来的月份——对不上账。
//
// 闭区间与 LogExportParams 的时间语义一致（见 logexport.go）：
// EndTime 那一刻本身算在内，由 ExportQueryEnd 转成半开区间去查库。
func MonthRange(year, month int) (time.Time, time.Time, error) {
	if month < 1 || month > 12 {
		return time.Time{}, time.Time{}, fmt.Errorf("账期月份必须在 1~12 之间，收到 %d", month)
	}
	if year < 2000 || year > 2200 {
		return time.Time{}, time.Time{}, fmt.Errorf("账期年份不合理: %d", year)
	}

	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, cstLocation)
	// 下月首日往前一秒 = 本月最后一秒。用 AddDate 而不是 Add(30*24h)：
	// 各月天数不同，按天加会算错二月的账期边界。
	end := start.AddDate(0, 1, 0).Add(-time.Second)
	return start, end, nil
}

// PeriodLabel 账期的展示标签，如 "2026-09"。
func PeriodLabel(year, month int) string {
	return fmt.Sprintf("%d-%02d", year, month)
}

// PreviousMonth 相对某个时刻的上一个自然月，用于任务页默认值「上月整月」。
//
// 按 cstLocation 取「年月」，不用调用方机器的本地时区——服务器在 UTC 时，
// 北京时间 9 月 1 日凌晨属于「北美时间的 8 月」这个陷阱会让人拿到错的默认账期。
func PreviousMonth(now time.Time) (year, month int) {
	t := now.In(cstLocation)
	// 当月 1 号往前一天 = 上月任意一天，取其年月即可。
	prev := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, cstLocation).AddDate(0, 0, -1)
	return prev.Year(), int(prev.Month())
}

// PeriodFromStartTime 从一个时段的**开始时刻**推导归属账期（年、月）。
//
// 为什么用开始日而不是结束日：周度任务会跨月（8/28~9/3），两份口径都说得通，
// 定成开始月是为了让「这一周的账」落在它开始的账期里，与「这个月的账赚多少」
// 的直觉一致。跨月任务整个计入开始月，不拆分——拆了就要按天分配金额，
// 而账单本身是一份不可分的文件。
//
// 按 cstLocation 取月，不用 UTC：北京时间月初那 8 小时若按 UTC 算会落到上月。
func PeriodFromStartTime(start time.Time) (year, month int) {
	t := start.In(cstLocation)
	return t.Year(), int(t.Month())
}
