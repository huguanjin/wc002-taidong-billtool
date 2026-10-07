package billing

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// 账单模板二：按 (分组, 模型) 汇总的简易账单。
//
// 面向「只要每个分组每个模型调用了多少次、花了多少额度」的客户。与模板一的区别是
// **完全不参与定价**：不算刊例、不查价表、不反推折扣、不估成本，金额就是站点实收
// 额度按 QuotaPerCNY 折算。
//
// 这带来一个实际好处：没有价表（price_table.xlsx / db_price_cache.json）的部署也能出这张表。
// 代价是它无法回答「单价×用量是否成立」「折扣是多少」这类问题——需要那些的客户用模板一。

// SimpleBillRow 模板二的一行：一个 (分组, 模型) 的汇总。
//
// 字段与手工核对用的 SQL 逐列对应，便于客户拿 SQL 结果与本表逐格比对：
//
//	SELECT `group`, model_name, COUNT(*) AS hit_count,
//	       SUM(prompt_tokens), SUM(completion_tokens), SUM(quota), ROUND(SUM(quota)/500000, 4)
//	FROM logs WHERE type = 2 ... GROUP BY `group`, model_name
//
// 与那条 SQL 唯一的差别是**退款**：本表把 type=6 的退款冲抵掉了（见 AggregateSimpleBill），
// 而示例 SQL 只取 type=2，在有任务退款的账期上会比实收偏高。
type SimpleBillRow struct {
	Group string `json:"group"`
	Model string `json:"model"`
	// HitCount 请求次数（只数消费行；任务的结算/退款行不算一次请求）。
	HitCount int `json:"hitCount"`
	// TotalPrompt / TotalCompletion 输入、输出 token 合计。
	TotalPrompt     float64 `json:"totalPrompt"`
	TotalCompletion float64 `json:"totalCompletion"`
	// TotalCacheRead / TotalCacheCreation 缓存读与缓存创建的 token 合计，取自日志 other
	// （cache_tokens、cache_creation_tokens[_5m/_1h]，见 rowCacheTokens）。
	//
	// 分成两列而不是合成一列：客户拿这两列各自乘自己的单价核对金额——缓存读比常规输入便宜得多、
	// 缓存创建反而更贵，合成一个数就没法核了。
	TotalCacheRead     float64 `json:"totalCacheRead"`
	TotalCacheCreation float64 `json:"totalCacheCreation"`
	// TotalQuota 额度合计，**净额**（消费 − 退款 + 补扣）。
	TotalQuota float64 `json:"totalQuota"`
	// TotalCostCNY 金额 = 额度 / QuotaPerCNY，即站点实收。
	TotalCostCNY float64 `json:"totalCostCny"`

	// ---- 成本核算。三项要么都有值、要么整体为 nil（见 SimpleBillRow.CostPartial）。----

	// OfficialListUSD 官方刊例（美金口径）＝ Σ(quota ÷ group_ratio) ÷ QuotaPerCNY。
	//
	// 反推而非查表：站内的 quota 就是「官方刊例 × 分组倍率 × QuotaPerCNY」算出来的，
	// 所以除回去就得到刊例。这条路径不需要价表，与模板二不依赖定价数据的定位一致。
	//
	// 国模渠道的行先 ÷ 汇率再累加：那些模型在站上按人民币报价，反推出来的本是人民币，
	// 归一成美金口径后这一列才是同一个币种（与模板一对国产模型的处理一致）。
	OfficialListUSD *float64 `json:"officialListUsd"`
	// UpstreamCostCNY 上游成本 ＝ 官方刊例USD × 汇率 × 上游折扣。
	//
	// 与成本利润表同一公式（见 CostRow.UpstreamCostCNY）。上游折扣怎么从倍率换算
	// 取决于渠道是否国模渠道，见 UpstreamDiscountFor。
	UpstreamCostCNY *float64 `json:"upstreamCostCny"`
	// ProfitCNY 利润 ＝ 金额 − 上游成本。
	ProfitCNY *float64 `json:"profitCny"`
	// CostPartial 为真表示上面三项只覆盖了一部分行（有行算不出成本）。
	//
	// 与「整行为空」是两种不同的状态，必须分开：整行为空是「没法算」，
	// CostPartial 是「算了，但只算了 99.9%」。前者不能给数，后者要给数并说明。
	//
	// 从前这里只有「全有或全无」两态：504100 行里有 392 行算不出来，
	// 整张表的成本列就全空。那个取舍是错的——用 99.9% 的行算出来的成本
	// 远比一片空白有用，只要把覆盖率说清楚就行（用户可以自己判断够不够用）。
	CostPartial bool `json:"costPartial"`
	// CostRows 参与了成本核算的行数；TotalRows 是该汇总行覆盖的全部消费行数。
	CostRows  int `json:"costRows"`
	TotalRows int `json:"totalRows"`
	// SkippedQuota 没算进成本的那部分净额度。用户据此判断这点缺口要不要紧：
	// 少 392 行里如果只差几块钱，成本数就是可用的；差一大截才需要去补。
	SkippedQuota float64 `json:"skippedQuota"`
	// SkipReasons 算不出成本的行数按原因分类（键见 CostSkipReason）。
	//
	// 分类而不是一个总数：缺倍率要补、缺渠道号要查日志，去处完全不同。
	SkipReasons map[string]int `json:"skipReasons,omitempty"`
}

// SimpleBillColumns 模板二**汇总表**的列名：账单与成本表的前九列都用它。
//
// 注意配套的脱敏日志**不是**这张表：它是逐行明细（见 WriteSimpleSanitizedLog），
// 列的加工口径与模板一一致。汇总留在账单里，明细才是脱敏日志该有的样子——
// 早先这里图省事把汇总表当脱敏日志写出去，结果两个文件内容一模一样，
// 客户拿它核不了任何一笔账。
//
// 成本三列（官方刊例 / 上游成本 / 利润）不在客户版里，它们只出现在独立的成本表
// （见 SimpleBillCostColumns）。这是有意的边界：
//
//	客户拿到的任何文件里都不该有我们的采购价与单笔毛利。
//
// 起初这三列是加在账单上的，靠「发出去之前自己删列」来兜——但那要求人永远不忘、
// 且逐列看清删对了，风险与收益不对等。挪进单独的成本表后，客户版文件里
// **根本不存在**这些列，不需要靠自觉。
var SimpleBillColumns = []string{
	"分组", "模型", "次数", "输入Token", "输出Token", "缓存读Token", "缓存创建Token",
	"额度", "金额（人民币）",
}

