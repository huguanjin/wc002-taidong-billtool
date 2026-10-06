package billing

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

// ---- 成本公式 ----

// TestUpstreamCostFormula 成本与站内结算同源：成本 = 官方刊例(人民币) × (上游倍率 / 7)。
//
// 文档给的两个手算样例（用 db_price_cache 的真实数字）：
//   - gpt-5-mini / AZ：OfficialUSD=24.233498、GroupRatio=1.8 → 结算 43.6205；
//     上游倍率同为 1.8 时成本应也等于 43.6205。
//   - gpt-5.5 / Codex(0.4)：OfficialUSD=12.119489、倍率 0.4 → 结算 5.1716；
//     上游倍率 0.4 时成本应为 12.119489 × 7 × (0.4/7) = 4.8478。
func TestUpstreamCostFormula(t *testing.T) {
	const rate = 7.0
	f := func(v float64) *float64 { return &v }

	cases := []struct {
		name        string
		officialUSD float64
		upstream    *float64
		wantCost    float64
		wantOK      bool
	}{
		// 文档里的样例值是四舍五入到分位的展示值（精确为 43.6202964），
		// 所以容差取分位以内而不是 1e-4。
		{"gpt-5-mini/AZ 倍率 1.8", 24.233498, f(1.8), 43.6205, true},
		{"gpt-5.5/Codex 倍率 0.4", 12.119489, f(0.4), 4.8478, true},
		// 倍率 0 表示上游免费：成本真的是 0，与「未维护」是两回事。
		{"倍率 0 表示免费", 10.0, f(0.0), 0.0, true},
		// nil 表示未维护：必须返回 ok=false，让调用方留空而不是写 0。
		{"未维护倍率", 10.0, nil, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := &CostRow{
				AggRow:        &AggRow{OfficialUSD: tc.officialUSD},
				UpstreamRatio: tc.upstream,
			}
			got, ok := row.UpstreamCostCNY(rate)
			assert.Equal(t, tc.wantOK, ok, "ok 必须区分「未维护」与「成本为 0」")
			if tc.wantOK {
				assert.InDelta(t, tc.wantCost, got, 0.001)
				// 与公式的另一种写法等价：OfficialUSD × 汇率 × (倍率/7)。
				assert.InDelta(t, tc.officialUSD*rate*(*tc.upstream/DiscountBaseFactor), got, 1e-9)
			}
		})
	}
}

// TestUpstreamCostEqualsSettleWhenRatiosMatch 交叉验证（文档验收项）：
// 上游倍率与客户分组倍率相同时，成本应等于结算额 V——同一恒等式的直接体现。
func TestUpstreamCostEqualsSettleWhenRatiosMatch(t *testing.T) {
	const rate = 7.0
	// 表达式计费行：quota = 表达式USD × groupRatio × 5e5，
	// 而 OfficialUSD = 表达式USD，于是结算额 = quota/5e5。
	const listUSD = 12.119489
	const groupRatio = 0.4
	row := &AggRow{
		OfficialUSD: listUSD, GroupRatio: groupRatio, Quota: listUSD * groupRatio * QuotaPerCNY,
		BillingMode: BillingModeTieredExpr, BillingExpr: "tier(\"base\", p*1)",
	}
	require.True(t, row.HasRatioDiscount())

	settle := OfficialListCNY(row, rate) * row.RatioDiscount()
	cost := &CostRow{AggRow: row, UpstreamRatio: &[]float64{groupRatio}[0]}
	got, ok := cost.UpstreamCostCNY(rate)
	require.True(t, ok)
	assert.InDelta(t, settle, got, 1e-9, "上游倍率等于分组倍率时，成本应等于结算额")
	assert.InDelta(t, row.Quota/QuotaPerCNY, got, 1e-9, "也应等于站内实收")
}

// ---- 倍率检查的三分类 ----

