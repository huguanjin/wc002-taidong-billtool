package billing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xuri/excelize/v2"
)

// 国模渠道标识：上游折扣的换算口径，以及它在两套模板、成本表、核对清单里的体现。
//
// 背景：站点充值 1 元 = 1 美金，国产模型按人民币报价，1 倍率分组就是原价。
// 所以国模渠道的上游倍率是「折扣」（0.4 = 4 折），海外渠道的倍率则是「每美金刊例的成本」
// （折扣 = 倍率 ÷ 7）。同一个 0.4，国模渠道的成本是海外渠道的 7 倍——
// 标错一个渠道成本就差 7 倍，而账面上完全看不出来，所以这里测得比较细。

// glmOther 取自真实国模日志首行（glm-5.2 / GLM 分组 / 渠道 998）的 other。
//
// 该行 prompt=21011（其中缓存命中 20288）、completion=210、quota=14366，复算：
//
//	(723×8 + 20288×2 + 210×28) ÷ 1e6 = 0.05224   （人民币刊例）
//	0.05224 × group_ratio 0.55 × 500000 = 14366   （与日志里的 quota 一致）
const glmOther = `{"cache_ratio":0.25,"cache_tokens":20288,"completion_ratio":3.5,"group_ratio":0.55,"model_price":-1,"model_ratio":4}`

func TestUpstreamDiscountFor(t *testing.T) {
	// 海外渠道：倍率 ÷ 7。
	assert.InDelta(t, 0.4/7, UpstreamDiscountFor(0.4, false), 1e-12)
	assert.InDelta(t, 1.8/7, UpstreamDiscountFor(1.8, false), 1e-12)

	// 国模渠道：倍率本身就是折扣——0.4 是 4 折，不是 0.057。
	assert.Equal(t, 0.4, UpstreamDiscountFor(0.4, true))
	// 倍率 1 的国模渠道 = 原价（站点 1 元 = 1 美金，国产模型 1 倍率分组即原价）。
	assert.Equal(t, 1.0, UpstreamDiscountFor(1, true))

	// 同一个倍率，两种口径差整整 DiscountBaseFactor 倍——这正是需要这个标识的原因。
	assert.InDelta(t, DiscountBaseFactor,
		UpstreamDiscountFor(0.4, true)/UpstreamDiscountFor(0.4, false), 1e-9)

	// 0 是合法值（谈成免费），两种口径都得是 0，不能被当作「没维护」。
	assert.Equal(t, 0.0, UpstreamDiscountFor(0, true))
	assert.Equal(t, 0.0, UpstreamDiscountFor(0, false))
}

// TestCostRowDomesticDiscount 模板一：同样的刊例、同样的倍率，国模渠道的成本是海外渠道的 7 倍。
func TestCostRowDomesticDiscount(t *testing.T) {
	const rate = 7.0
	ratio := 0.4
	agg := &AggRow{OfficialUSD: 10} // 官方刊例 10 美金 = ¥70

	overseas := &CostRow{AggRow: agg, UpstreamRatio: &ratio}
	domestic := &CostRow{AggRow: agg, UpstreamRatio: &ratio, UpstreamDomestic: true}

	od, ok := overseas.UpstreamDiscount()
	require.True(t, ok)
	assert.InDelta(t, 0.4/7, od, 1e-12)
	dd, ok := domestic.UpstreamDiscount()
	require.True(t, ok)
	assert.Equal(t, 0.4, dd, "国模渠道折扣就是倍率本身")

	oc, ok := overseas.UpstreamCostCNY(rate)
	require.True(t, ok)
	dc, ok := domestic.UpstreamCostCNY(rate)
	require.True(t, ok)
	assert.InDelta(t, 4.0, oc, 1e-9, "海外：每美金刊例成本 0.4 元，10 美金 = ¥4")
	assert.InDelta(t, 28.0, dc, 1e-9, "国模：¥70 的 4 折 = ¥28")

	// 倍率未维护时仍是 (0, false)，与国模标识无关——不能因为标了国模就按 0 算。
	unmaintained := &CostRow{AggRow: agg, UpstreamDomestic: true}
	_, ok = unmaintained.UpstreamCostCNY(rate)
	assert.False(t, ok)
}

