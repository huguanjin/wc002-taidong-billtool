package billing

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
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

// rowCacheTokens 取一行的缓存用量，返回 (缓存读, 缓存创建-5m, 缓存创建-1h)。
//
// 抽出来是因为这段判断有三个调用点：主账单聚合（aggregate.go）、脱敏日志明细
// （simplebill.go 的逐行展开）、简易账单的缓存列。各写一份的话，某些部署上
// 缓存数会一处有一处没有——那种差异极难发现，因为两边都「算得出来」，只是不一样。
//
// 判断依据是日志里到底给了什么：
//   - 有 cache_tokens / cache_creation_tokens 列（手工 SQL 导出的形态），用列值；
//     此时若 other 里带 5m/1h 拆分，拆分更细，优先按拆分取，
//     缓存读取「列值与 other 里的较大者」——列偶尔记 0，而 other 里其实有值。
//   - 没有这些列（工具导出的形态），只能从 other 里解析。
func rowCacheTokens(row []string, col map[string]int, other string) (cacheRead, cacheWrite5m, cacheWrite1h float64) {
	idxCacheTokens, hasCacheTokens := col["cache_tokens"]
	idxCacheCreation, hasCacheCreation := col["cache_creation_tokens"]

	if !hasCacheTokens || !hasCacheCreation {
		return ParseCacheTokens(other)
	}
	cacheReadCol := ToFloat(cellAt(row, idxCacheTokens))
	creationCol := ToFloat(cellAt(row, idxCacheCreation))
	cr2, w5, w1 := ParseCacheTokens(other)
	if w5 != 0 || w1 != 0 || strings.Contains(other, "cache_creation_tokens_5m") {
		return math.Max(cacheReadCol, cr2), w5, w1
	}
	return cacheReadCol, creationCol, 0
}