// SimpleBillCostColumns 成本表的列名：客户版诸列 + 成本三列。
var SimpleBillCostColumns = []string{
	"分组", "模型", "次数", "输入Token", "输出Token", "缓存读Token", "缓存创建Token",
	"额度", "金额（人民币）",
	"官方刊例（美金）", "上游成本（人民币）", "利润（人民币）",
}

// SimpleBillOptions 模板二的算法开关与运行时输入。
//
// 用一个结构体而不是往参数表上再加三个参数：可选项会随需求继续长，
// 而调用点只有一处——多一个参数就要改一次签名，结构体则只用加字段。
type SimpleBillOptions struct {
	// UpstreamRatios 渠道 ID → 上游倍率。由调用方（handler）从本地 PG 读好传入，
	// billing 包不连 PG——与 Params.ChannelUpstreamRatios 同一个约定。
	// 为空表示一条倍率都没有（首次部署、还没拉过渠道清单）。
	UpstreamRatios map[int]float64
	// DomesticChannels 渠道 ID → 是否国模渠道（只含标了的），与 UpstreamRatios 成对使用。
	// nil 表示一个都没标，全按海外口径算。换算规则见 UpstreamDiscountFor。
	DomesticChannels map[int]bool
	// CostColumns 是否写成本三列。
	//
	// 关掉时三列**仍然存在**但整列为空：列集合是固定的十列（账单与脱敏日志必须同构，
	// 见 WriteSimpleBill），所以关闭只意味着不填，不意味着少三列。
	CostColumns bool
	// KnownChannels 本地渠道清单里存在的渠道号集合。
	//
	// 用来区分两种缺失：清单里有的渠道补一下倍率就能算成本（值得提示用户去补），
	// 清单里没有的（业务库已硬删除）补不了，只能如实说明。两者混在一起会让
	// 用户去清单里找一个根本不存在的渠道。
	KnownChannels map[int]bool
	// ExchangeRate 人民币/美金汇率，用于把反推出来的刊例（美金）换回人民币去算成本。
	//
	// <= 0 时用 DefaultExchangeRate。这里必须与出账用的汇率一致：
	// 模板一的成本也是「刊例USD × 汇率 × 上游折扣」，两边用不同的汇率，
	// 同一份日志的两张表会给出两个成本数。
	ExchangeRate float64
}

// Rate 取汇率，未设置时回退默认值。导出是因为调用方记录「本次实际用的汇率」
// 时也要用同一个值（见 service.go 里写 CostTotals.RateCNYPerUSD），
// 各算各的会出现「账单按 7 算、摘要里却写着 0」。
func (o SimpleBillOptions) Rate() float64 { return o.rateOr() }

// rateOr 取汇率，未设置时回退默认值。
func (o SimpleBillOptions) rateOr() float64 {
	if o.ExchangeRate > 0 {
		return o.ExchangeRate
	}
	return DefaultExchangeRate
}

// WriteSimpleSanitizedLog 为模板二写逐行明细的脱敏日志。
//
// **它与账单不是一张表**：账单是 (分组, 模型) 的汇总，脱敏日志是 logs 表里的
// 逐条明细（一行一次请求）。
//
// 从前这里图省事，把汇总表当成脱敏日志写出去，结果是「脱敏日志」与「账单」
// 内容一模一样——那根本不是脱敏日志：客户拿它核不了任何一笔账，
// 也看不到自己每次请求的用量。汇总留在账单里，明细才是脱敏日志该有的样子。
//
// 复用模板一的那套写出器（SanitizedWriter），所以列的加工口径完全一致：
// other 整列丢弃、缓存列展开成四列、明细列从 other 里提出来。
// 这是刻意的——两套模板给客户的明细格式不该长得不一样。
func WriteSimpleSanitizedLog(path string, headers []string, rows [][]string,
	format string, includeBilling bool) error {

	ext, delimiter, isDelimited := sanitizedFormatInfo(format)
	_ = ext

	var w SanitizedWriter
	var err error
	if isDelimited {
		w, err = NewCSVSanitizedWriter(path, headers, delimiter, includeBilling)
	} else {
		w, err = NewExcelSanitizedWriter(path, headers, includeBilling)
	}
	if err != nil {
		return fmt.Errorf("初始化脱敏日志写出失败: %w", err)
	}

	writeErr := streamSimpleSanitizedRows(w, headers, rows, includeBilling)
	// Close 必须调用：Excel 写出器是流式的，不关就写不出完整的文件——
	// 而且不关的话文件可能是半截的，客户拿到会打不开。
	if closeErr := w.Close(); closeErr != nil && writeErr == nil {
		writeErr = fmt.Errorf("写出脱敏日志失败: %w", closeErr)
	}
	return writeErr
}