// TestAggregateCostByChannelCarriesDomesticFlag 两处建行（消费行、退款行）都要带上国模标识。
//
// newCostRow 抽出来就是为了这个：各写一份的话，标识很容易只在其中一处带上——
// 退款行的折扣会按海外口径算，同一个渠道的消费与退款冲抵出两个折扣，账面上看不出来。
func TestAggregateCostByChannelCarriesDomesticFlag(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota",
		"other", "type", "channel_id", "created_at"}
	rows := [][]string{
		// 退款行排在最前：渠道 101 的桶由「退款路径」建出来。
		{"glm-5.2", "GLM", "0", "0", "1000", `{"task_id":9,"group_ratio":0.55}`, "6", "101", "1788192059"},
		// 渠道 102 的桶由「消费路径」建出来。
		{"glm-5.2", "GLM", "21011", "210", "14366", glmOther, "2", "102", "1788192099"},
		// 渠道 103 没标国模，作对照。
		{"glm-5.2", "GLM", "21011", "210", "14366", glmOther, "2", "103", "1788192199"},
	}
	ratios := map[int]float64{101: 0.4, 102: 0.4, 103: 0.4}
	domestic := map[int]bool{101: true, 102: true}

	costRows, err := AggregateCostByChannel(rows, headers, nil, 7.0, false, nil, ratios, domestic,
		map[int]string{101: "A", 102: "B", 103: "C"})
	require.NoError(t, err)

	byChannel := map[int]*CostRow{}
	for _, cr := range costRows {
		byChannel[cr.ChannelID] = cr
	}
	require.Len(t, byChannel, 3)
	assert.True(t, byChannel[101].UpstreamDomestic, "退款路径建的行也要带国模标识")
	assert.True(t, byChannel[102].UpstreamDomestic, "消费路径建的行要带国模标识")
	assert.False(t, byChannel[103].UpstreamDomestic, "没标的渠道保持海外口径")

	// ChannelName 保持数据原样，国模标记只在写表时加（见 TestWriteCostLabelsDomesticChannel）。
	assert.Equal(t, "B", byChannel[102].ChannelName)
}

// TestSimpleBillDomesticChannelCost 模板二：国模渠道的成本与官方刊例按人民币口径算。
//
// 用汇率 8 而不是默认的 7：汇率恰为 7 时，旧口径（所有渠道都 ×汇率÷7）对国模渠道碰巧
// 与新口径同值，测试就分不出新旧了。
func TestSimpleBillDomesticChannelCost(t *testing.T) {
	const rate = 8.0
	// quota = 10 × 0.55 × 500000：反推出的刊例是 10（国模渠道里它是人民币）。
	const quota = "2750000"
	headers := simpleCostLogHeaders()
	rows := [][]string{
		{"glm-5.2", "GLM", "1000", "100", quota, `{"group_ratio":0.55}`, "2", "101"},
	}

	run := func(domestic map[int]bool) SimpleBillRow {
		got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
			CostColumns:      true,
			UpstreamRatios:   map[int]float64{101: 0.4},
			DomesticChannels: domestic,
			ExchangeRate:     rate,
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.NotNil(t, got[0].UpstreamCostCNY)
		require.NotNil(t, got[0].OfficialListUSD)
		require.NotNil(t, got[0].ProfitCNY)
		return got[0]
	}

	d := run(map[int]bool{101: true})
	assert.InDelta(t, 4.0, *d.UpstreamCostCNY, 1e-4, "国模：¥10 刊例的 4 折 = ¥4")
	assert.InDelta(t, 10.0/8, *d.OfficialListUSD, 1e-4, "刊例是人民币，÷汇率归一成美金口径")
	assert.InDelta(t, 5.5, d.TotalCostCNY, 1e-9, "客户金额不受影响：额度 ÷ 500000")
	assert.InDelta(t, 5.5-4.0, *d.ProfitCNY, 1e-4)

	o := run(nil)
	assert.InDelta(t, 10*8*(0.4/7), *o.UpstreamCostCNY, 1e-4, "海外口径：10 美金 × 汇率 × 倍率/7")
	assert.InDelta(t, 10.0, *o.OfficialListUSD, 1e-4)

	// 模板二里两种口径的关系：quota 已经把「站点单位下的刊例」定死了，所以这里不像模板一
	// 那样「同倍率国模成本是海外的 7 倍」——海外口径只比国模口径多一个 汇率/7 的因子。
	// 汇率恰为 7 时两者相等，这就是为什么只在汇率偏离 7 时才测得出新旧口径的差别。
	assert.InDelta(t, *d.UpstreamCostCNY*rate/DiscountBaseFactor, *o.UpstreamCostCNY, 1e-4)
}

