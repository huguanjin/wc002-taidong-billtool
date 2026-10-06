package billing

import (
	"fmt"
	"sort"
)

// 页面上的「解析分组」：从一份日志里列出出现过的分组，并给出每个分组
// **当前会自动算成什么折扣**，供结算人员对照着填手工折扣。
//
// 展示的折扣必须与出账时用的完全同源，所以这里直接复用 ComputeGroupDiscounts，
// 不另写一套简化算法——页面上显示 A、账单里算出 B 的话，这个功能就没有意义了。

// GroupDiscountPreview 一个分组在某个客户下的折扣现状。
type GroupDiscountPreview struct {
	// GroupKey 日志里的原始分组名，手工折扣就是按它维护的。
	GroupKey string `json:"groupKey"`
	// DisplayGroup 带倍率后缀的展示名（Codex(0.4)），与账单 C 列同形——
	// 让结算人员能拿着页面上的名字去账单里对号。
	DisplayGroup string `json:"displayGroup"`
	// GroupRatio 本次日志里该分组的倍率；0 表示日志没给。
	GroupRatio float64 `json:"groupRatio"`
	// AutoDiscount 不填手工折扣时，出账会用的那个折扣。
	AutoDiscount float64 `json:"autoDiscount"`
	// AutoSource 该折扣是怎么来的，见 discountSourceLabel。
	AutoSource string `json:"autoSource"`
	// Models 该分组下出现过的模型，供人工判断这一行是不是自己认识的那个分组。
	Models []string `json:"models"`
	// Rows 该分组在日志里的明细行数。
	Rows int `json:"rows"`
	// ManualDiscount 已维护的手工折扣；nil 表示没维护（不填就走 AutoDiscount）。
	ManualDiscount *float64 `json:"manualDiscount"`
	// Note 手工折扣的备注。
	Note string `json:"note"`
}

// PreviewGroupDiscounts 解析日志并算出每个分组的折扣现状。
//
// manual 是已维护的手工折扣（键 = KeyGroup）；它只用于**回填页面输入框**，
// 不参与下面算 AutoDiscount 的那次调用——页面上那两列要能互相独立地看：
// 「自动反推是多少」与「我填的是多少」。填完手工值后账单用哪个，由出账链路决定。
func PreviewGroupDiscounts(inputPath, priceTablePath, dbPriceCachePath string,
	params Params, manual map[string]float64) ([]GroupDiscountPreview, error) {

	book, exprSetting, err := loadBillingPriceBook(params, priceTablePath, dbPriceCachePath)
	if err != nil {
		return nil, err
	}
	mergeManualPrices(book, params.ManualPrices)
	preferPriceTable := PreferPriceTableFor(params.PriceSource)

	headers, rows, err := LoadLogRows(inputPath, params.Sheet, params.Encoding)
	if err != nil {
		return nil, fmt.Errorf("读取日志失败: %w", err)
	}

	exchangeRate := params.ExchangeRate
	if exchangeRate <= 0 {
		exchangeRate = DefaultExchangeRate
	}
	// includeBilling=false、不写脱敏日志：这里只要聚合结果，不需要附带计费参数列。
	agg, err := AggregateFromRows(rows, headers, book, exchangeRate, preferPriceTable,
		exprSetting, false, nil)
	if err != nil {
		return nil, fmt.Errorf("解析日志失败: %w", err)
	}

	// 刻意不传 manual：这一列要展示的是「不填手工折扣会算出什么」，
	// 传进去的话每个已维护的分组都会显示成手工值，两列就一样了，失去对照意义。
	auto := ComputeGroupDiscounts(agg.Rows, book, exchangeRate, DiscountOverrides{}, params.DomesticMarkers)

	byGroup := map[string]*GroupDiscountPreview{}
	models := map[string]map[string]bool{}
	for _, a := range agg.Rows {
		p, ok := byGroup[a.KeyGroup]
		if !ok {
			p = &GroupDiscountPreview{
				GroupKey:     a.KeyGroup,
				DisplayGroup: a.DisplayGroup(),
				GroupRatio:   a.GroupRatio,
				AutoDiscount: auto.Discounts[a.Group],
				AutoSource:   discountSourceLabel(auto, a),
				Rows:         0,
			}
			byGroup[a.KeyGroup] = p
			models[a.KeyGroup] = map[string]bool{}
		}
		p.Rows += a.Rows
		if !models[a.KeyGroup][a.Model] {
			models[a.KeyGroup][a.Model] = true
			p.Models = append(p.Models, a.Model)
		}
	}

	out := make([]GroupDiscountPreview, 0, len(byGroup))
	for key, p := range byGroup {
		// 同一分组可能被拆成多个倍率桶，展示倍率取较大的那个并注明——
		// 取 0（日志没给）或随机取一个都会让人以为分组只有一种倍率。
		if p.GroupRatio == 0 {
			for _, a := range agg.Rows {
				if a.KeyGroup == key && a.GroupRatio > p.GroupRatio {
					p.GroupRatio = a.GroupRatio
				}
			}
		}
		sort.Strings(p.Models)
		if d, ok := manual[key]; ok {
			v := d
			p.ManualDiscount = &v
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupKey < out[j].GroupKey })
	return out, nil
}

// discountSourceLabel 说明该分组的自动折扣是怎么来的，与账单 AB 列的措辞保持一致。
func discountSourceLabel(r DiscountResult, agg *AggRow) string {
	switch {
	case r.Underivable[agg.Group] != "":
		return "按站点实际倍率折算（无法反推，需人工确认）"
	case agg.HasRatioDiscount():
		return "按本次请求使用的分组倍率"
	case r.Derived[agg.Group]:
		return "反推（Σ结算/Σ总金额）"
	default:
		return "价表折扣"
	}
}
