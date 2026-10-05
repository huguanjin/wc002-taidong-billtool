package billing

import (
	"fmt"
	"path/filepath"
	"testing"

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

// ---- 成本表写出 ----

// TestWriteCostFromTemplateLayout 成本表的列布局：
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
		nil, nil, rate, false, nil))

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
