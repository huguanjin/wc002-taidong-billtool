package billing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 严格区分按次计费的成本估算。

// pcRow 造一行站内按次计费的日志：model_price=0.12、group_ratio=1.8 时，
// 每次 n=1 的 quota = 0.12 × 500000 × 1.8 = 108000。
func pcRow(model, group string, channel, n string) []string {
	quota := map[string]string{"1": "108000", "2": "216000", "3": "324000"}[n]
	return []string{model, group, "0", "0", quota,
		`{"model_price":0.12,"group_ratio":1.8}`, "2", channel}
}

// tokenRow 造一行站内按量的日志（model_price=-1）。
func tokenRow(model, group string, channel string, quota string) []string {
	return []string{model, group, "1000", "100", quota,
		`{"model_price":-1,"model_ratio":2.5,"completion_ratio":6,"group_ratio":1.8}`, "2", channel}
}

func strictCfg(entries map[ChannelModelKey]UpstreamBilling, headers []string, rows [][]string) *StrictPerCall {
	return NewStrictPerCall(true, entries, headers, rows)
}

func TestPerCallUnits(t *testing.T) {
	u, ok := PerCallUnits(216000, 0.12, 1.8, false)
	assert.True(t, ok)
	assert.InDelta(t, 2, u, 1e-12, "216000 ÷ (0.12×500000×1.8) = 2")

	u, ok = PerCallUnits(100000, 0.12, 1.8, false)
	assert.False(t, ok, "不是整数次：不猜，退回 1 次并标记不确定")
	assert.Equal(t, 1.0, u)

	u, ok = PerCallUnits(54000, 0.12, 1.8, true)
	assert.True(t, ok)
	assert.InDelta(t, 0.5, u, 1e-12, "退款行按比例折算，不要求整数")

	_, ok = PerCallUnits(108000, 0.12, 0, false)
	assert.False(t, ok, "缺 group_ratio 反解不出次数")
}

// 开关关闭（Strict=nil）时与改动前完全一致：按次行也只走倍率。
func TestStrictOffKeepsRatioPath(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{pcRow("vid-1", "g", "1", "1")}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns: true, UpstreamRatios: map[int]float64{1: 0.4},
	})
	require.NoError(t, err)
	r := got[0]
	require.NotNil(t, r.UpstreamCostCNY)
	// 刊例 0.12 USD × 汇率 × (0.4/7)
	assert.InDelta(t, 0.12*DefaultExchangeRate*0.4/DiscountBaseFactor, *r.UpstreamCostCNY, 1e-4)
	assert.Zero(t, r.PerCallUnits)
	assert.Zero(t, r.PerCallCostCNY)
}

// 维护成按次：成本 = 次数 × 单次费用，且不需要渠道倍率。
func TestStrictPerCallCostIsUnitsTimesFee(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		pcRow("vid-1", "g", "1", "1"),
		pcRow("vid-1", "g", "1", "3"), // 一次请求 3 张
	}
	cfg := map[ChannelModelKey]UpstreamBilling{
		{1, "vid-1"}: {Mode: UpstreamModePerCall, PerCallCNY: 0.5},
	}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns: true,
		// 故意不给渠道 1 的倍率：按次的成本不经倍率，不该被要求。
		UpstreamRatios: map[int]float64{},
		Strict:         strictCfg(cfg, headers, rows),
	})
	require.NoError(t, err)
	r := got[0]
	require.NotNil(t, r.UpstreamCostCNY, "按次已维护，无倍率也应算得出成本")
	assert.InDelta(t, 4*0.5, *r.UpstreamCostCNY, 1e-9, "(1+3) 次 × 0.5 元")
	assert.InDelta(t, 4, r.PerCallUnits, 1e-9)
	assert.InDelta(t, 2.0, r.PerCallCostCNY, 1e-9)
	assert.Zero(t, r.PerCallUncertainRows)
	assert.Equal(t, 2, r.CostRows)
	assert.False(t, r.CostPartial)
	// 利润 = 金额 − 成本；金额 = (108000+324000)/500000 = 0.864
	assert.InDelta(t, 0.864-2.0, *r.ProfitCNY, 1e-9)
	// 刊例仍可反推：4 次 × 0.12。
	assert.InDelta(t, 0.48, *r.OfficialListUSD, 1e-9)
}

