package billing

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

// audioRow 造一行 gpt-audio：销售倍率 4.5，quota 固定便于手算。
// 站内刊例（quota 量纲）= quota ÷ 4.5。
func audioRow(channel, quota string) []string {
	return []string{"gpt-audio", "oai", "100", "10", quota,
		`{"group_ratio":4.5,"model_price":-1,"model_ratio":1.25}`, "2", channel}
}

// 用户的场景：同一模型 gpt-audio 在两个渠道，渠道 846 上游倍率 5（高于销售倍率 4.5 → 亏），
// 渠道 1133 上游倍率 2.5（赚）。汇总表只能看到合起来的结果，渠道明细要能把亏的那个指出来。
func TestChannelBreakdownPinpointsLosingChannel(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		audioRow("846", "4500000"), audioRow("846", "4500000"),
		audioRow("1133", "4500000"),
	}
	opts := SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{846: 5, 1133: 2.5},
		ChannelNames:   map[int]string{846: "openai官-青衣-5", 1133: "骁宇-oai-2.5"},
	}
	sum, ch, err := AggregateSimpleBillDetailed(rows, headers, opts)
	require.NoError(t, err)
	require.Len(t, sum, 1)
	require.Len(t, ch, 2)

	byID := map[int]SimpleBillChannelRow{}
	for _, r := range ch {
		byID[r.ChannelID] = r
	}
	r846, r1133 := byID[846], byID[1133]

	assert.Equal(t, "openai官-青衣-5", r846.ChannelName)
	assert.Equal(t, 2, r846.HitCount)
	// 渠道 846：金额 = 2×9 = 18；刊例 = 2×4500000/4.5/500000×… 成本 = 刊例(量纲)/500000 × 汇率 × 5/7
	wantList846 := 2 * 4500000.0 / 4.5 / QuotaPerCNY
	assert.InDelta(t, wantList846*DefaultExchangeRate*5/DiscountBaseFactor, *r846.UpstreamCostCNY, 1e-4)
	assert.Less(t, *r846.ProfitCNY, 0.0, "上游倍率 5 高于销售倍率 4.5，必然亏")
	assert.Greater(t, *r1133.ProfitCNY, 0.0)
	assert.Equal(t, "按量", r846.BillingMode)

	// 销售倍率与盈亏平衡倍率：销售 4.5，盈亏平衡上游倍率也是 4.5（成本=收入）。
	require.NotNil(t, r846.SalesRatio)
	require.NotNil(t, r846.BreakEvenRatio)
	assert.InDelta(t, 4.5, *r846.SalesRatio, 1e-9)
	assert.InDelta(t, 4.5, *r846.BreakEvenRatio, 1e-9)
	assert.Greater(t, *r846.UpstreamRatio, *r846.BreakEvenRatio, "上游倍率高于盈亏平衡点 = 亏")
	assert.Less(t, *r1133.UpstreamRatio, *r1133.BreakEvenRatio)

	// 各渠道行加起来必须等于汇总行（同一循环里累加，不能各算各的）。
	var q, c, p, off float64
	var hits int
	for _, r := range ch {
		q += r.TotalQuota
		c += *r.UpstreamCostCNY
		p += *r.ProfitCNY
		off += *r.OfficialListUSD
		hits += r.HitCount
	}
	assert.InDelta(t, sum[0].TotalQuota, q, 1e-9)
	assert.InDelta(t, *sum[0].UpstreamCostCNY, c, 1e-3)
	assert.InDelta(t, *sum[0].ProfitCNY, p, 1e-3)
	assert.InDelta(t, *sum[0].OfficialListUSD, off, 1e-3)
	assert.Equal(t, sum[0].HitCount, hits)
}

// 国模渠道的盈亏平衡倍率是折扣口径（不除 7）。
func TestChannelBreakdownBreakEvenDomestic(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{{"glm-5.2", "g", "1", "1", "375000",
		`{"group_ratio":0.75,"model_price":-1}`, "2", "7"}}
	_, ch, err := AggregateSimpleBillDetailed(rows, headers, SimpleBillOptions{
		CostColumns:      true,
		UpstreamRatios:   map[int]float64{7: 0.6},
		DomesticChannels: map[int]bool{7: true},
	})
	require.NoError(t, err)
	require.Len(t, ch, 1)
	require.NotNil(t, ch[0].BreakEvenRatio)
	assert.InDelta(t, 0.75, *ch[0].BreakEvenRatio, 1e-9, "国模渠道：销售 0.75 折，盈亏平衡上游折扣也是 0.75")
	assert.InDelta(t, 0.75, *ch[0].SalesRatio, 1e-9)
	assert.Greater(t, *ch[0].ProfitCNY, 0.0)
}

