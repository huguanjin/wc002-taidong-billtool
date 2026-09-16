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

	params := BuildExprParams(model, uncached+cacheRead+cache5m+cache1h, uncached, out,
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
	t.Helper()
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.xlsx")
	outPath := filepath.Join(dir, "bill.xlsx")
	buildBillFixtureTemplate(t, templatePath)

	_, err := WriteBillFromTemplate(templatePath, outPath, rows, 2026, 9, book, discount, 7.0, true)
	require.NoError(t, err, "写出账单失败")
	return readBill(t, outPath)
}

// TestExprRowReconcileSingleTier 单档行：「单价 × 用量」必须能精确还原官方刊例。
func TestExprRowReconcileSingleTier(t *testing.T) {
	agg := exprTieredAgg(t, "gpt-6-astra", "oai", testExprAstra, []string{"base"}, astraBaseTok)

	rates, ok := ExprRowReconcile(testExprAstra, agg, exprAt())
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

	_, ok := ExprRowReconcile(testExprAstra, agg, exprAt())
	assert.False(t, ok, "跨档行不可还原，不能判为一致")

	// 反证：拿聚合后的总长度去求单价，得到的金额还原不出官方刊例。
	// 所以硬补一句「单价 × 总量」只会把误差藏起来。
	total := agg.Uncached + agg.CacheRead + agg.CacheWrite5m + agg.CacheWrite1h
	rates, err := ExtractExprRates(testExprAstra, exprAt(), total)
	require.NoError(t, err)
	diff := math.Abs(ExprUnitAmountUSD(agg, rates)/1e6 - agg.OfficialUSD)
	assert.Greater(t, diff, 1e-6, "跨档行用单一单价算出的金额与官方刊例必然有差")
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
	agg := exprTieredAgg(t, "deepseek-v3", "国产A", testExprAstra, []string{"base"}, astraBaseTok)

	book := &PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{"DeepSeek": 0.6}}
	discounts, derived := ComputeGroupDiscounts([]*AggRow{agg}, book, 7.0, nil, true)

	assert.Equal(t, 0.6, discounts["国产A"], "价表里有 DeepSeek 家族折扣，必须直接采用")
	assert.False(t, derived["国产A"], "走了价表就不算反推值")

	// 价表里没有该家族时退回反推，并标记为反推值。
	empty := &PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{}}
	discounts2, derived2 := ComputeGroupDiscounts([]*AggRow{agg}, empty, 7.0, nil, true)
	assert.True(t, derived2["国产A"], "价表没有该家族时必须标记为反推值")
	wantDerived := round((ExprQuota(agg.OfficialUSD, 1.0)/QuotaPerCNY)/(agg.OfficialUSD*7.0), DiscountDecimals)
	assert.Equal(t, wantDerived, discounts2["国产A"])

	// 强制折扣优先级最高，且不算反推。
	forced := 0.42
	discounts3, derived3 := ComputeGroupDiscounts([]*AggRow{agg}, book, 7.0, &forced, true)
	assert.Equal(t, 0.42, discounts3["国产A"])
	assert.False(t, derived3["国产A"])
}

// TestGroupDiscountNoDivisionByZero 零用量行不能让折扣变成 0/0。
func TestGroupDiscountNoDivisionByZero(t *testing.T) {
	agg := exprTieredAgg(t, "gpt-6-astra", "空分组", testExprAstra, nil, [5]float64{})
	book := &PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{}}

	discounts, derived := ComputeGroupDiscounts([]*AggRow{agg}, book, 7.0, nil, true)
	got := discounts["空分组"]
	assert.False(t, math.IsNaN(got), "零用量时折扣不能是 NaN")
	assert.Equal(t, 0.0, got)
	assert.True(t, derived["空分组"])
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
		rates, ok := ExprRowReconcile(agg.BillingExpr, agg, exprAt())
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
