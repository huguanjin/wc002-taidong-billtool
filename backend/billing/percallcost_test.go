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

// 维护成按次只管那个 (渠道, 模型)：同渠道上的其它模型仍走倍率，互不串。
func TestStrictPerCallOnlyAffectsThatChannelModel(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		pcRow("m", "g", "1", "1"),
		// 同一渠道另一个按量模型：站内 quota=1800000、group_ratio 1.8 → 刊例 2 USD
		tokenRow("n", "g", "1", "1800000"),
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
	byModel := map[string]SimpleBillRow{}
	for _, r := range got {
		byModel[r.Model] = r
	}
	require.NotNil(t, byModel["m"].UpstreamCostCNY)
	assert.InDelta(t, 0.5, *byModel["m"].UpstreamCostCNY, 1e-9)
	require.NotNil(t, byModel["n"].UpstreamCostCNY)
	assert.InDelta(t, 2.0*DefaultExchangeRate*0.7/DiscountBaseFactor, *byModel["n"].UpstreamCostCNY, 1e-4)
	assert.Zero(t, byModel["n"].PerCallCostCNY, "模型 n 没维护成按次，不计入按次成本")
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

// imgRow 造一行站内按量的图片请求：model_price=-1，但走 /v1/images/ 且 request_conversion 为 openai_image。
func imgRow(model, group, channel, quota string) []string {
	return []string{model, group, "1000", "100", quota,
		`{"model_price":-1,"model_ratio":2.5,"group_ratio":0.75,"request_conversion":["openai_image"],"request_path":"/v1/images/edits"}`,
		"2", channel}
}

// 站内按量的图片请求也属于「按次候选」：没维护就拦，列出来的是 (渠道, 模型)，同一模型在不同渠道分别列。
func TestStrictImageRequestsAreCandidatesPerChannel(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		imgRow("gpt-image-2", "g", "10", "50000"),
		imgRow("gpt-image-2", "g", "10", "50000"),
		imgRow("gpt-image-2", "g", "20", "70000"),
		imgRow("gpt-image-2", "g", "30", "90000"),
		tokenRow("text-model", "g", "10", "1800000"), // 文本按量模型不是候选
	}
	issues := CollectPerCallIssues(headers, rows, strictCfg(nil, headers, rows))
	require.Len(t, issues, 3, "渠道 10/20/30 上的 gpt-image-2 各一项")
	byCh := map[int]PerCallIssue{}
	for _, is := range issues {
		assert.Equal(t, "gpt-image-2", is.Model)
		byCh[is.ChannelID] = is
	}
	assert.Equal(t, 2, byCh[10].Rows)
	assert.InDelta(t, 2, byCh[10].Units, 1e-9, "没有站内单价时一行算 1 次")
	assert.InDelta(t, 0.2, byCh[10].AmountCNY, 1e-9)
	assert.InDelta(t, 0.1, byCh[10].AvgSiteCNY, 1e-9, "站内每次均价 = 金额 ÷ 次数")
	assert.Zero(t, byCh[10].SitePrice)
}

// 用户的场景：同一模型在渠道 A 按次 0.1、渠道 B 按次 0.2、渠道 C 按量，三者各算各的。
func TestStrictSameModelDifferentChannelsDifferentModes(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		imgRow("gpt-image-2", "g", "1", "100000"), // A
		imgRow("gpt-image-2", "g", "1", "100000"),
		imgRow("gpt-image-2", "g", "2", "100000"),  // B
		imgRow("gpt-image-2", "g", "3", "1800000"), // C：按量，站内 quota 1.8M、group_ratio 0.75 → 刊例 4.8 USD
	}
	cfg := map[ChannelModelKey]UpstreamBilling{
		{1, "gpt-image-2"}: {Mode: UpstreamModePerCall, PerCallCNY: 0.1},
		{2, "gpt-image-2"}: {Mode: UpstreamModePerCall, PerCallCNY: 0.2},
		{3, "gpt-image-2"}: {Mode: UpstreamModePerToken},
	}
	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:    true,
		UpstreamRatios: map[int]float64{3: 0.7}, // A、B 不需要倍率
		Strict:         strictCfg(cfg, headers, rows),
	})
	require.NoError(t, err)
	r := got[0]
	require.NotNil(t, r.UpstreamCostCNY)
	wantTokenPart := 4.8 * DefaultExchangeRate * 0.7 / DiscountBaseFactor
	assert.InDelta(t, 2*0.1+1*0.2+wantTokenPart, *r.UpstreamCostCNY, 1e-4)
	assert.InDelta(t, 3, r.PerCallUnits, 1e-9)
	assert.InDelta(t, 0.4, r.PerCallCostCNY, 1e-9)
	assert.Equal(t, 4, r.CostRows)
	assert.Zero(t, r.PerCallUncertainRows)

	// 全部维护后不再有待补项。
	assert.Empty(t, CollectPerCallIssues(headers, rows, strictCfg(cfg, headers, rows)))
}

// 手工指定成按次的 (渠道, 模型)，即使日志里没有任何按次迹象，该渠道上该模型的每一行也按次估。
func TestStrictManualPerCallAppliesWithoutEvidence(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		tokenRow("text-model", "g", "1", "1800000"),
		tokenRow("text-model", "g", "2", "1800000"),
	}
	cfg := map[ChannelModelKey]UpstreamBilling{{1, "text-model"}: {Mode: UpstreamModePerCall, PerCallCNY: 0.3}}
	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns: true, UpstreamRatios: map[int]float64{2: 0.7}, Strict: strictCfg(cfg, headers, rows),
	})
	require.NoError(t, err)
	r := got[0]
	assert.InDelta(t, 1, r.PerCallUnits, 1e-9, "只有渠道 1 那一行按次")
	assert.InDelta(t, 0.3+2.0*DefaultExchangeRate*0.7/DiscountBaseFactor, *r.UpstreamCostCNY, 1e-4)
}

// 维护页的核对：未维护 / 按次 / 按量三种状态都列出来，未维护排最前，按渠道分行。
func TestCollectPerCallStatus(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		imgRow("gpt-image-2", "g", "1", "50000"),
		imgRow("gpt-image-2", "g", "1", "50000"),
		imgRow("gpt-image-2", "g", "2", "70000"),
		imgRow("gpt-image-2", "g", "3", "90000"),
		tokenRow("text-model", "g", "1", "1800000"), // 不是候选，不出现
	}
	cfg := map[ChannelModelKey]UpstreamBilling{
		{1, "gpt-image-2"}: {Mode: UpstreamModePerCall, PerCallCNY: 0.05},
		{2, "gpt-image-2"}: {Mode: UpstreamModePerToken},
	}
	got := CollectPerCallStatus(headers, rows, strictCfg(cfg, headers, rows))
	require.Len(t, got, 3)
	assert.Equal(t, "none", got[0].Status, "未维护的排最前")
	assert.Equal(t, 3, got[0].ChannelID)

	byCh := map[int]PerCallStatusItem{}
	for _, it := range got {
		byCh[it.ChannelID] = it
	}
	assert.Equal(t, UpstreamModePerCall, byCh[1].Status)
	assert.InDelta(t, 0.05, byCh[1].PerCallCNY, 1e-12)
	require.NotNil(t, byCh[1].EstCostCNY)
	assert.InDelta(t, 0.1, *byCh[1].EstCostCNY, 1e-9, "2 次 × 0.05")
	assert.Equal(t, UpstreamModePerToken, byCh[2].Status)
	assert.Nil(t, byCh[2].EstCostCNY)

	assert.Nil(t, CollectPerCallStatus(headers, rows, nil))
}