// TestSimpleBillDomesticFlagIsNeutralAtBaseRate 汇率等于换算基数（默认 7）时，
// 国模标识不改变模板二的成本数，只改变「官方刊例」一列的币种口径。
//
// 这条是**有意钉住的现状**，不是巧合：站点充值 1 元 = 1 美金，quota 里的刊例单位
// 对国产模型是人民币、对海外模型是美金，而两种口径的成本都落到「刊例单位 × 倍率」。
// 升级后默认汇率下模板二的成本数不会变——变的是模板一（成本利润表）。
func TestSimpleBillDomesticFlagIsNeutralAtBaseRate(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		{"glm-5.2", "GLM", "1000", "100", "2750000", `{"group_ratio":0.55}`, "2", "101"},
	}
	cost := func(domestic map[int]bool) (costCNY, listUSD float64) {
		got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
			CostColumns: true, UpstreamRatios: map[int]float64{101: 0.4},
			DomesticChannels: domestic, ExchangeRate: DiscountBaseFactor,
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
		return *got[0].UpstreamCostCNY, *got[0].OfficialListUSD
	}
	domCost, domList := cost(map[int]bool{101: true})
	ovCost, ovList := cost(nil)

	assert.InDelta(t, ovCost, domCost, 1e-4, "汇率 = 7 时成本数相同")
	assert.InDelta(t, 4.0, domCost, 1e-4)
	assert.NotEqual(t, ovList, domList, "但官方刊例（美金）一列的口径不同：国模行归一成了美金")
}

// TestDomesticChannelCostMatchesAcrossTemplates 两套模板对同一国模渠道必须算出同一个成本。
//
// 两条路径的推导方向相反：模板一从模型倍率正算刊例，模板二从 quota 反推。
// 用真实日志里的一行（GLM-5.2，渠道 998）在两个汇率下各算一遍：
// 成本都应等于「人民币刊例 × 倍率」，且**与汇率无关**——汇率在国模渠道的式子里一进一出。
func TestDomesticChannelCostMatchesAcrossTemplates(t *testing.T) {
	const n = 1000 // 放大到成本远大于 4 位小数的取整误差
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota",
		"other", "type", "channel_id", "created_at"}
	var rows [][]string
	for i := 0; i < n; i++ {
		rows = append(rows, []string{"glm-5.2", "GLM", "21011", "210", "14366", glmOther, "2", "998", "1788192059"})
	}
	ratios := map[int]float64{998: 0.4}
	domestic := map[int]bool{998: true}

	// 人民币刊例 0.05224/行，4 折。
	want := float64(n) * 0.05224 * 0.4

	for _, rate := range []float64{7.0, 6.5} {
		t.Run(fmt.Sprintf("汇率%v", rate), func(t *testing.T) {
			costRows, err := AggregateCostByChannel(rows, headers, nil, rate, false, nil, ratios, domestic, nil)
			require.NoError(t, err)
			require.Len(t, costRows, 1)
			t1, ok := costRows[0].UpstreamCostCNY(rate)
			require.True(t, ok)

			got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
				CostColumns: true, UpstreamRatios: ratios, DomesticChannels: domestic, ExchangeRate: rate,
			})
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.NotNil(t, got[0].UpstreamCostCNY)

			assert.InDelta(t, want, t1, 1e-6, "模板一：人民币刊例 × 倍率")
			assert.InDelta(t, want, *got[0].UpstreamCostCNY, 1e-3, "模板二：从 quota 反推，应与模板一同值")
			assert.InDelta(t, t1, *got[0].UpstreamCostCNY, 1e-3, "两套模板必须一致")

			// 刊例（美金口径）也要一致：都是 人民币刊例 ÷ 汇率。
			require.NotNil(t, got[0].OfficialListUSD)
			assert.InDelta(t, costRows[0].OfficialUSD, *got[0].OfficialListUSD, 1e-3,
				"官方刊例（美金）两张表要对得上")
		})
	}
}

