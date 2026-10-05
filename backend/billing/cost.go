package billing

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CostRow 成本利润表的一行：主账单的一个桶再按渠道拆开。
//
// 复用 AggRow 承载前 29 列的语义（模型/分组/倍率/用量/刊例/结算），
// 额外挂上渠道与上游倍率。
type CostRow struct {
	*AggRow
	ChannelID   int
	ChannelName string
	// UpstreamRatio 为 nil 表示该渠道未维护倍率：成本列留空且不参与合计，
	// 并在备注里写明。静默按 0 算会让成本虚低、按 1 算会虚高，两者都会误导毛利判断。
	UpstreamRatio *float64
}

// UpstreamDiscount 上游折扣 = 上游倍率 / DiscountBaseFactor。
// 与站内折扣同一套基准（见 group_ratio_source.md），只是把「分组倍率」换成「上游倍率」。
func (c *CostRow) UpstreamDiscount() (float64, bool) {
	if c.UpstreamRatio == nil {
		return 0, false
	}
	return *c.UpstreamRatio / DiscountBaseFactor, true
}

// UpstreamCostCNY 上游成本（人民币）。
//
// 与站内结算对称：成本 = 官方刊例（人民币） × 上游折扣
//
//	= OfficialUSD × exchangeRate × (UpstreamRatio / DiscountBaseFactor)
//
// 倍率缺失时返回 (0, false)——调用方必须把这种情况与「成本真的是 0」区分开。
func (c *CostRow) UpstreamCostCNY(exchangeRate float64) (float64, bool) {
	d, ok := c.UpstreamDiscount()
	if !ok {
		return 0, false
	}
	return OfficialListCNY(c.AggRow, exchangeRate) * d, true
}