// 取不到渠道号 / 多渠道的行：单独成「渠道 0 / -1」，不会混进某个真实渠道；成本不算但额度要在。
func TestChannelBreakdownSpecialBuckets(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		audioRow("846", "900000"),
		audioRow("", "900000"),         // 无渠道号
		audioRow("846,1133", "900000"), // 多渠道
	}
	sum, ch, err := AggregateSimpleBillDetailed(rows, headers, SimpleBillOptions{
		CostColumns: true, UpstreamRatios: map[int]float64{846: 2.5, 1133: 2.5},
	})
	require.NoError(t, err)
	require.Len(t, ch, 3)
	by := map[int]SimpleBillChannelRow{}
	var q float64
	for _, r := range ch {
		by[r.ChannelID] = r
		q += r.TotalQuota
	}
	assert.Equal(t, "（日志里取不到渠道号）", by[0].ChannelName)
	assert.Equal(t, "（一行经多个渠道）", by[-1].ChannelName)
	assert.Nil(t, by[0].UpstreamCostCNY, "没有渠道号：成本不算，不报 0")
	assert.Nil(t, by[-1].UpstreamCostCNY)
	assert.Equal(t, 1, by[0].SkipReasons[string(SkipNoChannel)])
	assert.Equal(t, 1, by[-1].SkipReasons[string(SkipMultiChannel)])
	assert.NotNil(t, by[846].UpstreamCostCNY)
	assert.InDelta(t, sum[0].TotalQuota, q, 1e-9, "额度不因拆渠道而丢")
}

// 渠道没维护倍率：成本留空、备注说明，且不影响别的渠道。
func TestChannelBreakdownMissingRatio(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{audioRow("846", "900000"), audioRow("1133", "900000")}
	_, ch, err := AggregateSimpleBillDetailed(rows, headers, SimpleBillOptions{
		CostColumns: true, UpstreamRatios: map[int]float64{1133: 2.5},
	})
	require.NoError(t, err)
	by := map[int]SimpleBillChannelRow{}
	for _, r := range ch {
		by[r.ChannelID] = r
	}
	assert.Nil(t, by[846].UpstreamCostCNY)
	assert.Nil(t, by[846].UpstreamRatio)
	assert.Equal(t, 1, by[846].SkipReasons[string(SkipNoUpstreamRatio)])
	assert.NotNil(t, by[1133].UpstreamCostCNY)
}

// 严格区分按次：同一模型在三个渠道，按次 0.1 / 按次 0.2 / 按量，渠道明细各标各的计费方式。
func TestChannelBreakdownShowsPerCallPerChannel(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		imgRow("gpt-image-2", "g", "1", "100000"),
		imgRow("gpt-image-2", "g", "1", "100000"),
		imgRow("gpt-image-2", "g", "2", "100000"),
		imgRow("gpt-image-2", "g", "3", "1800000"),
	}
	cfg := map[ChannelModelKey]UpstreamBilling{
		{1, "gpt-image-2"}: {Mode: UpstreamModePerCall, PerCallCNY: 0.1},
		{2, "gpt-image-2"}: {Mode: UpstreamModePerCall, PerCallCNY: 0.2},
		{3, "gpt-image-2"}: {Mode: UpstreamModePerToken},
	}
	_, ch, err := AggregateSimpleBillDetailed(rows, headers, SimpleBillOptions{
		CostColumns: true, UpstreamRatios: map[int]float64{3: 0.7},
		Strict: strictCfg(cfg, headers, rows),
	})
	require.NoError(t, err)
	by := map[int]SimpleBillChannelRow{}
	for _, r := range ch {
		by[r.ChannelID] = r
	}
	assert.Equal(t, "按次", by[1].BillingMode)
	assert.InDelta(t, 0.1, by[1].PerCallFeeCNY, 1e-12)
	assert.InDelta(t, 2, by[1].PerCallUnits, 1e-9)
	assert.InDelta(t, 0.2, *by[1].UpstreamCostCNY, 1e-9)
	assert.Equal(t, "按次", by[2].BillingMode)
	assert.InDelta(t, 0.2, *by[2].UpstreamCostCNY, 1e-9)
	assert.Equal(t, "按量", by[3].BillingMode)
	// 按次渠道：盈亏平衡单次费用 = 站内金额 ÷ 次数。渠道 1：金额 0.4，2 次 → 0.2。
	require.NotNil(t, by[1].BreakEvenPerCall)
	assert.InDelta(t, 0.2, *by[1].BreakEvenPerCall, 1e-9)
	assert.Nil(t, by[1].BreakEvenRatio, "按次渠道没有按倍率估算的行，不给盈亏平衡倍率")
}