// TestSimpleBillMixedDomesticChannelsInOneRow 一个 (分组, 模型) 横跨国模与海外渠道时，
// 成本按各行自己的折扣累加，官方刊例按归一后的口径相加。
func TestSimpleBillMixedDomesticChannelsInOneRow(t *testing.T) {
	headers := simpleCostLogHeaders()
	rows := [][]string{
		{"m1", "G", "1", "1", "2750000", `{"group_ratio":0.55}`, "2", "101"}, // 刊例 10，国模
		{"m1", "G", "1", "1", "2750000", `{"group_ratio":0.55}`, "2", "102"}, // 刊例 10，海外
	}
	got, err := AggregateSimpleBill(rows, headers, SimpleBillOptions{
		CostColumns:      true,
		UpstreamRatios:   map[int]float64{101: 0.4, 102: 1.8},
		DomesticChannels: map[int]bool{101: true},
		ExchangeRate:     7,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].UpstreamCostCNY)

	// 国模：10 × 0.4 = 4；海外：10 × 7 × (1.8/7) = 18。
	assert.InDelta(t, 22.0, *got[0].UpstreamCostCNY, 1e-4, "逐行按各自渠道的折扣算，不能拿一个渠道代表整行")
	// 刊例：国模 10÷7（人民币归一成美金）+ 海外 10。
	assert.InDelta(t, 10.0/7+10.0, *got[0].OfficialListUSD, 1e-4)
}

// TestSummarizeCostUsesDomesticDiscount 成本利润摘要里的成本合计也走国模折扣。
func TestSummarizeCostUsesDomesticDiscount(t *testing.T) {
	const rate = 7.0
	ratio := 0.4
	mk := func(domestic bool) *CostRow {
		return &CostRow{
			AggRow: &AggRow{
				Model: "glm-5.2", Group: "GLM|0.55", KeyGroup: "GLM", GroupRatio: 0.55,
				Quota: 5000, Rows: 1, OfficialUSD: 10.0 / rate, // ¥10 刊例
			},
			ChannelID: 998, UpstreamRatio: &ratio, UpstreamDomestic: domestic,
		}
	}

	over, _ := SummarizeCost([]*CostRow{mk(false)}, NewPriceBook(), DiscountOverrides{}, rate, nil, 2026, 9, nil)
	dom, text := SummarizeCost([]*CostRow{mk(true)}, NewPriceBook(), DiscountOverrides{}, rate, nil, 2026, 9, nil)

	assert.InDelta(t, 10*0.4/7, over.CostCNY, 1e-4, "海外口径：¥10 × 0.4/7")
	assert.InDelta(t, 10*0.4, dom.CostCNY, 1e-4, "国模口径：¥10 × 4 折")
	assert.Contains(t, text, "上游成本", "摘要文字里也要体现")
}

// TestWriteCostLabelsDomesticChannel 成本利润表：国模渠道在名称列有标记，折扣列是倍率本身。
//
// 「上游折扣」一列国模渠道是 0.4、海外渠道是 0.057，不标出来的话读表的人会以为是哪一行填错了。
func TestWriteCostLabelsDomesticChannel(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.xlsx")
	outPath := filepath.Join(dir, "cost.xlsx")
	buildBillFixtureTemplate(t, templatePath)

	const rate = 7.0
	r := 0.4
	mk := func(channel int, name string, domestic bool) *CostRow {
		return &CostRow{
			AggRow: &AggRow{
				Model: "glm-5.2", Group: "GLM|0.55", KeyGroup: "GLM", GroupRatio: 0.55,
				Uncached: 1000, Output: 100, Quota: 5000, Rows: 1, OfficialUSD: 1.0,
				BillingMode: BillingModeTieredExpr, BillingExpr: `tier("base", p*1)`,
			},
			ChannelID: channel, ChannelName: name, UpstreamRatio: &r, UpstreamDomestic: domestic,
		}
	}
	rows := []*CostRow{mk(998, "GLM专线", true), mk(101, "AZ", false)}

	require.NoError(t, WriteCostFromTemplate(templatePath, outPath, rows, 2026, 9,
		nil, DiscountOverrides{}, rate, false, nil))

	file, err := excelize.OpenFile(outPath)
	require.NoError(t, err)
	defer file.Close()
	sheet := file.GetSheetName(0)

	assert.Equal(t, "GLM专线（国模渠道）", cell(t, file, sheet, 31, 3), "国模渠道名称列带标记")
	assert.Equal(t, "AZ", cell(t, file, sheet, 31, 4), "海外渠道名称原样")
	assert.Equal(t, "GLM专线", rows[0].ChannelName, "标记只在写表时加，数据字段保持原名")

	// AF 上游折扣：国模 = 0.4，海外 = 0.4/7。单元格套 3 位小数格式，按显示精度比。
	assert.InDelta(t, 0.4, ToFloat(cell(t, file, sheet, 32, 3)), 0.001)
	assert.InDelta(t, 0.4/7, ToFloat(cell(t, file, sheet, 32, 4)), 0.001)

	// AG 上游成本仍是引用 AC 与 AF 的公式——折扣不同，公式一致，所以 Excel 里可追溯。
	assert.Equal(t, "AC3*AF3*7", formula(t, file, sheet, 33, 3))
	assert.Equal(t, "AC4*AF4*7", formula(t, file, sheet, 33, 4))
}

