package billing

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMonthRangeBoundaries 账期是**闭区间**首日 00:00:00 ~ 末日 23:59:59，
// 且各月天数不同（二月、30 天月、31 天月）都要算对。
func TestMonthRangeBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		year, month int
		wantEndDay  int
	}{
		{"31 天月", 2026, 1, 31},
		{"平年二月", 2026, 2, 28},
		{"闰年二月", 2028, 2, 29},
		{"30 天月", 2026, 4, 30},
		{"十二月（跨年）", 2026, 12, 31},
		{"一月（年初）", 2026, 1, 31},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, err := MonthRange(tc.year, tc.month)
			require.NoError(t, err)

			assert.Equal(t, tc.year, start.Year())
			assert.Equal(t, tc.month, int(start.Month()))
			assert.Equal(t, 1, start.Day())
			assert.Equal(t, 0, start.Hour())
			assert.Equal(t, 0, start.Minute())
			assert.Equal(t, 0, start.Second())

			assert.Equal(t, tc.wantEndDay, end.Day(), "末日的日号要等于该月天数")
			assert.Equal(t, 23, end.Hour(), "闭区间右端是 23:59:59")
			assert.Equal(t, 59, end.Minute())
			assert.Equal(t, 59, end.Second())

			// 跨月：end 加一秒必须正好落到下月 1 号 00:00:00，
			// 这正是 ExportQueryEnd 转半开区间时依赖的性质。
			next := end.Add(time.Second)
			assert.Equal(t, 1, next.Day())
			assert.Equal(t, 0, next.Hour())
			assert.Equal(t, 0, next.Minute())
			assert.Equal(t, 0, next.Second())
		})
	}
}

// TestMonthRangeUsesCST 时区必须是固定的 +08:00，不是 UTC，也不是运行机器的本地时区。
//
// 这条是账单正确性的硬约束：容器多半跑 UTC，若按 UTC 算账期，
// 北京时间月初/月末各 8 小时的消费会被划进相邻月份，而账单上写的还是原账期。
func TestMonthRangeUsesCST(t *testing.T) {
	start, _, err := MonthRange(2026, 9)
	require.NoError(t, err)

	_, offset := start.Zone()
	assert.Equal(t, 8*3600, offset, "账期必须按北京时间 +08:00 构造")

	// 与 cstLocation 直接构造的结果逐纳秒相等。
	want := time.Date(2026, 9, 1, 0, 0, 0, 0, cstLocation)
	assert.True(t, start.Equal(want))

	// 与 UTC 口径确实不同：证明这个测试不是因为「反正都相等」而恒过。
	utcStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	assert.False(t, start.Equal(utcStart), "北京时间的月初不等同于 UTC 的月初")
	assert.Equal(t, 8*time.Hour, start.Sub(utcStart).Abs())
}

// TestMonthRangeRejectsBadInput 非法月份/年份要报错，不能静默算出别的月份。
//
// time.Date 对 month=13 会自动进位成次年 1 月——那种「静默纠错」正是账期错乱的来源。
func TestMonthRangeRejectsBadInput(t *testing.T) {
	for _, tc := range []struct{ year, month int }{
		{2026, 0}, {2026, 13}, {2026, -1}, {1800, 6}, {2500, 6},
	} {
		_, _, err := MonthRange(tc.year, tc.month)
		assert.Error(t, err, "year=%d month=%d 应当报错而不是静默进位", tc.year, tc.month)
	}
}

// TestPreviousMonth 默认账期「上月」按北京时间取年月。
func TestPreviousMonth(t *testing.T) {
	cases := []struct {
		name         string
		now          time.Time
		wantY, wantM int
	}{
		{
			"九月看八月",
			time.Date(2026, 9, 15, 10, 0, 0, 0, cstLocation), 2026, 8,
		},
		{
			"年初看去年十二月",
			time.Date(2026, 1, 5, 10, 0, 0, 0, cstLocation), 2025, 12,
		},
		{
			"三月一日看二月",
			time.Date(2026, 3, 1, 0, 0, 0, 0, cstLocation), 2026, 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y, m := PreviousMonth(tc.now)
			assert.Equal(t, tc.wantY, y)
			assert.Equal(t, tc.wantM, m)
		})
	}

	// 时区陷阱：北京时间 9 月 1 日 00:30 在 UTC 下还是 8 月 31 日。
	// 若用 UTC（或机器本地时区）算，「上月」会错成 7 月。
	beijingEarlySept := time.Date(2026, 9, 1, 0, 30, 0, 0, cstLocation)
	y, m := PreviousMonth(beijingEarlySept)
	assert.Equal(t, 2026, y)
	assert.Equal(t, 8, m, "必须按北京时间算，不能落到 7 月")
}

// TestPeriodLabel 展示标签格式固定为 YYYY-MM（月份补零）。
func TestPeriodLabel(t *testing.T) {
	assert.Equal(t, "2026-09", PeriodLabel(2026, 9))
	assert.Equal(t, "2026-12", PeriodLabel(2026, 12))
	assert.Equal(t, "2026-01", PeriodLabel(2026, 1))
}