// streamSimpleSanitizedRows 逐行展开缓存与明细列并写出。
//
// 与模板一的 AggregateFromRows 里那段写出逻辑同源（同样的取缓存列规则、
// 同样的退款行拦截），但**不跑计价**：模板二本来就不参与定价，
// 拉一本价表进来只为写明细，会在没有价表的部署上直接失败。
func streamSimpleSanitizedRows(w SanitizedRowWriter, headers []string, rows [][]string, includeBilling bool) error {
	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	idxOther, hasOther := col["other"]
	idxType, hasType := col["type"]
	idxPrompt, hasPrompt := col["prompt_tokens"]

	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		other := ""
		if hasOther {
			other = cellAt(row, idxOther)
		}

		// 退款/补扣行不写进客户版明细：那是站点与用户之间的额度往来，
		// 不是一次请求。与模板一同一处拦截，口径不能两样。
		if IsTaskQuotaAdjustment(other) {
			logType := ""
			if hasType {
				logType = cellAt(row, idxType)
			}
			if _, ok := QuotaAdjustmentDelta(logType, ToFloat(cellAt(row, col["quota"]))); ok {
				continue
			}
		}

		cacheRead, cacheWrite5m, cacheWrite1h := rowCacheTokens(row, col, other)

		details := ParseRowDetails(other, includeBilling)
		// 未命中输入量是个派生值（见 UncachedInputTokens），模板一由 priceRow 算出。
		// 这里不跑计价，就按同一个函数自己算——复用函数而不是复制算式，
		// 否则两套模板的「输入（未命中）」会算出不同的数。
		details.UncachedInputTokens = UncachedInputTokens(
			toFloatIdx(row, idxPrompt, hasPrompt), cacheRead, cacheWrite5m, cacheWrite1h, details.UsageSemantic)
		if err := w.WriteRow(row, cacheRead, cacheWrite5m, cacheWrite1h, details); err != nil {
			return err
		}
	}
	return nil
}

// toFloatIdx 取指定列的数字，列不存在时返回 0。
func toFloatIdx(row []string, idx int, ok bool) float64 {
	if !ok {
		return 0
	}
	return ToFloat(cellAt(row, idx))
}

// SimpleBillCostStat 成本覆盖情况，供调用方在结果里如实报出。
//
// 需要它是因为「部分行算不出成本」时成本是按能算的那部分算的：
// 页面若只显示一个利润数字，读的人会以为它是整体毛利。与成本利润表的
// CostTotals.PricedRows/TotalRows 同一个用意。
type SimpleBillCostStat struct {
	// Rows 参与成本核算的行数；TotalRows 是本表覆盖的全部消费行数。
	Rows      int
	TotalRows int
	// SkippedQuota 未计入成本的那部分净额度。
	SkippedQuota float64
	// SkipReasons 未计入成本的行数按原因分类。
	SkipReasons map[string]int
}

// Complete 全部行都参与了成本核算。
func (s SimpleBillCostStat) Complete() bool {
	return s.TotalRows > 0 && s.Rows == s.TotalRows
}

// SkippedRows 未计入成本的行数。
func (s SimpleBillCostStat) SkippedRows() int { return s.TotalRows - s.Rows }

// DescribeSkipReasons 把原因表拼成给人读的一句话，如
// 「392 行无渠道号、12 行缺上游倍率」。原因按固定顺序输出，保证文案稳定
// （map 遍历顺序随机，直接拼会让同一份结果显示成好几种样子）。
func DescribeSkipReasons(reasons map[string]int) string {
	if len(reasons) == 0 {
		return ""
	}
	order := []struct {
		key   CostSkipReason
		label string
	}{
		{SkipNoUpstreamRatio, "渠道未维护上游倍率"},
		{SkipNoChannel, "日志里取不到渠道号"},
		{SkipMultiChannel, "一行经多个渠道无法分摊"},
		{SkipNoGroupRatio, "缺分组倍率（group_ratio）"},
	}
	parts := []string{}
	for _, o := range order {
		if n := reasons[string(o.key)]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d 行%s", n, o.label))
		}
	}
	return strings.Join(parts, "、")
}

