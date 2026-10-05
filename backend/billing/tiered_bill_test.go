package billing

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

// 表达式取自线上 options 表的真实配置（data/db_price_cache.json）。
const (
	// gpt-6-astra：单档行与跨档行都用它。两档系数差别大，跨档时任何单一单价
	// 都还原不出金额，正好用来验证「跨档必须留空」。
	testExprAstra = `len <= 272000 ? tier("base", p * 10 + c * 50 + cr * 1 + cc * 12.5) : tier("tier_2", p * 20 + c * 75 + cr * 2 + cc * 25)`
	// gpt-5.6-sol：tier_1 档含 cc（5 分钟缓存创建）项，用来验证 K 列会被填上。
	testExprSol = `len < 272000 ? tier("tier_1", p * 4 + c * 20 + cr * 0.4 + cc * 5) : tier("tier_2", p * 8 + c * 30 + cr * 0.8 + cc * 10)`
)

func exprAt() time.Time {
	return time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
}

// buildBillFixtureTemplate 造一个与 data/bill_template.xlsx 同构的最小模板：
// 28 列原表头 + 追加在 AB 之后的 AC 列（官方刊例-美金，全表唯一允许裸数值的列）
// + 第 2 行注 + 会分别被清掉/重写的第 3、4 行。
func buildBillFixtureTemplate(t *testing.T, path string) {
	t.Helper()
	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	headers := []string{
		"账期月份", "模型名称", "分组标识",
		"缓存未命中Token量", "缓存未命中Token单价（美金/百万token）",
		"缓存命中Token量", "缓存命中Token单价（美金/百万token）",
		"输出Token量", "输出Token单价（美金/百万token）",
		"5分钟显式缓存创建Token量", "5分钟显式缓存创建Token单价（美金/百万token）",
		"1小时显式缓存创建Token量", "1小时显式缓存创建Token单价（美金/百万token）",
		"总Token量(公式)", "网页搜索次数", "图片/视频品质", "图片张数（张）",
		"视频时长(秒)", "总金额（人民币）", "折扣", "计量单位",
		"结算金额（人民币）", "结算金额（美金）", "是否与原厂定价一致",
		"刊例价-命中列表价", "刊例价-未命中列表价", "刊例价-输出列表价", "备注",
		"官方刊例-美金（可还原时为单价×用量公式，否则为官方刊例本身）",
	}
	require.Len(t, headers, 29, "模板列数必须是 28 列原表 + 追加的 AC 列")
	row := make([]interface{}, len(headers))
	for i, h := range headers {
		row[i] = h
	}
	require.NoError(t, f.SetSheetRow(sheet, "A1", &row))
	require.NoError(t, f.SetSheetRow(sheet, "A2", &[]interface{}{"", "", "注"}))
	require.NoError(t, f.SetSheetRow(sheet, "A3", &[]interface{}{"占位"}))
	require.NoError(t, f.SetSheetRow(sheet, "A4", &[]interface{}{"合计"}))
	require.NoError(t, f.SaveAs(path))
}

// exprTieredAgg 构造一行阶梯表达式模型的聚合结果。
//
// 「官方刊例」不手填，由 RunBillingExpr 现场算出，入参构造走与生产同一个
// BuildExprParams，测试才不会因为自己编了一个数而自说自话。
func exprTieredAgg(t *testing.T, model, group, expr string, tiers []string, tok [5]float64) *AggRow {
	t.Helper()
	uncached, cacheRead, out, cache5m, cache1h := tok[0], tok[1], tok[2], tok[3], tok[4]

	// 传原始 prompt（uncached+缓存），与生产一致：BuildExprParams 内部自行扣一次缓存。
	params := BuildExprParams(model, uncached+cacheRead+cache5m+cache1h, out,
		cacheRead, cache5m, cache1h, 0, 0, 0, 0, expr)
	res, err := RunBillingExpr(expr, params, exprAt())
	require.NoError(t, err, "表达式求值失败")

	// RunBillingExpr 返回的是「token 数 × 美金/百万 token」的原始和，没有除 1e6；
	// 生产代码在 aggregate.go 里除过之后才落成官方刊例。这里必须照做，否则
	// 「单价 × 用量 / 1e6 == 官方刊例」的判据从构造上就不可能成立。
	listUSD := res.USD / 1_000_000

	return &AggRow{
		Model: model, Group: group,
		Uncached: uncached, CacheRead: cacheRead, Output: out,
		CacheWrite5m: cache5m, CacheWrite1h: cache1h,
		Quota:       ExprQuota(listUSD, 1.0),
		Rows:        1,
		OfficialUSD: listUSD,
		BillingMode: BillingModeTieredExpr,
		BillingExpr: expr,
		ExprTiers:   tiers,
		LastAt:      exprAt(),
		// 这个辅助函数只填用量与金额，不管口径来源。空来源会让判据退化成
		// 「有刊例就能反推」，与生产不一致（生产在 aggregate.go 里一定会落一个
		// ListOrigin），所以显式给成外部对标价，让需要验「表达式行不反推」的
		// 测试自己去覆盖这个字段。
		ListOrigin: ListOriginExternal,
	}
}

