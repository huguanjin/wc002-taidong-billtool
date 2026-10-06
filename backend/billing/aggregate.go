package billing

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SanitizedRowWriter 脱敏日志的行写入接口，由 excel_write.go / csv_write.go 实现。
type SanitizedRowWriter interface {
	WriteRow(row []string, cacheRead, cacheWrite5m, cacheWrite1h float64, details RowDetails) error
}

// SanitizedWriter 脱敏日志写出器：在 SanitizedRowWriter 基础上要求实现收尾关闭。
type SanitizedWriter interface {
	SanitizedRowWriter
	Close() error
}

// AggregateResult 汇总结果，附带推断出的账期与统计信息。
type AggregateResult struct {
	Rows          []*AggRow
	Month         int
	Year          int
	RowCount      int
	CacheHitRows  int
	WebSearchRows int
}

var cstLocation = time.FixedZone("CST", 8*3600)

// CSTLocation 北京时间固定时区。对外的入口：日期/时间一律按 +08:00 解释，
// 与容器时区无关（容器通常是 UTC，用 time.Local 会整体偏 8 小时、错切账期）。
func CSTLocation() *time.Location { return cstLocation }

// AggregateFromRows 对应 log_to_bill.py 的 aggregate_from_rows：逐行解析缓存/语义/计费，
// 按 (model, group) 聚合，同时可选地把展开缓存列后的脱敏行写给 sanitizedWriter。
//
// exprSetting 提供 options 表里的阶梯计费表达式（可为 nil）。命中表达式的模型，
// 其官方美金刊例直接由表达式算出，不再依赖 ModelRatio/ModelPrice 那套表。
//
// includeBilling 控制写给 sanitizedWriter 的 RowDetails 是否附带站点内部计费参数
// （见 BillingDetails），不影响聚合/账单逻辑。
func AggregateFromRows(rows [][]string, headers []string, book *PriceBook, exchangeRate float64, preferPriceTable bool, exprSetting *BillingExprSetting, includeBilling bool, sanitizedWriter SanitizedRowWriter) (*AggregateResult, error) {
	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	required := []string{"model_name", "group", "prompt_tokens", "completion_tokens"}
	var missing []string
	for _, name := range required {
		if _, ok := col[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("日志缺少列: %v；实际列: %v", missing, headers)
	}

	idxModel := col["model_name"]
	idxGroup := col["group"]
	idxPrompt := col["prompt_tokens"]
	idxCompletion := col["completion_tokens"]
	idxQuota, hasQuota := col["quota"]
	idxOther, hasOther := col["other"]
	idxCreated, hasCreated := col["created_at"]
	idxCacheTokens, hasCacheTokens := col["cache_tokens"]
	idxCacheCreation, hasCacheCreation := col["cache_creation_tokens"]
	idxType, hasType := col["type"]

	// 聚合键包含「本次请求实际使用的分组倍率」：同一分组在账期内可能出现过
	// 多种倍率，而站内结算额 = 表达式美金 × 倍率，用一个折扣算不全这一组。
	// 倍率取自日志 other.group_ratio，按 4 位小数归桶以抗浮点误差。
	buckets := map[[3]string]*AggRow{}
	// 分组展示顺序仍按原始分组名的首次出现顺序。
	var groupOrder []string
	groupSeen := map[string]bool{}
	monthCounter := map[int]int{}
	yearCounter := map[int]int{}
	rowCount, cacheHitRows, webSearchRows := 0, 0, 0

	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		model := strings.TrimSpace(cellAt(row, idxModel))
		group := strings.TrimSpace(cellAt(row, idxGroup))
		if model == "" && group == "" {
			continue
		}
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
		// 本次请求实际使用的分组倍率：分桶与结算都靠它。
		// 取不到时记 0，该行单独成桶并交回原有折扣逻辑，不硬套倍率结算。
		groupRatio, _ := GroupRatioFromOther(other)

		// 倍率归桶：0 表示日志没给 group_ratio，单独成一桶，不与其他倍率混。
		ratioBucket := ""
		if groupRatio > 0 {
			ratioBucket = strconv.FormatFloat(round(groupRatio, 4), 'f', -1, 64)
		}
		key := [3]string{model, group, ratioBucket}

		// 异步任务的结算/退款行：只把额度调整并进桶，不做任何计价，也不写进脱敏日志。
		//
		// 这些行不是新的消费——任务提交时已经按预扣全额记过一条消费日志了，
		// 它们只是把预扣调回真实值。所以 token 列、刊例、按次张数全部加 0：
		// 刊例代表「这次请求值多少钱」，退款不改变这个事实，改变的是最终结算了多少。
		// 混进刊例会连带污染折扣反推的分母，算出一个两边都不对的折扣。
		//
		// 放在这个位置（脱敏写出之前、时间解析之前）是刻意的：客户版脱敏日志里
		// 不该出现退款行——那是站点与用户之间的额度往来，属于商务决定（已确认：
		// 客户版只给消费明细，账单金额已含冲抵）。放在这里一处拦下，比在写出路径里
		// 再加一层过滤更难漏。
		if IsTaskQuotaAdjustment(other) {
			logType := ""
			if hasType {
				logType = cellAt(row, idxType)
			}
			if delta, ok := QuotaAdjustmentDelta(logType, quota); ok {
				agg, exists := buckets[key]
				if !exists {
					agg = &AggRow{
						Model: model, Group: group, KeyGroup: group, GroupRatio: round(groupRatio, 4),
					}
					buckets[key] = agg
					if !groupSeen[group] {
						groupSeen[group] = true
						groupOrder = append(groupOrder, group)
					}
				}
				agg.QuotaDelta += delta
				agg.HasQuotaAdjustment = true
				// Rows 刻意不加：它不是一次请求，混进「请求行数」会让客户以为
				// 这个模型多了一次调用。
			}
			// 无论是否识别出 type，都不继续走下面的计价与写出分支。
			continue
		}

		var cacheRead, cacheWrite5m, cacheWrite1h float64
		if hasCacheTokens && hasCacheCreation {
			cacheReadCol := ToFloat(cellAt(row, idxCacheTokens))
			creationCol := ToFloat(cellAt(row, idxCacheCreation))
			cr2, w5, w1 := ParseCacheTokens(other)
			if w5 != 0 || w1 != 0 || strings.Contains(other, "cache_creation_tokens_5m") {
				cacheRead = math.Max(cacheReadCol, cr2)
				cacheWrite5m, cacheWrite1h = w5, w1
			} else {
				cacheRead = cacheReadCol
				cacheWrite5m, cacheWrite1h = creationCol, 0
			}
		} else {
			cacheRead, cacheWrite5m, cacheWrite1h = ParseCacheTokens(other)
		}


		// 该请求发生的时间：日志的 created_at 是 Unix 秒。出账面对历史日志，
		// 带 hour() 一类的表达式必须按请求当时的时刻判断（见 RunBillingExpr）。
		at := time.Now()
		if hasCreated {
			if ts, ok := parseUnixTimestamp(cellAt(row, idxCreated)); ok && ts > 0 {
				at = time.Unix(ts, 0)
			}
		}

		// 计费口径统一走 priceRow（见 rowpricing.go）：主账单与成本利润表共用同一份实现，
		// 分支语义只在一处维护。这里只负责把它算出的结果并进桶。
		pr := priceRow(model, other, prompt, completion,
			cacheRead, cacheWrite5m, cacheWrite1h, quota,
			book, exchangeRate, preferPriceTable, exprSetting, at)

		if pr.WebSearchCalls > 0 {
			webSearchRows++
		}
		if pr.HasCache {
			cacheHitRows++
		}
		if sanitizedWriter != nil {
			details := ParseRowDetails(other, includeBilling)
			details.UncachedInputTokens = pr.Uncached
			details.WebSearchCalls = pr.WebSearchCalls
			if err := sanitizedWriter.WriteRow(row, cacheRead, cacheWrite5m, cacheWrite1h, details); err != nil {
				return nil, err
			}
		}
		agg, exists := buckets[key]
		if !exists {
			// Group 是「含倍率层级」的桶键，折扣、结算、备注都按它查；
			// KeyGroup 保留原分组名供展示与价表折扣匹配。
			// 必须用复合键，否则同分组的不同倍率会共用同一个折扣，
			// 整组金额取决于第一行是哪个模型——不可复现的错误。
			groupKey := group
			if ratioBucket != "" {
				groupKey = group + "|" + ratioBucket
			}
			agg = &AggRow{
				Model: model, Group: groupKey, KeyGroup: group, GroupRatio: round(groupRatio, 4),
				BillingMode: pr.BillingMode,
				// ListOrigin 先落下第一行的来源，同桶后续行由 mergesListOrigin 合并。
				ListOrigin: pr.ListOrigin, ExprUnitCurrency: pr.ExprUnitCurrency,
			}
			buckets[key] = agg
			if !groupSeen[group] {
				groupSeen[group] = true
				groupOrder = append(groupOrder, group)
			}
		} else {
			// 同一 (模型,分组) 内可能混着两种口径：账期中途从 ratio 计费切到表达式，
			// 或表达式降级行按价表算。整桶来源只有在完全一致时才可信，
			// 不一致就记成 mixed，让折扣反推跳过这一桶而不是用半截分母算出个错数。
			agg.ListOrigin = mergesListOrigin(agg.ListOrigin, pr.ListOrigin)
			// 币种同样要一致：只要有一行是人民币系数，这一桶的单价列就得按人民币归一，
			// 否则单价列会混着两种量纲的数字，客户没法读。
			if pr.ExprUnitCurrency == "CNY" {
				agg.ExprUnitCurrency = "CNY"
			}
		}
		if pr.BillingExpr != "" {
			agg.BillingExpr = pr.BillingExpr
		}
		if pr.MatchedTier != "" && !containsString(agg.ExprTiers, pr.MatchedTier) {
			agg.ExprTiers = append(agg.ExprTiers, pr.MatchedTier)
		}
		agg.Uncached += pr.Uncached
		agg.CacheRead += pr.CacheRead
		agg.Output += pr.Output
		agg.CacheWrite5m += pr.CacheWrite5m
		agg.CacheWrite1h += pr.CacheWrite1h
		agg.Quota += pr.Quota
		agg.OfficialUSD += pr.OfficialUSD
		agg.WebSearchCalls += pr.WebSearchCalls
		agg.ImagePerCallCount += pr.ImagePerCallCount
		agg.Rows++
		rowCount++

		if hasCreated {
			if ts, ok := parseUnixTimestamp(cellAt(row, idxCreated)); ok && ts > 0 {
				dt := time.Unix(ts, 0)
				if dt.After(agg.LastAt) {
					agg.LastAt = dt
				}
				dt = dt.In(cstLocation)
				monthCounter[int(dt.Month())]++
				yearCounter[dt.Year()]++
			}
		}
	}

	if len(buckets) == 0 {
		return nil, fmt.Errorf("没有可汇总的日志行")
	}

	groupRank := map[string]int{}
	for i, g := range groupOrder {
		groupRank[g] = i
	}
	result := make([]*AggRow, 0, len(buckets))
	for _, agg := range buckets {
		result = append(result, agg)
	}
	// 排序键是 KeyGroup（原始分组名）而不是 Group：Group 现在是含倍率的桶键
	// （如 "AWS-专供分组|4.62"），拿它去查 groupRank 一律查不到、全为 0，
	// 结果退化成只按模型名排——同一分组的行被别的分组插在中间，
	// 客户看到「标识一、标识二、又回到标识一」。
	//
	// 同一个分组拆出的多个倍率桶排在一起，桶内按倍率、再按模型名，保证顺序稳定。
	sort.Slice(result, func(i, j int) bool {
		ri, rj := groupRank[result[i].KeyGroup], groupRank[result[j].KeyGroup]
		if ri != rj {
			return ri < rj
		}
		if result[i].KeyGroup != result[j].KeyGroup {
			return result[i].KeyGroup < result[j].KeyGroup
		}
		if result[i].GroupRatio != result[j].GroupRatio {
			return result[i].GroupRatio < result[j].GroupRatio
		}
		return result[i].Model < result[j].Model
	})

	month := mostCommon(monthCounter)
	if month == 0 {
		month = int(time.Now().In(cstLocation).Month())
	}
	year := mostCommon(yearCounter)
	if year == 0 {
		year = time.Now().In(cstLocation).Year()
	}

	return &AggregateResult{
		Rows: result, Month: month, Year: year,
		RowCount: rowCount, CacheHitRows: cacheHitRows, WebSearchRows: webSearchRows,
	}, nil
}