// TestCheckUpstreamRatiosThreeWay 日志里的渠道必须被分成三类，
// 尤其是「渠道表里查不到的」不能混进 Missing——那种渠道无法补录，页面引导会把人带偏。
func TestCheckUpstreamRatiosThreeWay(t *testing.T) {
	channels := map[int]ChannelInfo{
		101: {ChannelID: 101, Name: "AZ"},
		102: {ChannelID: 102, Name: "AWS"},
	}
	ratios := map[int]float64{101: 1.8}

	st := CheckUpstreamRatios([]int{101, 102, 999}, ratios, channels)

	require.Len(t, st.Maintained, 1)
	assert.Equal(t, 101, st.Maintained[0].ChannelID)

	require.Len(t, st.Missing, 1, "已维护的与未知的都不能落进 Missing")
	assert.Equal(t, 102, st.Missing[0].ChannelID)

	require.Len(t, st.UnknownChannelIDs, 1, "渠道表查不到的必须单独归类")
	assert.Equal(t, 999, st.UnknownChannelIDs[0])

	// 重复的渠道号只算一次。
	st2 := CheckUpstreamRatios([]int{101, 101, 101}, ratios, channels)
	assert.Len(t, st2.Maintained, 1)
	assert.Empty(t, st2.Missing)
	assert.Empty(t, st2.UnknownChannelIDs)

	// 全部维护好了就没有待办。
	st3 := CheckUpstreamRatios([]int{101}, ratios, channels)
	assert.Empty(t, st3.Missing)
	assert.Empty(t, st3.UnknownChannelIDs)
}

// TestCheckUpstreamRatiosMaintainedWinsOverUnknown 回归：渠道表里查不到、但倍率已维护的渠道
// 必须算 Maintained。
//
// 页面会把日志里出现、渠道表里没有的渠道号也列出来供就地补录（main.go 的
// handleCheckChannels）。如果这里先判渠道表成员资格，那个输入框就是个摆设：
// 用户填了、保存了，出账时仍被判成 Unknown 而拦下，永远出不来成本利润表。
func TestCheckUpstreamRatiosMaintainedWinsOverUnknown(t *testing.T) {
	channels := map[int]ChannelInfo{101: {ChannelID: 101, Name: "AZ"}}
	// 101 在渠道表里也已维护；765 不在渠道表里（业务库已硬删除），但同样维护了倍率。
	ratios := map[int]float64{101: 1.8, 765: 0.15}

	st := CheckUpstreamRatios([]int{101, 765}, ratios, channels)

	require.Len(t, st.Maintained, 2, "已维护倍率的渠道一律算已维护，与渠道表有无无关")
	require.Empty(t, st.Missing)
	assert.Empty(t, st.UnknownChannelIDs, "有倍率就不该再报「未知」")

	var find765 *ChannelInfo
	for i := range st.Maintained {
		if st.Maintained[i].ChannelID == 765 {
			find765 = &st.Maintained[i]
		}
	}
	require.NotNil(t, find765, "765 应出现在已维护清单里")
	assert.Contains(t, find765.Name, "765", "渠道表里查不到名字时要有占位名，不能留空")
}

