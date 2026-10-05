package billing

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fptr(v float64) *float64 { return &v }

// runAt 造一个「已执行过」的计划所需的时间戳。
func runAt() *time.Time {
	t := time.Date(2026, 9, 15, 10, 0, 0, 0, cstLocation)
	return &t
}

// TestSummarizeTasksProfitUsesCostedSettle 汇总口径的核心：利润必须用
// **参与成本核算的结算额**（costed_settle）减去成本，不能用全部结算额（settle）。
//
// 构造一个刻意让两者不等的场景：部分渠道没维护倍率，于是
//
//	settle_cny        = 10000（客户要付的全部）
//	costed_settle_cny =  6000（成本能对应上的那部分）
//	cost_cny          =  4000
//
// 正确利润 = 6000 − 4000 = 2000。
// 若错写成 10000 − 4000 = 6000，就是把「没算成本的 4000 元消费」当成零成本，虚高 3 倍。
func TestSummarizeTasksProfitUsesCostedSettle(t *testing.T) {
	tasks := []BillTask{{
		CustomerName: "某客户", PeriodYear: 2026, PeriodMonth: 9,
		SettleCNY: fptr(10000), CostedSettleCNY: fptr(6000), CostCNY: fptr(4000),
		ProfitCNY: fptr(2000), CostComplete: false,
		PricedRows: 8, TotalCostRows: 12,
		LastRunAt: runAt(),
	}}

	sums := SummarizeTasks(tasks)
	require.Len(t, sums, 1)
	s := sums[0]

	assert.Equal(t, 10000.0, s.SettleCNY, "对外结算额仍是全部行")
	assert.Equal(t, 6000.0, s.CostedSettleCNY)
	assert.Equal(t, 4000.0, s.CostCNY)
	assert.Equal(t, 2000.0, s.ProfitCNY, "利润必须用 costed_settle − cost")
	assert.Equal(t, 1, s.TaskCount)
	assert.Equal(t, 1, s.PartialCostCount, "有成本但覆盖不全，要计入 partial")
	assert.Equal(t, 0, s.MissingCostCount)

	// 反证：错误算法会得到 6000，明确断言不是它。
	assert.NotEqual(t, 6000.0, s.ProfitCNY,
		"用全部结算额减部分成本会虚高；这个断言就是在挡那种写法")

	// 毛利率的分母必须与利润同口径（costed_settle），不是全部结算额。
	assert.InDelta(t, 2000.0/6000.0*100, s.Margin, 0.01)
	wrongMargin := 2000.0 / 10000.0 * 100
	assert.Greater(t, math.Abs(s.Margin-wrongMargin), 0.01,
		"毛利率分母若用全部结算额会得到 %.2f，与正确值明显不同", wrongMargin)
}

// TestSummarizeTasksSkipsUnrunPlans 回归：**还没执行过的计划不计入汇总**。
//
// 计划可以「先建好、之后批量执行」，所以列表里必然存在一堆没跑过的。
// 它们没有金额，计进 TaskCount 会让「这个月导了几张账单」虚高，
// 而且呈现成「3 张账单、金额 0」这种自相矛盾的行。
func TestSummarizeTasksSkipsUnrunPlans(t *testing.T) {
	tasks := []BillTask{
		{
			Name: "9月整月", CustomerName: "甲", PeriodYear: 2026, PeriodMonth: 9,
			SettleCNY: fptr(1000), CostedSettleCNY: fptr(1000), CostCNY: fptr(400),
			ProfitCNY: fptr(600), CostComplete: true, LastRunAt: runAt(),
		},
		{
			// 同一账期的另一个计划，还没跑：时段设了，但没有任何金额。
			Name: "9月第1周", CustomerName: "甲", PeriodYear: 2026, PeriodMonth: 9,
			// 三个成本字段与结算额都是 nil
		},
	}

	sums := SummarizeTasks(tasks)
	require.Len(t, sums, 1)
	s := sums[0]

	assert.Equal(t, 1, s.TaskCount, "只数已执行的")
	assert.Equal(t, 1000.0, s.SettleCNY)
	assert.Equal(t, 400.0, s.CostCNY)
	assert.Equal(t, 600.0, s.ProfitCNY)
	assert.Equal(t, 1, s.UnrunCount, "未执行的单独计数")
	assert.Equal(t, 0, s.MissingCostCount,
		"未执行 ≠ 缺成本：前者根本没跑，后者跑了但没算出成本")

	// 反证：若把未执行的也算进 TaskCount，这里会是 2。
	assert.NotEqual(t, 2, s.TaskCount)
}