// AggregateSimpleBill 把日志行按 (分组, 模型) 汇总。
//
// 直接从原始行累加，不经过 AggRow：那是「带定价的桶」，而本模板不需要任何定价中间量。
// 分组取日志原值，不加 DisplayGroup() 那种倍率后缀——客户要与 SQL 结果对账，
// 后缀会让 `Codex` 变成 `Codex(0.4)`，反而对不上。
//
// 退款处理与主账单口径一致（见 IsTaskQuotaAdjustment）：任务的结算/退款行只改额度，
// 不累 token、不计次数。这一点在本模板上尤其要紧——它的金额就是额度本身，
// 若把任务失败已退还的预扣算进去，误差会全部落在账单上。
//
// # 成本列的反推口径
//
// 站内的 quota 是按 `官方刊例USD × group_ratio × QuotaPerCNY` 记的（ratio 行、
// 表达式行、按次行三条计费路径都满足这条恒等式），于是：
//
//	官方刊例USD = quota ÷ group_ratio ÷ QuotaPerCNY
//	上游成本CNY = 官方刊例USD × 汇率 × 上游折扣
//	上游折扣    = 上游倍率 ÷ DiscountBaseFactor        （默认，海外渠道）
//	            = 上游倍率                              （国模渠道，见 UpstreamDiscountFor）
//
// 国模渠道的模型在站上按人民币报价，反推出来的「刊例」本来就是人民币，
// 所以先除一次汇率归一成美金口径（见循环里的说明），算成本时再乘回来。
//
// 所以成本列**不需要价表**，也就不破坏本模板「不参与定价」的定位——
// 它是把已经记在额度里的信息除回去，不是重新算一遍价。
//
// 两条路径各自都要能算出来才写：
//   - group_ratio 取不到 → 刊例反推不出来（按 0 算会得到无穷大），整行留空
//   - 渠道号取不到、或该渠道没维护上游倍率 → 成本与利润留空，只写刊例
//
// 一个 (分组, 模型) 可能横跨多个渠道，所以成本是**逐行**按该行自己的渠道算完再汇总，
// 不能拿汇总行去查某个渠道的倍率——那会挑中一个渠道代表整行。只要有一行算不出来，
// 这个汇总行的官方刊例（要的输入更少）照写，成本与利润则整体留空：
// 报一个「部分渠道算了、部分没算」的成本比不报更危险，它看着像完整的。
func AggregateSimpleBill(rows [][]string, headers []string, opts SimpleBillOptions) ([]SimpleBillRow, error) {
	col := map[string]int{}
	for i, h := range headers {
		if h != "" {
			col[h] = i
		}
	}
	required := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota"}
	var missing []string
	for _, name := range required {
		if _, ok := col[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("日志缺少列: %v；实际列: %v（简易账单需要 model_name/group/prompt_tokens/completion_tokens/quota）", missing, headers)
	}

	idxModel := col["model_name"]
	idxGroup := col["group"]
	idxPrompt := col["prompt_tokens"]
	idxCompletion := col["completion_tokens"]
	idxQuota := col["quota"]
	idxOther, hasOther := col["other"]
	idxType, hasType := col["type"]

	// 渠道号的两个来源，与 ExtractChannelUsage 同一套回退（见 rowChannelIDs）。
	idxChannel, hasChannel := col["channel_id"]

	type key struct{ group, model string }
	buckets := map[key]*SimpleBillRow{}
	var order []key

	// 逐桶的成本累加器：行级算完再加进来，全部是「按这个桶里每行自己的倍率算出来的」。
	// 与 SimpleBillRow 上的展示字段分开存，是因为三者要分别判断「算没算全」。
	type costAcc struct {
		// skippedQuota 未能计入成本的净额度，以及按原因分类的行数。
		skippedQuota float64
		skipReasons  map[string]int
		// officialQuota 是反推出来的官方刊例，单位与 quota 相同（不是人民币）。
		//
		// 这是整个反推里最容易搞错单位的一处，所以名字写成 -Quota 而不是 -CNY：
		// delta ÷ group_ratio 去掉的是分组倍率这一个因子，剩下的仍是 quota 量纲
		// （quota = 刊例USD × 倍率 × QuotaPerCNY），要得到人民币还得再除 QuotaPerCNY。
		// 一旦把它当成人民币，成本会整体大 50 万倍——那种错在页面上只表现为
		// 「利润是个很大的负数」，很难一眼看出来是单位问题。
		officialQuota float64
		// upstreamCNY 是上游成本合计（人民币），逐行按该行自己的渠道倍率算完再加进来。
		// 与 officialQuota 不同量纲——这里已经是人民币，可直接与 TotalCostCNY 相减得利润。
		//
		// 必须逐行算：一个 (分组, 模型) 横跨多个渠道时各渠道倍率不同，
		// 用「汇总刊例 × 某一个渠道的倍率」会整体偏掉，而表面上完全看不出来。
		upstreamCNY float64
		rows        int // 参与成本核算的行数

	}
	costs := map[key]*costAcc{}
	rate := opts.rateOr()

	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		model := strings.TrimSpace(cellAt(row, idxModel))
		group := strings.TrimSpace(cellAt(row, idxGroup))
		if model == "" && group == "" {
			continue
		}
		quota := ToFloat(cellAt(row, idxQuota))
		other := ""
		if hasOther {
			other = cellAt(row, idxOther)
		}

		k := key{group, model}
		r, exists := buckets[k]
		if !exists {
			r = &SimpleBillRow{Group: group, Model: model}
			buckets[k] = r
			order = append(order, k)
		}
		r.TotalRows++
		if _, ok := costs[k]; !ok {
			costs[k] = &costAcc{}
		}
		ca := costs[k]

		// 这一行的净额度增量。退款行拿它去**减**，
		// 与 AggRow.SiteCNY 同一符号约定；消费行就是它本身。
		delta := quota

		if IsTaskQuotaAdjustment(other) {
			// 任务的结算/退款行：只调整额度，不累 token、不计次数、不算一次请求。
			logType := ""
			if hasType {
				logType = cellAt(row, idxType)
			}
			d, ok := QuotaAdjustmentDelta(logType, quota)
			if !ok {
				// 带 task_id 但既不是 2 也不是 6：认不出来就不动额度，
				// 免得按错误的符号把它加进去。
				continue
			}
			delta = -d
			r.TotalQuota += delta
		} else {
			r.HitCount++
			r.TotalPrompt += ToFloat(cellAt(row, idxPrompt))
			r.TotalCompletion += ToFloat(cellAt(row, idxCompletion))
			r.TotalQuota += quota

			// 缓存两列也只在消费行累加：退款行不是一次请求，它的缓存量是 0，
			// 但万一日志里记了值，累进去会让缓存数比实际调用量还大。
			cacheRead, cacheWrite5m, cacheWrite1h := rowCacheTokens(row, col, other)
			r.TotalCacheRead += cacheRead
			r.TotalCacheCreation += cacheWrite5m + cacheWrite1h
		}

		if !opts.CostColumns {
			continue
		}

		// 退款行也一起算：它冲抵的是某一行的额度，那一行的正负号变了，
		// 挂在它上面的刊例与成本按同一比例跟着变才是对的。跳过退款行会让
		// 成本与金额对不上——金额冲抵了、成本没冲抵，利润凭空变高。
		// 判据与预检共用 RowCostReason：两边各写一套的话，会出现
		// 「预检说没问题、账单说 392 行缺倍率」这种自相矛盾（那是修这个 bug 的起因）。
		reason, ids := RowCostReason(row, idxChannel, hasChannel, idxOther, hasOther,
			opts.UpstreamRatios, delta)
		if reason != SkipNone {
			// zero_delta 不计入缺失：额度为 0 的行本来就不影响成本，
			// 把它算进去会让用户去补一堆无关的倍率。
			if reason != SkipZeroDelta {
				ca.skippedQuota += delta
				if ca.skipReasons == nil {
					ca.skipReasons = map[string]int{}
				}
				ca.skipReasons[string(reason)]++
			}
			continue
		}

		costRatio, ok := GroupRatioFromOther(other)
		if !ok || costRatio <= 0 {
			// RowCostReason 只判渠道与倍率，group_ratio 在它之后判：
			// 反推刊例需要 group_ratio，没有它就把这一行算作缺分组倍率。
			ca.skippedQuota += delta
			if ca.skipReasons == nil {
				ca.skipReasons = map[string]int{}
			}
			ca.skipReasons[string(SkipNoGroupRatio)]++
			continue
		}
		upstream := opts.UpstreamRatios[ids[0]]
		domestic := opts.DomesticChannels[ids[0]]
		ca.rows++

		// 反推出来的刊例，仍是 quota 量纲（分组倍率这一个因子已除掉，见 officialQuota 的说明）。
		// 净额口径与 TotalQuota 一致（退款行为负），否则同一条退款在
		// 金额列与成本列上冲抵的方向会相反。
		listQuota := delta / costRatio
		if domestic {
			// 国模渠道承接的是站上按人民币报价的国产模型（1 元 = 1 美金充值），
			// 所以反推出来的「刊例」本来就是人民币。除回汇率，归一成美金口径：
			//   - 官方刊例（美金）这一列才不会把人民币与美金直接相加；
			//   - 与模板一 priceRow 对国产模型的处理同口径（OfficialUSD = 人民币刊例 ÷ 汇率），
			//     两张表同一渠道的刊例才对得上。
			// 模板二没有价表可查模型的币种，只能由渠道标识来告知。
			listQuota /= rate
		}
		ca.officialQuota += listQuota
		// 成本逐行算再累加，而不是「汇总刊例 × 某个倍率」：
		// 一个 (分组, 模型) 横跨多个渠道时，各渠道倍率不同，只有逐行加权才对。
		//
		// 单位换算走几步，与 CostRow.UpstreamCostCNY 完全一致：
		//   ÷ QuotaPerCNY 把 quota 量纲换成刊例美金（见上面的恒等式）
		//   × 汇率         换成人民币
		//   × 上游折扣     见 UpstreamDiscountFor（海外渠道 倍率/7，国模渠道 倍率本身）
		//
		// 国模渠道的式子是 (刊例人民币 ÷ 汇率) × 汇率 × 倍率 = 刊例人民币 × 倍率，
		// 汇率在里面一进一出，成本数与汇率无关（模板一的 OfficialListCNY × 折扣同理）。
		//
		// 升级影响（只在汇率恰好等于 DiscountBaseFactor=7 时成立）：
		// 旧口径对所有渠道都是「刊例 × 汇率 × 倍率 ÷ 7」，汇率为 7 时国模渠道的成本数
		// 碰巧与新口径相同，变的只有官方刊例那一列的币种（原先把人民币当美金写，现在归一了）。
		// 汇率不是 7 时旧口径的国模成本会偏 汇率/7 倍，新口径不再有这个偏差。
		ca.upstreamCNY += listQuota / QuotaPerCNY * rate * UpstreamDiscountFor(upstream, domestic)
	}

	out := make([]SimpleBillRow, 0, len(buckets))
	for _, k := range order {
		r := buckets[k]
		// 金额保留 4 位：与手工 SQL 的 ROUND(..., 4) 一致，客户逐格比对时不会差在精度上。
		r.TotalCostCNY = round(r.TotalQuota/QuotaPerCNY, MoneyDecimals)

		if opts.CostColumns {
			ca := costs[k]
			r.CostRows = ca.rows
			r.SkippedQuota = round(ca.skippedQuota, MoneyDecimals)
			r.SkipReasons = ca.skipReasons
			// 覆盖率不足但**不是零**时照样给数：504100 行里 392 行算不出来，
			// 拿剩下的 503708 行算出来的成本远比一片空白有用。把 CostPartial
			// 标出来，页面上说明「未覆盖 N 行、差 ¥X」，让用户自己判断够不够用。
			//
			// 一行都没算出来时（ca.rows == 0，比如渠道全都没维护倍率）才留空：
			// 那时没有任何依据可以外推，报 0 会被读成「上游免费」。
			if ca.rows > 0 {
				// 三项都 round 到金额精度：客户会拿计算器逐格复核，
				// 显示 4 位而内部多留几位，会让他手算的结果与表里差最后一位。
				officialUSD := round(ca.officialQuota/QuotaPerCNY, MoneyDecimals)
				upstreamCNY := round(ca.upstreamCNY, MoneyDecimals)
				profit := round(r.TotalCostCNY-upstreamCNY, MoneyDecimals)
				r.OfficialListUSD = &officialUSD
				r.UpstreamCostCNY = &upstreamCNY
				r.ProfitCNY = &profit
				r.CostPartial = ca.rows < r.TotalRows
			}
		}
		out = append(out, *r)
	}

	// 分组按首次出现顺序（与日志里各分组的自然顺序一致），组内按次数降序——
	// 与示例 SQL 的 ORDER BY `group`, hit_count DESC 对齐，客户对账时行序不用重新找。
	groupRank := map[string]int{}
	for i, k := range order {
		if _, seen := groupRank[k.group]; !seen {
			groupRank[k.group] = i
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return groupRank[out[i].Group] < groupRank[out[j].Group]
		}
		if out[i].HitCount != out[j].HitCount {
			return out[i].HitCount > out[j].HitCount
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// rowUpstreamRatio 取这一行适用的上游倍率。
//
// 只支持「一行一个渠道」这一种能唯一确定倍率的情形：
//
//   - 一个渠道且维护了倍率 → 用它
//   - 一个渠道但没维护倍率 → false（成本留空，不是按 1 算）
//   - 两个及以上渠道 → false。这类行是「一次请求经多个渠道」，
//     日志没说清额度怎么分摊到各渠道上，凭猜分摊会得出一个看似精确的成本，
//     而实际任何分摊方式都站得住——那种数比留空更坏。
//
// 倍率为 0 视为**未维护**而不是「上游免费」：0 值只可能来自默认零值，
// 而不是真的谈了个 0 折，按它算成本会得到 0 元成本与等于金额的利润。
func rowUpstreamRatio(row []string, idxChannel int, hasChannelCol bool,
	idxOther int, hasOtherCol bool, ratios map[int]float64) (float64, bool) {

	ids := rowChannelIDs(row, idxChannel, hasChannelCol, idxOther, hasOtherCol)
	if len(ids) != 1 {
		return 0, false
	}
	r, ok := ratios[ids[0]]
	if !ok || r < 0 {
		return 0, false
	}
	return r, true
}

// SimpleBillPeriod 从日志的 created_at 推断账期。
//
// 取出现次数最多的那个月，与 AggregateFromRows 的口径一致（那里也是按行计数取众数）。
// 文件名认不出来时才走这里：从「导出日志明细」导出的文件是
// 「日志查询_2026-09-01_2026-09-30_ab12cd.tsv」这种日期式名字，不含「N月」字样，
// monthFromFilename 认不出来——模板一在这种情况会退回日志内容，模板二同样需要。
//
// 返回 month=0 表示日志里没有可用的 created_at 列，调用方应显示「—」而不是编一个月份。
func SimpleBillPeriod(headers []string, rows [][]string) (year, month int) {
	idx, ok := columnIndex(headers, "created_at")
	if !ok {
		return 0, 0
	}

	// 统计 (年, 月) 组合的出现次数。跨月日志（如 8/28~9/3）会落在这里，
	// 取众数意味着账期归到行数多的那个月——与模板一的「按开始时间所在月」
	// 不完全等价，但模板二拿不到计划的开始时间，而且这点差异只影响摘要里那一行文字。
	counts := map[[2]int]int{}
	for _, row := range rows {
		ts, ok := parseUnixTimestamp(cellAt(row, idx))
		if !ok || ts <= 0 {
			continue
		}
		t := time.Unix(ts, 0).In(cstLocation)
		counts[[2]int{t.Year(), int(t.Month())}]++
	}
	if len(counts) == 0 {
		return 0, 0
	}

	best := [2]int{}
	bestCount := -1
	for k, c := range counts {
		// 平手时取较早的月份，保证结果可复现（map 遍历顺序随机）。
		if c > bestCount || (c == bestCount && (k[0] < best[0] || (k[0] == best[0] && k[1] < best[1]))) {
			best, bestCount = k, c
		}
	}
	return best[0], best[1]
}

// columnIndex 找列名在表头里的下标，找不到返回 false。
func columnIndex(headers []string, name string) (int, bool) {
	for i, h := range headers {
		if h == name {
			return i, true
		}
	}
	return 0, false
}

// SimpleBillWriteOptions 写出模板二时的开关。
type SimpleBillWriteOptions struct {
	// CostTable 写出成本表（汇总表的全部列 + 成本三列）而不是客户版账单。
	//
	// **只有成本表为 true**。成本三列是站点的内部数据（采购价与单笔毛利），
	// 客户拿到的账单里不该有；挪进单独的成本表后，客户版文件里根本不存在这些列，
	// 不需要靠「发出去之前记得删列」来兜——那要求人永远不忘。
	CostTable bool
}

// WriteSimpleBill 写出模板二的表。
//
// 客户版账表（账单、脱敏日志）与成本表共用这一个函数，靠 opts 区分列集合：
// 各写一份迟早会随时间走偏——那时同一笔账的两张表会对不上。
// 列集合的差别见 SimpleBillWriteOptions.CostTable。
//
// 表头写在代码里而不是读模板文件：列是固定的，且没有任何一张现成的模板可复用；
// 从零建表比让部署方多维护一个二进制模板文件可靠（漏挂文件的报错很难自解释）。
func WriteSimpleBill(path string, rows []SimpleBillRow, sheetName string, opts SimpleBillWriteOptions) error {
	f := excelize.NewFile()
	defer f.Close()

	if strings.TrimSpace(sheetName) == "" {
		sheetName = "简易账单"
	}
	// 改名后必须用**新名字**操作：GetSheetName(0) 拿到的是改名前的默认名
	// （Sheet1），继续用它会让每一次写入都报 "sheet Sheet1 does not exist"。
	// 这里重取一次而不是复用旧值，就是这么个一行的坑。
	if err := f.SetSheetName(f.GetSheetName(0), sheetName); err != nil {
		return fmt.Errorf("设置工作表名失败: %w", err)
	}
	sheet := f.GetSheetName(0)

	// 本次要写的列：成本表多三列，客户版账表没有。
	columns := SimpleBillColumns
	// 列号一律按**列名**查，不写死数字：这张表刚加过两列（缓存读/缓存创建），
	// 每加一列，后面所有列号都要跟着挪，而写死的数字漏改一处就会串列——
	// 串列不会报错，只是金额列里装着别的数，是最难发现的一类错。
	//
	// 查不到就 panic，**不能返回 0/1 之类的兜底值**：那些兜底值指向 A 列，
	// 而 A 列是合计行的「合计」标签所在。查不到时悄悄写进 A 列的表现是
	// 合计标签被一个 SUM 公式顶掉——表看起来正常，标签没了。
	// 列名写错是编译期查不出的，就让它在第一次跑到时立刻炸出来。
	//
	// colNum 闭包引用 columns 本身，所以在下面给它重新赋值（切到成本列集合）之后，
	// 查到的就是成本表里的列号，不需要两套常量。
	colNum := func(name string) int {
		idx, ok := columnIndex(columns, name)
		if !ok {
			panic(fmt.Sprintf("简易账单：列集合里没有 %q（可用：%v）", name, columns))
		}
		return idx + 1 // Excel 列号是 1 基
	}
	// 成本三列的列名。用来判断某一列是不是成本列，以及取成本表的列号。
	costColumnNames := []string{"官方刊例（美金）", "上游成本（人民币）", "利润（人民币）"}
	isCostColumn := map[string]bool{}
	for _, name := range costColumnNames {
		isCostColumn[name] = true
	}
	var costCols []int // 成本三列的 1 基列号，客户版为空
	if opts.CostTable {
		columns = SimpleBillCostColumns
		for _, name := range costColumnNames {
			costCols = append(costCols, colNum(name))
		}
	}

	styleHeader, err := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return err
	}
	styleAccounting, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(AccountingFmt)})
	if err != nil {
		return err
	}
	styleMoney, err := f.NewStyle(&excelize.Style{CustomNumFmt: strPtr(MoneyCNYFmt)})
	if err != nil {
		return err
	}
	styleMoneyBold, err := f.NewStyle(&excelize.Style{
		CustomNumFmt: strPtr(MoneyCNYFmt), Font: &excelize.Font{Bold: true},
	})
	if err != nil {
		return err
	}
	styleAccountingBold, err := f.NewStyle(&excelize.Style{
		CustomNumFmt: strPtr(AccountingFmt), Font: &excelize.Font{Bold: true},
	})
	if err != nil {
		return err
	}

	axis := func(col, r int) string {
		a, _ := excelize.CoordinatesToCellName(col, r)
		return a
	}

	header := make([]interface{}, len(columns))
	for i, h := range columns {
		header[i] = h
	}
	if err := f.SetSheetRow(sheet, "A1", &header); err != nil {
		return err
	}
	for i := range columns {
		if err := f.SetCellStyle(sheet, axis(i+1, 1), axis(i+1, 1), styleHeader); err != nil {
			return err
		}
	}

	costPartial := false // 有任何一行没算出成本，就不写成本列的合计

	// 数据从第 2 行开始：这张表没有模板里那种「第二行写说明」的约定，
	// 多留一行空白只会让客户以为是漏填。
	firstDataRow := 2
	for i, r := range rows {
		row := firstDataRow + i
		values := []interface{}{
			r.Group, r.Model, r.HitCount,
			r.TotalPrompt, r.TotalCompletion,
			r.TotalCacheRead, r.TotalCacheCreation,
			r.TotalQuota, r.TotalCostCNY,
		}
		if opts.CostTable {
			if r.CostPartial || (r.TotalRows > 0 && r.CostRows == 0) {
				costPartial = true
			}
			// 成本列**先补齐成空串**再逐格赋值：SetSheetRow 收的是一个定长切片，
			// 少给几格会让后面的列整体左移。空串与「没写」在 Excel 里都是空单元格。
			values = append(values, "", "", "")
			costVals := []*float64{r.OfficialListUSD, r.UpstreamCostCNY, r.ProfitCNY}
			for j, v := range costVals {
				if v != nil {
					values[costCols[j]-1] = *v
				}
			}
		}
		if err := f.SetSheetRow(sheet, axis(1, row), &values); err != nil {
			return err
		}
		// token 四列用千分位会计格式：数字大、客户要逐位核对，挤在一起很难读。
		for _, name := range []string{"输入Token", "输出Token", "缓存读Token", "缓存创建Token"} {
			col := colNum(name)
			if err := f.SetCellStyle(sheet, axis(col, row), axis(col, row), styleAccounting); err != nil {
				return err
			}
		}
		// 额度与金额用金额格式。
		for _, name := range []string{"额度", "金额（人民币）"} {
			col := colNum(name)
			if err := f.SetCellStyle(sheet, axis(col, row), axis(col, row), styleMoney); err != nil {
				return err
			}
		}
		// 成本三列的样式逐列指定（而不是写区间）：将来插一列不会连样式一起串位。
		for _, col := range costCols {
			if err := f.SetCellStyle(sheet, axis(col, row), axis(col, row), styleMoney); err != nil {
				return err
			}
		}
	}

	// 合计行。金额列写 SUM 公式而不是算好的数值：客户能在 Excel 里点开看它加了哪几行，
	// 与模板一合计行的做法一致。
	if len(rows) > 0 {
		lastDataRow := firstDataRow + len(rows) - 1
		totalRow := lastDataRow + 1
		if err := f.SetCellValue(sheet, axis(1, totalRow), "合计"); err != nil {
			return err
		}
		if err := f.SetCellStyle(sheet, axis(1, totalRow), axis(1, totalRow), styleHeader); err != nil {
			return err
		}
		sumCols := []struct {
			name  string
			style int
		}{
			{"次数", styleAccountingBold},
			{"输入Token", styleAccountingBold},
			{"输出Token", styleAccountingBold},
			{"缓存读Token", styleAccountingBold},
			{"缓存创建Token", styleAccountingBold},
			{"额度", styleMoneyBold},
			{"金额（人民币）", styleMoneyBold},
			{"官方刊例（美金）", styleMoneyBold},
			{"上游成本（人民币）", styleMoneyBold},
			{"利润（人民币）", styleMoneyBold},
		}
		for _, sc := range sumCols {
			// 成本三列在客户版里根本不存在，自然不写合计。
			// 这一判必须**在查列号之前**：查不到会 panic（那个 panic 是留给写错列名的），
			// 拿「本表根本没有这一列」去触发它就本末倒置了。
			//
			// 按列名判定而不是按列号大小：那种比较只有在成本列恰好排在最后、
			// 且客户版列数正好是成本表前缀时才成立，改一处就会悄悄失效。
			if isCostColumn[sc.name] && (!opts.CostTable || costPartial) {
				continue
			}
			// 成本三列只要有行没算出来就不写合计：SUM 会**跳过空单元格**，
			// 于是合计看起来是个正常数字、实际只加了有成本的那部分。
			// 那比留空更糟——留空至少看得出来「没算」，一个偏小的合计看不出来。
			col := colNum(sc.name)
			letter, _ := excelize.ColumnNumberToName(col)
			formula := fmt.Sprintf("SUM(%s%d:%s%d)", letter, firstDataRow, letter, lastDataRow)
			if err := f.SetCellFormula(sheet, axis(col, totalRow), formula); err != nil {
				return err
			}
			if err := f.SetCellStyle(sheet, axis(col, totalRow), axis(col, totalRow), sc.style); err != nil {
				return err
			}
		}

		// 成本算不全时在表末写一行说明。不写的话，收件人看到成本列是空的，
		// 只会以为是漏填或程序出错；写清楚了才知道是"这几个渠道还没维护上游倍率"，
		// 而且要去找谁补。这条说明与成本利润表末尾的处理一致。
		//
		// 客户版不写这一行：它整张表都没有成本列，说明「哪些行没算成本」
		// 既无对应列可看，又反过来透露了站内在算成本。
		if costPartial && opts.CostTable {
			noteRow := totalRow + 1
			// 按原因分开写：只说"有 392 行没算"会让人去翻日志，
			// 说清是渠道倍率还是分组倍率（或没有渠道号），用户直接知道去哪儿补。
			reasons := map[string]int{}
			var skippedQuota float64
			for _, r := range rows {
				for k, n := range r.SkipReasons {
					reasons[k] += n
				}
				skippedQuota += r.SkippedQuota
			}
			note := "注：有行的上游成本未计入——" + DescribeSkipReasons(reasons)
			if skippedQuota != 0 {
				note += fmt.Sprintf("，涉及净额度 ¥%s", trimMoney(skippedQuota/QuotaPerCNY))
			}
			note += "。"
			if reasons[string(SkipNoUpstreamRatio)] > 0 {
				note += "渠道倍率可在「账单导出任务」页补录后重新出账。"
			}
			if err := f.SetCellValue(sheet, axis(1, noteRow), note); err != nil {
				return err
			}
		}
	}

	// 列宽也按列名给：与列号一样，避免插入新列后宽度整体错位。
	// 没列在这里的列保持 Excel 默认宽度。
	widths := map[string]float64{
		"分组": 20, "模型": 28, "次数": 10,
		"输入Token": 16, "输出Token": 16, "缓存读Token": 16, "缓存创建Token": 16,
		"额度": 16, "金额（人民币）": 16,
		"官方刊例（美金）": 18, "上游成本（人民币）": 18, "利润（人民币）": 16,
	}
	for name, w := range widths {
		col, ok := columnIndex(columns, name)
		if !ok {
			continue // 客户版没有成本列，跳过即可
		}
		letter, _ := excelize.ColumnNumberToName(col + 1)
		if err := f.SetColWidth(sheet, letter, letter, w); err != nil {
			return err
		}
	}

	if err := f.SaveAs(path); err != nil {
		return fmt.Errorf("保存简易账单失败: %w", err)
	}
	return nil
}