// 渠道汇总按渠道合并，按金额降序；亏损渠道利润为负。
func TestSummarizeByChannel(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		audioRow("846", "4500000"),
		audioRow("1133", "900000"),
		{"other-model", "g2", "1", "1", "4500000", `{"group_ratio":4.5,"model_price":-1}`, "2", "846"},
	}
	_, ch, err := AggregateSimpleBillDetailed(rows, headers, SimpleBillOptions{
		CostColumns: true, UpstreamRatios: map[int]float64{846: 5, 1133: 2.5},
	})
	require.NoError(t, err)
	got := SummarizeByChannel(ch)
	require.Len(t, got, 2)
	assert.Equal(t, 846, got[0].ChannelID, "金额大的在前")
	assert.Equal(t, 2, got[0].Models, "846 跨两个模型")
	assert.Less(t, got[0].ProfitCNY, 0.0)
	assert.Greater(t, got[1].ProfitCNY, 0.0)
	assert.False(t, got[0].Partial)
}

// 没开成本核算：没有渠道明细（也没有渠道维度的意义）。
func TestChannelBreakdownOffWithoutCostColumns(t *testing.T) {
	headers := simpleCostLogHeaders()
	_, ch, err := AggregateSimpleBillDetailed([][]string{audioRow("846", "900000")}, headers, SimpleBillOptions{})
	require.NoError(t, err)
	assert.Empty(t, ch)
}

// 成本表写出两张附表；客户版账单不带；亏损行标红（至少渠道明细里有这一行且利润为负）。
func TestWriteSimpleCostTableWithChannelSheets(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{audioRow("846", "4500000"), audioRow("1133", "4500000")}
	sum, ch, err := AggregateSimpleBillDetailed(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{846: 5, 1133: 2.5},
		ChannelNames:   map[int]string{846: "青衣-5", 1133: "骁宇-2.5"},
	})
	require.NoError(t, err)

	dir := t.TempDir()
	costPath := filepath.Join(dir, "成本.xlsx")
	require.NoError(t, WriteSimpleBill(costPath, sum, "成本表", SimpleBillWriteOptions{CostTable: true, ChannelRows: ch}))
	billPath := filepath.Join(dir, "账单.xlsx")
	require.NoError(t, WriteSimpleBill(billPath, sum, "简易账单", SimpleBillWriteOptions{ChannelRows: ch}))

	cf, err := excelize.OpenFile(costPath)
	require.NoError(t, err)
	defer cf.Close()
	assert.Equal(t, []string{"成本表", "渠道明细", "渠道汇总"}, cf.GetSheetList())

	detail, err := cf.GetRows("渠道明细")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(detail), 3)
	assert.Equal(t, SimpleBillChannelColumns, detail[0])
	idx := func(name string) int {
		i, ok := columnIndex(SimpleBillChannelColumns, name)
		require.True(t, ok, name)
		return i
	}
	// 数据行：846 与 1133（同模型按金额相同时渠道号升序）。
	var row846 []string
	for _, r := range detail[1:] {
		if len(r) > 0 && r[0] == "846" {
			row846 = r
		}
	}
	require.NotNil(t, row846)
	assert.Equal(t, "青衣-5", row846[idx("渠道名称")])
	assert.Equal(t, "5", row846[idx("上游倍率")])
	assert.Equal(t, "按量", row846[idx("上游计费")])
	assert.Equal(t, "4.5", row846[idx("盈亏平衡上游倍率")])
	assert.Contains(t, row846[idx("利润（人民币）")], "-", "846 亏损，利润为负")

	// 合计行：成本三列只有全部渠道都算出来才写，这里都有 → 有公式。
	totalRow := len(detail)
	cell, err := cf.GetCellFormula("渠道明细", "J"+itoa(totalRow))
	require.NoError(t, err)
	assert.Contains(t, cell, "SUM(")

	sumRows, err := cf.GetRows("渠道汇总")
	require.NoError(t, err)
	assert.Equal(t, SimpleBillChannelTotalColumns, sumRows[0])
	assert.Len(t, sumRows, 3, "表头 + 两个渠道")

	bf, err := excelize.OpenFile(billPath)
	require.NoError(t, err)
	defer bf.Close()
	assert.Equal(t, []string{"简易账单"}, bf.GetSheetList(), "客户版账单不能带渠道明细（含采购倍率与毛利）")
}

func itoa(n int) string { return trimFixed(float64(n), 0) }
