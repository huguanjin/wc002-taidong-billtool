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

	buckets := map[[2]string]*AggRow{}
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

		if cacheRead != 0 || cacheWrite5m != 0 || cacheWrite1h != 0 {
			cacheHitRows++
		}

		imgMode := ImageBillingMode(model)
		semantic := InferUsageSemantic(model, UsageSemanticFromOther(other))
		uncached := UncachedInputTokens(prompt, cacheRead, cacheWrite5m, cacheWrite1h, semantic)
		wsCalls, wsPrice := ParseWebSearch(other)
		if wsCalls > 0 {
			webSearchRows++
		}

		if sanitizedWriter != nil {
			details := ParseRowDetails(other, includeBilling)
			details.UncachedInputTokens = uncached
			details.WebSearchCalls = wsCalls
			if err := sanitizedWriter.WriteRow(row, cacheRead, cacheWrite5m, cacheWrite1h, details); err != nil {
				return nil, err
			}
		}

		// 该请求发生的时间：日志的 created_at 是 Unix 秒。出账面对历史日志，
		// 带 hour() 一类的表达式必须按请求当时的时刻判断（见 RunBillingExpr）。
		at := time.Now()
		if hasCreated {
			if ts, ok := parseUnixTimestamp(cellAt(row, idxCreated)); ok && ts > 0 {
				at = time.Unix(ts, 0)
			}
		}

		var perCall, listUSD float64
		var exprUsed, matchedTier string
		// 本行刊例的来源，决定它能不能当折扣反推的分母（见 ListOrigin）。
		listOrigin := ListOriginNone
		// 表达式系数的计价币种：国产供应商家族在站上按人民币报价，系数落成
		// 「美金/百万token」单价前要除汇率；海外模型系数本身即美金，不除。
		exprUnitCurrency := "USD"
		billingMode := imgMode
		if billingMode == "" {
			billingMode = "token"
		}

		// 表达式优先取日志自带的（计费当时生效的规则），其次取 options 表当前配置——
		// 但只有当日志自己既没有 expr_b64、也没有 model_ratio/completion_ratio 快照时，
		// 才允许回落到 options 表现在的配置。模型在账期内可能从 ratio 计费切换成
		// billing_expr 阶梯计费，若日志本身是切换前的 ratio 快照，绝不能套用「现在」的表达式，
		// 否则会把整条历史请求按今天才生效的计费口径重算，刊例严重失真。
		exprStr := ""
		rowModelRatio, rowCompletionRatio, rowCacheRatio, hasRowRatio := ParseRowRatioPricing(other)
		if imgMode != "per_call" {
			exprStr = ParseBillingExpr(other)
			if exprStr == "" && !hasRowRatio && exprSetting != nil {
				exprStr = exprSetting.Expr(model)
			}
			if exprStr != "" {
				billingMode = BillingModeTieredExpr
			}
		}

		if imgMode == "per_call" {
			if mp := ParseModelPrice(other); mp > 0 {
				listUSD = mp
				// 按次计费的固定美金价来自日志 model_price，是一份外部刊例。
				listOrigin = ListOriginExternal
			}
			perCall = 1.0
		} else if exprStr != "" {
			img, imgO, ai, ao := ParseExtraTokens(other)
			params := BuildExprParams(model, prompt, uncached, completion,
				cacheRead, cacheWrite5m, cacheWrite1h, img, imgO, ai, ao, exprStr)
			res, err := RunBillingExpr(exprStr, params, at)
			if err != nil {
				// 表达式跑不通时回落到按量价表，宁可价格略有偏差也不整行失败。
				basePrice, _ := ResolvePrice(model, book, preferPriceTable, exchangeRate)
				if tier, ok := TieredModelPrices[model]; ok && (basePrice == nil || basePrice.Source == "per_call") {
					basePrice = &ModelPrice{InputPerM: tier.Low[0], OutputPerM: tier.Low[1], Currency: "USD", Source: "tiered_low"}
				}
				listUSD = RowListUSD(model, prompt, uncached, cacheRead, cacheWrite5m, cacheWrite1h, completion, basePrice, wsCalls, wsPrice)
				billingMode = "token"
				listOrigin = ListOriginExternal
			} else {
				listUSD = res.USD / 1_000_000
				if VendorFamily(model) != "" {
					// 国产供应商家族的 billing_expr 系数是人民币、不是美元；这里先除回
					// exchangeRate，下游 OfficialListCNY = officialUSD * exchangeRate 才能正确
					// 换回原始人民币刊例，否则会被多乘一次汇率，把国产模型的"官方刊例"放大约 exchangeRate 倍。
					//
					// 注意这是载荷性写法，不是重复除法：OfficialUSD 必须是「美金」口径
					// （AC 列按美金、S 列再 ×汇率还原人民币），除去的这一层由下游乘回来。
					// 同一币种还要传给单价列（见 ExprUnitCurrency），否则单价列留着人民币数字
					// 被当成美金，AC 的「单价×用量」公式会整体放大 exchangeRate 倍。
					listUSD /= exchangeRate
					exprUnitCurrency = "CNY"
				}
				exprUsed = exprStr
				matchedTier = res.MatchedTier
				// 刊例由站内表达式算出，不是外部对标价：这一行不能参与折扣反推。
				listOrigin = ListOriginExpr
			}
		} else if hasRowRatio {
			// 该请求当时是按 ModelRatio/CompletionRatio/CacheRatio 直接计费（未命中
			// billing_expr），系数换算方式与 fetchModelPricesFromDB 保持一致：
			// ratio=1 对应官方基准 $0.002/1K token（$2/MTok）。国产供应商家族的
			// ModelRatio 在这套系统里是按人民币报价（1 元=1 美金充值），同样需要
			// 先除回 exchangeRate，避免换回人民币时被多乘一次汇率。
			inp := rowModelRatio * 2
			outp := inp * rowCompletionRatio
			crp := inp * rowCacheRatio
			if VendorFamily(model) != "" {
				inp, outp, crp = inp/exchangeRate, outp/exchangeRate, crp/exchangeRate
			}
			listUSD = (uncached*inp + cacheRead*crp + completion*outp) / 1_000_000
			if wsCalls > 0 && wsPrice > 0 {
				listUSD += wsCalls * wsPrice / 1000.0
			}
			billingMode = "token"
			// ratio 快照的换算基准是官方锚点（ratio=1 → $2/MTok），算外部对标价。
			listOrigin = ListOriginExternal
		} else {
			basePrice, _ := ResolvePrice(model, book, preferPriceTable, exchangeRate)
			if tier, ok := TieredModelPrices[model]; ok && (basePrice == nil || basePrice.Source == "per_call") {
				basePrice = &ModelPrice{InputPerM: tier.Low[0], OutputPerM: tier.Low[1], Currency: "USD", Source: "tiered_low"}
			}
			listUSD = RowListUSD(model, prompt, uncached, cacheRead, cacheWrite5m, cacheWrite1h, completion, basePrice, wsCalls, wsPrice)
			if listUSD > 0 {
				listOrigin = ListOriginExternal
			}
		}

		key := [2]string{model, group}
		agg, exists := buckets[key]
		if !exists {
			agg = &AggRow{
				Model: model, Group: group, BillingMode: billingMode, ListOrigin: listOrigin,
				ExprUnitCurrency: exprUnitCurrency,
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
			agg.ListOrigin = mergesListOrigin(agg.ListOrigin, listOrigin)
			// 币种同样要一致：只要有一行是人民币系数，这一桶的单价列就得按人民币归一，
			// 否则单价列会混着两种量纲的数字，客户没法读。
			if exprUnitCurrency == "CNY" {
				agg.ExprUnitCurrency = "CNY"
			}
		}
		if exprUsed != "" {
			agg.BillingExpr = exprUsed
		}
		if matchedTier != "" && !containsString(agg.ExprTiers, matchedTier) {
			agg.ExprTiers = append(agg.ExprTiers, matchedTier)
		}
		agg.Uncached += uncached
		agg.CacheRead += cacheRead
		agg.Output += completion
		agg.CacheWrite5m += cacheWrite5m
		agg.CacheWrite1h += cacheWrite1h
		agg.Quota += quota
		agg.OfficialUSD += listUSD
		agg.WebSearchCalls += wsCalls
		agg.ImagePerCallCount += perCall
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
	sort.Slice(result, func(i, j int) bool {
		ri, rj := groupRank[result[i].Group], groupRank[result[j].Group]
		if ri != rj {
			return ri < rj
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