// TestGenerateCostTableUnknownChannelDoesNotBlock 回归：未知渠道不该拦住成本利润表。
//
// 这类渠道业务库已查不到、无法补录，拦下来等于成本利润表永远出不来。
// 处置与页面提示、DEPLOY.md 一致：成本列留空、不计入合计，但**成本利润表照常生成**。
func TestGenerateCostTableUnknownChannelDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.xlsx")
	billPath := filepath.Join(dir, "账单.xlsx")
	buildBillFixtureTemplate(t, templatePath)

	headers, rows := buildCostFixture(t)
	// 这一批日志原本走 101 与 102。把第一行改成未知渠道 999，
	// 让日志里只剩「已维护的 102」和「渠道表查不到的 999」——
	// 这样才只触发未知渠道、不触发待补录，验证的是「未知渠道不拦」。
	chIdx := 0
	for i, h := range headers {
		if h == "channel_id" {
			chIdx = i
		}
	}
	rows[0][chIdx] = "999"

	params := Params{
		ChannelUpstreamRatios: map[int]float64{102: 1.8},
		ChannelNames:          map[int]string{102: "AZ", 999: "渠道 999"},
		ChannelInfos:          map[int]ChannelInfo{102: {ChannelID: 102, Name: "AZ"}},
	}

	costPath, totals, summaryText, blocked, missing, unknown, err := generateCostTable(
		"", templatePath, billPath, rows, headers,
		&PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{}},
		params, DiscountOverrides{}, 7.0, false, 2026, 9)
	require.NoError(t, err)

	assert.False(t, blocked, "只有未知渠道时不该拦下成本利润表")
	assert.Empty(t, missing)
	assert.Equal(t, []int{999}, unknown, "未知渠道仍要如实报出，供页面提示")
	assert.NotEmpty(t, costPath, "成本利润表应正常生成")
	require.NotNil(t, totals, "成功生成时应一并给出合计")
	assert.NotEmpty(t, summaryText, "成功生成时应一并给出可复制的说明文字")
}

// TestWriteCostProfitColumn 成本利润表的 AH 列（利润 = V − AG）：
//   - 已维护倍率的行写 IF 判空公式，不是写死的数值，可在 Excel 里追溯；
//   - 未维护倍率的行必须留空——Excel 把空当 0，直接写 V-AG 会按「上游免费」算出虚高毛利；
//   - 合计行对 AH 求 SUM，空行被自动跳过。
func TestWriteCostProfitColumn(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.xlsx")
	outPath := filepath.Join(dir, "cost.xlsx")
	buildBillFixtureTemplate(t, templatePath)

	const rate = 7.0
	f := func(v float64) *float64 { return &v }
	rows := []*CostRow{
		{AggRow: &AggRow{
			Model: "gpt-5.5", Group: "Codex|0.4", KeyGroup: "Codex", GroupRatio: 0.4,
			Uncached: 1000, Output: 100, Quota: 5000, Rows: 1, OfficialUSD: 12.119489,
			BillingMode: BillingModeTieredExpr, BillingExpr: `tier("base", p*1)`,
		}, ChannelID: 101, ChannelName: "AZ", UpstreamRatio: f(0.4)},
		{AggRow: &AggRow{
			Model: "gpt-5.5", Group: "Codex|0.4", KeyGroup: "Codex", GroupRatio: 0.4,
			Uncached: 2000, Output: 200, Quota: 9000, Rows: 1, OfficialUSD: 6.0,
			BillingMode: BillingModeTieredExpr, BillingExpr: `tier("base", p*1)`,
		}, ChannelID: 102, ChannelName: "AWS"}, // 未维护倍率
	}

	require.NoError(t, WriteCostFromTemplate(templatePath, outPath, rows, 2026, 9,
		nil, DiscountOverrides{}, rate, false, nil))

	file, err := excelize.OpenFile(outPath)
	require.NoError(t, err)
	defer file.Close()
	sheet := file.GetSheetName(0)
	cellRows, err := file.GetRows(sheet)
	require.NoError(t, err)

	assert.Equal(t, "利润（人民币）", cellAt(cellRows[0], 33), "AH 列表头")

	// 第一行：利润写成引用同行 V 与 AG 的公式，而不是固定金额。
	assert.Equal(t, `IF(AG3="","",V3-AG3)`, formula(t, file, sheet, 34, 3),
		"AH 应为可追溯的公式，且对空白 AG 做保护")

	// 第二行（未维护倍率）：AF/AG/AH 三列都必须留空。
	assert.Equal(t, "", cell(t, file, sheet, 32, 4), "未维护倍率的 AF 留空")
	assert.Equal(t, "", cell(t, file, sheet, 33, 4), "未维护倍率的 AG 留空")
	assert.Equal(t, "", cell(t, file, sheet, 34, 4),
		"未维护倍率的 AH 必须留空：Excel 把空当 0，V-AG 会算出虚高毛利")

	// 合计行对 AH 求 SUM（AG 已是同样处理）。
	totalAxis := mustAxis(34, 5)
	totalFormula, err := file.GetCellFormula(sheet, totalAxis)
	require.NoError(t, err)
	assert.Contains(t, totalFormula, "SUM(", "AH 合计应是 SUM 公式")
	assert.Contains(t, totalFormula, "AH3:AH4", "求和范围应覆盖两行数据")
}