// 用量全部取自 data/按照阶梯计费计算价格.xlsx 的真实行，金额由表达式现场算出后
// 与账单上那一行核对过，不是编出来的数。
var (
	// gpt-6-astra / oai（真实账单第 35 行）：63 未命中 + 105 输出，总长 168，
	// 命中 base 档，刊例 0.04116 元。
	astraBaseTok = [5]float64{63, 0, 105, 0, 0}
	// gpt-5.6-sol / oai（真实账单第 34 行）：1956 未命中 + 1322 输出，总长 3278，
	// 命中 tier_1 档，刊例 0.239848 元。
	solTier1Tok = [5]float64{1956, 0, 1322, 0, 0}
	// gpt-6-astra / az定制（真实账单第 33 行）：总长远超 272,000，base 与 tier_2
	// 混合计价，刊例 212030.68969599827 元——任何单一单价都还原不出来。
	astraCrossTierTok = [5]float64{11_156_008, 1_613_882_470, 48_989_822, 1_999_367_404, 0}
)

func readBill(t *testing.T, path string) (*excelize.File, string) {
	t.Helper()
	f, err := excelize.OpenFile(path)
	require.NoError(t, err, "打开账单失败")
	t.Cleanup(func() { _ = f.Close() })
	return f, f.GetSheetName(0)
}

func cell(t *testing.T, f *excelize.File, sheet string, col, row int) string {
	t.Helper()
	axis, err := excelize.CoordinatesToCellName(col, row)
	require.NoError(t, err)
	v, err := f.GetCellValue(sheet, axis)
	require.NoError(t, err)
	return v
}

func formula(t *testing.T, f *excelize.File, sheet string, col, row int) string {
	t.Helper()
	axis, err := excelize.CoordinatesToCellName(col, row)
	require.NoError(t, err)
	v, err := f.GetCellFormula(sheet, axis)
	require.NoError(t, err)
	return v
}

func writeTieredBill(t *testing.T, rows []*AggRow, discount *float64, book *PriceBook) (*excelize.File, string) {
	return writeTieredBillMarked(t, rows, discount, book, nil)
}

func writeTieredBillMarked(t *testing.T, rows []*AggRow, discount *float64, book *PriceBook, markers []string) (*excelize.File, string) {
	t.Helper()
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.xlsx")
	outPath := filepath.Join(dir, "bill.xlsx")
	buildBillFixtureTemplate(t, templatePath)

	_, err := WriteBillFromTemplate(templatePath, outPath, rows, 2026, 9, book, discount, 7.0, true, markers)
	require.NoError(t, err, "写出账单失败")
	return readBill(t, outPath)
}

// TestExprRowReconcileSingleTier 单档行：「单价 × 用量」必须能精确还原官方刊例。
func TestExprRowReconcileSingleTier(t *testing.T) {
	agg := exprTieredAgg(t, "gpt-6-astra", "oai", testExprAstra, []string{"base"}, astraBaseTok)

	rates, ok := ExprRowReconcile(testExprAstra, agg, exprAt(), 7.0)
	require.True(t, ok, "单档行应当可还原")
	assert.Equal(t, 10.0, rates.InputPerM)
	assert.Equal(t, 50.0, rates.OutputPerM)
	assert.Equal(t, 1.0, rates.CacheReadPerM)
	assert.Equal(t, 12.5, rates.CacheWritePerM)

	// 判据本身：Σ(单价 × 用量) / 1e6 == 官方美金，容差 1e-6。
	reconcileUSD := ExprUnitAmountUSD(agg, rates) / 1e6
	assert.InDelta(t, agg.OfficialUSD, reconcileUSD, 1e-6, "单价加总应还原官方刊例")
}

// TestExprRowReconcileCrossTier 跨档行：没有任何单一单价能还原金额，必须判为不可还原。
func TestExprRowReconcileCrossTier(t *testing.T) {
	agg := exprTieredAgg(t, "gpt-6-astra", "az定制", testExprAstra,
		[]string{"base", "tier_2"}, astraCrossTierTok)

	_, ok := ExprRowReconcile(testExprAstra, agg, exprAt(), 7.0)
	assert.False(t, ok, "跨档行不可还原，不能判为一致")

	// 不可还原靠的是**结构判据**（命中过多档），不是「一定算出差值」。
	// 这份数据里缓存读/写压倒性主导、且它们全在 tier_2（¥2/25），
	// 于是拿聚合总长度去选档恰好也落在 tier_2，单一单价反而能精确还原——
	// 差为 0 并不代表这行可以按「单价 × 用量」写进账单。
	// 真正的风险是聚合总长度选档本身没有依据：总长取的是「各请求长度之和」，
	// 与任何单个请求的长度都不相等，选到哪一档纯属巧合，混档行随时会选错。
	total := agg.Uncached + agg.CacheRead + agg.CacheWrite5m + agg.CacheWrite1h
	rates, err := ExtractExprRates(testExprAstra, exprAt(), total)
	require.NoError(t, err)
	diff := math.Abs(ExprUnitAmountUSD(agg, rates)/1e6 - agg.OfficialUSD)
	assert.Less(t, diff, 1e-6,
		"此数据里缓存读占比极高且都在同一档，单价比巧合地能还原——正因如此，判据不能靠数值差")
}

