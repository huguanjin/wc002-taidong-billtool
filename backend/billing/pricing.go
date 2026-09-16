package billing

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ToFloat 对应 _to_float：空/非法一律返回 0。
func ToFloat(value string) float64 {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return f
}

func jsonNumber(v interface{}) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return t
	case string:
		return ToFloat(t)
	case bool:
		return 0
	default:
		return 0
	}
}

// ParseCacheTokens 对应 parse_cache_tokens：优先解析 other 里的结构化缓存字段，
// 解析失败（非规整 JSON）时回退到 extract_cache_columns 的容错解析。
func ParseCacheTokens(other string) (cacheRead, cacheWrite5m, cacheWrite1h float64) {
	text := strings.TrimSpace(other)
	if text == "" {
		return 0, 0, 0
	}

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err == nil && data != nil {
		cacheRead = jsonNumber(data["cache_tokens"])
		cc5Raw, has5 := data["cache_creation_tokens_5m"]
		cc1Raw, has1 := data["cache_creation_tokens_1h"]
		if has5 || has1 {
			return cacheRead, jsonNumber(cc5Raw), jsonNumber(cc1Raw)
		}

		cc := jsonNumber(data["cache_creation_tokens"])
		if cc == 0 {
			cc = jsonNumber(data["cache_write_tokens"])
		}
		ratio := CacheWrite5mMult
		if r, ok := data["cache_creation_ratio"]; ok && r != nil {
			ratio = ratioOrDefault(r, ratio)
		} else if r, ok := data["cache_creation_ratio_5m"]; ok && r != nil {
			ratio = ratioOrDefault(r, ratio)
		}
		if ratio >= 1.9 {
			return cacheRead, 0, cc
		}
		return cacheRead, cc, 0
	}

	creation, cacheTokens := ExtractCacheFields(other)
	cr := 0.0
	if cacheTokens != nil {
		cr = float64(*cacheTokens)
	}
	cc := 0.0
	if creation != nil {
		cc = float64(*creation)
	}
	return cr, cc, 0
}

func ratioOrDefault(v interface{}, def float64) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return def
}

