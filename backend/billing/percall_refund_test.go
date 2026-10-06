package billing

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖两类漏算的修复：
//  1. 按次计费的判定改由日志证据（model_price）决定，不再靠模型名猜——
//     以前 gpt-image-2.5-flare 这类「名字像图片、实际按量计费」的模型会被强制走
//     按次分支，而站点并没有给它配固定价，于是刊例恒为 0，客户拿到账单发现白送。
//  2. 异步任务的退款/补扣结算行要纳入导出并冲抵额度，但**不计价**。

// perCallRow 造一行按次计费日志：other 里带 model_price > 0。
func perCallRow(t *testing.T, model string, quota, modelPrice, groupRatio float64) pricedRow {
	t.Helper()
	other := `{"group_ratio":` + trimRatio(groupRatio) +
		`,"model_price":` + trimRatio(modelPrice) + `,"model_ratio":0}`
	return priceRow(model, other, 0, 0, 0, 0, 0, quota,
		NewPriceBook(), 7.0, false, nil, time.Now())
}

// ratioRow 造一行按量倍率日志：other 里 model_price = -1、model_ratio > 0。
func ratioRow(t *testing.T, model string, prompt, completion, quota, groupRatio float64) pricedRow {
	t.Helper()
	other := `{"group_ratio":` + trimRatio(groupRatio) +
		`,"model_price":-1,"model_ratio":2.5,"completion_ratio":6,"cache_ratio":0}`
	return priceRow(model, other, prompt, completion, 0, 0, 0, quota,
		NewPriceBook(), 7.0, false, nil, time.Now())
}

// TestPerCallDecidedByLogNotModelName 这是根因一的核心：
// 「名字像图片模型」不再决定走哪条计费分支，日志里的 model_price 才决定。
func TestPerCallDecidedByLogNotModelName(t *testing.T) {
	t.Run("名字像图片但按量计费：必须走 ratio，刊例不为 0", func(t *testing.T) {
		// 站点把这两个模型配成了 ratio 计费（db 缓存里 billingMode = "ratio"），
		// 日志里 model_price = -1。修复前它们被名字启发式判成按次，刊例算出 0。
		for _, model := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
			r := ratioRow(t, model, 1_000_000, 100_000, 0, 0.75)

			assert.Positive(t, r.OfficialUSD, "%s 的刊例必须大于 0（按 ratio 算出）", model)
			assert.Zero(t, r.ImagePerCallCount, "%s 不是按次计费，不该记调用次数", model)
			assert.Equal(t, "token", r.BillingMode)
			assert.Equal(t, ListOriginExternal, r.ListOrigin, "ratio 快照换来的是外部对标价")
		}
	})

	t.Run("日志给了固定价：才走按次", func(t *testing.T) {
		r := perCallRow(t, "gpt-image-2-all", 45000, 0.12, 0.75)

		assert.InDelta(t, 0.12, r.OfficialUSD, 1e-9, "刊例 = 单次价 × n（此处 n=1）")
		assert.Equal(t, 1.0, r.ImagePerCallCount)
		assert.Equal(t, "per_call", r.BillingMode)
		assert.Equal(t, ListOriginExternal, r.ListOrigin)
	})

	t.Run("名字不像图片但日志给了固定价：同样按次", func(t *testing.T) {
		// 判据只看证据，与该模型长什么样无关。以前一个非图片模型即使日志里
		// 写了 model_price 也走不到按次分支，会去价表里找一个根本不存在的价。
		r := perCallRow(t, "some-weird-model", 45000, 0.12, 0.75)

		assert.InDelta(t, 0.12, r.OfficialUSD, 1e-9)
		assert.Equal(t, 1.0, r.ImagePerCallCount)
	})
}