// AggregateCostByChannel 按 (模型, 分组, 倍率桶, 渠道) 聚合，供成本利润表使用。
//
// 刻意与 AggregateFromRows 分开：主账单的桶键不含渠道，把 channelId 加进去
// 会让账单被按渠道拆成大量重复行（同一模型出现十几行），对客户是明确的回归。
// 成本利润表是独立产物，各按各的粒度聚合。
//
// quotas 是渠道 → 上游倍率；缺失的渠道其成本列留空，由调用方在备注里说明。
func AggregateCostByChannel(rows [][]string, headers []string, book *PriceBook, exchangeRate float64,
	preferPriceTable bool, exprSetting *BillingExprSetting, quotas map[int]float64,
	channelNames map[int]string) ([]*CostRow, error) {

	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	required := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "channel_id"}
	var missing []string
	for _, name := range required {
		if _, ok := col[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// channel_id 缺失是常见情形（旧日志），提示要说清楚是"重新导出"而不是"配置错了"。
		return nil, fmt.Errorf("日志缺少列 %v，无法按渠道估算成本；请用「导出日志明细」重新导出一份带 channel_id 的日志", missing)
	}

	idxModel := col["model_name"]
	idxGroup := col["group"]
	idxPrompt := col["prompt_tokens"]
	idxCompletion := col["completion_tokens"]
	idxChannel := col["channel_id"]
	idxQuota, hasQuota := col["quota"]
	idxOther, hasOther := col["other"]
	idxCreated, hasCreated := col["created_at"]
	idxCacheTokens, hasCacheTokens := col["cache_tokens"]
	idxCacheCreation, hasCacheCreation := col["cache_creation_tokens"]

	type costKey struct {
		model, group, ratio string
		channel             int
	}
	buckets := map[costKey]*CostRow{}
	var order []costKey

	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		model := strings.TrimSpace(cellAt(row, idxModel))
		group := strings.TrimSpace(cellAt(row, idxGroup))
		if model == "" && group == "" {
			continue
		}
		channelID := int(ToFloat(cellAt(row, idxChannel)))
		prompt := ToFloat(cellAt(row, idxPrompt))
		completion := ToFloat(cellAt(row, idxCompletion))
		quota := 0.0
		if hasQuota {
			quota = ToFloat(cellAt(row, idxQuota))
		}
		other := ""
		if hasOther {
			other = cellAt(row, idxOther)
		}
		groupRatio, _ := GroupRatioFromOther(other)

		var cacheRead, cacheWrite5m, cacheWrite1h float64
		if hasCacheTokens && hasCacheCreation {
			cacheReadCol := ToFloat(cellAt(row, idxCacheTokens))
			creationCol := ToFloat(cellAt(row, idxCacheCreation))
			cr2, w5, w1 := ParseCacheTokens(other)
			if w5 != 0 || w1 != 0 || strings.Contains(other, "cache_creation_tokens_5m") {
				cacheRead = maxFloat(cacheReadCol, cr2)
				cacheWrite5m, cacheWrite1h = w5, w1
			} else {
				cacheRead = cacheReadCol
				cacheWrite5m, cacheWrite1h = creationCol, 0
			}
		} else {
			cacheRead, cacheWrite5m, cacheWrite1h = ParseCacheTokens(other)
		}

		at := time.Now()
		if hasCreated {
			if ts, ok := parseUnixTimestamp(cellAt(row, idxCreated)); ok && ts > 0 {
				at = time.Unix(ts, 0)
			}
		}

		// 与主账单共用同一份定价实现。
		pr := priceRow(model, other, prompt, completion,
			cacheRead, cacheWrite5m, cacheWrite1h, quota,
			book, exchangeRate, preferPriceTable, exprSetting, at)

		ratioBucket := ""
		if groupRatio > 0 {
			ratioBucket = strconv.FormatFloat(round(groupRatio, 4), 'f', -1, 64)
		}

		k := costKey{model: model, group: group, ratio: ratioBucket, channel: channelID}
		cr, exists := buckets[k]
		if !exists {
			groupKey := group
			if ratioBucket != "" {
				groupKey = group + "|" + ratioBucket
			}
			agg := &AggRow{
				Model: model, Group: groupKey, KeyGroup: group, GroupRatio: round(groupRatio, 4),
				BillingMode: pr.BillingMode, ListOrigin: pr.ListOrigin,
				ExprUnitCurrency: pr.ExprUnitCurrency,
			}
			if v, ok := quotas[channelID]; ok {
				ratio := v
				cr = &CostRow{AggRow: agg, ChannelID: channelID, UpstreamRatio: &ratio}
			} else {
				cr = &CostRow{AggRow: agg, ChannelID: channelID}
			}
			cr.ChannelName = channelNames[channelID]
			if cr.ChannelName == "" {
				cr.ChannelName = fmt.Sprintf("渠道 %d", channelID)
			}
			buckets[k] = cr
			order = append(order, k)
		} else {
			cr.ListOrigin = mergesListOrigin(cr.ListOrigin, pr.ListOrigin)
			if pr.ExprUnitCurrency == "CNY" {
				cr.ExprUnitCurrency = "CNY"
			}
		}
		if pr.BillingExpr != "" {
			cr.BillingExpr = pr.BillingExpr
		}
		if pr.MatchedTier != "" && !containsString(cr.ExprTiers, pr.MatchedTier) {
			cr.ExprTiers = append(cr.ExprTiers, pr.MatchedTier)
		}
		cr.Uncached += pr.Uncached
		cr.CacheRead += pr.CacheRead
		cr.Output += pr.Output
		cr.CacheWrite5m += pr.CacheWrite5m
		cr.CacheWrite1h += pr.CacheWrite1h
		cr.Quota += pr.Quota
		cr.OfficialUSD += pr.OfficialUSD
		cr.WebSearchCalls += pr.WebSearchCalls
		cr.ImagePerCallCount += pr.ImagePerCallCount
		cr.Rows++
		if hasCreated {
			if ts, ok := parseUnixTimestamp(cellAt(row, idxCreated)); ok && ts > 0 {
				dt := time.Unix(ts, 0)
				if dt.After(cr.LastAt) {
					cr.LastAt = dt
				}
			}
		}
	}

	if len(buckets) == 0 {
		return nil, fmt.Errorf("没有可汇总的日志行")
	}

	out := make([]*CostRow, 0, len(order))
	for _, k := range order {
		out = append(out, buckets[k])
	}
	// 与账单同源的排序：分组 → 倍率 → 模型 → 渠道，保证同分组连续、
	// 同模型的不同渠道也相邻，便于并排比对。
	sort.Slice(out, func(i, j int) bool {
		if out[i].KeyGroup != out[j].KeyGroup {
			return out[i].KeyGroup < out[j].KeyGroup
		}
		if out[i].GroupRatio != out[j].GroupRatio {
			return out[i].GroupRatio < out[j].GroupRatio
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].ChannelID < out[j].ChannelID
	})
	return out, nil
}

