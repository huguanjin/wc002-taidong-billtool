package billing

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ModelPrice 单模型单价（已换算为 USD/MTok）。
type ModelPrice struct {
	InputPerM  float64
	OutputPerM float64
	Currency   string // USD | CNY
	Source     string
	Category   string
	Channel    string
}

// AggRow 按 (模型, 分组) 汇总的一行。
type AggRow struct {
	Model        string
	Group        string
	Uncached     float64
	CacheRead    float64
	Output       float64
	CacheWrite5m float64
	CacheWrite1h float64
	Quota        float64
	Rows         int
	// 按请求累计的官方美金刊例（含阶梯价与 web_search）
	OfficialUSD float64
	WebSearchCalls float64
	// 图片按次计费的调用次数；按量图片模型此字段为 0
	ImagePerCallCount float64
	BillingMode       string // token | per_call | tiered_expr
	// BillingExpr 该模型的阶梯计费表达式（来自日志 other.expr_b64 或 options 表）；
	// 非阶梯模型为空。
	BillingExpr string
	// ExprTiers 本账期内表达式命中的档位名集合（按首次命中顺序），
	// 用于在账单里说明这行的刊例由哪几档混合而成。
	ExprTiers []string
	// LastAt 本行最后一次请求发生的时刻（日志 created_at），为零值表示日志没有该列。
	// 折算表达式单价时必须用它，而不是出账时刻——deepseek-v4.1-flash 这类
	// 表达式带 hour("Asia/Shanghai") 峰谷倍率，用 now() 会得到与账期无关的单价。
	LastAt time.Time
	// KeyGroup 原始分组标识，用于账单展示（C 列）与价表折扣匹配。
	// 与 Group 的区别：Group 可能是「分组+倍率」的复合键（见 GroupRatio），
	// 而 KeyGroup 始终是日志里那个原始分组名。
	KeyGroup string
	// GroupRatio 本桶实际使用的分组倍率（日志 other.group_ratio）。
	//
	// 必须把它纳入聚合维度：同一个分组在账期内可能出现过多种倍率（换套餐、
	// 渠道切换、GroupGroupRatio 变更、共享分组被多个套餐使用），而站内结算额是
	// 「表达式美金 × 本次倍率」，用单一折扣无法把这组账算对。0 表示日志没给该字段。
	GroupRatio float64
	// ListOrigin 本行 OfficialUSD 的来源（见 ListOrigin* 常量）。
	// 只有外部对标价才能当折扣反推的分母；站内公式自算出来的数字反推不出商务折扣。
	ListOrigin ListOrigin
	// ExprUnitCurrency 表达式系数的计价币种："CNY" 表示该模型在站上按人民币报价
	// （1元=1美金的充值比例，系数 p*1 就是「每百万 1 元」），"USD" 表示系数本身即美金。
	//
	// 这个标记只影响单价列与 AC 列的显示换算：账单模板的单价列表头写的是
	// 「美金/百万token」，人民币系数原样填进去，客户按美金读会虚高 7 倍，
	// 且 AC 列公式（单价×用量）会连带把总金额放大约汇率倍。
	// 聚合口径（OfficialUSD 已折算成美金、×汇率还原人民币）与它无关。
	ExprUnitCurrency string
	// QuotaDelta 本桶内**额度调整行**的净额，单位与 Quota 相同（quota 计数）。
	//
	// 额度调整行指异步任务的结算与退款日志（other 里带 task_id）：任务提交时按预扣
	// 全额记一条消费日志，完成或失败后再补一条差额/退款日志。这些行**不是**新的消费，
	// 只是把预扣的额度调回真实值，所以：
	//   - 正数表示站点退还给用户（type=6 退款），净结算要减掉它；
	//   - 负数表示补扣（type=2 的 delta>0 结算行），净结算要加上它。
	//
	// 它们绝不能累加进 Quota / OfficialUSD / 各 token 列：刊例代表「这次请求本来就
	// 值多少钱」，退款不改变这个事实，改变的是最终结算了多少。混进刊例会连带污染
	// 折扣反推的分母（见 HasRatioDiscount 的说明）。
	QuotaDelta float64
	// HasQuotaAdjustment 本桶是否含额度调整行。
	// 含退款时 HasRatioDiscount 的恒等式不成立，该桶必须退出「按精确倍率结算」那条路。
	HasQuotaAdjustment bool
}