// TestPerCallCountInferredFromQuota 张数 n 从 quota 反推。
//
// 日志的 other 里没有 n 这个键，但站内计费式是
// quota = model_price × QuotaPerCNY × group_ratio × n，反解即可。
func TestPerCallCountInferredFromQuota(t *testing.T) {
	t.Run("n=1", func(t *testing.T) {
		r := perCallRow(t, "gpt-image-2-all", 45000, 0.12, 0.75)
		assert.Equal(t, 1.0, r.ImagePerCallCount)
		assert.InDelta(t, 0.12, r.OfficialUSD, 1e-9)
	})

	t.Run("n=8", func(t *testing.T) {
		// 360000 / (0.12 × 500000 × 0.75) = 8
		r := perCallRow(t, "gpt-image-2-all", 360000, 0.12, 0.75)
		assert.Equal(t, 8.0, r.ImagePerCallCount, "张数要按量算，不能恒为 1")
		assert.InDelta(t, 0.96, r.OfficialUSD, 1e-9, "刊例 = 单次价 × 8")
	})

	t.Run("反推不出整数：退回 n=1 并标记不确定", func(t *testing.T) {
		// group_ratio 缺失 → 反推式不成立，不能硬套。
		other := `{"model_price":0.12,"model_ratio":0}`
		r := priceRow("gpt-image-2-all", other, 0, 0, 0, 0, 0, 12345,
			NewPriceBook(), 7.0, false, nil, time.Now())

		assert.Equal(t, 1.0, r.ImagePerCallCount, "不猜，退回 1 次")
		assert.InDelta(t, 0.12, r.OfficialUSD, 1e-9, "刊例按 1 次算")
		assert.True(t, r.PerCallCountUncertain, "要标记张数无法确认，供账单备注说明")
	})
}

// TestNoSilentZeroWhenPriceMissing 静默出 0 是本类问题最危险的表现：
// 金额是 0 却不报任何错，客户拿到账单才发现白送。
//
// 修复后仍然可能取不到价（日志既没有 model_price 也没有 model_ratio，
// 价表里也查不到），但那种行必须落进 missingPriceModels，不能只留一个 0。
func TestNoSilentZeroWhenPriceMissing(t *testing.T) {
	// 一个名字像图片、但日志里三种计费证据都没有的模型，价表里也没有。
	row := &AggRow{
		Model: "gpt-image-9-unknown", Group: "g", KeyGroup: "g",
		Uncached: 1000, Rows: 1, Quota: 500_000,
	}

	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other"}
	rows := [][]string{{"gpt-image-9-unknown", "g", "1000", "0", "500000", `{"group_ratio":0.75,"model_price":-1}`}}

	agg, err := AggregateFromRows(rows, headers, NewPriceBook(), 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, agg.Rows, 1)

	got := agg.Rows[0]
	assert.Zero(t, got.OfficialUSD, "确实取不到价")
	// 关键：0 必须伴随「这个模型没有可用定价」的记录，而不是静默通过。
	assert.False(t, HasKnownListPrice(got),
		"取不到价的行不能被当作有价——账单会据此把它列进缺失定价清单")
	_ = row
}

// TestTaskQuotaAdjustmentDetection 额度调整行的判别依据是 other.task_id。
//
// 用 type 单值判会漏：补扣的结算行也是 type=2，与原始消费行同类型。
func TestTaskQuotaAdjustmentDetection(t *testing.T) {
	cases := []struct {
		name  string
		other string
		want  bool
	}{
		{"原始消费行（无 task_id）", `{"is_task":true,"model_price":0.12}`, false},
		{"退款行（有 task_id）", `{"task_id":42,"reason":"failed"}`, true},
		{"补扣结算行（有 task_id）", `{"task_id":42,"pre_consumed_quota":100}`, true},
		{"task_id 是字符串", `{"task_id":"42"}`, true},
		{"task_id 为 0 视为没有", `{"task_id":0}`, false},
		{"普通消费行", `{"group_ratio":1.8}`, false},
		{"other 为空", ``, false},
		{"other 不是 JSON", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsTaskQuotaAdjustment(tc.other))
		})
	}
}

// TestQuotaAdjustmentDeltaSign 额度调整的方向靠 logs.type 判，不靠 quota 的符号。
//
// 站内两种日志的 quota 都是**正数**：退款（type=6）写退还额，补扣（type=2）写补扣额。
// 以为「退款是负数」会让冲抵方向整个反过来——账单会变成加钱而不是减钱。
func TestQuotaAdjustmentDeltaSign(t *testing.T) {
	d, ok := QuotaAdjustmentDelta("6", 1000)
	assert.True(t, ok)
	assert.Equal(t, 1000.0, d, "退款是正数，调用方用减法并入（净结算 = quota − delta）")

	d, ok = QuotaAdjustmentDelta("2", 300)
	assert.True(t, ok)
	assert.Equal(t, -300.0, d, "补扣结算行是负数，会把净结算加回去")

	_, ok = QuotaAdjustmentDelta("1", 1000)
	assert.False(t, ok, "充值与消费无关，不识别")

	_, ok = QuotaAdjustmentDelta("", 1000)
	assert.False(t, ok, "type 缺失时宁可不动，也不要按猜错的方向冲抵")
}