// ---- 观测与核对清单 ----

func observeHeaders() []string {
	return []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "type", "channel_id"}
}

func TestObserveChannels(t *testing.T) {
	rows := [][]string{
		// 渠道 101：三次消费 + 一笔退款。
		{"glm-5.2", "GLM", "1", "1", "1000", `{"group_ratio":0.55}`, "2", "101"},
		{"glm-5.2", "GLM", "1", "1", "1000", `{"group_ratio":0.55}`, "2", "101"},
		{"glm-5.1", "GLM", "1", "1", "500", `{"group_ratio":0.55}`, "2", "101"},
		{"glm-5.2", "GLM", "0", "0", "300", `{"task_id":5,"group_ratio":0.55}`, "6", "101"},
		// 一行经两个渠道：额度怎么分摊不明，行数与额度都不归属，但「出现过」要记。
		{"gpt-5", "AZ", "1", "1", "700", `{"group_ratio":1}`, "2", "102,103"},
		// 渠道 104：国产与海外模型混跑。
		{"glm-5.2", "GLM", "1", "1", "100", `{"group_ratio":0.55}`, "2", "104"},
		{"gpt-5", "AZ", "1", "1", "100", `{"group_ratio":1}`, "2", "104"},
		// 没有渠道号的行不归任何渠道。
		{"glm-5.2", "GLM", "1", "1", "999", `{"group_ratio":0.55}`, "2", "0"},
	}
	obs := ObserveChannels(observeHeaders(), rows)

	require.Contains(t, obs, 101)
	o := obs[101]
	assert.Equal(t, 3, o.Rows, "退款行不是一次请求，不计入行数")
	assert.InDelta(t, 1000+1000+500-300, o.QuotaNet, 1e-9, "净额度：消费 − 退款")
	assert.Equal(t, []string{"GLM"}, o.Groups)
	assert.Equal(t, []string{"glm-5.2", "glm-5.1"}, o.Models, "按行数降序；退款行带的模型名不计")
	assert.True(t, o.AllDomesticModels)
	assert.True(t, o.AnyDomesticModels)

	// 多渠道行：两个渠道都「出现过」这个分组与模型，但没有归属额度与行数。
	for _, id := range []int{102, 103} {
		require.Contains(t, obs, id)
		assert.Equal(t, []string{"AZ"}, obs[id].Groups)
		assert.Equal(t, []string{"gpt-5"}, obs[id].Models)
		assert.Zero(t, obs[id].Rows, "多渠道行不归属行数")
		assert.Zero(t, obs[id].QuotaNet, "多渠道行不归属额度——分摊是猜的")
		assert.False(t, obs[id].AnyDomesticModels, "只跑海外模型")
	}

	// 混跑渠道：Any 为真、All 为假，页面据此提示「混有非国产模型」。
	mixed := obs[104]
	assert.True(t, mixed.AnyDomesticModels)
	assert.False(t, mixed.AllDomesticModels)
	assert.Equal(t, []string{"AZ", "GLM"}, mixed.Groups)

	assert.NotContains(t, obs, 0, "渠道号 0 = 没有渠道")
	assert.Len(t, obs, 4)
}