// TestSummarizeCostSkipsUnpricedRows 合计口径：未维护倍率的行**两边都不计**。
//
// 只扣成本不扣结算额会得出「全量结算 − 部分成本」——利润虚高，是最危险的错法。
// 同时必须报出有多少行没覆盖，否则这段文字会被读成整体毛利。
func TestSummarizeCostSkipsUnpricedRows(t *testing.T) {
	const rate = 7.0
	f := func(v float64) *float64 { return &v }
	// 两行刊例相同、分组相同，只有渠道倍率不同：一行已维护 0.4，一行未维护。
	mk := func(channel int, ratio *float64) *CostRow {
		return &CostRow{
			AggRow: &AggRow{
				Model: "gpt-5.5", Group: "Codex|0.4", KeyGroup: "Codex", GroupRatio: 0.4,
				Quota: 5000, Rows: 1, OfficialUSD: 12.119489,
				BillingMode: BillingModeTieredExpr, BillingExpr: `tier("base", p*1)`,
			},
			ChannelID: channel, UpstreamRatio: ratio,
		}
	}
	priced := mk(101, f(0.4))
	unpriced := mk(102, nil)

	totals, text := SummarizeCost([]*CostRow{priced, unpriced}, NewPriceBook(), DiscountOverrides{}, rate, nil, 2026, 9, nil)

	assert.Equal(t, 2, totals.TotalRows)
	assert.Equal(t, 1, totals.PricedRows, "只有一行参与合计")

	// 只有已维护那一行的金额进了合计。
	wantSettle := round(OfficialListCNY(priced.AggRow, rate)*priced.RatioDiscount(), MoneyDecimals)
	assert.InDelta(t, wantSettle, totals.SettleCNY, 1e-6, "结算额只算已覆盖的行")
	assert.InDelta(t, wantSettle, totals.CostCNY, 1e-6, "上游倍率等于分组倍率，成本应等于结算额")
	assert.InDelta(t, 0.0, totals.ProfitCNY, 1e-6)
	// ChannelCount 是「表里出现的渠道数」，与是否维护倍率无关——
	// 102 未维护但它确实在表中占一行，所以是 2。
	assert.Equal(t, 2, totals.ChannelCount)

	// 文案必须写明有行没覆盖，并给出账期。
	assert.Contains(t, text, "2026-09", "账期要写出来")
	assert.Contains(t, text, "结算金额")
	assert.Contains(t, text, "上游成本")
	assert.Contains(t, text, "利润")
	assert.Contains(t, text, "1 行因渠道未维护上游倍率未计入", "漏掉的行数必须说明")
}

