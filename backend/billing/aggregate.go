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
	WriteRow(row []string, cacheRead, cacheWrite5m, cacheWrite1h float64) error
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
func AggregateFromRows(rows [][]string, headers []string, book *PriceBook, exchangeRate float64, preferPriceTable bool, sanitizedWriter SanitizedRowWriter) (*AggregateResult, error) {
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

		if sanitizedWriter != nil {
			if err := sanitizedWriter.WriteRow(row, cacheRead, cacheWrite5m, cacheWrite1h); err != nil {
				return nil, err
			}
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

		basePrice, _ := ResolvePrice(model, book, preferPriceTable, exchangeRate)
		if tier, ok := TieredModelPrices[model]; ok && (basePrice == nil || basePrice.Source == "per_call") {
			basePrice = &ModelPrice{InputPerM: tier.Low[0], OutputPerM: tier.Low[1], Currency: "USD", Source: "tiered_low"}
		}

		var perCall, listUSD float64
		if imgMode == "per_call" {
			if mp := ParseModelPrice(other); mp > 0 {
				listUSD = mp
			}
			perCall = 1.0
		} else {
			listUSD = RowListUSD(model, prompt, uncached, cacheRead, cacheWrite5m, cacheWrite1h, completion, basePrice, wsCalls, wsPrice)
		}

		key := [2]string{model, group}
		agg, exists := buckets[key]
		if !exists {
			mode := imgMode
			if mode == "" {
				mode = "token"
			}
			agg = &AggRow{Model: model, Group: group, BillingMode: mode}
			buckets[key] = agg
			if !groupSeen[group] {
				groupSeen[group] = true
				groupOrder = append(groupOrder, group)
			}
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
				dt := time.Unix(ts, 0).In(cstLocation)
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

func parseUnixTimestamp(v string) (int64, bool) {
	s := strings.TrimSpace(v)
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