// ExprUnitDivisor 表达式单价列落成「美金/百万token」要除的数：
// 人民币计价的系数除汇率，美金计价的不除。汇率缺失时按 1 处理（等于不换算），
// 宁可显示原值也不要把行弄成 0 单价。
func (a *AggRow) ExprUnitDivisor(exchangeRate float64) float64 {
	if a.ExprUnitCurrency != "CNY" || exchangeRate <= 0 {
		return 1
	}
	return exchangeRate
}

// HasRatioDiscount 该行能否直接按「本次请求实际使用的分组倍率」结算。
//
// 只对**阶梯表达式计费**的行成立，因为站内的 quota 就是表达式算出来的：
//
//	quota = 表达式USD × GroupRatio × QuotaPerCNY
//	而 OfficialUSD = 表达式USD
//
// 于是 结算额 = OfficialUSD × 汇率 × (GroupRatio / 汇率) = quota / QuotaPerCNY，
// 与站内实收逐行严格相等。
//
// 外部对标价的行（官网价/价表/ratio 快照）不满足这个关系：它们的 GroupRatio
// 与「对标价」之间没有这种推导关系，硬套会把金额算错，所以返回 false 交回原逻辑。
//
// 桶内含额度调整（任务退款/补扣）时同样返回 false：那个恒等式的两边一边用 quota、
// 一边用 OfficialUSD，退款只会动 quota（净额变了而刊例没变），等式立刻不成立。
// 硬按倍率结算会让这一桶少收/多收恰好等于退款额的钱，而且看起来一切正常。
// 交回反推路径后，结算系数会由「净结算 / 刊例人民币」算出，账实重新相符。
func (a *AggRow) HasRatioDiscount() bool {
	if a.HasQuotaAdjustment {
		return false
	}
	return a.GroupRatio > 0 && a.BillingMode == BillingModeTieredExpr && a.BillingExpr != ""
}

// RatioDiscount 本次请求实际使用倍率对应的折扣：倍率 / DiscountBaseFactor。
// 与 group_ratio_source.md 的换算约定一致（倍率 1 对应折扣 1/7）。
func (a *AggRow) RatioDiscount() float64 {
	return a.GroupRatio / DiscountBaseFactor
}

// DisplayGroup 账单 C 列展示用的分组名。
//
// 同一分组下出现多种倍率时会被拆成多行，光看「Codex」两行会困惑，
// 因此把倍率缀在名字后面（Codex(0.4)）让客户能对上号。
// 倍率唯一时保持原样，账单外观与以前一致。
func (a *AggRow) DisplayGroup() string {
	if !a.HasRatioDiscount() {
		return a.KeyGroup
	}
	return fmt.Sprintf("%s(%s)", a.KeyGroup, trimRatio(a.GroupRatio))
}

// trimRatio 倍率转成展示文本：去掉多余的 0（0.4000 → 0.4，1.0000 → 1）。
func trimRatio(r float64) string {
	return strconv.FormatFloat(r, 'f', -1, 64)
}

// ListOrigin 标出官方刊例（AggRow.OfficialUSD）是从哪里来的。
//
// 这个区分是「能不能反推折扣」的唯一依据，比按厂商名猜国产/海外更可靠：
// 反推的分母必须是一份与站点自身定价无关的外部对标价，否则 Σ结算/Σ总金额
// 恢复出来的只是站点自己的 group_ratio，不是商务谈定的折扣。
type ListOrigin string