// TestSummarizeTasksSkipsTasksWithoutCost 执行过但没有成本口径的计划
// （未勾选成本表、或渠道倍率被拦）**一个数都不该进成本链路**。
func TestSummarizeTasksSkipsTasksWithoutCost(t *testing.T) {
	tasks := []BillTask{
		{
			CustomerName: "甲", PeriodYear: 2026, PeriodMonth: 9,
			SettleCNY: fptr(1000), CostedSettleCNY: fptr(1000), CostCNY: fptr(400),
			ProfitCNY: fptr(600), CostComplete: true, LastRunAt: runAt(),
		},
		{
			// 执行过、只有结算额，没有成本三件套。
			CustomerName: "乙", PeriodYear: 2026, PeriodMonth: 9,
			SettleCNY: fptr(8000), LastRunAt: runAt(),
		},
	}

	sums := SummarizeTasks(tasks)
	require.Len(t, sums, 1)
	s := sums[0]

	assert.Equal(t, 2, s.TaskCount)
	assert.Equal(t, 9000.0, s.SettleCNY, "两个任务的结算额都要计入对外口径")
	assert.Equal(t, 400.0, s.CostCNY, "只有有成本的那个贡献成本")
	assert.Equal(t, 1000.0, s.CostedSettleCNY, "同口径结算额只含那一个任务")
	assert.Equal(t, 600.0, s.ProfitCNY)

	assert.Equal(t, 1, s.MissingCostCount, "缺成本的任务数要报出来")
	assert.Equal(t, 0, s.PartialCostCount, "它不算 partial——它压根没成本")
	assert.Equal(t, 0, s.UnrunCount)

	// 关键反证：若把 8000 也算进利润链路，利润会变成 8600。
	assert.NotEqual(t, 8600.0, s.ProfitCNY, "无成本任务的结算额不能进利润计算")
}

// TestSummarizeTasksZeroCostTask 上游倍率维护成 0（上游免费）是**有效成本**，
// 必须与「没有成本」区分开：0 表示真的不花钱，nil 表示不知道。
func TestSummarizeTasksZeroCostTask(t *testing.T) {
	tasks := []BillTask{{
		CustomerName: "免费渠道客户", PeriodYear: 2026, PeriodMonth: 9,
		SettleCNY: fptr(500), CostedSettleCNY: fptr(500), CostCNY: fptr(0),
		ProfitCNY: fptr(500), CostComplete: true, LastRunAt: runAt(),
	}}

	s := SummarizeTasks(tasks)[0]
	assert.Equal(t, 0.0, s.CostCNY)
	assert.Equal(t, 500.0, s.ProfitCNY, "成本为 0 时利润等于结算额")
	assert.Equal(t, 0, s.MissingCostCount, "成本 0 是有效成本，不是缺成本")
	assert.Equal(t, 0, s.PartialCostCount)
}