// SimpleBillTotals 合计，供调用方组装结果与前端摘要。
type SimpleBillTotals struct {
	HitCount        int
	TotalPrompt     float64
	TotalCompletion float64
	// TotalCacheRead / TotalCacheCreation 缓存读与缓存创建的 token 合计，
	// 与表内两列同源（都来自 rowCacheTokens），所以摘要里的数能和文件对上。
	TotalCacheRead     float64
	TotalCacheCreation float64
	TotalQuota         float64
	TotalCostCNY       float64

	// ---- 成本三项。与成本利润表的 CostTotals 同一约定：算不全时是 nil 而不是 0。----

	// OfficialListUSD / UpstreamCostCNY 官方刊例（美金）与上游成本（人民币）合计。
	//
	// **只覆盖算得出成本的那些行**，所以必须与 AmountCoveredCNY 一起看：
	// 单独一个成本合计配上全部行的金额，会算出一个偏小的成本、偏大的利润。
	OfficialListUSD *float64
	UpstreamCostCNY *float64
	// AmountCoveredCNY 参与成本核算的那部分金额（人民币）。利润只能用它与成本相减。
	AmountCoveredCNY float64
	// ProfitCNY 利润 = AmountCoveredCNY − UpstreamCostCNY，口径与上式一致。
	ProfitCNY *float64
	// Cost 覆盖情况。Cost.Rows < Cost.TotalRows 说明利润不是整体毛利，页面必须说明。
	//
	// 注意 TotalRows 对**全部行**累计（含没算出成本的），Rows 只累计算出来的，
	// 所以两者不等有两种情形：有行算不出来，或者整表根本没开成本核算。
	// 后者两个 Missing* 都是 0——调用方据此区分「算不全」与「没开」。
	Cost SimpleBillCostStat
}