const (
	// ListOriginExternal 外部对标价：内置官网价、人工维护报价表、业务库实时价、
	// 阶梯绝对价、按次固定价，或日志自带的 ratio 快照。
	// 注意 ratio 快照虽由日志给出，换算基准（ratio=1 → $2/MTok）是官方锚点，
	// 不是站内自定的公式，因此算外部对标价。
	ListOriginExternal ListOrigin = "external"
	// ListOriginExpr 站内 billing_expr 表达式自算的刊例：站点自己的定价公式。
	// 拿它当分母反推，得到的是式子里隐含的 group_ratio，与商务折扣无关。
	ListOriginExpr ListOrigin = "expr"
	// ListOriginMixed 该 (模型,分组) 行内部混用了多种来源，分母口径不统一，不可反推。
	ListOriginMixed ListOrigin = "mixed"
	// ListOriginNone 一点价都取不到。
	ListOriginNone ListOrigin = "none"
)

// mergesListOrigin 把新出现的来源并入桶上已记录的来源：
// 只有整桶来源一致且为外部对标价时，这一桶的分母口径才可信。
func mergesListOrigin(current, incoming ListOrigin) ListOrigin {
	if current == "" {
		return incoming
	}
	if current == incoming {
		return current
	}
	return ListOriginMixed
}

// SiteCNY 站点人民币净结算额。
//
// 站点实收 = Σ(消费行 quota) − Σ(退款) + Σ(补扣)，见 QuotaDelta 的符号约定。
// 没有额度调整行时就是 Quota / 500000，与改动前逐位一致。
//
// 刻意不加 Max(0, ...)：某期退款多于消费时净额就是负的，夹到 0 会把差异藏起来，
// 而「账面上看起来正常、实际少了钱」正是这次要消灭的失败模式。
func (a *AggRow) SiteCNY() float64 {
	return (a.Quota - a.QuotaDelta) / QuotaPerCNY
}

// PriceBook 报价表加载结果。
type PriceBook struct {
	ByModel   map[string]ModelPrice
	Discounts map[string]float64
}

func NewPriceBook() *PriceBook {
	return &PriceBook{ByModel: map[string]ModelPrice{}, Discounts: map[string]float64{}}
}

// PriceSource 模型单价来源：内置官方价 / 人工维护报价表(xlsx) / 业务数据库实时。
type PriceSource string

// 出账模板标识。
const (
	// BillTemplateStandard 模板一：29 列明细账单。空值等同于此。
	BillTemplateStandard = "standard"
	// BillTemplateSimple 模板二：按 (分组, 模型) 汇总的简易账单，金额 = 额度 / 500000。
	BillTemplateSimple = "simple"
)

// IsSimpleBillTemplate 是否走简易账单模板。空值按标准模板处理。
func IsSimpleBillTemplate(t string) bool {
	return strings.TrimSpace(t) == BillTemplateSimple
}
const (
	PriceSourceOfficial   PriceSource = "official"
	PriceSourcePriceTable PriceSource = "price_table"
	PriceSourceDB         PriceSource = "db"
)

// ManualPriceInput 用户在「检查模型价格覆盖」环节手动补全的单模型单价（USD/MTok）。
type ManualPriceInput struct {
	InputPerM  float64 `json:"inputPerM"`
	OutputPerM float64 `json:"outputPerM"`
}

