package billing

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fptr(v float64) *float64 { return &v }

// TestSummarizeTasksProfitUsesCostedSettle 汇总口径的核心：利润必须用
// **参与成本核算的结算额**（costed_settle）减去成本，不能用全部结算额（settle）。
//
// 构造一个刻意让两者不等的场景：一个任务里，部分渠道没维护倍率，于是
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
		SettleCNY: 10000, CostedSettleCNY: fptr(6000), CostCNY: fptr(4000),
		ProfitCNY: fptr(2000), CostComplete: false,
		PricedRows: 8, TotalCostRows: 12,
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

// TestSummarizeTasksSkipsTasksWithoutCost 没有成本的任务（未勾选成本表、
// 或渠道倍率被拦）**一个数都不该进成本链路**。
func TestSummarizeTasksSkipsTasksWithoutCost(t *testing.T) {
	tasks := []BillTask{
		{
			CustomerName: "甲", PeriodYear: 2026, PeriodMonth: 9,
			SettleCNY: 1000, CostedSettleCNY: fptr(1000), CostCNY: fptr(400),
			ProfitCNY: fptr(600), CostComplete: true, PricedRows: 5, TotalCostRows: 5,
		},
		{
			// 未勾选生成成本利润表：三个成本字段都是 nil，只有结算额。
			CustomerName: "乙", PeriodYear: 2026, PeriodMonth: 9,
			SettleCNY: 8000,
		},
	}

	sums := SummarizeTasks(tasks)
	require.Len(t, sums, 1)
	s := sums[0]

	assert.Equal(t, 2, s.TaskCount)
	assert.Equal(t, 9000.0, s.SettleCNY, "两个任务的结算额都要计入对外口径")
	assert.Equal(t, 400.0, s.CostCNY, "只有有成本的那个任务贡献成本")
	assert.Equal(t, 1000.0, s.CostedSettleCNY, "同口径结算额只含那一个任务")
	assert.Equal(t, 600.0, s.ProfitCNY)

	assert.Equal(t, 1, s.MissingCostCount, "缺成本的任务数要报出来")
	assert.Equal(t, 0, s.PartialCostCount, "它不算 partial——它压根没成本")

	// 关键反证：若把 8000 也算进利润链路，利润会变成 8600。
	assert.NotEqual(t, 8600.0, s.ProfitCNY, "无成本任务的结算额不能进利润计算")
}

// TestSummarizeTasksZeroCostTask 上游倍率维护成 0（上游免费）是**有效成本**，
// 必须与「没有成本」区分开：0 表示真的不花钱，nil 表示不知道。
func TestSummarizeTasksZeroCostTask(t *testing.T) {
	tasks := []BillTask{{
		CustomerName: "免费渠道客户", PeriodYear: 2026, PeriodMonth: 9,
		SettleCNY: 500, CostedSettleCNY: fptr(500), CostCNY: fptr(0),
		ProfitCNY: fptr(500), CostComplete: true, PricedRows: 3, TotalCostRows: 3,
	}}

	s := SummarizeTasks(tasks)[0]
	assert.Equal(t, 0.0, s.CostCNY)
	assert.Equal(t, 500.0, s.ProfitCNY, "成本为 0 时利润等于结算额")
	assert.Equal(t, 0, s.MissingCostCount, "成本 0 是有效成本，不是缺成本")
	assert.Equal(t, 0, s.PartialCostCount)
}

// TestSummarizeTasksGroupsByPeriod 按 (年,月) 分组，不同账期不能混在一起；
// 结果按账期倒序（最新在前），与任务列表的排序一致。
func TestSummarizeTasksGroupsByPeriod(t *testing.T) {
	tasks := []BillTask{
		{CustomerName: "甲", PeriodYear: 2026, PeriodMonth: 8, SettleCNY: 100,
			CostedSettleCNY: fptr(100), CostCNY: fptr(50), ProfitCNY: fptr(50), CostComplete: true},
		{CustomerName: "乙", PeriodYear: 2026, PeriodMonth: 9, SettleCNY: 200,
			CostedSettleCNY: fptr(200), CostCNY: fptr(100), ProfitCNY: fptr(100), CostComplete: true},
		{CustomerName: "丙", PeriodYear: 2025, PeriodMonth: 12, SettleCNY: 300,
			CostedSettleCNY: fptr(300), CostCNY: fptr(150), ProfitCNY: fptr(150), CostComplete: true},
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
}

// TestSummarizeTasksNoDivisionByZero 结算额为 0 时毛利率不该出现 NaN/Inf
// （JSON 序列化 NaN 会直接报错，把接口打挂）。
func TestSummarizeTasksNoDivisionByZero(t *testing.T) {
	tasks := []BillTask{{
		CustomerName: "零消费", PeriodYear: 2026, PeriodMonth: 9,
		SettleCNY: 0, CostedSettleCNY: fptr(0), CostCNY: fptr(0),
		ProfitCNY: fptr(0), CostComplete: true,
	}}
	s := SummarizeTasks(tasks)[0]
	assert.Equal(t, 0.0, s.Margin)
	assert.False(t, s.Margin != s.Margin, "毛利率不能是 NaN")
}