// 同一模型同一渠道，按次行与按量行混在一起：按次的走单次费用，按量的走倍率，互不串。
func TestStrictMixedPerCallAndPerTokenRows(t *testing.T) {
	headers := simpleCostLogHeaders()
	// 站内按量的行：quota=1800000、group_ratio 1.8 → 刊例 2 USD
	rows := [][]string{
		pcRow("m", "g", "1", "1"),
		tokenRow("m", "g", "1", "1800000"),
	}
	cfg := map[ChannelModelKey]UpstreamBilling{
		{1, "m"}: {Mode: UpstreamModePerCall, PerCallCNY: 0.5},
	}
	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{1: 0.7},
		Strict:         strictCfg(cfg, headers, rows),
	})
	require.NoError(t, err)
	r := got[0]
	require.NotNil(t, r.UpstreamCostCNY)
	// 按量那行站内非按次，不受严格模式影响，仍按倍率估；按次那行 1 次 × 0.5。
	want := 0.5 + 2.0*DefaultExchangeRate*0.7/DiscountBaseFactor
	assert.InDelta(t, want, *r.UpstreamCostCNY, 1e-4)
	assert.InDelta(t, 0.5, r.PerCallCostCNY, 1e-9, "只有按次那行计入按次成本")
}

// 维护成按量：沿用倍率路径，结果与不开严格模式一致。
func TestStrictPerTokenConfigUsesRatio(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{pcRow("m", "g", "1", "1")}
	cfg := map[ChannelModelKey]UpstreamBilling{{1, "m"}: {Mode: UpstreamModePerToken}}

	base, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns: true, UpstreamRatios: map[int]float64{1: 0.4},
	})
	require.NoError(t, err)
	strict, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns: true, UpstreamRatios: map[int]float64{1: 0.4},
		Strict: strictCfg(cfg, headers, rows),
	})
	require.NoError(t, err)
	assert.Equal(t, *base[0].UpstreamCostCNY, *strict[0].UpstreamCostCNY)
	assert.Zero(t, strict[0].PerCallUnits)
}

// 站内按次、却没维护上游计费方式：该行成本不算，原因单独分类，不会悄悄按倍率估。
func TestStrictPendingRowIsSkippedNotEstimated(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{pcRow("m", "g", "1", "1")}

	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{1: 0.4}, // 有倍率也不能拿来估
		Strict:         strictCfg(nil, headers, rows),
	})
	require.NoError(t, err)
	r := got[0]
	assert.Nil(t, r.UpstreamCostCNY, "未维护 → 不给成本，而不是给一个按倍率估的数")
	assert.Equal(t, 1, r.SkipReasons[string(SkipNoPerCallConfig)])
	assert.Contains(t, DescribeSkipReasons(r.SkipReasons), "按次计费模型未维护上游计费方式")
}

// 站内按量的模型不受严格模式影响，也不需要维护。
func TestStrictIgnoresTokenBilledModels(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{tokenRow("m", "g", "1", "1800000")}
	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns: true, UpstreamRatios: map[int]float64{1: 0.4},
		Strict: strictCfg(nil, headers, rows),
	})
	require.NoError(t, err)
	require.NotNil(t, got[0].UpstreamCostCNY)
	assert.Empty(t, got[0].SkipReasons)
}

// 任务退款行（other 里没有 model_price）要按同批日志里该模型的站内单次价判定，并冲抵成本。
func TestStrictTaskRefundOffsetsPerCallCost(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		pcRow("vid", "g", "1", "2"), // 2 次，108000×2
		// 退款 1 次：type=6，other 无 model_price
		{"vid", "g", "0", "0", "108000", `{"task_id":7,"group_ratio":1.8}`, "6", "1"},
	}
	cfg := map[ChannelModelKey]UpstreamBilling{{1, "vid"}: {Mode: UpstreamModePerCall, PerCallCNY: 1}}
	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns: true, Strict: strictCfg(cfg, headers, rows),
	})
	require.NoError(t, err)
	r := got[0]
	require.NotNil(t, r.UpstreamCostCNY)
	assert.InDelta(t, 1.0, *r.UpstreamCostCNY, 1e-9, "(2 − 1) 次 × 1 元")
	assert.InDelta(t, 1.0, r.PerCallUnits, 1e-9)
	assert.Equal(t, 1, r.HitCount, "退款行不算一次请求")
}

// 次数反推不出整数：按 1 次计，并如实标出。
func TestStrictUncertainUnitsAreReported(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{{"m", "g", "0", "0", "100000", `{"model_price":0.12,"group_ratio":1.8}`, "2", "1"}}
	cfg := map[ChannelModelKey]UpstreamBilling{{1, "m"}: {Mode: UpstreamModePerCall, PerCallCNY: 2}}
	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns: true, Strict: strictCfg(cfg, headers, rows),
	})
	require.NoError(t, err)
	assert.InDelta(t, 2.0, *got[0].UpstreamCostCNY, 1e-9)
	assert.Equal(t, 1, got[0].PerCallUncertainRows)
}