// TestObserveChannelsFallsBackToOther 没有 channel_id 列时从 other.admin_info.use_channel 取。
// 与出账、预检共用 rowChannelIDs，三处对「这一行属于哪个渠道」必须同一个答案。
func TestObserveChannelsFallsBackToOther(t *testing.T) {
	headers := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "type"}
	rows := [][]string{
		{"glm-5.2", "GLM", "1", "1", "1000", `{"group_ratio":0.55,"admin_info":{"use_channel":["998"]}}`, "2"},
	}
	obs := ObserveChannels(headers, rows)
	require.Contains(t, obs, 998)
	assert.Equal(t, 1, obs[998].Rows)
}

func TestBuildChannelReviewOrdersByAmountAndCarriesConfig(t *testing.T) {
	obs := map[int]*ChannelObservation{
		1: {ChannelID: 1, Rows: 5, QuotaNet: 500000, Groups: []string{"A"}, Models: []string{"glm-5.2"},
			AllDomesticModels: true, AnyDomesticModels: true},
		2: {ChannelID: 2, Rows: 9, QuotaNet: 2500000, Groups: []string{"B"}, Models: []string{"gpt-5"}},
		3: {ChannelID: 3, Rows: 1, QuotaNet: 500000}, // 与渠道 1 同额：同额按渠道号升序
		4: {ChannelID: 4, Rows: 1, QuotaNet: 0},
	}
	ratios := map[int]float64{1: 0.4, 2: 1.8}
	domestic := map[int]bool{1: true}
	channels := map[int]ChannelInfo{1: {ChannelID: 1, Name: "GLM专线"}, 2: {ChannelID: 2, Name: "AZ"}}

	got := BuildChannelReview(obs, ratios, domestic, channels)
	require.Len(t, got, 4)

	// 先看对成本影响最大的：金额降序，同额按渠道号升序。
	assert.Equal(t, []int{2, 1, 3, 4}, []int{got[0].ChannelID, got[1].ChannelID, got[2].ChannelID, got[3].ChannelID})

	assert.Equal(t, 5.0, got[0].AmountCNY, "2500000 ÷ 500000")
	assert.Equal(t, "AZ", got[0].Name)
	require.NotNil(t, got[0].UpstreamRatio)
	assert.Equal(t, 1.8, *got[0].UpstreamRatio)
	assert.False(t, got[0].IsDomestic)
	assert.True(t, got[0].Known)

	assert.True(t, got[1].IsDomestic)
	assert.True(t, got[1].AllDomesticModels)
	assert.Equal(t, []string{"glm-5.2"}, got[1].Models)

	// 渠道 3 不在清单里、也没维护：名称带说明，倍率为 nil（页面据此高亮并要求填写）。
	assert.False(t, got[2].Known)
	assert.Contains(t, got[2].Name, "不在渠道清单里")
	assert.Nil(t, got[2].UpstreamRatio)
}

func TestEnrichIssues(t *testing.T) {
	issues := []ChannelIssue{{ChannelID: 1, Name: "a"}, {ChannelID: 2, Name: "b"}}
	obs := map[int]*ChannelObservation{
		1: {ChannelID: 1, Models: []string{"glm-5.2"}, AllDomesticModels: true, AnyDomesticModels: true},
	}
	EnrichIssues(issues, obs)

	assert.Equal(t, []string{"glm-5.2"}, issues[0].Models)
	assert.True(t, issues[0].AllDomesticModels)
	assert.True(t, issues[0].AnyDomesticModels)
	assert.Empty(t, issues[1].Models, "没观测到的渠道不动")
	assert.False(t, issues[1].AnyDomesticModels)
}