// ParseWebSearch 返回 (web_search_call_count, web_search_price $/1k calls)。
func ParseWebSearch(other string) (calls float64, price float64) {
	text := strings.TrimSpace(other)
	if text == "" {
		return 0, 0
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err != nil || data == nil {
		return 0, 0
	}
	calls = jsonNumber(data["web_search_call_count"])
	if calls <= 0 {
		if v, ok := data["web_search"]; ok {
			if b, isBool := v.(bool); (isBool && b) || (!isBool && v != nil && v != false) {
				calls = 1
			}
		}
	}
	price = jsonNumber(data["web_search_price"])
	return calls, price
}

// ParseModelPrice 日志 other.model_price；>0 表示按次固定美金价，-1 表示按 ratio。
func ParseModelPrice(other string) float64 {
	text := strings.TrimSpace(other)
	if text == "" {
		return -1.0
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err != nil || data == nil {
		return -1.0
	}
	if v, ok := data["model_price"]; ok && v != nil {
		return jsonNumber(v)
	}
	return -1.0
}

// ParseBillingExpr 从日志 other 里取本次请求实际使用的阶梯计费表达式。
// 上游计费时把表达式以 base64 写进 other.expr_b64，因此日志自带「当时生效的规则」，
// 比事后去读 options 表更贴近事实——option 表可能已经改过。
func ParseBillingExpr(other string) string {
	text := strings.TrimSpace(other)
	if text == "" {
		return ""
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err != nil || data == nil {
		return ""
	}
	raw, ok := data["expr_b64"]
	if !ok || raw == nil {
		return ""
	}
	s, isStr := raw.(string)
	if !isStr || s == "" {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(decoded))
}

// ParseMatchedTier 日志 other.matched_tier：上游计费时 tier() 命中的档位名。
func ParseMatchedTier(other string) string {
	text := strings.TrimSpace(other)
	if text == "" {
		return ""
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err != nil || data == nil {
		return ""
	}
	if v, ok := data["matched_tier"]; ok && v != nil {
		if s, isStr := v.(string); isStr {
			return s
		}
	}
	return ""
}

// ParseExtraTokens 从日志 other 里取图片/音频的 token 明细，供 gpt-realtime 一类
// 表达式（引用 img/img_o/ai/ao）计价。日志里没有这些字段时一律返回 0，
// 此时这些 token 仍留在 p/c 里按基础价计费，不会凭空多算费用。
func ParseExtraTokens(other string) (img, imgO, ai, ao float64) {
	text := strings.TrimSpace(other)
	if text == "" {
		return 0, 0, 0, 0
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err != nil || data == nil {
		return 0, 0, 0, 0
	}
	details, _ := data["prompt_tokens_details"].(map[string]interface{})
	compDetails, _ := data["completion_tokens_details"].(map[string]interface{})
	img = jsonNumber(details["image_tokens"])
	ai = jsonNumber(details["audio_tokens"])
	ao = jsonNumber(compDetails["audio_tokens"])
	imgO = jsonNumber(compDetails["image_tokens"])
	return img, imgO, ai, ao
}

// ImageBillingMode 图片模型计费方式：token / per_call；非图片返回 ""。
func ImageBillingMode(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.Contains(m, "gemini") && strings.Contains(m, "image") {
		if strings.HasSuffix(m, "-token") {
			return "token"
		}
		return "per_call"
	}
	if m == "gpt-image-2" {
		return "token"
	}
	if strings.HasPrefix(m, "gpt-image") || (strings.HasPrefix(m, "gpt") && strings.Contains(m, "image")) {
		return "per_call"
	}
	return ""
}

func IsOpenAIOrGeminiModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4") ||
		strings.HasPrefix(m, "gemini-") || strings.HasPrefix(m, "kimi-")
}

func IsAnthropicModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "claude")
}

// InferUsageSemantic 推断 prompt_tokens 语义：anthropic=已是未命中；openai=含缓存。
func InferUsageSemantic(model string, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if IsAnthropicModel(model) {
		return "anthropic"
	}
	if IsOpenAIOrGeminiModel(model) {
		return "openai"
	}
	return "anthropic"
}

func UsageSemanticFromOther(other string) string {
	text := strings.TrimSpace(other)
	if text == "" {
		return ""
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err != nil || data == nil {
		return ""
	}
	if v, ok := data["usage_semantic"]; ok && v != nil {
		if s, isStr := v.(string); isStr {
			return s
		}
	}
	return ""
}

// UncachedInputTokens anthropic：prompt 已是未命中；openai/gemini：prompt 含缓存，需扣减。
func UncachedInputTokens(promptTokens, cacheRead, cacheWrite5m, cacheWrite1h float64, usageSemantic string) float64 {
	if usageSemantic == "" || usageSemantic == "anthropic" {
		return math.Max(0, promptTokens)
	}
	return math.Max(0, promptTokens-cacheRead-cacheWrite5m-cacheWrite1h)
}

// ResolveTierPrices 若模型有阶梯配置，返回该请求的 (input, output, cache_read) $/MTok。
func ResolveTierPrices(model string, promptTokens float64) (input, output, cacheRead float64, ok bool) {
	cfg, exists := TieredModelPrices[model]
	if !exists {
		return 0, 0, 0, false
	}
	useLow := promptTokens < cfg.Threshold
	if cfg.Op == "le" {
		useLow = promptTokens <= cfg.Threshold
	}
	chosen := cfg.Low
	if !useLow {
		chosen = cfg.High
	}
	return chosen[0], chosen[1], chosen[2], true
}

// CacheUnitPrices 返回 (缓存读, 写5m, 写1h) $/MTok。
func CacheUnitPrices(model string, inputPerM float64) (read, w5, w1 float64) {
	read = inputPerM * CacheReadMult
	if v, ok := CacheReadAbsUSD[model]; ok {
		read = v
	}
	return read, inputPerM * CacheWrite5mMult, inputPerM * CacheWrite1hMult
}

// ResolvePrice 返回 (价格, 备注)；价格已换算为 USD/MTok。nil 表示缺少定价。
func ResolvePrice(model string, book *PriceBook, preferPriceTable bool, exchangeRate float64) (*ModelPrice, string) {
	var notes []string
	table := ModelPrice{}
	hasTable := false
	if book != nil {
		table, hasTable = book.ByModel[model]
	}
	officialAnthropic, hasAnthropic := OfficialAnthropicPrices[model]
	officialGemini, hasGemini := OfficialGeminiTextPrices[model]
	officialKimi, hasKimi := OfficialKimiPrices[model]
	officialImage, hasImage := OfficialImageTokenPrices[model]
	mode := ImageBillingMode(model)

	if mode == "per_call" {
		return &ModelPrice{
			InputPerM: 0, OutputPerM: 0, Currency: "USD",
			Source: "per_call", Category: "Image", Channel: "per_call",
		}, "图片按次计费，刊例取日志 model_price"
	}

	var chosen *ModelPrice
	switch {
	case hasImage && !preferPriceTable:
		chosen = &ModelPrice{InputPerM: officialImage.Input, OutputPerM: officialImage.Output, Currency: "USD", Source: "image_official", Category: "Image", Channel: "token"}
		if hasTable && priceDiffers(table, officialImage) {
			notes = append(notes, "报价表价格与图片官网不一致，已按官网价")
		}
	case hasGemini && !preferPriceTable:
		chosen = &ModelPrice{InputPerM: officialGemini.Input, OutputPerM: officialGemini.Output, Currency: "USD", Source: "gemini_official", Category: "Gemini", Channel: "official"}
		if hasTable && priceDiffers(table, officialGemini) {
			notes = append(notes, "报价表价格与 Gemini 官网不一致，已按官网价")
		}
	case hasAnthropic && !preferPriceTable:
		chosen = &ModelPrice{InputPerM: officialAnthropic.Input, OutputPerM: officialAnthropic.Output, Currency: "USD", Source: "anthropic_official", Category: "Anthropic", Channel: "official"}
		if hasTable && priceDiffers(table, officialAnthropic) {
			notes = append(notes, "报价表价格与 Anthropic 官网不一致，已按官网价")
		}
	case hasKimi && !preferPriceTable:
		chosen = &ModelPrice{InputPerM: officialKimi.Input, OutputPerM: officialKimi.Output, Currency: "USD", Source: "kimi_official", Category: "Kimi", Channel: "official"}
		if hasTable && priceDiffers(table, officialKimi) {
			notes = append(notes, "报价表价格与 Kimi 官网不一致，已按官网价")
		}
	case hasTable:
		t := table
		chosen = &t
	case hasImage:
		chosen = &ModelPrice{InputPerM: officialImage.Input, OutputPerM: officialImage.Output, Currency: "USD", Source: "image_official", Category: "Image", Channel: "token"}
	case hasGemini:
		chosen = &ModelPrice{InputPerM: officialGemini.Input, OutputPerM: officialGemini.Output, Currency: "USD", Source: "gemini_official", Category: "Gemini", Channel: "official"}
	case hasAnthropic:
		chosen = &ModelPrice{InputPerM: officialAnthropic.Input, OutputPerM: officialAnthropic.Output, Currency: "USD", Source: "anthropic_official", Category: "Anthropic", Channel: "official"}
	case hasKimi:
		chosen = &ModelPrice{InputPerM: officialKimi.Input, OutputPerM: officialKimi.Output, Currency: "USD", Source: "kimi_official", Category: "Kimi", Channel: "official"}
	}

	if chosen == nil {
		return nil, "缺少官方/报价表定价"
	}

	if chosen.Currency == "CNY" {
		chosen = &ModelPrice{
			InputPerM: chosen.InputPerM / exchangeRate, OutputPerM: chosen.OutputPerM / exchangeRate,
			Currency: "USD", Source: chosen.Source + "_cny_converted", Category: chosen.Category, Channel: chosen.Channel,
		}
		notes = append(notes, "原报价为人民币，已按汇率折算美元")
	}

	return chosen, strings.Join(notes, "；")
}

func priceDiffers(table ModelPrice, official PriceUSD) bool {
	const eps = 1e-9
	return math.Abs(table.InputPerM-official.Input) > eps || math.Abs(table.OutputPerM-official.Output) > eps
}

// RowListUSD 单条请求的官方美金刊例（含阶梯与 web_search）。
func RowListUSD(model string, promptTokens, uncached, cacheRead, cacheWrite5m, cacheWrite1h, output float64,
	basePrice *ModelPrice, webSearchCalls, webSearchPricePer1k float64) float64 {

	var inp, outp, crp float64
	if ti, to, tc, ok := ResolveTierPrices(model, promptTokens); ok {
		inp, outp, crp = ti, to, tc
	} else if basePrice != nil {
		inp, outp = basePrice.InputPerM, basePrice.OutputPerM
		crp = inp * CacheReadMult
		if v, ok := CacheReadAbsUSD[model]; ok {
			crp = v
		}
	} else {
		if webSearchCalls > 0 && webSearchPricePer1k > 0 {
			return webSearchCalls * webSearchPricePer1k / 1000.0
		}
		return 0
	}

	w5p := inp * CacheWrite5mMult
	w1p := inp * CacheWrite1hMult
	usd := (uncached*inp + cacheRead*crp + output*outp + cacheWrite5m*w5p + cacheWrite1h*w1p) / 1_000_000
	if webSearchCalls > 0 && webSearchPricePer1k > 0 {
		usd += webSearchCalls * webSearchPricePer1k / 1000.0
	}
	return usd
}

// OfficialListUSD 未打折美金刊例：汇总阶段已按请求累计（含阶梯与 web_search）。
func OfficialListUSD(agg *AggRow) float64 { return agg.OfficialUSD }

// OfficialListCNY 总金额（人民币）= 官方美金 × 汇率（不截断）。
func OfficialListCNY(agg *AggRow, exchangeRate float64) float64 {
	return OfficialListUSD(agg) * exchangeRate
}

func round(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}

// SettleCNY 结算金额（人民币），保留4位小数。
func SettleCNY(agg *AggRow) float64 {
	return round(agg.SiteCNY(), MoneyDecimals)
}

// HasKnownListPrice 该行是否有可信的官方刊例，用于折扣分母的取舍。
//
// 阶梯表达式模型在 ModelRatio/ModelPrice 里通常查不到价（上游由表达式定价，
// 价表里没有它的条目），但表达式本身已经算出了刊例，这类行必须计入分母；
// 真的一点价都取不到的模型才排除，否则会把折扣整体拉高。
func HasKnownListPrice(agg *AggRow) bool {
	if agg.OfficialUSD > 0 {
		return true
	}
	return agg.BillingMode == BillingModeTieredExpr && agg.BillingExpr != ""
}

// ComputeGroupDiscounts 每个分组标识的折扣。
//
// 口径优先级（高到低）：
//  1. forcedDiscount：调用方显式指定的统一折扣，直接覆盖，不做任何查表；
//  2. 价表折扣 sheet：按模型厂商家族（见 VendorFamily）匹配，命中即用价表值；
//  3. 反推：该组 Σ结算人民币 / Σ总金额人民币。
//
// 只有走到第 3 步的分组才算「折扣为反推值」，需要由调用方在账单备注里写明——
// 反推值只能保证账面对得上，并不能说明商务上谈定的折扣是多少。
//
// 返回值第二项是「靠反推得到折扣」的分组集合，供写账单时标注。
func ComputeGroupDiscounts(rows []*AggRow, book *PriceBook, exchangeRate float64, forcedDiscount *float64, preferPriceTable bool) (map[string]float64, map[string]bool) {
	if forcedDiscount != nil {
		result := make(map[string]float64, len(rows))
		for _, agg := range rows {
			result[agg.Group] = round(*forcedDiscount, DiscountDecimals)
		}
		return result, map[string]bool{}
	}

	discounts := map[string]float64{}
	// 价表优先：同一分组下若各模型的厂商家族折扣不一致，以先命中者为准，
	// 并把该组记为「混合折扣」，由写账单时在备注里提示复核。
	tableGroups := map[string]bool{}
	for _, agg := range rows {
		if book == nil {
			break
		}
		if d, ok := tableDiscount(book, agg.Model); ok {
			if _, seen := discounts[agg.Group]; !seen || tableGroups[agg.Group] {
				discounts[agg.Group] = round(d, DiscountDecimals)
				tableGroups[agg.Group] = true
			}
		}
	}

	settleByGroup := map[string]float64{}
	listByGroup := map[string]float64{}
	for _, agg := range rows {
		if !HasKnownListPrice(agg) {
			continue
		}
		settleByGroup[agg.Group] += SettleCNY(agg)
		listByGroup[agg.Group] += OfficialListCNY(agg, exchangeRate)
	}

	derived := map[string]bool{}
	for group, settle := range settleByGroup {
		if _, ok := discounts[group]; ok {
			continue
		}
		listing := listByGroup[group]
		raw := 0.0
		// 分母为 0（整组零用量）时不反推，避免 0/0 把折扣写成 0。
		if listing > 0 {
			raw = settle / listing
		}
		discounts[group] = round(raw, DiscountDecimals)
		derived[group] = true
	}
	return discounts, derived
}

// tableDiscount 按模型厂商家族在价表折扣 sheet 里查折扣。
func tableDiscount(book *PriceBook, model string) (float64, bool) {
	if book == nil {
		return 0, false
	}
	family := VendorFamily(model)
	if family == "" {
		return 0, false
	}
	if d, ok := book.Discounts[family]; ok {
		return d, true
	}
	return 0, false
}

// vendorModelPrefixes 模型名 → 厂商家族名的前缀映射。
//
// 价表折扣 sheet 的键是厂商家族名（DeepSeek / GLM / Minimax / 可灵），
// 而日志里的分组标识是客户自己的业务分组（vip、az定制、AWSB opus5 …），
// 两者不是一个命名空间，所以只能从模型名反推家族。
// 前缀按「长前缀优先」匹配（minimax-m 早于 mini），避免误判。
var vendorModelPrefixes = []struct {
	prefix string
	family string
}{
	{"deepseek", "DeepSeek"},
	{"glm", "GLM"},
	{"chatglm", "GLM"},
	{"minimax", "Minimax"},
	{"kling", "可灵"},
	{"可灵", "可灵"},
}

// VendorFamily 从模型名推断厂商家族，用于匹配价表折扣；无法判断时返回 ""。
func VendorFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}
	for _, vp := range vendorModelPrefixes {
		if strings.HasPrefix(m, vp.prefix) {
			return vp.family
		}
	}
	return ""
}

// DiscountFromPriceTable 按模型厂商家族取价表折扣（供单行标注使用）。
func DiscountFromPriceTable(book *PriceBook, model string) (float64, bool) {
	return tableDiscount(book, model)
}

// ParseDiscountText 对应 parse_discount_text：兼容百分数、"6折"、纯小数写法。
func ParseDiscountText(value string) (float64, bool) {
	text := strings.TrimSpace(value)
	if text == "" || text == "待定" {
		return 0, false
	}
	if strings.HasSuffix(text, "%") {
		if f, err := strconv.ParseFloat(strings.TrimSuffix(text, "%"), 64); err == nil {
			return f / 100.0, true
		}
		return 0, false
	}
	if strings.HasSuffix(text, "折") {
		numText := strings.TrimSuffix(text, "折")
		if n, err := strconv.ParseFloat(numText, 64); err == nil {
			if n > 1 {
				return n / 10.0, true
			}
			return n, true
		}
		return 0, false
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		if f <= 1 {
			return f, true
		}
		return 0, false
	}
	return 0, false
}

// SortedGroupNames 按名称排序的分组列表，便于日志/摘要稳定输出。
func SortedGroupNames(discounts map[string]float64) []string {
	names := make([]string, 0, len(discounts))
	for g := range discounts {
		names = append(names, g)
	}
	sort.Strings(names)
	return names
}