// TestTieredBillSingleTierRowIsReproducible 单档行的账单：S 是「单价 × 用量」公式，
// X 判「是」，单价列被填上，Y/Z/AA 与 E/G/I 方向一致，折扣注明是反推值。
func TestTieredBillSingleTierRowIsReproducible(t *testing.T) {
	agg := exprTieredAgg(t, "gpt-6-astra", "oai", testExprAstra, []string{"base"}, astraBaseTok)
	f, sheet := writeTieredBill(t, []*AggRow{agg}, nil, nil)

	// 单价列：E/G/I/K 分别是表达式系数（K 为 5 分钟档系数 12.5）。
	assert.Equal(t, "10", cell(t, f, sheet, 5, 3), "E 应为表达式输入单价")
	assert.Equal(t, "1", cell(t, f, sheet, 7, 3), "G 应为表达式缓存读单价")
	assert.Equal(t, "50", cell(t, f, sheet, 9, 3), "I 应为表达式输出单价")
	assert.Equal(t, "12.5", cell(t, f, sheet, 11, 3), "K 应为 5 分钟缓存创建单价")

	// AC（第 29 列，追加在 AB 之后）：可还原行的官方刊例由「单价 × 用量」公式给出，
	// 不含硬编码金额。
	ac := formula(t, f, sheet, 29, 3)
	require.NotEmpty(t, ac, "AC 必须是公式而不是裸数值")
	assert.Contains(t, ac, "D3*N(E3)")
	assert.Contains(t, ac, "H3*N(I3)")
	assert.NotContains(t, ac, fmt.Sprint(agg.OfficialUSD), "AC 不应出现硬编码的美金刊例")

	// S 必须是公式，统一引用 AC 再乘汇率，不再各写一套分支。
	s := formula(t, f, sheet, 19, 3)
	require.NotEmpty(t, s, "S 必须是公式而不是裸数值")
	assert.Contains(t, s, "AC3")
	assert.Contains(t, s, "*7", "S 应当乘上汇率")
	assert.NotContains(t, s, fmt.Sprint(agg.OfficialUSD), "S 不应出现硬编码的美金刊例")

	// V = S × T，W = V ÷ 汇率，都必须是公式。
	assert.Equal(t, "S3*T3", formula(t, f, sheet, 22, 3), "V 应为 总金额 × 折扣")
	assert.Equal(t, "V3/7", formula(t, f, sheet, 23, 3), "W 应为 结算人民币 ÷ 汇率")

	assert.Equal(t, "是", cell(t, f, sheet, 24, 3), "单价×用量可还原刊例，X 应为「是」")

	// Y/Z/AA 方向必须与标题一致：Y 同 E（未命中/输入），Z 同 G（命中/缓存读），AA 同 I（输出）。
	assert.Equal(t, "10", cell(t, f, sheet, 25, 3), "Y 应为未命中（输入）单价，同 E")
	assert.Equal(t, "1", cell(t, f, sheet, 26, 3), "Z 应为命中（缓存读）单价，同 G")
	assert.Equal(t, "50", cell(t, f, sheet, 27, 3), "AA 应为输出单价，同 I")

	note := cell(t, f, sheet, 28, 3)
	assert.Contains(t, note, "本行命中档位：base")
	assert.Contains(t, note, "折扣为反推值", "价表没这个家族时必须注明折扣是反推来的")
}

// TestTieredBillCrossTierRowLeavesPricesBlank 跨档行：单价列留空、X=否、
// AB 写明「本行跨档，单价不适用，请按总金额核对」，且 S 没有把误差反算进单价。
func TestTieredBillCrossTierRowLeavesPricesBlank(t *testing.T) {
	agg := exprTieredAgg(t, "gpt-6-astra", "az定制", testExprAstra,
		[]string{"base", "tier_2"}, astraCrossTierTok)
	f, sheet := writeTieredBill(t, []*AggRow{agg}, nil, nil)

	for _, tc := range []struct {
		col  int
		name string
	}{
		{5, "E"}, {7, "G"}, {9, "I"}, {11, "K"}, {13, "M"},
	} {
		assert.Empty(t, cell(t, f, sheet, tc.col, 3),
			"%s 列在跨档行必须留空，不能编一个「平均单价」出来", tc.name)
	}

	assert.Equal(t, "否", cell(t, f, sheet, 24, 3), "跨档行 X 应为「否」")

	// S 仍是公式（官方美金刊例 × 汇率），但不能写成单价 × 用量。
	s := formula(t, f, sheet, 19, 3)
	require.NotEmpty(t, s, "S 必须是公式而不是裸数值")
	assert.Contains(t, s, "*7")
	assert.NotContains(t, s, "D3*E3", "跨档行的 S 不能写成单价 × 用量——那是把误差藏起来")

	note := cell(t, f, sheet, 28, 3)
	assert.Contains(t, note, "跨 2 档", "AB 应写明跨了哪几档")
	assert.Contains(t, note, "本行跨档，单价不适用，请按总金额核对")
}

