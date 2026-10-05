package billing

import (
	"encoding/base64"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// b64 把表达式编成日志 other.expr_b64 的形态。
func b64(expr string) string {
	return base64.StdEncoding.EncodeToString([]byte(expr))
}

// 本文件锁定「账单金额与站内实收相符」这一契约。
//
// 背景：曾出现同一份 9 月日志站内实收 59.45 元、账单却算出 73.27 元的偏差，
// 根因有两处，都在表达式计费路径上：
//  1. 表达式入参把缓存 token 扣了两次（BuildExprParams 收到的是已扣过缓存的 uncached，
//     内部又按 used["cr"] 扣一次），顺带让阶梯档位判断用了扣减后的长度；
//  2. 聚合键是 (model, group)，丢掉了「本次请求实际使用的分组倍率」这一维度，
//     而一个分组在账期内可能出现过多种倍率，单一折扣算不对整组账。
//
// 这里用**日志里的真实数值**做基线（不是编的数），与 quota 交叉验证。

// gpt-5.5 的表达式（取自 db_price_cache.json）：
// len < 272000 ? tier("base", p*5 + c*30 + cr*0.5) : tier("tier_2", p*10 + c*45 + cr*1)
const testExprGpt55 = `len < 272000 ? tier("base", p * 5 + c * 30 + cr * 0.5) : tier("tier_2", p * 10 + c * 45 + cr * 1)`

// TestExprParamsDeductCacheOnce 表达式入参只扣一次缓存。
//
// 真实日志行：gpt-5.5 / Codex，prompt=4739，cache_tokens=3840，completion=3525，
// group_ratio=1.0，quota=56083。上游口径是「p 扣掉缓存后按 p*5 计」，
// 于是表达式 USD = (899*5 + 3525*30 + 3840*0.5)/1e6 = 0.112165，
// ×500000 = 56082.5，与日志 quota 吻合到整数。
//
// 修复前这里是「扣两次」：p 变成 899-3840 → 截断为 0，USD = 0.107670，
// 少算约 4%，缓存越大的请求偏得越多。
func TestExprParamsDeductCacheOnce(t *testing.T) {
	prompt, cacheRead, completion := 4739.0, 3840.0, 3525.0
	const quota = 56083.0

	params := BuildExprParams("gpt-5.5", prompt, completion, cacheRead, 0, 0, 0, 0, 0, 0, testExprGpt55)
	assert.Equal(t, 899.0, params.P, "p 应是 prompt 扣掉缓存读一次的值")
	assert.Equal(t, prompt, params.Len, "档位判断用的是原始长度，不是扣减后的")

	res, err := RunBillingExpr(testExprGpt55, params, exprAt())
	require.NoError(t, err)
	// 注意单位：res.USD 是「单价 × token 数」的原始和，ExprQuota 内部自己除 1e6，
	// 不要再除一次——两边单位不一致正是这里最开始写错的地方。
	assert.InDelta(t, 0.112165, res.USD/1_000_000, 1e-9)

	// 核心断言：与站内实收一致性（group_ratio = 1，故直接可比）。
	assert.InDelta(t, quota, ExprQuota(res.USD, 1.0), 0.5,
		"表达式算出的 quota 必须与日志 quota 吻合")
}

// TestExprTierUsesRawLength 阶梯档位必须用原始上下文长度判断。
// 传扣减后的长度时，缓存大的请求会被误判进低档，单价差一倍。
func TestExprTierUsesRawLength(t *testing.T) {
	// prompt 略超 272000，且缓存很大：扣减后的长度会掉到阈值以下。
	prompt, cacheRead := 300_000.0, 290_000.0

	params := BuildExprParams("gpt-5.5", prompt, 100, cacheRead, 0, 0, 0, 0, 0, 0, testExprGpt55)
	assert.Equal(t, prompt, params.Len, "Len 必须是原始 prompt")

	res, err := RunBillingExpr(testExprGpt55, params, exprAt())
	require.NoError(t, err)
	// 命中 tier_2：p = 10000 按 *10、c = 100 按 *45、cr = 290000 按 *1。
	wantUSD := (10_000.0*10 + 100.0*45 + 290_000.0*1) / 1_000_000
	assert.InDelta(t, wantUSD, res.USD/1_000_000, 1e-9, "应命中 tier_2 而不是 base")
	assert.Equal(t, "tier_2", res.MatchedTier)
}

// TestGroupRatioSplitsBucketsAndReconciles 核心回归：同一 (model, group) 下
// 出现多种倍率时，必须按倍率拆桶、各自用自己的倍率结算，且逐桶与 quota 相符。
//
// 真实数据：gpt-5.5 / Codex 下有 65 行倍率 0.4、19 行倍率 1.0。
// 修复前整组共用一个折扣（取「第一行」的 siteDiscount = 0.239），
// 把 0.4 那批按 0.239 结算，整组多收 13.95 元。
func TestGroupRatioSplitsBucketsAndReconciles(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "created_at"}

	// 两行同 (model, group)，group_ratio 不同；quota 按站内口径给：
	// quota = 表达式USD × group_ratio × 500000。
	mk := func(prompt, comp, cr, gr float64) (string, float64) {
		params := BuildExprParams("gpt-5.5", prompt, comp, cr, 0, 0, 0, 0, 0, 0, testExprGpt55)
		res, err := RunBillingExpr(testExprGpt55, params, exprAt())
		require.NoError(t, err)
		// ExprQuota 内部自己除 1e6，传原始 res.USD。
		quota := ExprQuota(res.USD, gr)
		other := `{"model_ratio":0,"completion_ratio":0,"group_ratio":` + trimRatio(gr) +
			`,"cache_tokens":` + trimRatio(cr) + `,"expr_b64":"` + b64(testExprGpt55) + `"}`
		return other, quota
	}

	otherA, quotaA := mk(4739, 3525, 3840, 0.4)
	otherB, quotaB := mk(8000, 500, 3840, 1.0)

	rows := [][]string{
		{"gpt-5.5", "Codex", "4739", "3525", trimRatio(quotaA), otherA, "1789470821"},
		{"gpt-5.5", "Codex", "8000", "500", trimRatio(quotaB), otherB, "1789470821"},
	}

	agg, err := AggregateFromRows(rows, headers, nil, 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, agg.Rows, 2, "同一分组的两种倍率必须拆成两个桶，不能合并")

	byRatio := map[float64]*AggRow{}
	for _, a := range agg.Rows {
		byRatio[a.GroupRatio] = a
		assert.Equal(t, "Codex", a.KeyGroup, "KeyGroup 保留原始分组名，供展示与价表匹配")
	}
	require.Contains(t, byRatio, 0.4)
	require.Contains(t, byRatio, 1.0)

	disc := ComputeGroupDiscounts(agg.Rows, nil, 7.0, nil, false, nil)

	// 每桶折扣 = 倍率 / 7（精确值），且金额与 quota 严格相符。
	for _, ratio := range []float64{0.4, 1.0} {
		a := byRatio[ratio]
		assert.InDelta(t, ratio/7.0, disc.SettleFactor(a.Group), 1e-12,
			"倍率 %.1f 的结算系数应为 倍率/7", ratio)

		settle := OfficialListCNY(a, 7.0) * disc.SettleFactor(a.Group)
		assert.InDelta(t, a.Quota/QuotaPerCNY, settle, 1e-9,
			"倍率 %.1f 的桶：结算额必须与 quota 折算额严格相等", ratio)
	}

	// 合计等于 Σ quota 折元——这正是修复前对不上的那个数。
	var totalSettle, totalQuota float64
	for _, a := range agg.Rows {
		totalSettle += OfficialListCNY(a, 7.0) * disc.SettleFactor(a.Group)
		totalQuota += a.Quota / QuotaPerCNY
	}
	assert.InDelta(t, totalQuota, totalSettle, 1e-9)
}

