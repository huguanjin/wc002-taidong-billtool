package billing

import (
	"time"
)

// pricedRow 一行日志的计费解析结果。
//
// 抽出来是为了让「主账单聚合」与「成本表按渠道聚合」共用同一份定价实现。
// 之前每处各写一遍分支的代价已经显现过：ratio 路径漏算缓存创建那一项，
// 在月账单上差了 311.97 元（7%）而无人察觉。同一口径只能有一份实现。
type pricedRow struct {
	// 用量（已按 usage_semantic 归一：uncached 是「未命中」量）
	Uncached     float64
	CacheRead    float64
	CacheWrite5m float64
	CacheWrite1h float64
	Output       float64
	Quota        float64

	// 官方美金刊例（该行的计价结果）
	OfficialUSD float64
	// ListOrigin 刊例来源（见 ListOrigin*）
	ListOrigin ListOrigin
	// BillingMode token | per_call | tiered_expr
	BillingMode string
	// BillingExpr 该行命中的阶梯表达式（非阶梯为空）
	BillingExpr string
	MatchedTier string
	// ExprUnitCurrency 表达式系数的计价币种：CNY / USD
	ExprUnitCurrency string
	// WebSearchCalls 网页搜索次数
	WebSearchCalls float64
	// ImagePerCallCount 按次计费时的调用次数
	ImagePerCallCount float64
	// HasCache 该行是否出现缓存用量
	HasCache bool
}

// priceRow 解析并计价一行日志。
//
// model/group/other 与该行的 token 明细；at 是该请求发生的时刻——带 hour()
// 一类峰谷倍率的表达式必须按请求当时判断，不能用 now()。
func priceRow(model, other string, prompt, completion, cacheRead, cacheWrite5m, cacheWrite1h, quota float64,
	book *PriceBook, exchangeRate float64, preferPriceTable bool, exprSetting *BillingExprSetting, at time.Time) pricedRow {

	imgMode := ImageBillingMode(model)
	semantic := InferUsageSemantic(model, UsageSemanticFromOther(other))
	uncached := UncachedInputTokens(prompt, cacheRead, cacheWrite5m, cacheWrite1h, semantic)
	wsCalls, wsPrice := ParseWebSearch(other)

	r := pricedRow{
		Uncached: uncached, CacheRead: cacheRead, CacheWrite5m: cacheWrite5m,
		CacheWrite1h: cacheWrite1h, Output: completion, Quota: quota,
		WebSearchCalls: wsCalls,
		ListOrigin:     ListOriginNone,
		// 表达式系数默认按美金计价；国产供应商家族在下面改成 CNY。
		ExprUnitCurrency: "USD",
		BillingMode:      imgMode,
	}
	if r.BillingMode == "" {
		r.BillingMode = "token"
	}
	r.HasCache = cacheRead != 0 || cacheWrite5m != 0 || cacheWrite1h != 0

	var listUSD float64

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
			r.BillingMode = BillingModeTieredExpr
		}
	}

	switch {
	case imgMode == "per_call":
		if mp := ParseModelPrice(other); mp > 0 {
			listUSD = mp
			// 按次计费的固定美金价来自日志 model_price，是一份外部刊例。
			r.ListOrigin = ListOriginExternal
		}
		r.ImagePerCallCount = 1.0

	case exprStr != "":
		img, imgO, ai, ao := ParseExtraTokens(other)
		// 表达式入参必须传原始 prompt，不是扣过缓存的 uncached：
		// BuildExprParams 内部会按表达式引用的子类变量扣一次，传 uncached 会扣两次，
		// 把缓存命中的请求算少，并让阶梯档位判断用错长度（跨 272000 阈值会判错档，
		// 单价差一倍）。与主库 BuildTieredTokenParams 同口径。
		params := BuildExprParams(model, prompt, completion,
			cacheRead, cacheWrite5m, cacheWrite1h, img, imgO, ai, ao, exprStr)
		res, err := RunBillingExpr(exprStr, params, at)
		if err != nil {
			// 表达式跑不通时回落到按量价表，宁可价格略有偏差也不整行失败。
			basePrice, _ := ResolvePrice(model, book, preferPriceTable, exchangeRate)
			if tier, ok := TieredModelPrices[model]; ok && (basePrice == nil || basePrice.Source == "per_call") {
				basePrice = &ModelPrice{InputPerM: tier.Low[0], OutputPerM: tier.Low[1], Currency: "USD", Source: "tiered_low"}
			}
			listUSD = RowListUSD(model, prompt, uncached, cacheRead, cacheWrite5m, cacheWrite1h, completion, basePrice, wsCalls, wsPrice)
			r.BillingMode = "token"
			r.ListOrigin = ListOriginExternal
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
				r.ExprUnitCurrency = "CNY"
			}
			r.BillingExpr = exprStr
			r.MatchedTier = res.MatchedTier
			// 刊例由站内表达式算出，不是外部对标价：这一行不能参与折扣反推。
			r.ListOrigin = ListOriginExpr
		}

	case hasRowRatio:
		// 该请求当时是按 ModelRatio/CompletionRatio/CacheRatio 直接计费（未命中
		// billing_expr），系数换算方式与 fetchModelPricesFromDB 保持一致：
		// ratio=1 对应官方基准 $0.002/1K token（$2/MTok）。国产供应商家族的
		// ModelRatio 在这套系统里是按人民币报价（1 元=1 美金充值），同样需要
		// 先除回 exchangeRate，避免换回人民币时被多乘一次汇率。
		inp := rowModelRatio * 2
		outp := inp * rowCompletionRatio
		crp := inp * rowCacheRatio
		// 缓存创建同样要计价，倍率取日志自带的 cache_creation_ratio(_1h)。
		// 曾经这里只算了未命中/缓存读/输出三项，导致带缓存创建的请求少算——
		// 月账单里仅这一项就差了 311.97 元（占 7%），而且悄无声息。
		w5p, w1p := ParseCacheWritePrices(inp, other)
		if VendorFamily(model) != "" {
			inp, outp, crp = inp/exchangeRate, outp/exchangeRate, crp/exchangeRate
			w5p, w1p = w5p/exchangeRate, w1p/exchangeRate
		}
		listUSD = (uncached*inp + cacheRead*crp + completion*outp +
			cacheWrite5m*w5p + cacheWrite1h*w1p) / 1_000_000
		if wsCalls > 0 && wsPrice > 0 {
			listUSD += wsCalls * wsPrice / 1000.0
		}
		r.BillingMode = "token"
		// ratio 快照的换算基准是官方锚点（ratio=1 → $2/MTok），算外部对标价。
		r.ListOrigin = ListOriginExternal

	default:
		basePrice, _ := ResolvePrice(model, book, preferPriceTable, exchangeRate)
		if tier, ok := TieredModelPrices[model]; ok && (basePrice == nil || basePrice.Source == "per_call") {
			basePrice = &ModelPrice{InputPerM: tier.Low[0], OutputPerM: tier.Low[1], Currency: "USD", Source: "tiered_low"}
		}
		listUSD = RowListUSD(model, prompt, uncached, cacheRead, cacheWrite5m, cacheWrite1h, completion, basePrice, wsCalls, wsPrice)
		if listUSD > 0 {
			r.ListOrigin = ListOriginExternal
		}
	}

	r.OfficialUSD = listUSD
	return r
}