// TestTieredBillBlanksOneHourCacheWriteWhenCoefficientZero cc1h 系数为 0 时，
// K 列必须是空单元格而不是 0——写 0 会被当成「1 小时缓存免费」。
func TestTieredBillBlanksOneHourCacheWriteWhenCoefficientZero(t *testing.T) {
	agg := exprTieredAgg(t, "gpt-5.6-sol", "oai", testExprSol, []string{"tier_1"}, solTier1Tok)
	f, sheet := writeTieredBill(t, []*AggRow{agg}, nil, nil)

	// 表达式没引用 cc1h，系数为 0；即便用量列有值也不能写出 0 单价。
	assert.Equal(t, "", cell(t, f, sheet, 13, 3), "M 列在 cc1h 系数为 0 时必须留空")

	// 反向锚点：cc 系数确实是 5，K 应当被写上，说明留空是针对 cc1h 的而不是整块没填。
	assert.Equal(t, "5", cell(t, f, sheet, 11, 3), "K 列应写上 5 分钟缓存创建单价")
}

// TestGroupDiscountPrefersPriceTable 折扣优先取价表：价表里有该厂商家族的折扣时不再反推。
func TestGroupDiscountPrefersPriceTable(t *testing.T) {
	// 用显式金额而不是 exprTieredAgg 的合成比例：真实日志里 quota 折算出来的人民币
	// 就是刊例人民币乘以实际折扣（这里 1 美金刊例 × 7 汇率 × 0.5 = 3.5 元 = 1,750,000 quota）。
	// 借 ExprQuota 造 quota 会得到一个与刊例无关的量级，站点倍率兜底那条路径就验不出东西。
	row := &AggRow{
		Model: "deepseek-v3", Group: "国产A",
		Uncached: 1_000_000, Output: 100_000, Rows: 1,
		OfficialUSD: 1.0, Quota: 1_750_000,
		BillingMode: BillingModeTieredExpr, BillingExpr: testExprAstra, ListOrigin: ListOriginExpr,
	}

	book := &PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{"DeepSeek": 0.6}}
	result := ComputeGroupDiscounts([]*AggRow{row}, book, 7.0, nil, true, nil)

	assert.Equal(t, 0.6, result.Discounts["国产A"], "价表里有 DeepSeek 家族折扣，必须直接采用")
	assert.False(t, result.Derived["国产A"], "走了价表就不算反推值")

	// 价表里没有该家族时不再硬推：表达式行的刊例是站内公式自算的，没有外部对标价，
	// 反推出来的只是式子里的 group_ratio。折扣退到该行实际计费倍率，并记入 Underivable。
	empty := &PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{}}
	result2 := ComputeGroupDiscounts([]*AggRow{row}, empty, 7.0, nil, true, nil)
	assert.False(t, result2.Derived["国产A"], "站内表达式行不可反推")
	assert.Equal(t, 0.5, result2.Discounts["国产A"], "应退回站点实际计费倍率（3.5/(1×7)）")
	assert.Contains(t, result2.Underivable["国产A"], "无法反推折扣", "必须写明不可反推的原因")

	// 强制折扣优先级最高，且不算反推，也不留 Underivable 备注。
	forced := 0.42
	result3 := ComputeGroupDiscounts([]*AggRow{row}, book, 7.0, &forced, true, nil)
	assert.Equal(t, 0.42, result3.Discounts["国产A"])
	assert.False(t, result3.Derived["国产A"])
	assert.Empty(t, result3.Underivable)
}

// TestGroupDiscountNoDivisionByZero 零用量行不能让折扣变成 0/0。
func TestGroupDiscountNoDivisionByZero(t *testing.T) {
	agg := exprTieredAgg(t, "gpt-6-astra", "空分组", testExprAstra, nil, [5]float64{})
	book := &PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{}}

	result := ComputeGroupDiscounts([]*AggRow{agg}, book, 7.0, nil, true, nil)
	got := result.Discounts["空分组"]
	assert.False(t, math.IsNaN(got), "零用量时折扣不能是 NaN")
	assert.Equal(t, 0.0, got, "零用量行的站点倍率折算是 0")
	assert.False(t, result.Derived["空分组"], "零用量不可反推")
	assert.NotEmpty(t, result.Underivable["空分组"], "零用量分组也必须提示人工确认")
}

// TestDerivableListPriceSkipsExpressionRows 判据本身：只有外部对标价才可反推。
// 这是「国产模型折扣反推异常」的真正根因——与模型是不是国产无关，
// 任何走站内表达式计费的行都没有外部对标价，拿它当分母恢复出来的是 group_ratio。
func TestDerivableListPriceSkipsExpressionRows(t *testing.T) {
	exprRow := &AggRow{
		Model: "deepseek-v4.1-flash", Group: "国产模型", OfficialUSD: 1.0,
		BillingMode: BillingModeTieredExpr, BillingExpr: testExprAstra, ListOrigin: ListOriginExpr,
	}
	assert.True(t, HasKnownListPrice(exprRow), "表达式算出过刊例，仍算「有价」供展示")
	assert.False(t, DerivableListPrice(exprRow, nil), "站内表达式刊例不可作反推分母")

	externalRow := &AggRow{
		Model: "claude-sonnet-5", Group: "海外", OfficialUSD: 1.0, ListOrigin: ListOriginExternal,
	}
	assert.True(t, DerivableListPrice(externalRow, nil), "外部对标价可反推")

	mixedRow := &AggRow{
		Model: "claude-sonnet-5", Group: "混用", OfficialUSD: 1.0, ListOrigin: ListOriginMixed,
	}
	assert.False(t, DerivableListPrice(mixedRow, nil), "账期内混用两种口径的桶不可反推")

	noPrice := &AggRow{Model: "unknown-x", Group: "未知", ListOrigin: ListOriginNone}
	assert.False(t, DerivableListPrice(noPrice, nil), "没有刊例不可反推")
}