// ParseRowDetails 解析脱敏日志新增的逐条明细列（3.1/3.2 契约），全部来自 other
// 的单次 JSON 解析，复用 jsonNumber 取值，不为每个字段单独 Unmarshal。
// 非 JSON / 空字符串时返回零值，不报错、不 panic——缓存族字段的容错解析仍由
// ParseCacheTokens 的回退路径负责，与本函数相互独立。
func ParseRowDetails(other string, includeBilling bool) RowDetails {
	var details RowDetails
	text := strings.TrimSpace(other)
	if text == "" {
		return details
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err != nil || data == nil {
		return details
	}

	if s, ok := data["usage_semantic"].(string); ok {
		details.UsageSemantic = s
	}
	details.InputTokensTotal = optFloat(data, "input_tokens_total")
	details.CacheWriteTokens = optFloat(data, "cache_write_tokens")
	details.TextInput = optFloat(data, "text_input")
	details.TextOutput = optFloat(data, "text_output")
	details.AudioOutput = optFloat(data, "audio_output")
	details.ImageOutput = optFloat(data, "image_output")

	details.AudioInput = optFloat(data, "audio_input")
	if details.AudioInput == nil || *details.AudioInput == 0 {
		if fallback := optFloat(data, "audio_input_token_count"); fallback != nil {
			details.AudioInput = fallback
		}
	}

	if compDetails, ok := data["completion_tokens_details"].(map[string]interface{}); ok {
		details.ReasoningTokens = optFloat(compDetails, "reasoning_tokens")
	}
	if details.ReasoningTokens == nil {
		// 主库当前多数日志形态不写 completion_tokens_details.reasoning_tokens；
		// 顶层 reasoning_tokens 是否存在由实际日志决定，留这个兜底以便将来自动生效。
		details.ReasoningTokens = optFloat(data, "reasoning_tokens")
	}

	if arr, ok := data["tool_surcharges"].([]interface{}); ok {
		var parts []string
		for _, item := range arr {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := m["name"].(string)
			countRaw, hasCount := m["count"]
			priceRaw, hasPrice := m["price"]
			if name == "" || !hasCount || !hasPrice || countRaw == nil || priceRaw == nil {
				continue // count/price 缺失的元素跳过，不整体报错
			}
			parts = append(parts, fmt.Sprintf("%s×%s@%s", name, formatFloat(jsonNumber(countRaw)), formatFloat(jsonNumber(priceRaw))))
		}
		details.ToolSurcharges = strings.Join(parts, ";")
	}

	if includeBilling {
		details.Billing = BillingDetails{
			ModelRatio:           optFloat(data, "model_ratio"),
			CompletionRatio:      optFloat(data, "completion_ratio"),
			GroupRatio:           optFloat(data, "group_ratio"),
			UserGroupRatio:       optFloat(data, "user_group_ratio"),
			CacheRatio:           optFloat(data, "cache_ratio"),
			CacheCreationRatio:   optFloat(data, "cache_creation_ratio"),
			CacheCreationRatio5m: optFloat(data, "cache_creation_ratio_5m"),
			CacheCreationRatio1h: optFloat(data, "cache_creation_ratio_1h"),
			ModelPrice:           optFloat(data, "model_price"),
			PreConsumedQuota:     optFloat(data, "pre_consumed_quota"),
			ActualQuota:          optFloat(data, "actual_quota"),
		}
		if s, ok := data["billing_mode"].(string); ok {
			details.Billing.BillingMode = s
		}
		if s, ok := data["matched_tier"].(string); ok {
			details.Billing.MatchedTier = s
		}
	}

	return details
}

// optFloat 取 data[key]；缺失或为 null 时返回 nil（表示「没有这个字段」，与「值为 0」区分）。
func optFloat(data map[string]interface{}, key string) *float64 {
	v, ok := data[key]
	if !ok || v == nil {
		return nil
	}
	f := jsonNumber(v)
	return &f
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

// ParseRowRatioPricing 取日志 other 里直接记录的 ModelRatio/CompletionRatio/CacheRatio——
// 这是该请求「计费当时」真实生效的定价快照，与 ParseBillingExpr 同样优先于 options 表现在
// 的配置：同一个模型在账期内可能从 ratio 计费切换成 billing_expr 阶梯计费，options 表现在的
// expr 配置不能倒推套用到切换前仍按 ratio 计费的历史行上。
// ok=false 表示 other 没有同时给出 model_ratio 与 completion_ratio（非这套计费方式，或解析失败）。
func ParseRowRatioPricing(other string) (modelRatio, completionRatio, cacheRatio float64, ok bool) {
	text := strings.TrimSpace(other)
	if text == "" {
		return 0, 0, 0, false
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err != nil || data == nil {
		return 0, 0, 0, false
	}
	mrRaw, hasMR := data["model_ratio"]
	crRaw, hasCR := data["completion_ratio"]
	if !hasMR || !hasCR {
		return 0, 0, 0, false
	}
	return jsonNumber(mrRaw), jsonNumber(crRaw), jsonNumber(data["cache_ratio"]), true
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
		strings.HasPrefix(m, "gemini-") || strings.HasPrefix(m, "kimi-") ||
		// 国产供应商家族（DeepSeek/GLM/Minimax/Qwen）走的都是 OpenAI 兼容接口，
		// prompt_tokens 口径同样含缓存部分，需要按 openai 语义扣减，不能落到
		// anthropic 分支的兜底（兜底会把整段 prompt 都当成未命中，重复计费）。
		strings.HasPrefix(m, "deepseek") || strings.HasPrefix(m, "glm") ||
		strings.HasPrefix(m, "chatglm") || strings.HasPrefix(m, "minimax") ||
		strings.HasPrefix(m, "qwen")
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

	// 这里刻意不再按 ImageBillingMode 提前返回零价桩。
	//
	// 那个桩的写法是「模型名像 gpt-image* → 单价 0，刊例去日志里取 model_price」，
	// 但它把整条查价链路都截断了：价格表、内建图片官价、db_price_cache 里明明有的
	// 价格全都查不到。gpt-image-2.5-flare / -sunburst 就是这样被算成 0 的——
	// 它们在价格缓存里有值（5/30，来自 ModelRatio），站点也把 billing_mode 配成了
	// ratio，只有这个「名字像图片」的判定认为它们该按次计费。
	//
	// 按次与否现在由 priceRow 依据日志里的 model_price 决定，与查价彻底解耦：
	// 一个模型按次卖，它的行会带 model_price，那条路径根本不经过这里。

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

// DerivableListPrice 判断该行的刊例能不能当折扣反推的分母。
//
// 反推要成立，分母必须是一份**与站点自身定价无关的外部对标价**。站内
// billing_expr 表达式算出来的刊例是站点自己的定价公式，拿它去除站内结算额，
// 恢复出来的只是式子里隐含的 group_ratio，不是商务谈定的折扣——这正是国产模型
// 反推失真的根因，与模型是不是国产无关：任何走表达式计费的行都一样。
//
// 命中的排除条件（按优先级）：
//  1. 完全没有刊例（连表达式都没有）；
//  2. 刊例由站内表达式或混合口径算出（ListOriginExpr / ListOriginMixed）；
//  3. 被人工标记为国产/站内定价的分组或模型（manualMarkers）。
//
// 第三种是兜底：模型名推厂商家族本来就覆盖不全（doubao/ernie/hunyuan 等都没有
// 前缀），而识别不到时的失败方向是静默按海外处理、折扣悄悄算错。所以判定不靠
// 猜厂商，而由用户对具体分组显式标注。
func DerivableListPrice(agg *AggRow, manualMarkers []string) bool {
	if !HasKnownListPrice(agg) {
		return false
	}
	switch agg.ListOrigin {
	case ListOriginExpr, ListOriginMixed:
		return false
	}
	return !IsDomesticMarked(agg.Model, agg.Group, manualMarkers)
}

// DiscountResult ComputeGroupDiscounts 的结果。
type DiscountResult struct {
	// Discounts 分组 → 折扣（展示用，已按 DiscountDecimals 取整）。
	Discounts map[string]float64
	// SettleFactors 分组 → 结算用系数。
	//
	// 对按「本次倍率」结算的表达式行，这个值是**精确的**倍率/7，不做四舍五入：
	// 用取整后的折扣去乘，整表会产生约 0.05 元的系统性偏差（账期越大越明显），
	// 而那个偏差没有任何业务含义，纯粹是显示精度的副作用。
	// 其余分组的系数与其展示折扣相同。
	SettleFactors map[string]float64
	// Derived 折扣来自「Σ结算/Σ总金额」反推的分组。
	Derived map[string]bool
	// Underivable 既没有价表折扣、又不能反推的分组 → 原因，供账单备注写清楚
	// 折扣是从哪来的、为什么这个数需要人工确认。
	Underivable map[string]string
	// Manual 折扣来自「客户 + 分组」的手工维护值（见 DiscountOverrides.Manual）。
	// 与 Derived 互斥：手工值就是真值，不需要反推，也不该被当成反推值提示复核。
	Manual map[string]bool
}

// SettleFactor 取该桶的结算系数：优先用精确值，缺失时退回展示折扣。
func (r DiscountResult) SettleFactor(group string) float64 {
	if v, ok := r.SettleFactors[group]; ok {
		return v
	}
	return r.Discounts[group]
}

// DiscountOverrides 出账时的折扣覆盖，两个来源按优先级从高到低排列。
//
// 合成一个结构体而不是两个散参数：折扣覆盖的调用点有六处（账单、成本利润表、
// 成本汇总、前端摘要），参数越多越容易在某一处漏传——而漏传的表现是金额悄悄算错，
// 不会报任何错。合成结构体后，多一个来源只需要改这一处和拼装处。
type DiscountOverrides struct {
	// Forced 全局强制折扣（出账页「折扣」输入框、任务设置里的默认参数）。
	// 非 nil 时覆盖一切，包括下面 Manual 里的手工值——它是用户在本次出账里
	// 显式填的数，意图最明确。
	Forced *float64
	// Manual 按分组的手工折扣，键是 AggRow.KeyGroup（不含倍率的原始分组名，如 Codex）。
	//
	// 键不带倍率是刻意的：这个功能存在的理由就是「new-api 里的分组倍率没及时更新」，
	// 倍率本身就是不准的那个东西。挂到「分组|倍率」上，等于把正确的折扣绑在错误的键上，
	// 业务方哪天把倍率改对了，手工折扣反而匹配不上、悄悄失效。
	Manual map[string]float64
}

// ComputeGroupDiscounts 每个分组标识的折扣。
//
// 口径优先级（高到低）：
//  1. ov.Forced：调用方显式指定的统一折扣，直接覆盖，不做任何查表；
//  2. ov.Manual：按分组的手工折扣（线下谈定、new-api 里没及时更新），命中即用；
//  3. 价表折扣 sheet：按模型厂商家族（见 VendorFamily）匹配，命中即用价表值；
//  4. 反推：该组 Σ结算人民币 / Σ总金额人民币，只累加 DerivableListPrice 为真的行。
//
// 只有走到最后一步的分组才算「折扣为反推值」，需要由调用方在账单备注里写明——
// 反推值只能保证账面对得上，并不能说明商务上谈定的折扣是多少。
//
// 折扣按 agg.Group（已含倍率层级的桶键）解析。同一分组下如果出现过多种倍率，
// 它会被拆成多个桶，每个桶各自结算——用一个折扣套整组必然算错，且错多少取决于
// 该组第一行是哪个模型，这种不确定性比数值偏差本身更危险。
//
// 手工折扣是唯一的例外：它按 KeyGroup 命中，一个键覆盖该分组下的**所有**倍率桶，
// 因为那些桶的倍率差异恰恰来自同一个过期配置，商务上谈的是一个价。
//
// 既查不到价表折扣、又没有一行可反推、也没有可用倍率的分组，不能编一个数塞进账单：
// 折扣回退到「按 quota 加权的组内平均实际倍率」，并记入 Underivable 由调用方要求人工确认。
func ComputeGroupDiscounts(rows []*AggRow, book *PriceBook, exchangeRate float64, ov DiscountOverrides, manualMarkers []string) DiscountResult {
	if ov.Forced != nil {
		result := map[string]float64{}
		for _, agg := range rows {
			result[agg.Group] = round(*ov.Forced, DiscountDecimals)
		}
		return DiscountResult{
			Discounts: result, SettleFactors: map[string]float64{},
			Derived: map[string]bool{}, Underivable: map[string]string{},
			Manual: map[string]bool{},
		}
	}

	discounts := map[string]float64{}
	// 结算系数：只在按倍率结算时与展示折扣不同（精确值 vs 取整值）。
	settleFactors := map[string]float64{}
	// 哪些分组用了手工折扣，供账单备注写明来源。
	manualGroups := map[string]bool{}
	if len(ov.Manual) > 0 {
		// 手工折扣排在价表和反推之前：这个功能的意义就是「反推出来的那个数不可信」，
		// 若还让价表或反推先定下折扣，手工值就成了永远轮不到的死配置。
		for _, agg := range rows {
			if _, already := manualGroups[agg.Group]; already {
				continue
			}
			d, ok := ov.Manual[agg.KeyGroup]
			if !ok {
				continue
			}
			// 展示值与结算系数都用手工值。反推那条链路之所以「显示取整、结算用精确商」，
			// 是因为精确商才是账实相符的真值；手工折扣本身就是真值（商务谈定的那个数），
			// 没有隐藏精度可言，两者相等才符合预期。
			discounts[agg.Group] = round(d, DiscountDecimals)
			settleFactors[agg.Group] = round(d, DiscountDecimals)
			manualGroups[agg.Group] = true
		}
	}

	// 按倍率结算的行（站内表达式计费，以及国产模型的计费快照行）直接按「本次请求实际使用的倍率」结算：
	// 站内 quota = 表达式USD × GroupRatio × QuotaPerCNY，而 OfficialUSD = 表达式USD，
	// 于是 折扣 = GroupRatio / DiscountBaseFactor 时，
	// 结算额 = OfficialUSD × 汇率 × 折扣 == quota / QuotaPerCNY，与实收逐行严格相等。
	// 这是表达式行的正确口径，不需要（也不能）靠反推得到。
	for _, agg := range rows {
		if _, already := discounts[agg.Group]; already {
			continue
		}
		if !agg.HasRatioDiscount() {
			continue
		}
		discounts[agg.Group] = round(agg.RatioDiscount(), DiscountDecimals)
		// 结算用未取整的精确比值，避免整表累计出 0.05 元量级的无意义偏差。
		settleFactors[agg.Group] = agg.RatioDiscount()
	}

	// 这一步必须排在价表折扣之前：国产模型（DeepSeek/GLM/Qwen…）同时命中价表的厂商家族折扣，
	// 但站点实收是按分组倍率扣的（0.75 倍率即 75 折），价表里的折扣不能压过它，
	// 否则账单与站内实收对不上，且备注还写着「按分组倍率结算」。

	// 价表优先：同一分组下若各模型的厂商家族折扣不一致，以先命中者为准，
	// 并把该组记为「混合折扣」，由写账单时在备注里提示复核。
	tableGroups := map[string]bool{}
	for _, agg := range rows {
		if book == nil {
			break
		}
		if _, already := discounts[agg.Group]; already {
			continue
		}
		if d, ok := tableDiscount(book, agg.Model); ok {
			discounts[agg.Group] = round(d, DiscountDecimals)
			tableGroups[agg.Group] = true
		}
	}

	derived := map[string]bool{}
	underivable := map[string]string{}

	// 剩下的走反推：分母只累加口径可信的行。
	// 表达式行与倍率行不参与——它们的折扣已由上面的分支确定，且比值型口径
	// 反推出来的只是式子里的 group_ratio，不是商务折扣。
	settleByGroup := map[string]float64{}
	listByGroup := map[string]float64{}
	skippedByGroup := map[string]int{}
	for _, agg := range rows {
		if !DerivableListPrice(agg, manualMarkers) || agg.HasRatioDiscount() {
			skippedByGroup[agg.Group]++
			continue
		}
		settleByGroup[agg.Group] += SettleCNY(agg)
		listByGroup[agg.Group] += OfficialListCNY(agg, exchangeRate)
	}

	for _, agg := range rows {
		if _, ok := discounts[agg.Group]; ok {
			// 已定折扣的桶：如果组内还有别的桶没能定折扣，这里不做处理——
			// underivable 是按桶键记录的，会在下面各自那轮里写入。
			continue
		}
		listing := listByGroup[agg.Group]
		if listing <= 0 {
			// 该桶一行都不可反推、也没有可用倍率：退到加权的实际倍率。
			exact := weightedSiteDiscount(rows, agg.Group, exchangeRate)
			discounts[agg.Group] = round(exact, DiscountDecimals)
			settleFactors[agg.Group] = exact
			underivable[agg.Group] = underivableReason(agg, manualMarkers)
			continue
		}
		// 反推值同样「显示取整、结算用精确商」：分母是刊例、分子是实收，
		// 用精确商结算时 结算额 == Σ quota，账实逐行相符；
		// 把商四舍五入到 3 位再乘会引入与金额无关的、纯显示精度造成的偏差。
		//
		// 每个桶都要单独算一遍：同一分组拆出的多个桶各自的「结算/刊例」并不相同，
		// 用一个桶的值去填另一个桶就会互相覆盖（先写入的被后写入的盖掉）。
		exact := settleByGroup[agg.Group] / listing
		discounts[agg.Group] = round(exact, DiscountDecimals)
		settleFactors[agg.Group] = exact
		derived[agg.Group] = true
		if skippedByGroup[agg.Group] > 0 {
			underivable[agg.Group] = underivableReason(agg, manualMarkers)
		}
	}
	return DiscountResult{
		Discounts: discounts, SettleFactors: settleFactors,
		Derived: derived, Underivable: underivable, Manual: manualGroups,
	}
}

// weightedSiteDiscount 同一桶内按 quota 加权的实际倍率对应的折扣。
//
// 取代原来「取分组第一行代表整组」的写法：那个做法下，整组折扣取决于第一行
// 是哪个模型，账期数据顺序一变金额就变，属于不可复现的错误。
// 这里用 quota 加权，整桶折算额除以整桶刊例，与账实相符的含义一致。
func weightedSiteDiscount(rows []*AggRow, groupKey string, exchangeRate float64) float64 {
	if exchangeRate <= 0 {
		return 0
	}
	var settle, listing float64
	for _, agg := range rows {
		if agg.Group != groupKey || agg.OfficialUSD <= 0 {
			continue
		}
		settle += agg.Quota / QuotaPerCNY
		listing += agg.OfficialUSD * exchangeRate
	}
	if listing <= 0 {
		return 0
	}
	return settle / listing
}

// siteDiscount 单行的实际倍率对应折扣：quota 折算人民币 ÷ 该行清单刊例人民币。
// 分子分母都换成人民币比较，量纲才对得上——拿人民币除美金会得出一个
// 被汇率放大的数（如 3.5 而不是 0.5），那种数字写进账单比不写更误导。
func siteDiscount(agg *AggRow, exchangeRate float64) float64 {
	if agg.OfficialUSD <= 0 || exchangeRate <= 0 {
		return 0
	}
	return (agg.Quota / QuotaPerCNY) / (agg.OfficialUSD * exchangeRate)
}

// underivableReason 生成「该组不能反推」的原因说明，写进账单备注。
//
// 口径类原因（站内公式/混合口径/无刊例）优先于标记类原因：前两者是这一行客观上
// 就没有外部对标价，说清楚比归咎于人工标记有用得多，也便于事后核对。
func underivableReason(agg *AggRow, manualMarkers []string) string {
	switch agg.ListOrigin {
	case ListOriginExpr:
		return "该分组按站内表达式（billing_expr）计费，刊例由站点自有公式算出，不具备外部对标价，无法反推折扣"
	case ListOriginMixed:
		return "该分组内混用了站内表达式与外部价两种口径，分母不统一，无法反推折扣"
	}
	if IsDomesticMarked(agg.Model, agg.Group, manualMarkers) {
		return "该分组在「国产/站内定价标识」里被指定为站内定价，不参与折扣反推"
	}
	return "该分组没有可用的外部对标刊例，无法反推折扣"
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
	{"kimi", "Kimi"},
	{"qwen", "Qwen"},
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

// IsDomesticMarked 判断某 (model, group) 是否被用户人工标记为国产/站内定价：
// manualMarkers 里一条可以是分组名（精确匹配）或模型名前缀（前缀匹配，大小写不敏感）。
//
// 这里**只**认人工标记，不掺厂商前缀的自动识别。自动识别那条路走的是刊例来源
// （ListOrigin）：模型叫不叫 deepseek 和它有没有外部对标价是两件事——
// 挂第三方部署的 deepseek 一样有官方对标价，而站内自建的任何模型都没有。
// 把厂商名当判据会在两个方向同时出错，所以它只保留在币种换算与价表折扣匹配上用。
//
// 仅用于排除折扣反推，不改变折扣的计算口径与数值来源优先级。
func IsDomesticMarked(model, group string, manualMarkers []string) bool {
	modelLower := strings.ToLower(strings.TrimSpace(model))
	groupTrimmed := strings.TrimSpace(group)
	for _, raw := range manualMarkers {
		marker := strings.TrimSpace(raw)
		if marker == "" {
			continue
		}
		if marker == groupTrimmed {
			return true
		}
		if strings.HasPrefix(modelLower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// GroupRatioFromOther 从日志 other 里取本次请求实际使用的分组倍率。
//
// 解析走容错的 iterTextCandidates（原文 → URL 解码 → 去转义引号），与
// ExtractCacheFields 同一套：日志里确实存在整段不是合法 JSON 的 other
// （admin_info.channel_affinity.key_hint 里嵌了转义引号，形如 "{\"de...28\"}"），
// 严格的 json.Unmarshal 会整行失败，导致这些行取不到倍率、
// 在按倍率分档结算时被误判。ok=false 表示该行没有给倍率。
func GroupRatioFromOther(other string) (float64, bool) {
	text := strings.TrimSpace(other)
	if text == "" {
		return 0, false
	}
	for _, candidate := range iterTextCandidates(text) {
		parsed, ok := parseJSONValue(candidate)
		if !ok {
			parsed, ok = parseEmbeddedJSON(candidate)
		}
		if !ok {
			continue
		}
		if v, found := findKeyRecursive(parsed, "group_ratio"); found {
			if n, isNum := parseFloatValue(v); isNum && n > 0 {
				return n, true
			}
		}
	}
	// 整段都解析不出来时退回正则，从原始文本里抠出这个字段。
	if m := groupRatioRe.FindStringSubmatch(text); m != nil {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil && f > 0 {
			return f, true
		}
	}
	return 0, false
}

// parseFloatValue 从 JSON 解出来的值里取浮点数。
// 不能复用 cache.go 的 parseNumber——它返回 int64，会把倍率 0.4 截断成 0。
func parseFloatValue(value interface{}) (float64, bool) {
	switch t := value.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f, true
		}
		return 0, false
	case string:
		s := strings.Trim(strings.TrimSpace(t), `"'`)
		if s == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
		return 0, false
	}
	return 0, false
}

// groupRatioRe 兜底的 group_ratio 提取：匹配 "group_ratio": 0.4 这类片段。
var groupRatioRe = regexp.MustCompile(`"group_ratio"\s*:\s*"?([0-9]*\.?[0-9]+)"?`)

// ParseCacheWritePrices 解析缓存创建的单价（$/MTok）。
//
// ratio 计费路径要用日志自带的 cache_creation_ratio / cache_creation_ratio_1h：
// 它们与 model_ratio 同一套基准（相对输入倍数的倍数），
// 例如 model_ratio=2.5（=$5/MTok）、cache_creation_ratio=1.25 → 6.25、_1h=2 → 10。
// 字段缺失时回落到内置的 CacheWrite5mMult / CacheWrite1hMult。
//
// 日志里确实存在整段不是合法 JSON 的 other（key_hint 里嵌了转义引号），
// 这类行取不到倍率，用兜底倍数即可——它正是官方倍数，不会算错。
func ParseCacheWritePrices(inputPerM float64, other string) (w5m, w1h float64) {
	r5m, r1h := CacheWrite5mMult, CacheWrite1hMult

	text := strings.TrimSpace(other)
	if text != "" {
		if data := parseOtherJSON(text); data != nil {
			if r := optFloat(data, "cache_creation_ratio_5m"); r != nil && *r > 0 {
				r5m = *r
			} else if r := optFloat(data, "cache_creation_ratio"); r != nil && *r > 0 {
				r5m = *r
			}
			if r := optFloat(data, "cache_creation_ratio_1h"); r != nil && *r > 0 {
				r1h = *r
			}
		}
	}
	return inputPerM * r5m, inputPerM * r1h
}

// parseOtherJSON 解析 other；失败返回 nil。容错：先原文，再逐级去转义。
func parseOtherJSON(text string) map[string]interface{} {
	for _, candidate := range iterTextCandidates(text) {
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(candidate), &data); err == nil && data != nil {
			return data
		}
	}
	return nil
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