// ExtractDistinctModels 提取日志中出现的去重模型名（按名称排序），用于生成账单前
// 检查价格覆盖情况，不做完整聚合。
func ExtractDistinctModels(headers []string, rows [][]string) ([]string, error) {
	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	idxModel, ok := col["model_name"]
	if !ok {
		return nil, fmt.Errorf("日志缺少列: [model_name]；实际列: %v", headers)
	}

	seen := map[string]bool{}
	var models []string
	for _, row := range rows {
		model := strings.TrimSpace(cellAt(row, idxModel))
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	sort.Strings(models)
	return models, nil
}

// ExtractDistinctGroups 提取日志中出现的去重分组标识（按名称排序），
// 供出账前勾选「国产/站内定价」分组——分组名是客户自己的业务分组，
// 事先无法枚举，只能从待出账的日志里现取。
func ExtractDistinctGroups(headers []string, rows [][]string) ([]string, error) {
	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	idxGroup, ok := col["group"]
	if !ok {
		return nil, fmt.Errorf("日志缺少列: [group]；实际列: %v", headers)
	}

	seen := map[string]bool{}
	var groups []string
	for _, row := range rows {
		group := strings.TrimSpace(cellAt(row, idxGroup))
		if group == "" || seen[group] {
			continue
		}
		seen[group] = true
		groups = append(groups, group)
	}
	sort.Strings(groups)
	return groups, nil
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func parseUnixTimestamp(v string) (int64, bool) {	s := strings.TrimSpace(v)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return int64(f), true
}

// mostCommon 返回出现次数最多的键；0 表示无数据（与 Python most_common 语义近似，
// 计数相同时的平局顺序不保证与 Python 完全一致，属可接受的边缘差异）。
func mostCommon(counter map[int]int) int {
	best, bestCount := 0, -1
	for k, c := range counter {
		if c > bestCount {
			best, bestCount = k, c
		}
	}
	return best
}