// TestDerivableListPriceHonorsManualMarkers 人工标识兜底：模型名前缀推不出的
// 站内定价分组，由用户显式标注后同样排除出反推。
func TestDerivableListPriceHonorsManualMarkers(t *testing.T) {
	row := &AggRow{
		Model: "doubao-pro-32k", Group: "国产模型", OfficialUSD: 1.0, ListOrigin: ListOriginExternal,
	}
	assert.Equal(t, "", VendorFamily(row.Model), "doubao 不在内置厂商前缀表里，自动识别不到")
	assert.True(t, DerivableListPrice(row, nil), "未标记时可反推")

	// 按分组名标记（精确匹配）。
	assert.False(t, DerivableListPrice(row, []string{"国产模型"}), "分组名命中即排除")
	// 按模型名前缀标记（前缀匹配，大小写不敏感）。
	assert.False(t, DerivableListPrice(row, []string{"DOUBAO"}), "模型前缀命中即排除")
	// 无关标识不影响。
	assert.True(t, DerivableListPrice(row, []string{"qianwen", "海外分组"}), "无关标识不影响判定")
}

// TestAggregateMarksExpressionRowAsExprOrigin 聚合阶段就必须把口径记到行上，
// 否则下游拿不到「这个数字是站内公式算的」这一事实。
func TestAggregateMarksExpressionRowAsExprOrigin(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "created_at"}
	other := `{"expr_b64":"bGVuIDw9IDI3MjAwMCA/IHRpZXIoImJhc2UiLCBwICogMTAgKyBjICogNTApIDogdGllcigidGllcl8yIiwgcCAqIDIwICsgYyAqIDc1KQ==","cache_tokens":0}`
	rows := [][]string{{"deepseek-v4.1-flash", "国产模型", "100000", "2000", "12345", other, "1755000000"}}

	result, err := AggregateFromRows(rows, headers, nil, 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	assert.Equal(t, ListOriginExpr, result.Rows[0].ListOrigin, "表达式行必须标成 expr 来源")
	assert.False(t, DerivableListPrice(result.Rows[0], nil), "该行不可参与反推")
}

// TestAggregateMarksRatioRowAsExternalOrigin ratio 快照的换算基准是官方锚点
// （ratio=1 → $2/MTok），不是站内自有公式，因此仍算外部对标价、可参与反推。
func TestAggregateMarksRatioRowAsExternalOrigin(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "created_at"}
	other := `{"model_ratio":1,"completion_ratio":4,"cache_ratio":0.1,"cache_tokens":0}`
	rows := [][]string{{"claude-sonnet-5", "海外", "100000", "2000", "12345", other, "1755000000"}}

	result, err := AggregateFromRows(rows, headers, nil, 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	assert.Equal(t, ListOriginExternal, result.Rows[0].ListOrigin, "ratio 快照应标成外部对标价")
	assert.True(t, DerivableListPrice(result.Rows[0], nil), "ratio 行可参与反推")
}

// TestAggregateMarksMixedOriginWhenRowShapesDiffer 同一个 (模型,分组) 内两种口径混用时，
// 整桶记成 mixed 并退出反推——半截分母算出来的折扣比不反推更危险。
func TestAggregateMarksMixedOriginWhenRowShapesDiffer(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "created_at"}
	exprOther := `{"expr_b64":"bGVuIDw9IDI3MjAwMCA/IHRpZXIoImJhc2UiLCBwICogMTAgKyBjICogNTApIDogdGllcigidGllcl8yIiwgcCAqIDIwICsgYyAqIDc1KQ=="}`
	ratioOther := `{"model_ratio":1,"completion_ratio":4,"cache_ratio":0.1}`
	rows := [][]string{
		{"deepseek-v4.1-flash", "国产模型", "100000", "2000", "12345", exprOther, "1755000000"},
		{"deepseek-v4.1-flash", "国产模型", "50000", "1000", "6789", ratioOther, "1755100000"},
	}

	result, err := AggregateFromRows(rows, headers, nil, 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	assert.Equal(t, ListOriginMixed, result.Rows[0].ListOrigin, "两种口径混用应标成 mixed")
	assert.False(t, DerivableListPrice(result.Rows[0], nil), "mixed 桶不可反推")
}

// TestBillNotesUndeterminableDiscount 账单备注必须写清楚折扣是怎么来的：
// 不可反推的分组不能只留一个看起来像谈定值的数字。
func TestBillNotesUndeterminableDiscount(t *testing.T) {
	agg := &AggRow{
		Model: "deepseek-v4.1-flash", Group: "国产模型",
		Uncached: 1_000_000, Output: 100_000, Rows: 1,
		OfficialUSD: 1.0, Quota: 1_750_000,
		BillingMode: BillingModeTieredExpr, BillingExpr: testExprAstra,
		ExprTiers: []string{"base"}, ListOrigin: ListOriginExpr, LastAt: exprAt(),
	}
	empty := &PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{}}

	f, sheet := writeTieredBill(t, []*AggRow{agg}, nil, empty)
	note := cell(t, f, sheet, 28, 3)
	assert.Contains(t, note, "无法反推折扣", "备注要写明不可反推")
	assert.Contains(t, note, "人工确认", "备注要要求人工确认合同折扣")
	assert.NotContains(t, note, "折扣取自价表", "不可反推时不能声称折扣来自价表")
}