// TestSingleRatioGroupUnchanged 单倍率分组不回归：折扣仍等于倍率/7，金额与 quota 相符。
// 这是全表最大的一桶（gpt-5-mini / AZ 12082 行），任何回归都会立刻暴露。
func TestSingleRatioGroupUnchanged(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other"}
	const gr = 1.8
	params := BuildExprParams("gpt-5-mini", 100_000, 5_000, 0, 0, 0, 0, 0, 0, 0, testExprGpt55)
	res, err := RunBillingExpr(testExprGpt55, params, exprAt())
	require.NoError(t, err)
	quota := ExprQuota(res.USD, gr)

	other := `{"group_ratio":` + trimRatio(gr) + `,"expr_b64":"` + b64(testExprGpt55) + `"}`
	rows := [][]string{{"gpt-5-mini", "AZ", "100000", "5000", trimRatio(quota), other}}

	agg, err := AggregateFromRows(rows, headers, nil, 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, agg.Rows, 1)

	disc := ComputeGroupDiscounts(agg.Rows, nil, 7.0, nil, false, nil)
	a := agg.Rows[0]
	assert.InDelta(t, gr/7.0, disc.SettleFactor(a.Group), 1e-12)
	assert.InDelta(t, a.Quota/QuotaPerCNY, OfficialListCNY(a, 7.0)*disc.SettleFactor(a.Group), 1e-9)
}

// TestGroupRatioParsedFromMalformedOther 容错解析：日志里确实存在整段不是合法
// JSON 的 other（admin_info.channel_affinity.key_hint 里嵌了转义引号）。
// 这类行必须仍能取到 group_ratio，否则在按倍率结算时会被漏掉。
func TestGroupRatioParsedFromMalformedOther(t *testing.T) {
	// 真实日志里的形态：key_hint 的值里嵌了未正确转义的引号。
	malformed := `{"admin_info":{"channel_affinity":{"key_hint":"{\"de...28\"}","key_path":"metadata.user_id"}},` +
		`"cache_creation_ratio":1.25,"cache_tokens":3840,"group_ratio":0.4,"model_ratio":5}`

	// 先确认这段确实不是合法 JSON——否则这个测试就没在测容错。
	assert.NotPanics(t, func() {
		gr, ok := GroupRatioFromOther(malformed)
		assert.True(t, ok, "非法 JSON 的 other 也必须能取到 group_ratio")
		assert.InDelta(t, 0.4, gr, 1e-9)
	})

	// 正常 JSON 与缺字段的情形。
	gr, ok := GroupRatioFromOther(`{"group_ratio":1.8,"cache_tokens":1}`)
	require.True(t, ok)
	assert.InDelta(t, 1.8, gr, 1e-9)

	_, ok = GroupRatioFromOther(`{"cache_tokens":1}`)
	assert.False(t, ok, "没有该字段时应返回 false 而不是 0")

	_, ok = GroupRatioFromOther("")
	assert.False(t, ok)
}