// TestSummarizeTasksGroupsByPeriod 按 (年,月) 分组，不同账期不能混在一起；
// 结果按账期倒序（最新在前），与计划列表的排序一致。
func TestSummarizeTasksGroupsByPeriod(t *testing.T) {
	mk := func(name string, y, m int, settle, cost float64) BillTask {
		profit := settle - cost
		return BillTask{
			CustomerName: name, PeriodYear: y, PeriodMonth: m,
			SettleCNY: fptr(settle), CostedSettleCNY: fptr(settle), CostCNY: fptr(cost),
			ProfitCNY: fptr(profit), CostComplete: true, LastRunAt: runAt(),
		}
	}
	tasks := []BillTask{
		mk("甲", 2026, 8, 100, 50),
		mk("乙", 2026, 9, 200, 100),
		mk("丙", 2025, 12, 300, 150),
	}

	sums := SummarizeTasks(tasks)
	require.Len(t, sums, 3, "三个不同账期应各成一组")

	// 倒序：2026-09 → 2026-08 → 2025-12
	assert.Equal(t, 2026, sums[0].Year)
	assert.Equal(t, 9, sums[0].Month)
	assert.Equal(t, 2026, sums[1].Year)
	assert.Equal(t, 8, sums[1].Month)
	assert.Equal(t, 2025, sums[2].Year)
	assert.Equal(t, 12, sums[2].Month, "跨年排序要按年月一起比")

	// 每个账期各自成组，金额互不串味：利润 = costed_settle − cost。
	for i, wantProfit := range []float64{100, 50, 150} {
		assert.Equal(t, 1, sums[i].TaskCount)
		assert.Equal(t, wantProfit, sums[i].ProfitCNY,
			"账期 %d-%02d 的利润应只含本组任务", sums[i].Year, sums[i].Month)
	}
}

// TestSummarizeTasksEmpty 空输入返回空切片，不是 nil、更不能 panic。
func TestSummarizeTasksEmpty(t *testing.T) {
	sums := SummarizeTasks(nil)
	assert.NotNil(t, sums)
	assert.Empty(t, sums)

	sums = SummarizeTasks([]BillTask{})
	assert.Empty(t, sums)

	// 只有未执行计划时：汇总行存在但不含任何金额。
	sums = SummarizeTasks([]BillTask{{PeriodYear: 2026, PeriodMonth: 9}})
	require.Len(t, sums, 1)
	assert.Equal(t, 0, sums[0].TaskCount)
	assert.Equal(t, 1, sums[0].UnrunCount)
	assert.Equal(t, 0.0, sums[0].SettleCNY)
}

// TestSummarizeTasksNoDivisionByZero 结算额为 0 时毛利率不该出现 NaN/Inf
// （JSON 序列化 NaN 会直接报错，把接口打挂）。
func TestSummarizeTasksNoDivisionByZero(t *testing.T) {
	tasks := []BillTask{{
		CustomerName: "零消费", PeriodYear: 2026, PeriodMonth: 9,
		SettleCNY: fptr(0), CostedSettleCNY: fptr(0), CostCNY: fptr(0),
		ProfitCNY: fptr(0), CostComplete: true, LastRunAt: runAt(),
	}}
	s := SummarizeTasks(tasks)[0]
	assert.Equal(t, 0.0, s.Margin)
	assert.False(t, s.Margin != s.Margin, "毛利率不能是 NaN")
}

// TestBillTaskDisplayName 计划没起名时用「客户 + 时段」兜底，
// 让列表里每条都读得懂，而不是一排空白。
func TestBillTaskDisplayName(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, cstLocation)
	end := time.Date(2026, 9, 7, 23, 59, 59, 0, cstLocation)

	withName := BillTask{Name: "9月第1周", CustomerName: "示例科技", StartTime: &start, EndTime: &end}
	assert.Equal(t, "9月第1周", withName.DisplayName())

	noName := BillTask{CustomerName: "示例科技", StartTime: &start, EndTime: &end}
	assert.Equal(t, "示例科技 09-01~09-07", noName.DisplayName())

	noBoth := BillTask{CustomerName: "示例科技"}
	assert.Equal(t, "示例科技", noBoth.DisplayName(), "连时段都没有时至少给出客户名")
}