// UpstreamRatioStatus 成本估算前的渠道倍率检查结果。
type UpstreamRatioStatus struct {
	// Missing 日志里出现、但尚未维护倍率的渠道（渠道表里查得到）。
	Missing []ChannelInfo `json:"missing"`
	// Maintained 已维护倍率的渠道。
	Maintained []ChannelInfo `json:"maintained"`
	// UnknownChannelIDs 日志里有、但渠道表里查不到的渠道号。
	//
	// 必须与 Missing 分开：这类渠道多半已在业务库被硬删除，**无法维护倍率**，
	// 页面不该引导用户去"补录"一个不存在的渠道。成本利润表里如实标注，
	// 而不是静默按 0（成本虚低）或按 1（成本虚高）——两种都会误导毛利判断。
	UnknownChannelIDs []int `json:"unknownChannelIds"`
}

// CheckUpstreamRatios 比对日志里用到的渠道集合与已维护的倍率。
//
// 判据以**倍率**为先，渠道表成员资格只是「能不能补录」的补充信息：
// 已维护倍率的渠道一律算 Maintained，哪怕它已不在渠道表里（业务库硬删除的渠道，
// 历史账期仍可能引用）。否则会出现「页面让人填、填了也不认」的死循环。
func CheckUpstreamRatios(channelIDs []int, ratios map[int]float64, channels map[int]ChannelInfo) UpstreamRatioStatus {
	var st UpstreamRatioStatus
	seen := map[int]bool{}
	for _, id := range channelIDs {
		if seen[id] {
			continue
		}
		seen[id] = true

		if _, has := ratios[id]; has {
			// 渠道表里查不到名字的，给个占位名，别让清单里出现空白。
			info, known := channels[id]
			if !known {
				info = ChannelInfo{ChannelID: id, Name: fmt.Sprintf("渠道 %d（渠道清单里没有）", id)}
			}
			st.Maintained = append(st.Maintained, info)
			continue
		}

		info, known := channels[id]
		if !known {
			st.UnknownChannelIDs = append(st.UnknownChannelIDs, id)
			continue
		}
		st.Missing = append(st.Missing, info)
	}
	sort.Slice(st.Missing, func(i, j int) bool { return st.Missing[i].ChannelID < st.Missing[j].ChannelID })
	sort.Slice(st.Maintained, func(i, j int) bool { return st.Maintained[i].ChannelID < st.Maintained[j].ChannelID })
	sort.Ints(st.UnknownChannelIDs)
	return st
}

// ExtractChannelIDs 取出日志里出现过的渠道号（去重、升序）。
func ExtractChannelIDs(headers []string, rows [][]string) ([]int, error) {
	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	idx, ok := col["channel_id"]
	if !ok {
		return nil, fmt.Errorf("日志缺少列 [channel_id]；请用「导出日志明细」重新导出一份带 channel_id 的日志")
	}
	seen := map[int]bool{}
	var ids []int
	for _, row := range rows {
		id := int(ToFloat(cellAt(row, idx)))
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids, nil
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