// Params 一次出账请求的参数，对应 log_to_bill.py 的命令行参数。
type Params struct {
	// BillTemplate 出账模板：空 / BillTemplateStandard = 模板一（29 列明细，
	// 刊例 × 折扣、带脱敏日志与成本利润表）；BillTemplateSimple = 模板二
	// （按分组+模型的汇总，金额直接取 quota 折算，不参与定价）。
	//
	// 留空即模板一是刻意的：老的计划任务与前端缓存里都没有这个字段，
	// 默认值必须是「与改动前完全一致」的那一个。
	BillTemplate string

	Month             int     // 0 表示未指定，从日志推断
	Year              int     // 0 表示未指定，从日志推断
	Discount          *float64 // nil 表示不强制，按分组自动反推
	// ManualDiscounts 按「客户 + 分组」手工维护的折扣：键是日志里的原始分组名
	// （AggRow.KeyGroup，如 Codex），值是该分组的结算折扣。
	//
	// 由调用方在出账前从本地 PG 读好传入（见 CustomerGroupDiscountMap），
	// 与全局的 Discount 一起合成 DiscountOverrides。为空表示没有手工折扣，
	// 一切照旧走反推——手动上传日志那条路径没有客户概念，就留空。
	ManualDiscounts map[string]float64
	ExchangeRate      float64
	KeepLog           bool
	SanitizedLog      bool
	SanitizedFormat   string // "xlsx"（默认，为空时等同）| "csv" | "tsv"
	PriceSource       PriceSource // 空值等同 PriceSourceOfficial
	Sheet             string
	Encoding          string
	// ManualPrices 缺失定价模型的手动补全价格，键为日志里的原始模型名，
	// 生成账单时会合并进 PriceBook.ByModel（Source: "manual_override"）。
	ManualPrices map[string]ManualPriceInput
	// IncludeBillingParams 控制脱敏日志是否附带站点内部计费参数列（见 SanitizedBillingColumns）。
	// 默认 false：这些字段暴露内部定价倍率，是否对客户可见属于商务决定。
	IncludeBillingParams bool
	// GenerateCost 是否额外生成一张成本利润表（账单全部列 + 渠道/上游折扣/上游成本）。
	// 需要日志含 channel_id 列，且日志用到的渠道都已维护上游倍率。
	GenerateCost bool
	// ChannelUpstreamRatios 渠道 ID → 上游倍率。由 handler 从本地 PG 读好传入，
	// billing 包不直接连 PG——保持「读配置」与「算账」分离，也便于测试注入。
	ChannelUpstreamRatios map[int]float64
	// ChannelNames 渠道 ID → 渠道名称（来自本地渠道清单快照）。
	ChannelNames map[int]string
	// ChannelInfos 渠道 ID → 渠道信息，用于成本估算前的倍率检查。
	ChannelInfos map[int]ChannelInfo

	// CustomerName 客户名，只用于给产物文件名加后缀。
	//
	// 产物都落在同一个 job 目录（下载用），但用户会同时下载好几个客户的账单，
	// 落到本地 Downloads 里全叫「账单_xxx.xlsx」，分不清谁是谁。
	// 手动上传日志那条路径没有客户概念，留空即可——为空时文件名与原来完全一致。
	CustomerName string

	// SummaryHeader 成本利润摘要开头的定位行（客户、账号、时段），由调用方拼好后传入。
	// billing 包不认识「客户」这个概念，也不知道时段是从哪来的，所以这里只负责原样印出去。
	// 为空表示不写——手动上传日志那条路径没有这些信息。
	SummaryHeader []string

	// DomesticMarkers 人工标记的国产/站内定价标识：一条一个，可以是分组标识
	// （精确匹配，如「国产模型」），也可以是模型名前缀（前缀匹配，如「doubao」）。
	// 命中的 (模型,分组) 不参与折扣反推——模型名推厂商家族覆盖不全，识别不到时
	// 会静默按海外处理、折扣悄悄算错，这里给用户一个显式兜底。
	DomesticMarkers []string
}

// RowDetails 脱敏日志需要额外展开的单行明细，全部来自日志 other 字段
// 或聚合循环已算出的计费口径值。指针字段为 nil 表示「该日志没有这个字段」，
// 写出时留空，不要写成 0；UncachedInputTokens / WebSearchCalls 是计费口径值，
// 与现有 4 个缓存列行为一致，始终写数字（含 0）。
type RowDetails struct {
	UncachedInputTokens float64
	UsageSemantic       string
	InputTokensTotal    *float64
	CacheWriteTokens    *float64
	TextInput           *float64
	TextOutput          *float64
	AudioInput          *float64
	AudioOutput         *float64
	ImageOutput         *float64
	ReasoningTokens     *float64
	WebSearchCalls      float64
	ToolSurcharges      string
	// Billing 仅当 Params.IncludeBillingParams 为真时才会被填充。
	Billing BillingDetails
}

// BillingDetails 站点内部计费参数（可选输出，默认关闭，见 SanitizedBillingColumns）。
type BillingDetails struct {
	ModelRatio           *float64
	CompletionRatio      *float64
	GroupRatio           *float64
	UserGroupRatio       *float64
	CacheRatio           *float64
	CacheCreationRatio   *float64
	CacheCreationRatio5m *float64
	CacheCreationRatio1h *float64
	ModelPrice           *float64
	BillingMode          string
	MatchedTier          string
	PreConsumedQuota     *float64
	ActualQuota          *float64
}