// TestBillTaskWallClockJSON 时段以**北京时间墙上时间**字符串暴露给前端。
//
// 这是前后端时区往返的关键一环：前端的 <input type="datetime-local"> 没有时区概念，
// 若把带偏移的 RFC3339 喂给它，浏览器会按本地时区换算——服务端在 UTC 时，
// 用户来回编辑一次计划就整体偏 8 小时，而且界面上看不出任何异常。
func TestBillTaskWallClockJSON(t *testing.T) {
	// 库里存的是带时区的时刻；序列化出来必须是北京时间的墙上表示。
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, cstLocation)
	end := time.Date(2026, 9, 30, 23, 59, 59, 0, cstLocation)
	task := BillTask{ID: 1, Name: "9月", StartTime: &start, EndTime: &end}

	raw, err := json.Marshal(task)
	require.NoError(t, err)

	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &got))

	assert.Equal(t, "2026-09-01T00:00:00", got["startAt"], "必须是北京时间墙上时间")
	assert.Equal(t, "2026-09-30T23:59:59", got["endAt"])

	// 反证：同一时刻若按 UTC 表示会是 8 月 31 日 16:00——证明转换真的发生了。
	utcWall := start.UTC().Format(WallClockLayout)
	assert.Equal(t, "2026-08-31T16:00:00", utcWall, "构造反证的前提")
	assert.NotEqual(t, utcWall, got["startAt"], "不能把 UTC 墙上时间给前端")
}

// TestBillTaskWallClockJSONUnset 未设置时段时给空串，不给零值日期。
//
// 零值会序列化成 "0001-01-01T00:00:00"，前端的日期输入框会显示一个荒唐的年份，
// 用户以为计划被改坏了。
func TestBillTaskWallClockJSONUnset(t *testing.T) {
	task := BillTask{ID: 1, Name: "还没定时段"}

	raw, err := json.Marshal(task)
	require.NoError(t, err)

	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &got))

	assert.Equal(t, "", got["startAt"])
	assert.Equal(t, "", got["endAt"])
}

// TestWallClockRoundTrip 往返一致性：后端给出去的墙上时间，被前端原样回传后
// 解析回来必须是**同一个时刻**。
//
// 这一条覆盖的正是「编辑计划」的完整链路：读取 → 回填输入框 → 用户不改 → 保存。
// 中间任何一次时区换算都会让时刻漂移，而漂移后的账单会多算或少算那几小时的消费。
func TestWallClockRoundTrip(t *testing.T) {
	original := time.Date(2026, 9, 1, 0, 0, 0, 0, cstLocation)
	task := BillTask{StartTime: &original, EndTime: &original}

	// 1. 后端 → 前端：拿到墙上时间字符串。
	wall := task.StartAt()
	assert.Equal(t, "2026-09-01T00:00:00", wall)

	// 2. 前端原样回传 → 后端解析（与 handleSaveBillTask 走同一个函数）。
	parsed, _, err := ParseExportTime(wall)
	require.NoError(t, err)

	// 3. 必须还是同一个时刻。
	assert.True(t, original.Equal(parsed),
		"往返后时刻漂移了：原 %s，解析回 %s", original, parsed)
	assert.Equal(t, 0, parsed.Hour(), "小时数必须保持 0，不能被时区差值改掉")

	// 精确到秒的场景：峰谷计费依赖请求时刻，秒级漂移也会影响金额。
	withSeconds := time.Date(2026, 9, 1, 13, 45, 30, 0, cstLocation)
	task2 := BillTask{StartTime: &withSeconds}
	wall2 := task2.StartAt()
	assert.Equal(t, "2026-09-01T13:45:30", wall2)

	parsed2, _, err := ParseExportTime(wall2)
	require.NoError(t, err)
	assert.True(t, withSeconds.Equal(parsed2), "秒级时刻往返后也必须完全一致")
}