// TestBillNotesManualMarkerExcludedFromDerivation 人工标识命中的分组：
// 折扣退回站点倍率，且在备注里说明是被标记排除的。
//
// 用 gpt-4o（价表里有价）而不是造一个查不到价的模型名：账单写出时
// ResolvePrice 为 nil 会走不到备注分支，测试就绕开了要验的逻辑。
func TestBillNotesManualMarkerExcludedFromDerivation(t *testing.T) {
	agg := &AggRow{
		Model: "claude-sonnet-5", Group: "国产模型",
		Uncached: 1_000_000, Output: 100_000, Quota: 3_500_000, Rows: 1,
		OfficialUSD: 1.0, ListOrigin: ListOriginExternal, BillingMode: "token",
	}
	empty := &PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{}}

	f, sheet := writeTieredBillMarked(t, []*AggRow{agg}, nil, empty, []string{"国产模型"})
	note := cell(t, f, sheet, 28, 3)
	assert.Contains(t, note, "站内定价标识", "备注要写明是被人工标记排除的")
	assert.Contains(t, note, "人工确认")
}

// TestExtractDistinctGroups 分组列表供前端勾选国产/站内定价：
// 必须去重、去空白、按名称排序，且缺列时报错而不是静默返回空。
func TestExtractDistinctGroups(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens"}
	rows := [][]string{
		{"deepseek-v4.1-flash", "国产模型", "100"},
		{"qwen3.8-max", "国产模型", "200"},
		{"gpt-5.4", " AZ定制 ", "300"},
		{"gemini-2.5-pro", "vip", "400"},
		{"claude-sonnet-5", "", "500"},
		{"claude-sonnet-5", "vip", "600"},
	}

	groups, err := ExtractDistinctGroups(headers, rows)
	require.NoError(t, err)
	assert.Equal(t, []string{"AZ定制", "vip", "国产模型"}, groups,
		"应按名称排序、去重，并去掉分组名两端的空白；空分组不计入")

	// 缺列必须报错，否则前端会把「没有分组列」当成「日志里没有分组」而静默放行。
	_, err = ExtractDistinctGroups([]string{"model_name"}, rows)
	assert.Error(t, err)
}

// TestExtractDistinctGroupsFeedsDomesticMarkers 分组列表勾出来的名字必须能被
// DerivableListPrice 认出来：列表给用户看的是 trimmed 名字，判定用的也是 trimmed 名字。
func TestExtractDistinctGroupsFeedsDomesticMarkers(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens"}
	rows := [][]string{{"doubao-pro-32k", " 国产模型 ", "100"}}

	groups, err := ExtractDistinctGroups(headers, rows)
	require.NoError(t, err)
	require.Equal(t, []string{"国产模型"}, groups)

	row := &AggRow{
		Model: "doubao-pro-32k", Group: strings.TrimSpace(rows[0][1]),
		OfficialUSD: 1.0, ListOrigin: ListOriginExternal,
	}
	assert.False(t, DerivableListPrice(row, groups), "勾选该分组后不再参与反推")
}


