package billing

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRange() (start, end time.Time) {
	return time.Date(2026, 9, 1, 0, 0, 0, 0, cstLocation),
		time.Date(2026, 9, 7, 23, 59, 59, 0, cstLocation)
}

// TestValidateTasksForRun 批量执行前的整体校验：每条计划独立判定，
// 返回结果与输入**等长且同序**（前端要把错误对回表格行）。
func TestValidateTasksForRun(t *testing.T) {
	start, end := testRange()
	customers := map[int64]Customer{
		1: {ID: 1, Name: "有账号", Usernames: "a1\na2"},
		2: {ID: 2, Name: "没账号", Usernames: ""},
	}
	ratios := map[int]float64{101: 1.8}

	tasks := []BillTask{
		{ID: 10, CustomerID: 1, Name: "正常", StartTime: &start, EndTime: &end, GenerateCost: true},
		{ID: 11, CustomerID: 2, Name: "没账号", StartTime: &start, EndTime: &end},
		{ID: 12, CustomerID: 999, Name: "客户不存在", StartTime: &start, EndTime: &end},
		{ID: 13, CustomerID: 1, Name: "没时段"},
	}

	got := ValidateTasksForRun(tasks, customers, ratios)
	require.Len(t, got, 4, "必须与输入等长")

	// 顺序对应，否则前端无法把错误标到正确的行上。
	for i, wantID := range []int64{10, 11, 12, 13} {
		assert.Equal(t, wantID, got[i].TaskID, "第 %d 条应对应任务 %d", i, wantID)
	}

	assert.True(t, got[0].OK(), "正常计划应通过")

	assert.False(t, got[1].OK())
	assert.Contains(t, got[1].Error, "没有配置业务库账号")

	assert.False(t, got[2].OK())
	assert.Contains(t, got[2].Error, "客户不存在")

	assert.False(t, got[3].OK())
	assert.Contains(t, got[3].Error, "还没有设置时段")
}

// TestValidateTasksForRunCostWithoutRatios 勾了成本利润表但一个渠道倍率都没维护时
// 提前拦住——否则执行下去成本表会被整表拦下，白导一次日志。
func TestValidateTasksForRunCostWithoutRatios(t *testing.T) {
	start, end := testRange()
	customers := map[int64]Customer{1: {ID: 1, Name: "甲", Usernames: "a1"}}

	task := BillTask{ID: 1, CustomerID: 1, StartTime: &start, EndTime: &end, GenerateCost: true}

	// 没有任何渠道倍率 → 拦住。
	got := ValidateTasksForRun([]BillTask{task}, customers, map[int]float64{})
	require.Len(t, got, 1)
	assert.False(t, got[0].OK())
	assert.Contains(t, got[0].Error, "还没有维护任何渠道上游倍率")

	// 维护了至少一个 → 放行。部分维护是允许的：未维护的渠道会在成本表里留空并标注，
	// 不该因此拦住整个计划（否则新加的渠道会把所有计划都卡死）。
	got = ValidateTasksForRun([]BillTask{task}, customers, map[int]float64{101: 1.8})
	assert.True(t, got[0].OK(), "部分维护应放行")

	// 没勾成本表 → 与倍率无关。
	noCost := task
	noCost.GenerateCost = false
	got = ValidateTasksForRun([]BillTask{noCost}, customers, map[int]float64{})
	assert.True(t, got[0].OK(), "未勾成本表时不该检查渠道倍率")
}

// TestValidateTasksForRunBadRanges 时段的各种非法形态都要被拦，
// 且文案指向用户能做的动作。
func TestValidateTasksForRunBadRanges(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, cstLocation)
	customers := map[int64]Customer{1: {ID: 1, Name: "甲", Usernames: "a1"}}

	cases := []struct {
		name    string
		start   *time.Time
		end     *time.Time
		wantMsg string
	}{
		{
			name:    "结束早于开始",
			start:   &base,
			end:     timePtr(base.Add(-24 * time.Hour)),
			wantMsg: "结束时间早于开始时间",
		},
		{
			name:    "只有开始",
			start:   &base,
			end:     nil,
			wantMsg: "还没有设置时段",
		},
		{
			name:  "跨度超上限",
			start: &base,
			// 93 天 > MaxExportDays(92)
			end:     timePtr(base.Add(93 * 24 * time.Hour)),
			wantMsg: "超过上限",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := BillTask{ID: 1, CustomerID: 1, StartTime: tc.start, EndTime: tc.end}
			got := ValidateTasksForRun([]BillTask{task}, customers, map[int]float64{101: 1.8})
			require.Len(t, got, 1)
			assert.False(t, got[0].OK())
			assert.Contains(t, got[0].Error, tc.wantMsg)
		})
	}
}

// TestValidateTasksForRunEmpty 空输入返回空切片，不 panic。
func TestValidateTasksForRunEmpty(t *testing.T) {
	got := ValidateTasksForRun(nil, nil, nil)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

// TestPeriodFromStartTime 归属账期由**开始日**推导，跨月任务整个计入开始月。
func TestPeriodFromStartTime(t *testing.T) {
	cases := []struct {
		name         string
		start        time.Time
		wantY, wantM int
	}{
		{
			"月初", time.Date(2026, 9, 1, 0, 0, 0, 0, cstLocation), 2026, 9,
		},
		{
			"月末", time.Date(2026, 9, 30, 23, 59, 59, 0, cstLocation), 2026, 9,
		},
		{
			// 跨月任务（8/28~9/3）：整个计入开始月 8 月。
			"跨月取开始月", time.Date(2026, 8, 28, 0, 0, 0, 0, cstLocation), 2026, 8,
		},
		{
			"跨年", time.Date(2025, 12, 31, 12, 0, 0, 0, cstLocation), 2025, 12,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y, m := PeriodFromStartTime(tc.start)
			assert.Equal(t, tc.wantY, y)
			assert.Equal(t, tc.wantM, m)
		})
	}
}

// TestPeriodFromStartTimeCST 时区必须是 +08:00。
//
// 陷阱：北京时间 9 月 1 日 00:30，用 UTC 看还是 8 月 31 日——
// 若按 UTC 推导，月初那 8 小时的账会被归到上个月。
func TestPeriodFromStartTimeCST(t *testing.T) {
	beijingEarlySept := time.Date(2026, 9, 1, 0, 30, 0, 0, cstLocation)
	y, m := PeriodFromStartTime(beijingEarlySept)
	assert.Equal(t, 2026, y)
	assert.Equal(t, 9, m, "必须按北京时间算")

	// 反证：同一瞬间若按 UTC 取月会得到 8 月。断言函数给出的**不是**那个月，
	// 这样才能证明时区处理真的生效了，而不是「碰巧两个口径一致」。
	utcMonth := int(beijingEarlySept.UTC().Month())
	assert.Equal(t, 8, utcMonth, "这个瞬间在 UTC 下确实是 8 月（构造这个反证的前提）")
	assert.NotEqual(t, utcMonth, m, "推导结果不能等于 UTC 口径的月份")

	// 反过来的边界：北京时间 9 月 1 日 07:59 在 UTC 下已经是 8 月 31 日 23:59，
	// 但北京时间仍是 9 月——依然必须是 9 月。
	beforeShift := time.Date(2026, 9, 1, 7, 59, 0, 0, cstLocation)
	y2, m2 := PeriodFromStartTime(beforeShift)
	assert.Equal(t, 2026, y2)
	assert.Equal(t, 9, m2)
}

func timePtr(t time.Time) *time.Time { return &t }