// TestRefundOnlyOffsetsQuotaNotPrice 根因二的核心：
// 退款行只改结算额度，绝不改刊例与 token——刊例代表「这次请求值多少钱」，
// 退款不改变这个事实。混进去会连带污染折扣反推的分母。
func TestRefundOnlyOffsetsQuotaNotPrice(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "type"}
	rows := [][]string{
		// 原始消费行：预扣 500000（1 元）
		{"gpt-5.5", "vip", "1000000", "100000", "500000", `{"group_ratio":1.8,"model_ratio":2.5,"completion_ratio":6}`, "2"},
		// 退款行：退还 200000（0.4 元），token 全 0
		{"gpt-5.5", "vip", "0", "0", "200000", `{"task_id":7,"reason":"failed","group_ratio":1.8,"model_price":0.12}`, "6"},
	}

	agg, err := AggregateFromRows(rows, headers, NewPriceBook(), 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, agg.Rows, 1)

	got := agg.Rows[0]
	assert.Equal(t, 500000.0, got.Quota, "Quota 只累计消费行")
	assert.True(t, got.HasQuotaAdjustment)
	assert.Equal(t, 200000.0, got.QuotaDelta, "退款记为正数")

	// 净结算 = (500000 − 200000) / 500000 = 0.6 元
	assert.InDelta(t, 0.6, got.SiteCNY(), 1e-9, "净结算要减掉退款")

	// 刊例与 token 只由消费行贡献：退款行的 model_price=0.12 若被计价，
	// 这里会多出 0.12；token 全 0 但被当消费行就会多算一行。
	assert.Positive(t, got.OfficialUSD, "消费行应当算出了刊例")
	assert.Equal(t, 1, got.Rows, "退款行不是一次请求，不该计入请求行数")

	// 含退款的桶必须退出「按精确倍率结算」——那个恒等式一边用 quota 一边用刊例，
	// 退款只动 quota，等式不再成立。
	assert.False(t, got.HasRatioDiscount(),
		"含额度调整的桶不能按倍率精确结算，否则会少收恰好等于退款额的钱")
}

// TestRefundBucketSettlesToNetAmount 端到端口径：
// 含退款的桶，结算额必须等于净额，且折扣 = 净额 ÷ 刊例。
func TestRefundBucketSettlesToNetAmount(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "type"}
	rows := [][]string{
		{"gpt-5.5", "vip", "1000000", "100000", "700000", `{"group_ratio":1.8,"model_price":-1,"model_ratio":2.5,"completion_ratio":6}`, "2"},
		{"gpt-5.5", "vip", "0", "0", "200000", `{"task_id":7,"group_ratio":1.8}`, "6"},
	}
	agg, err := AggregateFromRows(rows, headers, NewPriceBook(), 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, agg.Rows, 1)

	got := agg.Rows[0]
	require.True(t, got.HasQuotaAdjustment)
	require.Positive(t, got.OfficialUSD)

	disc := ComputeGroupDiscounts(agg.Rows, NewPriceBook(), 7.0, DiscountOverrides{}, nil)
	listCNY := OfficialListCNY(got, 7.0)
	settle := round(listCNY*disc.SettleFactor(got.Group), MoneyDecimals)

	// 结算必须落在净额上：(700000 − 200000) / 500000 = 1.0 元
	assert.InDelta(t, 1.0, got.SiteCNY(), 1e-9, "净结算")
	assert.InDelta(t, got.SiteCNY(), settle, 1e-6,
		"结算额（刊例 × 结算系数）必须等于净额，账实才相符")
	assert.False(t, disc.Derived[got.Group] == false && disc.SettleFactor(got.Group) == got.RatioDiscount(),
		"不该再用倍率算出的那个系数")
}