// TestDomesticExprUnitColumnsAreUSD 人民币计价的表达式系数必须归一成美金再进单价列。
//
// 账单模板的单价列表头写的是「美金/百万token」，而国产模型在站上按人民币报价
// （1元=1美金的充值比例，表达式里的 p*1 就是「每百万 1 元」）。系数原样填进美金列，
// 客户按美金读会虚高 7 倍，且 AC 列（单价×用量）会连带把总金额放大一个汇率倍数。
func TestDomesticExprUnitColumnsAreUSD(t *testing.T) {
	const rate = 7.0
	// p*1 + c*4：人民币口径，即每百万输入 1 元、输出 4 元。
	expr := `(tier("default", p * 1 + c * 4))`
	at := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)

	makeAgg := func(model, currency string) *AggRow {
		agg := &AggRow{
			Model: model, Group: "国产模型",
			Uncached: 1_000_000, Output: 1_000_000, Rows: 1,
			BillingMode: BillingModeTieredExpr, BillingExpr: expr,
			ExprTiers: []string{"default"}, LastAt: at, ExprUnitCurrency: currency,
		}
		// 传原始 prompt（uncached + 各类缓存），与生产一致：BuildExprParams 内部自行扣一次。
		params := BuildExprParams(model,
			agg.Uncached+agg.CacheRead+agg.CacheWrite5m+agg.CacheWrite1h, agg.Output,
			agg.CacheRead, agg.CacheWrite5m, agg.CacheWrite1h, 0, 0, 0, 0, expr)
		res, err := RunBillingExpr(expr, params, at)
		require.NoError(t, err)
		agg.OfficialUSD = res.USD / 1_000_000
		if VendorFamily(model) != "" {
			agg.OfficialUSD /= rate
		}
		return agg
	}

	// 人民币计价：单价列应是人民币 ÷ 汇率，且能还原官方美金刊例。
	rate1, ok := ExprRowReconcile(expr, makeAgg("deepseek-v4.1-flash", "CNY"), at, rate)
	require.True(t, ok, "归一后应能还原官方美金刊例")
	assert.InDelta(t, 1.0/rate, rate1.InputPerM, 1e-9, "1 元/百万 应折成 1/7 美金/百万")
	assert.InDelta(t, 4.0/rate, rate1.OutputPerM, 1e-9)

	// 同一份系数若币种没标对，就还原不出美金刊例——这正是修复前的症状，
	// 也是这个标记存在的意义：不能靠模型名去猜，得由聚合阶段明确记下来。
	_, okUSD := ExprRowReconcile(expr, makeAgg("deepseek-v4.1-flash", "USD"), at, rate)
	assert.False(t, okUSD, "币种标错时单价无法还原刊例，判据必须拦住")

	// 海外模型系数本身就是美金，不参与归一。
	// 注意这里必须用海外自己的表达式（testExprAstra，base 档 p*10 + c*50）造刊例，
	// 不能复用人民币表达式的 makeAgg——那份刊例和美金系数对不上，比出来的不是币种问题。
	overseas := &AggRow{
		Model: "gpt-6-astra", Group: "oai",
		Uncached: 1_000_000, Output: 1_000_000, Rows: 1,
		BillingMode: BillingModeTieredExpr, BillingExpr: testExprAstra,
		ExprTiers: []string{"base"}, LastAt: at, ExprUnitCurrency: "USD",
	}
	params := BuildExprParams(overseas.Model,
		overseas.Uncached+overseas.CacheRead+overseas.CacheWrite5m+overseas.CacheWrite1h, overseas.Output,
		0, 0, 0, 0, 0, 0, 0, testExprAstra)
	res, err := RunBillingExpr(testExprAstra, params, at)
	require.NoError(t, err)
	overseas.OfficialUSD = res.USD / 1_000_000

	rate2, ok2 := ExprRowReconcile(testExprAstra, overseas, at, rate)
	require.True(t, ok2, "海外美金系数应能还原刊例")
	// 2,000,000 token 超过 testExprAstra 的 272,000 阈值，命中 tier_2 档（p*20 + c*75）。
	assert.InDelta(t, 20.0, rate2.InputPerM, 1e-9, "美金系数不应被汇率改动")
	assert.InDelta(t, 75.0, rate2.OutputPerM, 1e-9)
}

// TestAggregateMarksExprUnitCurrency 聚合阶段必须记下表达式系数的币种，
// 否则写账单时无从知道该不该除以汇率。
func TestAggregateMarksExprUnitCurrency(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "created_at"}
	// expr: tier("default", p * 1 + c * 4)
	exprB64 := "dGllcigiZGVmYXVsdCIsIHAgKiAxICsgYyAqIDQp"
	rows := [][]string{
		{"deepseek-v4.1-flash", "国产模型", "100000", "2000", "12345", `{"expr_b64":"` + exprB64 + `"}`, "1755000000"},
		{"claude-sonnet-5", "海外", "100000", "2000", "12345", `{"expr_b64":"` + exprB64 + `"}`, "1755000000"},
	}

	result, err := AggregateFromRows(rows, headers, nil, 7.0, false, nil, false, nil)
	require.NoError(t, err)
	require.Len(t, result.Rows, 2)

	byModel := map[string]*AggRow{}
	for _, r := range result.Rows {
		byModel[r.Model] = r
	}
	assert.Equal(t, "CNY", byModel["deepseek-v4.1-flash"].ExprUnitCurrency,
		"国产供应商家族的表达式系数按人民币计价")
	assert.Equal(t, "USD", byModel["claude-sonnet-5"].ExprUnitCurrency,
		"非国产模型的表达式系数即美金")
	assert.Equal(t, 7.0, byModel["deepseek-v4.1-flash"].ExprUnitDivisor(7.0))
	assert.Equal(t, 1.0, byModel["claude-sonnet-5"].ExprUnitDivisor(7.0))
}

// 国产厂商模型只要挂的是第三方部署、有官方对标价，就照样能反推；
// 「是不是国产」和「有没有外部对标价」是两回事，判据以后者为准。
func TestAutoVendorFamilyDoesNotSuppressDerivation(t *testing.T) {
	agg := &AggRow{
		Model: "deepseek-v3", Group: "deepseek组",
		Uncached: 1_000_000, Quota: 3_500_000, Rows: 1,
		OfficialUSD: 1.0, ListOrigin: ListOriginExternal, BillingMode: "token",
	}
	assert.Equal(t, "DeepSeek", VendorFamily(agg.Model), "厂商家族仍能自动识别（用于币种与价表折扣）")
	assert.False(t, IsDomesticMarked(agg.Model, agg.Group, nil), "自动识别不产生人工标记")

	empty := &PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{}}
	result := ComputeGroupDiscounts([]*AggRow{agg}, empty, 7.0, nil, true, nil)
	assert.True(t, result.Derived["deepseek组"], "有外部对标价的国产模型仍可反推")
	assert.Empty(t, result.Underivable)
}