func TestCheckChannelRatiosCarriesDomestic(t *testing.T) {
	usage := map[int]*ChannelUsage{
		1: {ChannelID: 1, Groups: []string{"GLM"}},
		2: {ChannelID: 2, Groups: []string{"GLM"}},
	}
	ratios := map[int]float64{1: 0.4} // 渠道 2 没维护
	domestic := map[int]bool{1: true, 2: true}
	channels := map[int]ChannelInfo{1: {ChannelID: 1, Name: "a"}, 2: {ChannelID: 2, Name: "b"}}

	res := CheckChannelRatios(usage, ratios, domestic, channels, map[int]int{2: 7})

	require.Len(t, res.UsedChannels, 2)
	assert.True(t, res.UsedChannels[0].IsDomestic)
	require.Len(t, res.Missing, 1)
	assert.Equal(t, 2, res.Missing[0].ChannelID)
	assert.True(t, res.Missing[0].IsDomestic, "先标了国模、倍率还没填的渠道，标识要带到待补录清单里")
	assert.Equal(t, 7, res.Missing[0].RowCount)
}

// ---- 核对开关的闸门 ----

// TestNeedsUpstreamReview 四个条件缺一不可。每一行都对应一种真实的出错方式。
func TestNeedsUpstreamReview(t *testing.T) {
	on := BillTask{CheckCost: true, ReviewUpstream: true}

	assert.True(t, needsUpstreamReview(on, false, 3), "勾了核对、做成本核算、有渠道、没跳过 → 要核对")

	// 无限弹窗：用户在弹窗里点了继续，重跑时必须放行。
	assert.False(t, needsUpstreamReview(on, true, 3), "skip 为真必须放行，否则永远出不了账")

	// 没勾核对。
	assert.False(t, needsUpstreamReview(BillTask{CheckCost: true}, false, 3))

	// 勾了核对但根本没做成本：没有上游数据可核对。
	assert.False(t, needsUpstreamReview(BillTask{ReviewUpstream: true}, false, 3))

	// 只勾了「生成成本利润表」（没勾成本核算）同样是在做成本，应当核对。
	assert.True(t, needsUpstreamReview(BillTask{GenerateCost: true, ReviewUpstream: true}, false, 3))

	// 日志里一个渠道都没观测到：弹个空表只会让人困惑。
	assert.False(t, needsUpstreamReview(on, false, 0))
}

// TestChannelRatioInputKeepsUnsentFields JSON 里没传的字段解出来是 nil（= 保持库里原值），
// 传了 false / 空串则是有效值。这是 Note 与 IsDomestic 用指针的全部理由。
func TestChannelRatioInputKeepsUnsentFields(t *testing.T) {
	var onlyRatio ChannelRatioInput
	require.NoError(t, json.Unmarshal([]byte(`{"channelId":1,"upstreamRatio":0.4}`), &onlyRatio))
	assert.Nil(t, onlyRatio.Note, "老页面不带 note：必须是 nil，不能被当成「清空备注」")
	assert.Nil(t, onlyRatio.IsDomestic, "老页面不带 isDomestic：必须是 nil，不能被当成「取消国模」")

	var explicit ChannelRatioInput
	require.NoError(t, json.Unmarshal([]byte(`{"channelId":1,"upstreamRatio":0.4,"note":"","isDomestic":false}`), &explicit))
	require.NotNil(t, explicit.Note)
	assert.Equal(t, "", *explicit.Note, "显式空串 = 清空备注")
	require.NotNil(t, explicit.IsDomestic)
	assert.False(t, *explicit.IsDomestic, "显式 false = 取消国模")

	var cleared ChannelRatioInput
	require.NoError(t, json.Unmarshal([]byte(`{"channelId":1,"upstreamRatio":null}`), &cleared))
	assert.Nil(t, cleared.UpstreamRatio, "倍率 null = 取消维护（页面清空输入框的语义）")
}

// ---- 端到端：Params.ChannelDomestic 要一路传到成本合计 ----
//
// 单测 AggregateCostByChannel / AggregateSimpleBill 证明不了这条线接通了：
// service.go 里那一处传参若漏掉，标识存了、页面也勾了，成本却照旧按海外口径算——而且不报错。

// glmLogRows 放大 n 倍的国模日志行（真实首行，GLM-5.2，渠道 998）。
func glmLogRows(n int) [][]string {
	rows := make([][]string, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, []string{"glm-5.2", "GLM", "21011", "210", "14366", glmOther, "2", "998", "1788192059"})
	}
	return rows
}

var glmLogHeaders = []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota",
	"other", "type", "channel_id", "created_at"}