// TestFormatCostSummaryFullCoverage 全覆盖时不应出现「未计入」的注解，且金额不带多余尾零。
func TestFormatCostSummaryFullCoverage(t *testing.T) {
	totals := CostTotals{
		SettleCNY: 15146.6056, CostCNY: 9000, ProfitCNY: 6146.6056,
		PricedRows: 12, TotalRows: 12, ChannelCount: 3, RateCNYPerUSD: 7,
	}
	text := FormatCostSummary(totals, 2026, 9, nil)

	assert.NotContains(t, text, "未计入", "全覆盖时不该有保留说明")
	assert.Contains(t, text, "账期：2026-09")
	// 有汇率时美金金额跟在人民币后面，且按同一汇率换算：15146.6056/7 = 2163.80。
	assert.Contains(t, text, "结算金额：¥15146.6056（$2163.8）")
	assert.Contains(t, text, "上游成本：¥9000（$1285.71）")
	assert.Contains(t, text, "利润：¥6146.6056（$878.09），毛利率 40.58%")
	assert.Contains(t, text, "覆盖渠道：3 个")
	assert.Contains(t, text, "汇率：7（人民币/美金）")

	// header 为 nil 时必须与加头之前逐字节一致：手动上传日志那条路径没有客户与时段，
	// 凭空多出「客户：」这种空行会让人以为漏传了参数。
	assert.NotContains(t, text, "客户：")
	assert.NotContains(t, text, "账号：")
	assert.NotContains(t, text, "时段：")
	assert.True(t, strings.HasPrefix(text, "账期：2026-09"), "无头时账期仍应是第一行")
}

// TestFormatCostSummaryWithoutExchangeRate 没有汇率时不写美金，也不编一个汇率。
//
// 按未知汇率换出来的美金数字，比不写更危险：收件人会当成真实账面对待。
func TestFormatCostSummaryWithoutExchangeRate(t *testing.T) {
	totals := CostTotals{
		SettleCNY: 15146.6056, CostCNY: 9000, ProfitCNY: 6146.6056,
		PricedRows: 12, TotalRows: 12, ChannelCount: 3,
	}
	text := FormatCostSummary(totals, 2026, 9, nil)

	assert.NotContains(t, text, "$", "没汇率就不该出现美金金额")
	assert.NotContains(t, text, "汇率：")
	assert.Contains(t, text, "结算金额：¥15146.6056")
}

// TestFormatCostSummaryUSDUsesOwnRateSummary 摘要里的美金必须用**这次出账**的汇率。
//
// 回归点：汇率 7.3 是用户改过的设置。如果换算处去读全局默认的 7.0，
// 报出去的结算金额会比真实应收少一截，而数字看起来很合理，没人会怀疑。
func TestFormatCostSummaryUSDUsesOwnRateSummary(t *testing.T) {
	totals := CostTotals{
		SettleCNY: 15146.6056, CostCNY: 9000, ProfitCNY: 6146.6056,
		PricedRows: 12, TotalRows: 12, ChannelCount: 3, RateCNYPerUSD: 7.3,
	}
	text := FormatCostSummary(totals, 2026, 9, nil)

	assert.Contains(t, text, "$2074.88", "15146.6056 / 7.3")
	assert.NotContains(t, text, "$2163.8", "不能按默认汇率 7.0 算")
	assert.Contains(t, text, "汇率：7.3（人民币/美金）")
}

// TestFormatCostSummaryWithHeader 有定位行时，客户/账号/时段必须写在账期之前。
//
// 顺序和位置都要钉住：这几行是给收件人确认「这段话覆盖的是谁、哪一段」用的，
// 掉到金额后面或者互相换位，读起来就是另一回事了。
func TestFormatCostSummaryWithHeader(t *testing.T) {
	totals := CostTotals{
		SettleCNY: 15146.6056, CostCNY: 9000, ProfitCNY: 6146.6056,
		PricedRows: 12, TotalRows: 12, ChannelCount: 3, RateCNYPerUSD: 7,
	}
	header := []string{"客户：钛动", "账号：tecdc3.0、tecdc3.1", "时段：2026-09-01 00:00:00 ~ 2026-09-30 23:59:59"}

	text := FormatCostSummary(totals, 2026, 9, header)

	// 摘要文字是按行读的，整体比对最不容易漏掉中间某个字段被挪走。
	want := `客户：钛动
账号：tecdc3.0、tecdc3.1
时段：2026-09-01 00:00:00 ~ 2026-09-30 23:59:59
账期：2026-09
结算金额：¥15146.6056（$2163.8）
上游成本：¥9000（$1285.71）
利润：¥6146.6056（$878.09），毛利率 40.58%
覆盖渠道：3 个；明细行：12 行
汇率：7（人民币/美金）`
	assert.Equal(t, want, text)
}