// SumSimpleBill 把各行加总。金额单独累加各行（而不是用总额度再算一次），
// 这样合计与逐行之和逐位相等，客户手工加总不会差出几分钱。
//
// 成本三项在**有一行算不出来时整体为 nil**：SUM 跳过空值，若把有成本的行加起来
// 当合计，会得到一个看着正常、实际漏了一部分上游成本的数——那正是成本核算最不能出的错。
// 同时仍把已覆盖的金额与行数报出来（AmountCoveredCNY / Cost），
// 让调用方能说清"利润只覆盖了 90% 的行"，而不是给一个孤零零的、无出处的利润数字。
func SumSimpleBill(rows []SimpleBillRow) SimpleBillTotals {
	var t SimpleBillTotals
	var official, upstream, profit float64
	hasCost := false
	for _, r := range rows {
		t.HitCount += r.HitCount
		t.TotalPrompt += r.TotalPrompt
		t.TotalCompletion += r.TotalCompletion
		t.TotalCacheRead += r.TotalCacheRead
		t.TotalCacheCreation += r.TotalCacheCreation
		t.TotalQuota += r.TotalQuota
		t.TotalCostCNY += r.TotalCostCNY

		// 覆盖情况对**每一行**都累计，包括没算出成本的那些——
		// 只在有成本的行上累加的话，「有 2 行没算成本」这件事根本进不了合计，
		// 调用方也就无从判断这个成本口径完整不完整。
		t.Cost.TotalRows += r.TotalRows
		t.Cost.SkippedQuota += r.SkippedQuota
		for k, n := range r.SkipReasons {
			if t.Cost.SkipReasons == nil {
				t.Cost.SkipReasons = map[string]int{}
			}
			t.Cost.SkipReasons[k] += n
		}

		// 三列要么同时有值要么整体为 nil（见 AggregateSimpleBill），
		// 所以只看其中一个就够，不必三处都判。
		if r.UpstreamCostCNY == nil || r.OfficialListUSD == nil || r.ProfitCNY == nil {
			// 这一行没有成本（整桶一行都没算出来，或没开成本核算）。
			// 不把整体置 false——**部分覆盖也要给合计**：
			// 504100 行里 392 行算不出来时，用剩下的算出来的成本远比不给有用。
			continue
		}
		t.Cost.Rows += r.CostRows
		hasCost = true
		official += *r.OfficialListUSD
		upstream += *r.UpstreamCostCNY
		profit += *r.ProfitCNY
		t.AmountCoveredCNY += r.TotalCostCNY
	}
	// 只要**有任何一行**算出了成本就给合计：行数覆盖率由 Cost 如实带上，
	// 调用方据此决定要不要加一句「只覆盖了 N/M 行」。
	// 全表一行都没算出来（比如倍率全没维护）时给 nil——那时没有任何依据，
	// 报 0 会被读成「上游免费」。
	if hasCost {
		o := round(official, MoneyDecimals)
		u := round(upstream, MoneyDecimals)
		p := round(profit, MoneyDecimals)
		t.OfficialListUSD = &o
		t.UpstreamCostCNY = &u
		t.ProfitCNY = &p
		t.AmountCoveredCNY = round(t.AmountCoveredCNY, MoneyDecimals)
	}
	return t
}