// TestNegativeNetAmountNotClamped 某期退款多于消费时，净额就是负的。
// 夹到 0 会把差异藏起来——而「看起来正常、实际少了钱」正是要消灭的失败模式。
func TestNegativeNetAmountNotClamped(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "type"}
	rows := [][]string{
		{"gpt-5.5", "vip", "100000", "10000", "100000", `{"group_ratio":1.8,"model_ratio":2.5,"completion_ratio":6}`, "2"},
		{"gpt-5.5", "vip", "0", "0", "900000", `{"task_id":9,"group_ratio":1.8}`, "6"},
	}

	agg, err := AggregateFromRows(rows, headers, NewPriceBook(), 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, agg.Rows, 1)

	got := agg.Rows[0]
	// (100000 − 900000) / 500000 = −1.6 元
	assert.InDelta(t, -1.6, got.SiteCNY(), 1e-9, "净额为负时必须原样输出负数")
	assert.Negative(t, got.SiteCNY())
}

// TestMakeupSettlementRowNotPricedTwice 补扣结算行（type=2、带 task_id、token 全 0）
// 不能当消费行再计一次价——一个任务的刊例被算两次，账单会凭空多出一份钱。
func TestMakeupSettlementRowNotPricedTwice(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "type"}
	rows := [][]string{
		// 原始消费行：预扣 500000
		{"gpt-5.5", "vip", "1000000", "100000", "500000", `{"is_task":true,"group_ratio":1.8,"model_price":0.12}`, "2"},
		// 补扣结算行：实际 800000，补 300000。它带着 model_price，
		// 若被当消费行会再加一次 0.12 的刊例。
		{"gpt-5.5", "vip", "0", "0", "300000", `{"task_id":5,"pre_consumed_quota":500000,"actual_quota":800000,"model_price":0.12,"group_ratio":1.8}`, "2"},
	}

	agg, err := AggregateFromRows(rows, headers, NewPriceBook(), 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, agg.Rows, 1)

	got := agg.Rows[0]
	// 刊例只由原始消费行贡献一次。
	assert.InDelta(t, 0.12, got.OfficialUSD, 1e-9,
		"补扣行的 model_price 不能把刊例再加一遍")
	// Quota 只累计原始消费行；补扣是额度调整，进 QuotaDelta。
	assert.Equal(t, 500000.0, got.Quota, "Quota 只含原始消费行")
	assert.Equal(t, -300000.0, got.QuotaDelta, "补扣记负数，净结算要加回来")
	// 净结算 = (500000 − (−300000)) / 500000 = 1.6 元，即预扣 + 补扣的总额度。
	assert.InDelta(t, 1.6, got.SiteCNY(), 1e-9)
}

// TestRefundRowExcludedFromSanitizedLog 客户版脱敏日志里不该出现退款行。
//
// 那是站点与用户之间的额度往来，不是消费明细；账单金额已经含冲抵，
// 客户拿账单核对即可。这条是商务决策，写死在聚合层一处比在写出路径加过滤更难漏。
func TestRefundRowExcludedFromSanitizedLog(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "type"}
	rows := [][]string{
		{"gpt-5.5", "vip", "1000000", "100000", "500000", `{"group_ratio":1.8,"model_ratio":2.5,"completion_ratio":6}`, "2"},
		{"gpt-5.5", "vip", "0", "0", "200000", `{"task_id":7,"group_ratio":1.8}`, "6"},
	}

	w := &recordingSanitizedWriter{}
	_, err := AggregateFromRows(rows, headers, NewPriceBook(), 7.0, false, nil, false, w)
	require.NoError(t, err)

	assert.Len(t, w.rows, 1, "只应有消费行进脱敏日志，退款行不写")
	// 脱敏日志会丢掉 other 列，所以列序与原始日志不同：
	// 保留的 base 列是 model_name(0) / group(1) / type(2) / created_at(3) / ... / quota(4)。
	assert.Equal(t, "gpt-5.5", w.rows[0][0])
	assert.Equal(t, "500000", w.rows[0][4], "写出的是消费行（quota 500000），不是退款行")
}

// recordingSanitizedWriter 记录写出的行，用来断言脱敏日志的内容。
type recordingSanitizedWriter struct {
	rows [][]string
}

func (w *recordingSanitizedWriter) WriteRow(row []string, _, _, _ float64, _ RowDetails) error {
	cp := make([]string, len(row))
	copy(cp, row)
	w.rows = append(w.rows, cp)
	return nil
}