// TestSummaryHeader 定位行的内容与时段格式。
func TestSummaryHeader(t *testing.T) {
	customer := Customer{Name: "钛动", Usernames: "tecdc3.0\ntecdc3.1"}
	// 服务端可能跑在 UTC，这里故意给 UTC 时刻：必须转成北京时间再打印，
	// 否则写出去的时间比页面看到的早 8 小时，对账时对不上。
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).In(cstLocation)
	end := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC).In(cstLocation)

	got := summaryHeader(customer, start, end)
	want := []string{
		"客户：钛动",
		"账号：tecdc3.0、tecdc3.1",
		"时段：2026-09-01 08:00:00 ~ 2026-10-01 07:59:59",
	}
	assert.Equal(t, want, got)

	// 没配账号时不写空行：一个空的「账号：」比不写更让人困惑。
	noAccount := summaryHeader(Customer{Name: "某客户"}, start, end)
	assert.Len(t, noAccount, 2)
	assert.Equal(t, "客户：某客户", noAccount[0])
	assert.True(t, strings.HasPrefix(noAccount[1], "时段："))
}

// ---- 按渠道展开 ----

// buildCostFixture 造一批日志行：同一 (模型, 分组) 走两个渠道。
func buildCostFixture(t *testing.T) (headers []string, rows [][]string) {
	t.Helper()
	headers = []string{
		"model_name", "group", "prompt_tokens", "completion_tokens", "quota",
		"other", "created_at", "channel_id",
	}
	// 两行同模型同分组、分别走渠道 101 与 102，group_ratio 相同。
	mk := func(channel int) []string {
		return []string{
			"gpt-5.5", "Codex", "1000", "100", "5000",
			`{"model_ratio":2.5,"completion_ratio":5,"cache_ratio":0.1,"group_ratio":0.4}`,
			"1789470821", fmt.Sprintf("%d", channel),
		}
	}
	return headers, [][]string{mk(101), mk(102)}
}

// TestAggregateCostByChannelSplitsRows 同一 (模型,分组) 走两个渠道必须展开成两行，
// 各自金额正确、合计等于两者之和。这正是「主账单不按渠道拆行」的前提。
func TestAggregateCostByChannelSplitsRows(t *testing.T) {
	headers, rows := buildCostFixture(t)
	ratios := map[int]float64{101: 1.8, 102: 0.4}
	names := map[int]string{101: "AZ", 102: "AWS"}

	costRows, err := AggregateCostByChannel(rows, headers, nil, 7.0, false, nil, ratios, names)
	require.NoError(t, err)
	require.Len(t, costRows, 2, "两个渠道应各成一行")

	for _, cr := range costRows {
		assert.Equal(t, "Codex", cr.KeyGroup, "C 列仍是分组标识，不含渠道")
		assert.Equal(t, 0.4, cr.GroupRatio)
		switch cr.ChannelID {
		case 101:
			assert.Equal(t, "AZ", cr.ChannelName)
			require.NotNil(t, cr.UpstreamRatio)
			assert.Equal(t, 1.8, *cr.UpstreamRatio)
		case 102:
			assert.Equal(t, "AWS", cr.ChannelName)
			require.NotNil(t, cr.UpstreamRatio)
			assert.Equal(t, 0.4, *cr.UpstreamRatio)
		default:
			t.Fatalf("意外的渠道号 %d", cr.ChannelID)
		}
	}

	// 两行金额各自正确，且合计等于两者之和。
	c101, ok1 := costRows[0].UpstreamCostCNY(7.0)
	c102, ok2 := costRows[1].UpstreamCostCNY(7.0)
	require.True(t, ok1)
	require.True(t, ok2)

	// 每行的成本 = 该行刊例(人民币) × 上游折扣，按各自渠道的倍率算。
	for _, cr := range costRows {
		d, ok := cr.UpstreamDiscount()
		require.True(t, ok)
		want := OfficialListCNY(cr.AggRow, 7.0) * d
		got, _ := cr.UpstreamCostCNY(7.0)
		assert.InDelta(t, want, got, 1e-9, "渠道 %d 的成本应按自己的倍率算", cr.ChannelID)
	}
	assert.NotEqual(t, c101, c102, "两个渠道倍率不同，成本不应相同")
}