// Summary 返回给前端展示的结果摘要。
type Summary struct {
	Year               int              `json:"year"`
	Month              int              `json:"month"`
	Rows               []RowSummary     `json:"rows"`
	SettleCNYTotal     float64          `json:"settleCnyTotal"`
	ListCNYTotal       float64          `json:"listCnyTotal"`
	OverallDiscount    float64          `json:"overallDiscount"`
	MissingPriceModels []string         `json:"missingPriceModels"`
	RowCount           int              `json:"rowCount"`
	CacheHitRows       int              `json:"cacheHitRows"`
	WebSearchRows      int              `json:"webSearchRows"`
	// CostTotals 成本利润表的合计，仅当成本利润表**成功生成**时非 nil。
	// 由后端算好而不是让前端去解析 xlsx：金额口径必须与表内公式（成本=AC×AF×汇率）
	// 完全一致，两边各算一份必然随时间漂移。
	CostTotals *CostTotals `json:"costTotals,omitempty"`
}

// CostTotals 成本利润表的三个合计数，供结果区展示与复制。
//
// 注意 CostCNY 只累加**已维护倍率**的行：未维护的渠道按 nil 跳过，
// 所以 PricedRows 可能小于总行数，此时利润是「已覆盖部分」的利润而非全量。
// 前端必须把这个区别说出来，否则会被读成整体毛利。
type CostTotals struct {
	// SettleCNY 结算额合计（对应成本利润表 V 列合计）。
	SettleCNY float64 `json:"settleCny"`
	// CostCNY 上游成本合计（对应 AG 列合计，未维护倍率的行不计入）。
	CostCNY float64 `json:"costCny"`
	// ProfitCNY 利润合计 = SettleCNY − CostCNY。
	ProfitCNY float64 `json:"profitCny"`
	// PricedRows 参与了成本合计的行数；TotalRows 是成本利润表的全部行数。
	PricedRows int `json:"pricedRows"`
	TotalRows  int `json:"totalRows"`
	// ChannelCount 成本利润表里覆盖到的渠道数（去重）。
	ChannelCount int `json:"channelCount"`
	// RateCNYPerUSD 算出上列人民币金额时用的汇率（人民币/美金）。
	//
	// 有的客户按美金结算，摘要要同时给出美金金额；而换算用的汇率必须跟着一起写出来——
	// 单给一个美金数字，收件人无法判断它是对着 7.0 还是 7.3 算的，对账时就会吵架。
	//
	// 字段名不叫 exchangeRate：设置里那个 exchangeRate 是**当前默认值**，
	// 这个是**本次出账实际用的值**（历史任务用的是当时的设置，两者会不一样）。
	RateCNYPerUSD float64 `json:"rateCnyPerUsd"`
}

// RowSummary 单个 (模型, 分组) 汇总行，供前端表格展示。
type RowSummary struct {
	Model        string  `json:"model"`
	Group        string  `json:"group"`
	Uncached     float64 `json:"uncached"`
	CacheRead    float64 `json:"cacheRead"`
	Output       float64 `json:"output"`
	CacheWrite5m float64 `json:"cacheWrite5m"`
	CacheWrite1h float64 `json:"cacheWrite1h"`
	Quota        float64 `json:"quota"`
	SettleCNY    float64 `json:"settleCny"`
	ListCNY      float64 `json:"listCny"`
	// Discount 展示用折扣（已按 DiscountDecimals 取整）。
	Discount float64 `json:"discount"`
	// SettleFactor 结算用系数：按倍率结算与反推结算时它是**精确值**，
	// 不是 Discount 的取整值。SettleCNY == round(ListCNY × SettleFactor, 4)，
	// 用 Discount 去乘只能得到近似值（差额来自显示精度，不是业务差异）。
	// 页面/账单要复核金额时应该乘这个。
	SettleFactor float64 `json:"settleFactor"`
	Rows         int     `json:"rows"`
	HasPrice     bool    `json:"hasPrice"`
}