// 预检：按次且未维护的 (渠道, 模型) 被列出来，按金额降序，并带上参考单价。
func TestCollectPerCallIssues(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		pcRow("a", "g1", "1", "1"),
		pcRow("a", "g2", "1", "2"),
		pcRow("b", "g1", "2", "1"),
		pcRow("done", "g1", "3", "1"),
		tokenRow("a", "g1", "1", "1800000"), // 按量，不算
	}
	cfg := map[ChannelModelKey]UpstreamBilling{{3, "done"}: {Mode: UpstreamModePerCall, PerCallCNY: 1}}
	strict := strictCfg(cfg, headers, rows)

	issues := CollectPerCallIssues(headers, rows, strict)
	require.Len(t, issues, 2)
	assert.Equal(t, 1, issues[0].ChannelID, "金额大的在前")
	assert.Equal(t, "a", issues[0].Model)
	assert.Equal(t, 2, issues[0].Rows)
	assert.InDelta(t, 3, issues[0].Units, 1e-9)
	assert.InDelta(t, 0.12, issues[0].SitePrice, 1e-12)
	assert.Equal(t, []string{"g1", "g2"}, issues[0].Groups)
	assert.Equal(t, 2, issues[1].ChannelID)

	assert.Nil(t, CollectPerCallIssues(headers, rows, nil), "开关关闭不产出任何待维护项")
}

// 预检与出账共用判据：按次已维护的行不再被算成「缺渠道倍率」。
func TestCountRowCostReasonsStrictSkipsRatioForPerCall(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{pcRow("m", "g", "1", "1")}
	cfg := map[ChannelModelKey]UpstreamBilling{{1, "m"}: {Mode: UpstreamModePerCall, PerCallCNY: 1}}

	counts, missing, _ := CountRowCostReasonsStrict(headers, rows, map[int]float64{}, strictCfg(cfg, headers, rows))
	assert.Zero(t, counts[SkipNoUpstreamRatio])
	assert.Empty(t, missing)

	// 关闭严格模式时仍然缺倍率（与改动前一致）。
	counts, missing, _ = CountRowCostReasonsStrict(headers, rows, map[int]float64{}, nil)
	assert.Equal(t, 1, counts[SkipNoUpstreamRatio])
	assert.Equal(t, []int{1}, missing)

	// 未维护 → 单列一类。
	counts, _, _ = CountRowCostReasonsStrict(headers, rows, map[int]float64{1: 0.4}, strictCfg(nil, headers, rows))
	assert.Equal(t, 1, counts[SkipNoPerCallConfig])
}

func TestExtractRatioChannelIDsExcludesPerCallOnlyChannels(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		pcRow("m", "g", "1", "1"), // 渠道 1：只有按次
		pcRow("m", "g", "2", "1"), // 渠道 2：按次 + 按量
		tokenRow("n", "g", "2", "1800000"),
		tokenRow("n", "g", "3", "1800000"), // 渠道 3：只有按量
	}
	cfg := map[ChannelModelKey]UpstreamBilling{
		{1, "m"}: {Mode: UpstreamModePerCall, PerCallCNY: 1},
		{2, "m"}: {Mode: UpstreamModePerCall, PerCallCNY: 1},
	}
	ids, err := ExtractRatioChannelIDs(headers, rows, strictCfg(cfg, headers, rows))
	require.NoError(t, err)
	assert.Equal(t, []int{2, 3}, ids)

	all, err := ExtractRatioChannelIDs(headers, rows, nil)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, all, "关闭时与 ExtractChannelIDs 一致")
}

func TestChannelModelBillingInputValidate(t *testing.T) {
	ok := ChannelModelBillingInput{ChannelID: 1, Model: "m", Mode: UpstreamModePerCall, PerCallCNY: 0.3}
	assert.NoError(t, ok.Validate())
	assert.NoError(t, ChannelModelBillingInput{ChannelID: 1, Model: "m", Mode: UpstreamModePerToken}.Validate())

	assert.Error(t, ChannelModelBillingInput{ChannelID: 1, Model: "m", Mode: UpstreamModePerCall}.Validate(),
		"按次的单次费用为 0 会被读成上游免费，必须拒绝")
	assert.Error(t, ChannelModelBillingInput{ChannelID: 1, Model: "m", Mode: "x"}.Validate())
	assert.Error(t, ChannelModelBillingInput{ChannelID: 0, Model: "m", Mode: UpstreamModePerToken}.Validate())
	assert.Error(t, ChannelModelBillingInput{ChannelID: 1, Model: " ", Mode: UpstreamModePerToken}.Validate())
}

// 摘要里要把按次部分单独说清楚。
func TestSimpleBillSummaryMentionsPerCall(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{pcRow("m", "g", "1", "2")}
	cfg := map[ChannelModelKey]UpstreamBilling{{1, "m"}: {Mode: UpstreamModePerCall, PerCallCNY: 0.5}}
	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns: true, Strict: strictCfg(cfg, headers, rows),
	})
	require.NoError(t, err)
	totals := SumSimpleBill(got)
	text := FormatSimpleBillSummary(got, totals, 2026, 9, nil)
	assert.Contains(t, text, "其中按次计费：2 次，上游成本 ¥1")
}