// TestAggregateCostByChannelMissingColumn 日志没有 channel_id 时要报错，
// 且提示要指向「重新导出」而不是含糊的「缺列」。
func TestAggregateCostByChannelMissingColumn(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other"}
	rows := [][]string{{"m", "g", "1", "1", "1", ""}}

	_, err := AggregateCostByChannel(rows, headers, nil, 7.0, false, nil, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "channel_id")
	assert.Contains(t, err.Error(), "重新导出", "提示要能指导用户下一步动作")
}

// TestExtractChannelIDs 取渠道号：去重、升序、忽略 0（无渠道）。
func TestExtractChannelIDs(t *testing.T) {
	headers := []string{"model_name", "channel_id"}
	rows := [][]string{
		{"m", "102"}, {"m", "101"}, {"m", "102"}, {"m", "0"}, {"m", ""},
	}
	ids, err := ExtractChannelIDs(headers, rows)
	require.NoError(t, err)
	assert.Equal(t, []int{101, 102}, ids)

	_, err = ExtractChannelIDs([]string{"model_name"}, rows)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "channel_id")
}

// ---- 未维护渠道不静默进合计 ----

// TestUnmaintainedChannelCostIsEmpty 未维护倍率的渠道：成本值必须为空，
// 而不是 0——写 0 会被读成「上游免费」，把毛利虚高。
func TestUnmaintainedChannelCostIsEmpty(t *testing.T) {
	headers, rows := buildCostFixture(t)
	// 只维护渠道 101；102 未维护。
	costRows, err := AggregateCostByChannel(rows, headers, nil, 7.0, false, nil,
		map[int]float64{101: 1.8}, map[int]string{101: "AZ", 102: "AWS"})
	require.NoError(t, err)
	require.Len(t, costRows, 2)

	byChannel := map[int]*CostRow{}
	for _, cr := range costRows {
		byChannel[cr.ChannelID] = cr
	}

	maintained := byChannel[101]
	require.NotNil(t, maintained.UpstreamRatio)
	_, ok := maintained.UpstreamCostCNY(7.0)
	assert.True(t, ok, "已维护的渠道应有成本")

	missing := byChannel[102]
	assert.Nil(t, missing.UpstreamRatio, "未维护的渠道倍率为 nil")
	cost, ok := missing.UpstreamCostCNY(7.0)
	assert.False(t, ok, "未维护的渠道不能给出成本——否则会静默进合计")
	assert.Equal(t, 0.0, cost, "返回 0 只是零值，调用方必须靠 ok=false 判断并留空")
}

// ---- 成本利润表写出 ----

