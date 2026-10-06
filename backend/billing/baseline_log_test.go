package billing

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBaselineOnRealLog 用真实生产日志核对交付基线。
//
// 样例日志含 12625 行、覆盖真实的价格缓存与分组倍率分布，是发现「改动让金额整体漂移」
// 的唯一手段——合成 fixture 的模型与倍率都是编出来的，漂移了也看不出来。
//
// 日志本身不在仓库里（体积与数据敏感性），所以默认跳过。要跑它：
//
//	cd backend
//	SAMPLE_LOG=/path/to/日志查询_2026-09-01_2026-09-30.tsv go test ./billing/ -run TestBaselineOnRealLog -v
//
// 断言的是 gpt-image-2-all 这一个桶的三条数字，来自修复前的实测：
// 它是「按次计费路径在 model_price > 0 时工作正常」的正例，改造后不许漂移。
func TestBaselineOnRealLog(t *testing.T) {
	logPath := os.Getenv("SAMPLE_LOG")
	if logPath == "" {
		t.Skip("未设置 SAMPLE_LOG，跳过真实日志基线回归")
	}
	book, exprSetting, _, err := LoadDBPriceCache("../../data/db_price_cache.json")
	require.NoError(t, err)

	headers, rows, err := LoadLogRows(logPath, "", "")
	require.NoError(t, err)
	agg, err := AggregateFromRows(rows, headers, book, 7.0, true, exprSetting, false, nil)
	require.NoError(t, err)

	var imgRow *AggRow
	var totalQuota, totalOfficial float64
	for _, a := range agg.Rows {
		totalQuota += a.Quota
		totalOfficial += a.OfficialUSD
		if a.Model == "gpt-image-2-all" {
			imgRow = a
		}
	}
	require.NotNil(t, imgRow, "样例日志里应有 gpt-image-2-all")

	// 基线三条数字（文档 1.1）：修完不许漂移。
	assert.InDelta(t, 1.8, imgRow.OfficialUSD, 1e-9, "gpt-image-2-all 的刊例")
	assert.InDelta(t, 675000, imgRow.Quota, 1e-9, "gpt-image-2-all 的额度")
	assert.InDelta(t, 15, imgRow.ImagePerCallCount, 1e-9, "15 行 × n=1")
	assert.False(t, imgRow.HasQuotaAdjustment, "样例日志无退款")

	// 结算金额：文档记的 1.3482 是用**取整到 3 位的折扣 0.107** 算的；
	// 实际出账用精确系数 0.75/7 = 0.107142857（避免整表累计出显示精度造成的偏差），
	// 得到 1.35。两者是同一个量的两种精度，实现没有漂移——这里把两者都钉住。
	assert.InDelta(t, 1.35, OfficialListCNY(imgRow, 7.0)*(imgRow.GroupRatio/DiscountBaseFactor), 1e-9,
		"结算金额（精确结算系数）")
	assert.InDelta(t, 1.3482, OfficialListCNY(imgRow, 7.0)*0.107, 1e-9,
		"结算金额（文档口径：折扣取整到 0.107）")

	// 该份日志无 type=6，所以「Σ消费 − Σ退款」就等于 Σquota。
	var totalDelta float64
	for _, a := range agg.Rows {
		totalDelta += a.QuotaDelta
	}
	assert.Zero(t, totalDelta, "样例日志没有退款行")
	assert.InDelta(t, 59.453262, totalQuota/QuotaPerCNY, 1e-6,
		"全表 Σquota/500000 = 59.453262 元（文档 1.1 的口径）")
}