// TestGenerateCostTableHonorsParamsDomestic 模板一的成本利润表。
func TestGenerateCostTableHonorsParamsDomestic(t *testing.T) {
	const rate = 7.0
	rows := glmLogRows(1000)

	run := func(domestic map[int]bool) *CostTotals {
		dir := t.TempDir()
		templatePath := filepath.Join(dir, "template.xlsx")
		buildBillFixtureTemplate(t, templatePath)
		params := Params{
			ChannelUpstreamRatios: map[int]float64{998: 0.4},
			ChannelDomestic:       domestic,
			ChannelNames:          map[int]string{998: "GLM专线"},
			ChannelInfos:          map[int]ChannelInfo{998: {ChannelID: 998, Name: "GLM专线"}},
		}
		costPath, totals, _, blocked, _, _, err := generateCostTable(
			"", templatePath, filepath.Join(dir, "账单.xlsx"), rows, glmLogHeaders,
			// 价表里要有这个模型：写表器对「价表查不到、但日志带倍率快照」的行会解引用空指针
			// （既有行为，与国模标识无关），这里只是不去踩它。
			&PriceBook{
				ByModel: map[string]ModelPrice{"glm-5.2": {
					InputPerM: 1.1, OutputPerM: 3.9, Currency: "USD", Source: "price_table", Category: "GLM", Channel: "x"}},
				Discounts: map[string]float64{},
			},
			params, DiscountOverrides{}, rate, false, 2026, 9)
		require.NoError(t, err)
		require.False(t, blocked)
		require.NotEmpty(t, costPath)
		require.NotNil(t, totals)
		return totals
	}

	dom := run(map[int]bool{998: true})
	ov := run(nil)

	want := 1000 * 0.05224 * 0.4 // 人民币刊例 × 4 折
	assert.InDelta(t, want, dom.CostCNY, 1e-3, "标了国模：人民币刊例 × 倍率")
	assert.InDelta(t, want/DiscountBaseFactor, ov.CostCNY, 1e-3, "没标：按海外口径再 ÷7")
	assert.InDelta(t, DiscountBaseFactor*ov.CostCNY, dom.CostCNY, 1e-3, "同一渠道同一倍率，标不标国模成本差 7 倍")
}

// TestGenerateSimpleBillHonorsParamsDomestic 模板二，经完整的 GenerateBill 入口。
//
// 汇率取 6.5 而不是 7：汇率为 7 时两种口径的成本数相同（见 TestSimpleBillDomesticFlagIsNeutralAtBaseRate），
// 测不出传参有没有接通。
func TestGenerateSimpleBillHonorsParamsDomestic(t *testing.T) {
	const rate = 6.5
	dir := t.TempDir()
	logPath := filepath.Join(dir, "日志查询_2026-09-01_2026-09-30_aa.tsv")
	var b strings.Builder
	b.WriteString(strings.Join(glmLogHeaders, "\t") + "\n")
	for _, r := range glmLogRows(1000) {
		b.WriteString(strings.Join(r, "\t") + "\n")
	}
	require.NoError(t, os.WriteFile(logPath, []byte(b.String()), 0o644))

	run := func(domestic map[int]bool) *CostTotals {
		outDir := filepath.Join(t.TempDir(), "out")
		require.NoError(t, os.MkdirAll(outDir, 0o755))
		res, err := GenerateBill(logPath, "", "", "", outDir, Params{
			BillTemplate: BillTemplateSimple, CheckCost: true, ExchangeRate: rate,
			ChannelUpstreamRatios: map[int]float64{998: 0.4},
			ChannelDomestic:       domestic,
			ChannelKnownIDs:       map[int]bool{998: true},
		})
		require.NoError(t, err)
		require.NotNil(t, res.CostTotals)
		return res.CostTotals
	}

	dom := run(map[int]bool{998: true})
	ov := run(nil)

	want := 1000 * 0.05224 * 0.4
	assert.InDelta(t, want, dom.CostCNY, 1e-2, "标了国模：人民币刊例 × 倍率，与汇率无关")
	assert.InDelta(t, want*rate/DiscountBaseFactor, ov.CostCNY, 1e-2, "没标：多一个 汇率/7 的因子")
	assert.NotEqual(t, dom.CostCNY, ov.CostCNY, "传参没接通的话，这两个数会相等")
}