// TestWriteCostFromTemplateLayout 成本利润表的列布局：
//   - A~AC 与账单逐列同构（渠道信息不塞进 C 列，否则与账单的行对不上）；
//   - AD=渠道ID、AE=渠道名称、AF=上游折扣、AG=上游成本（人民币，公式）；
//   - 未维护倍率的行 AF/AG 必须留空，且不参与合计。
func TestWriteCostFromTemplateLayout(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.xlsx")
	outPath := filepath.Join(dir, "cost.xlsx")
	buildBillFixtureTemplate(t, templatePath)

	const rate = 7.0
	f := func(v float64) *float64 { return &v }
	rows := []*CostRow{
		{
			AggRow: &AggRow{
				Model: "gpt-5.5", Group: "Codex|0.4", KeyGroup: "Codex", GroupRatio: 0.4,
				Uncached: 1000, Output: 100, Quota: 5000, Rows: 1, OfficialUSD: 12.119489,
				BillingMode: BillingModeTieredExpr, BillingExpr: "tier(\"base\", p*1)",
			},
			ChannelID: 101, ChannelName: "AZ", UpstreamRatio: f(0.4),
		},
		{
			AggRow: &AggRow{
				Model: "gpt-5.5", Group: "Codex|0.4", KeyGroup: "Codex", GroupRatio: 0.4,
				Uncached: 2000, Output: 200, Quota: 9000, Rows: 1, OfficialUSD: 6.0,
				BillingMode: BillingModeTieredExpr, BillingExpr: "tier(\"base\", p*1)",
			},
			ChannelID: 102, ChannelName: "AWS", // 未维护倍率
		},
	}

	require.NoError(t, WriteCostFromTemplate(templatePath, outPath, rows, 2026, 9,
		nil, DiscountOverrides{}, rate, false, nil))

	file, err := excelize.OpenFile(outPath)
	require.NoError(t, err)
	defer file.Close()
	sheet := file.GetSheetName(0)
	cellRows, err := file.GetRows(sheet)
	require.NoError(t, err)

	// 表头：追加列由代码写进 AD~AG。
	assert.Equal(t, "渠道ID", cellAt(cellRows[0], 29))
	assert.Equal(t, "渠道名称", cellAt(cellRows[0], 30))
	assert.Equal(t, "上游折扣", cellAt(cellRows[0], 31))
	assert.Equal(t, "上游成本（人民币）", cellAt(cellRows[0], 32))

	// 第一行（数据从第 3 行开始）：C 列仍是分组标识，不含渠道。
	assert.Equal(t, "Codex(0.4)", cell(t, file, sheet, 3, 3), "C 列保持分组标识写法")
	assert.Equal(t, 101.0, ToFloat(cell(t, file, sheet, 30, 3)), "AD 渠道ID")
	assert.Equal(t, "AZ", cell(t, file, sheet, 31, 3), "AE 渠道名称")

	// AF 上游折扣 = 0.4/7。单元格套着 3 位小数格式（与账单 T 列同款），
	// 读回来是显示值 0.057，所以按显示精度比。
	disc := cell(t, file, sheet, 32, 3)
	assert.InDelta(t, 0.4/DiscountBaseFactor, ToFloat(disc), 0.001, "AF 上游折扣")

	// AG 上游成本是公式，引用同行的 AC 与 AF，而不是写死的数值。
	costFormula := formula(t, file, sheet, 33, 3)
	assert.Equal(t, "AC3*AF3*7", costFormula, "AG 应为可追溯的公式")

	// 第二行：未维护倍率 → AF/AG 留空（不能写 0，那会被读成上游免费）。
	assert.Equal(t, "", cell(t, file, sheet, 32, 4), "未维护倍率的 AF 必须留空")
	assert.Equal(t, "", cell(t, file, sheet, 33, 4), "未维护倍率的 AG 必须留空")

	// 合计行（两条数据行之后的第 5 行）对 AG 求和；
	// 未维护那行 AG 为空，SUM 会自动跳过它——这正是「不静默进合计」的机制。
	totalAxis := mustAxis(33, 5)
	totalFormula, err := file.GetCellFormula(sheet, totalAxis)
	require.NoError(t, err)
	assert.Contains(t, totalFormula, "SUM(", "AG 合计应是 SUM 公式")
	assert.Contains(t, totalFormula, "AG3:AG4", "求和范围应覆盖两行数据")
}