// TestTieredPricesCoverExpressionModels 表达式模型的兜底价表不能被漏掉，
// 否则降级路径会静默按 0 计价。
func TestTieredPricesCoverExpressionModels(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
		tier, ok := TieredModelPrices[model]
		require.True(t, ok, "%s 必须在 TieredModelPrices 里有兜底价", model)
		assert.Greater(t, tier.Low[0], 0.0, "%s 低档输入单价应为正", model)
		assert.Greater(t, tier.High[0], tier.Low[0], "%s 高档输入单价应高于低档", model)
		assert.NotEmpty(t, tier.Op)
	}

	// 阈值比较符必须与表达式一致，不能自行统一成一种写法。
	assert.Equal(t, "le", TieredModelPrices["gpt-6-astra"].Op)
	assert.Equal(t, "lt", TieredModelPrices["gpt-5.6-sol"].Op)

	// 系数与表达式一致（gpt-6-astra base：p*10 + c*50 + cr*1）。
	assert.Equal(t, 10.0, TieredModelPrices["gpt-6-astra"].Low[0])
	assert.Equal(t, 50.0, TieredModelPrices["gpt-6-astra"].Low[1])
	assert.Equal(t, 1.0, TieredModelPrices["gpt-6-astra"].Low[2])
}

// TestBillFormulasReconcileAcrossRows 逐行核对账单的自洽性：
//   - 单档行：Σ(单价 × 用量) / 1e6 == 官方刊例美金（容差 1e-6）
//   - 任意行：V == S × T，W == V ÷ 汇率
//   - 跨档行：单价列留空且 X == "否"
//
// 失败时把行号与差额列出来，不靠肉眼看表。
func TestBillFormulasReconcileAcrossRows(t *testing.T) {
	rows := []*AggRow{
		exprTieredAgg(t, "gpt-6-astra", "oai", testExprAstra, []string{"base"}, astraBaseTok),
		exprTieredAgg(t, "gpt-6-astra", "az定制", testExprAstra, []string{"base", "tier_2"}, astraCrossTierTok),
		exprTieredAgg(t, "gpt-5.6-sol", "oai", testExprSol, []string{"tier_1"}, solTier1Tok),
	}
	f, sheet := writeTieredBill(t, rows, nil, nil)

	const exchangeRate = 7.0
	var failures []string
	add := func(format string, args ...interface{}) {
		failures = append(failures, fmt.Sprintf(format, args...))
	}

	for i, agg := range rows {
		r := 3 + i

		// 1. 单价 × 用量 能否还原官方刊例。
		rates, ok := ExprRowReconcile(agg.BillingExpr, agg, exprAt(), 7.0)
		if ok {
			unitUSD := ExprUnitAmountUSD(agg, rates) / 1e6
			if delta := math.Abs(unitUSD - agg.OfficialUSD); delta >= 1e-6 {
				add("第 %d 行：Σ(单价×用量)/1e6 = %.10f，官方刊例 = %.10f，差 %.3e > 1e-6",
					r, unitUSD, agg.OfficialUSD, delta)
			}
			// 单价列必须真的写上，否则客户看不到可复现的依据。
			for _, col := range []int{5, 9} {
				if cell(t, f, sheet, col, r) == "" {
					add("第 %d 行：可还原但 %s 列单价为空", r, colName(col))
				}
			}
			if got := cell(t, f, sheet, 24, r); got != "是" {
				add("第 %d 行：可还原但 X = %q，应为「是」", r, got)
			}
		} else {
			for _, col := range []int{5, 7, 9, 11, 13} {
				if got := cell(t, f, sheet, col, r); got != "" {
					add("第 %d 行：不可还原但 %s 列有值 %q，应留空", r, colName(col), got)
				}
			}
			if got := cell(t, f, sheet, 24, r); got != "否" {
				add("第 %d 行：不可还原但 X = %q，应为「否」", r, got)
			}
		}

		// 2. S 必须是公式，且不含硬编码金额（跨档行允许「刊例 × 汇率」的写法，
		//    但那同样是算式，不是裸数值）。
		sf := formula(t, f, sheet, 19, r)
		if sf == "" {
			add("第 %d 行：S 列不是公式，是裸数值 %q", r, cell(t, f, sheet, 19, r))
		}
		if strings.Contains(sf, fmt.Sprint(agg.OfficialUSD)) && ok {
			add("第 %d 行：可还原行的 S 不应写死美金刊例", r)
		}

		// 3. V == S × T，W == V ÷ 汇率。
		if got, want := formula(t, f, sheet, 22, r), fmt.Sprintf("S%d*T%d", r, r); got != want {
			add("第 %d 行：V 公式为 %q，应为 %q", r, got, want)
		}
		if got, want := formula(t, f, sheet, 23, r), fmt.Sprintf("V%d/%s", r, formatFloat(exchangeRate)); got != want {
			add("第 %d 行：W 公式为 %q，应为 %q", r, got, want)
		}

		// 4. 折扣必须有值，且与表里写的一致（V/S）。
		disc := cell(t, f, sheet, 20, r)
		if disc == "" {
			add("第 %d 行：折扣列为空", r)
		}

		// 5. AB 必须能追到折扣来源。
		if note := cell(t, f, sheet, 28, r); !strings.Contains(note, "折扣") {
			add("第 %d 行：AB 未注明折扣来源，实际为 %q", r, note)
		}
	}

	assert.Empty(t, failures, "账单自洽性核对未通过：\n%s", strings.Join(failures, "\n"))
}

func colName(col int) string {
	name, _ := excelize.ColumnNumberToName(col)
	return name
}