// TestParseFloatValueKeepsFraction 倍率是小数，解析不能截断成整数。
// cache.go 的 parseNumber 返回 int64，会把 0.4 变成 0——这里必须用浮点解析。
func TestParseFloatValueKeepsFraction(t *testing.T) {
	for _, tc := range []struct {
		in   interface{}
		want float64
	}{
		{0.4, 0.4},
		{1.0, 1.0},
		{1.8, 1.8},
		{"0.4", 0.4},
		{float64(0.75), 0.75},
	} {
		got, ok := parseFloatValue(tc.in)
		require.True(t, ok, "%v 应能解析", tc.in)
		assert.InDelta(t, tc.want, got, 1e-12)
	}
}

// TestDisplayGroupSuffixesRatio 展示名：多倍率时 C 列带倍率，唯一时保持原名。
func TestDisplayGroupSuffixesRatio(t *testing.T) {
	multi := &AggRow{KeyGroup: "Codex", GroupRatio: 0.4,
		BillingMode: BillingModeTieredExpr, BillingExpr: testExprGpt55}
	assert.Equal(t, "Codex(0.4)", multi.DisplayGroup())
	// 浮点除法不保证逐位相同（0.4/7 与 0.4÷7 在末尾可能差一个 ulp），用容差比。
	assert.InDelta(t, 0.4/7.0, multi.RatioDiscount(), 1e-15)

	whole := &AggRow{KeyGroup: "Codex", GroupRatio: 1,
		BillingMode: BillingModeTieredExpr, BillingExpr: testExprGpt55}
	assert.Equal(t, "Codex(1)", whole.DisplayGroup())

	// 非表达式行不按倍率结算，展示名不带后缀。
	noExpr := &AggRow{KeyGroup: "AZ", GroupRatio: 1.8, BillingMode: "token"}
	assert.False(t, noExpr.HasRatioDiscount(), "外部对标价行不满足倍率关系，不能按倍率结算")
	assert.Equal(t, "AZ", noExpr.DisplayGroup())
}

// TestSettleFactorNotRounded 结算系数必须是精确值，不是取整后的展示折扣。
// 用取整值乘会引入与金额无关、纯显示精度造成的偏差（整表约 0.05 元量级）。
func TestSettleFactorNotRounded(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other"}
	// 1.8 / 7 = 0.257142857…，取 3 位是 0.257，误差约 1.4e-4。
	const gr = 1.8
	params := BuildExprParams("gpt-5-mini", 100_000, 5_000, 0, 0, 0, 0, 0, 0, 0, testExprGpt55)
	res, err := RunBillingExpr(testExprGpt55, params, exprAt())
	require.NoError(t, err)
	quota := ExprQuota(res.USD, gr)

	other := `{"group_ratio":` + trimRatio(gr) + `,"expr_b64":"` + b64(testExprGpt55) + `"}`
	rows := [][]string{{"gpt-5-mini", "AZ", "100000", "5000", trimRatio(quota), other}}

	agg, err := AggregateFromRows(rows, headers, nil, 7.0, false, nil, false, nil)
	require.NoError(t, err)
	disc := ComputeGroupDiscounts(agg.Rows, nil, 7.0, nil, false, nil)
	a := agg.Rows[0]

	exact := disc.SettleFactor(a.Group)
	shown := disc.Discounts[a.Group]
	assert.Equal(t, round(shown, DiscountDecimals), shown, "展示折扣按 3 位取整")
	assert.InDelta(t, gr/7.0, exact, 1e-15, "结算系数是精确的 倍率/7")

	// 核心：用精确系数结算与站内实收一致。
	exactSettle := OfficialListCNY(a, 7.0) * exact
	assert.InDelta(t, a.Quota/QuotaPerCNY, exactSettle, 1e-9,
		"精确系数结算必须与 quota 折算额一致")

	// 反证：拿展示折扣（3 位取整值）去乘会偏离实收。
	// 偏离量随倍率不同而变——1.8/7 的取整误差约 1.4e-4，折算成金额虽小却是系统性的、
	// 与业务无关的偏差；倍率更“碎”时偏差更大。
	roundedSettle := OfficialListCNY(a, 7.0) * shown
	assert.Greater(t, math.Abs(exactSettle-roundedSettle), 1e-9,
		"用取整后的展示折扣结算会偏离实收——所以结算必须用精确系数")
}
