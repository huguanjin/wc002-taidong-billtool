package billing

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

// pricedRow 一行日志的计费解析结果。
//
// 抽出来是为了让「主账单聚合」与「成本利润表按渠道聚合」共用同一份定价实现。
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
	// PerCallCountUncertain 按次计费的行没能从日志反推出张数，已按 1 次计。
	// 调用方据此在账单备注里说明，避免客户拿「单价 × 用量」核对时对不上却不知道为什么。
	PerCallCountUncertain bool
	// HasCache 该行是否出现缓存用量
	HasCache bool
	// SiteRatioList 刊例是由日志自带的计费快照（表达式或 model_ratio）算出的，
	// 即 quota = 刊例 × group_ratio 这条恒等式成立。国产模型据此直接按分组倍率结算。
	SiteRatioList bool
}

// IsTaskQuotaAdjustment 该行是不是异步任务的额度调整行（退款或补扣结算），而不是一次消费。
//
// 判据是 other 里有没有 task_id，**不是** logs.type 或 content 文本：
//   - 原始消费行由 service.LogTaskConsumption 记录，只在提交时记一条，**不写 task_id**；
//   - 结算行（RecalculateTaskQuota）与退款行（RefundTaskQuota、midjourney 构图失败）
//     都会写 task_id。
//
// 用 type 单值判会漏：补扣的结算行也是 type=2（与消费行同类型），只有 task_id 能把
// 「这次请求值多少钱」与「这笔预扣最后调回多少」区分开。
func IsTaskQuotaAdjustment(other string) bool {
	text := strings.TrimSpace(other)
	if text == "" {
		return false
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(text), &data); err != nil || data == nil {
		return false
	}
	v, ok := data["task_id"]
	// 兼容数字与字符串两种写法：JSON 里的 id 可能是 123 也可能是 "123"，
	// 空串与 0 都当作「没有 task_id」。
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t) != "" && strings.TrimSpace(t) != "0"
	default:
		return jsonNumber(v) != 0
	}
}

// QuotaAdjustmentDelta 额度调整行的带符号净额（单位：quota）。
//
// **正数表示退还**，调用方要用减法并入结算；这与日志里 quota 恒为正数的形态有关：
// 站内的退款日志（type=6）写的是 `quota = 退还额`（正数），补扣的结算行
// （type=2、delta>0）写的是 `quota = 补扣额`（正数）。所以方向必须靠 type 判，
// 不能指望 quota 的符号。
//
// 无法识别 type 时返回 0 并返回 false：宁可这行不冲抵（金额偏高、可被人工发现），
// 也不要按猜错的方向冲抵（金额偏低且看起来合理，没人会去查）。
func QuotaAdjustmentDelta(logType string, quota float64) (float64, bool) {
	switch strings.TrimSpace(logType) {
	case "6":
		// 退款：站点把预扣的额度还回来，净结算要减掉。
		return quota, true
	case "2":
		// 补扣结算行：任务是按预扣全额记的消费，这里把差额补上，净结算要加上。
		return -quota, true
	default:
		return 0, false
	}
}

// priceRow 解析并计价一行日志。
//
// model/group/other 与该行的 token 明细；at 是该请求发生的时刻——带 hour()
// 一类峰谷倍率的表达式必须按请求当时判断，不能用 now()。
func priceRow(model, other string, prompt, completion, cacheRead, cacheWrite5m, cacheWrite1h, quota float64,
	book *PriceBook, exchangeRate float64, preferPriceTable bool, exprSetting *BillingExprSetting, at time.Time) pricedRow {

	// 是否按次计费，由**日志自带的证据**决定，不靠模型名猜。
	//
	// 站点按次卖的模型会在 other 里写 model_price > 0，这就是最可靠的依据；
	// 而 model_price 取不到时返回 -1，取到 0 也是 0，两者都表示「站点这次不是按固定价计费的」。
	//
	// 原先的判据是 ImageBillingMode(model)=="per_call"，即「名字像 gpt-image* 就当按次」。
	// 那个启发式把 gpt-image-2.5-flare / -sunburst 误判成按次，于是跳过了 ratio 与表达式
	// 两条路径，而站点对它们并没有配固定价——最终刊例恒为 0，客户拿到账单发现这些模型白送。
	// 更隐蔽的是它不报错：0 会一路写进 AC 列，看起来只是「这个模型没价」。
	modelPrice := ParseModelPrice(other)
	perCall := modelPrice > 0

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
		BillingMode:      ImageBillingMode(model),
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
	// 只有按次计费的行才跳过表达式：按次卖的行刊例就是 model_price，不该再套
	// 按 token 计费的表达式。反过来，**不按次的行一律照走**——原先这里是
	// `imgMode != "per_call"`，让所有名字像图片的模型都被拦在表达式之外。
	if !perCall {
		exprStr = ParseBillingExpr(other)
		if exprStr == "" && !hasRowRatio && exprSetting != nil {
			exprStr = exprSetting.Expr(model)
		}
		if exprStr != "" {
			r.BillingMode = BillingModeTieredExpr
		}
	}

	switch {
	case perCall:
		// 反推张数 n：日志 other 里没有 n 这个键，但站内计费式是
		//   quota = model_price × QuotaPerCNY × group_ratio × n
		// 反解即可。验算过一份真实日志：45000 / (0.12 × 500000 × 0.75) = 1。
		//
		// 反推不出接近整数的正数，说明这行日志的形态与上式不符（比如 group_ratio 缺失），
		// 此时**不猜**：退回 n = 1，并由调用方在备注里标明张数无法确认。
		// 猜错方向的代价不对称——猜大了向客户多收钱，比少收难解释得多。
		n := 1.0
		if rowGroupRatio, ok := GroupRatioFromOther(other); ok {
			if est := quota / (modelPrice * QuotaPerCNY * rowGroupRatio); est >= 1 && math.Abs(est-math.Round(est)) < 1e-6 {
				n = math.Round(est)
			} else {
				r.PerCallCountUncertain = true
			}
		} else {
			r.PerCallCountUncertain = true
		}
		listUSD = modelPrice * n
		r.ImagePerCallCount = n
		// 按次计费的固定美金价来自日志 model_price，是一份外部刊例。
		r.ListOrigin = ListOriginExternal
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
			// basePrice 为 nil 只表示「价表里查不到」，此时用阶梯表的低档价兜底展示。
			// 这里原本还判 basePrice.Source == "per_call"，但 ResolvePrice 已不再
			// 产出那个来源（按次与否改由日志的 model_price 决定），留着是死条件。
			if tier, ok := TieredModelPrices[model]; ok && basePrice == nil {
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
			r.SiteRatioList = true
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
		r.SiteRatioList = true
		// ratio 快照的换算基准是官方锚点（ratio=1 → $2/MTok），算外部对标价。
		r.ListOrigin = ListOriginExternal

	default:
		basePrice, _ := ResolvePrice(model, book, preferPriceTable, exchangeRate)
		// 同上：只按「查不到价」兜底。
		if tier, ok := TieredModelPrices[model]; ok && basePrice == nil {
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
