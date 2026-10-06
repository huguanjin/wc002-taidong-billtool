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
	// TotalQuota 额度合计，**净额**（消费 − 退款 + 补扣）。
	TotalQuota float64 `json:"totalQuota"`
	// TotalCostCNY 金额 = 额度 / QuotaPerCNY，即站点实收。
	TotalCostCNY float64 `json:"totalCostCny"`

	// ---- 成本核算。未维护倍率的渠道让这三项整体为 nil（见 SimpleBillRow.CostMissing）。----

	// OfficialListUSD 官方刊例（美金）＝ Σ(quota ÷ group_ratio) ÷ QuotaPerCNY。
	//
	// 反推而非查表：站内的 quota 就是「官方刊例 × 分组倍率 × QuotaPerCNY」算出来的，
	// 所以除回去就得到刊例。这条路径不需要价表，与模板二不依赖定价数据的定位一致。
	OfficialListUSD *float64 `json:"officialListUsd"`
	// UpstreamCostCNY 上游成本 ＝ 官方刊例USD × 上游倍率 ÷ DiscountBaseFactor。
	//
	// 与成本利润表同一公式（见 CostRow.UpstreamCostCNY），只是那边还要用汇率把刊例
	// 从美金换成人民币，这里刊例本来就是从 quota 反推出来的人民币，直接乘即可。
	UpstreamCostCNY *float64 `json:"upstreamCostCny"`
	// ProfitCNY 利润 ＝ 金额 − 上游成本。
	ProfitCNY *float64 `json:"profitCny"`
	// CostMissing 该行有行的渠道倍率没维护，成本算不全，上面三项为 nil。
	//
	// 为什么是 nil 而不是 0：0 会被读成「上游免费」，利润虚高——这正是成本核算
	// 最不能出的错（与成本利润表同一约定，见 taskrun.go 里三个成本字段保持 nil 的理由）。
	CostMissing bool `json:"costMissing"`
	// CostRows 参与了成本核算的行数；TotalRows 是该汇总行覆盖的全部消费行数。
	// 两者不等说明有一部分行的渠道倍率没维护。
	CostRows  int `json:"costRows"`
	TotalRows int `json:"totalRows"`
	// MissingRatioRows / MissingChannelRows 没算进成本的行数，按原因分开——
	// 前者是日志缺 group_ratio（反推不出刊例），后者是渠道号取不到或没维护上游倍率。
	// 分开是为了让提示能说清去哪儿补，而不是笼统的一句「有行没算成本」。
	MissingRatioRows   int `json:"missingRatioRows"`
	MissingChannelRows int `json:"missingChannelRows"`
}

// SimpleBillColumns 模板二的列名，账单与汇总脱敏日志共用同一套（客户已确认两者列一致）。
var SimpleBillColumns = []string{
	"分组", "模型", "次数", "输入Token", "输出Token", "额度", "金额（人民币）",
	"官方刊例（美金）", "上游成本（人民币）", "利润（人民币）",
}

// SimpleBillCostColumns 成本三列的下标区间（0 基，闭区间）：官方刊例 / 上游成本 / 利润。
//
// 抽出来是因为「哪些列是成本列」在四处要用：表头样式、合计行、表末备注、
// 前端摘要。写死成 7/8/9 散在各处，将来加一列就会漏改一两处，
// 而漏改的表现是**金额串列**——最不容易一眼看出来的那种错。
const (
	SimpleBillCostColFirst = 7
	SimpleBillCostColLast  = 9
)

// SimpleBillOptions 模板二的算法开关与运行时输入。
//
// 用一个结构体而不是往参数表上再加三个参数：可选项会随需求继续长，
// 而调用点只有一处——多一个参数就要改一次签名，结构体则只用加字段。
type SimpleBillOptions struct {
	// UpstreamRatios 渠道 ID → 上游倍率。由调用方（handler）从本地 PG 读好传入，
	// billing 包不连 PG——与 Params.ChannelUpstreamRatios 同一个约定。
	// 为空表示一条倍率都没有（首次部署、还没拉过渠道清单）。
	UpstreamRatios map[int]float64
	// CostColumns 是否写成本三列。
	//
	// 关掉时三列**仍然存在**但整列为空：列集合是固定的十列（账单与脱敏日志必须同构，
	// 见 WriteSimpleBill），所以关闭只意味着不填，不意味着少三列。
	CostColumns bool
	// ExchangeRate 人民币/美金汇率，用于把反推出来的刊例（美金）换回人民币去算成本。
	//
	// <= 0 时用 DefaultExchangeRate。这里必须与出账用的汇率一致：
	// 模板一的成本也是「刊例USD × 汇率 × 上游折扣」，两边用不同的汇率，
	// 同一份日志的两张表会给出两个成本数。
	ExchangeRate float64
}

// rateOr 取汇率，未设置时回退默认值。
func (o SimpleBillOptions) rateOr() float64 {
	if o.ExchangeRate > 0 {
		return o.ExchangeRate
	}
	return DefaultExchangeRate
}

// SimpleBillCostStat 成本覆盖情况，供调用方在结果里如实报出。
//
// 需要它是因为「部分行的渠道没维护倍率」时成本是按有倍率的那部分算的：
// 页面若只显示一个利润数字，读的人会以为它是整体毛利。与成本利润表的
// CostTotals.PricedRows/TotalRows 同一个用意。
type SimpleBillCostStat struct {
	// Rows 参与成本核算的行数；TotalRows 是本表覆盖的全部消费行数。
	Rows      int
	TotalRows int
	// MissingRatioRows group_ratio 缺失、无法反推刊例的行数。
	MissingRatioRows int
	// MissingChannelRows 渠道号取不到、或渠道没维护上游倍率的行数。
	MissingChannelRows int
}

// Complete 全部行都参与了成本核算。
func (s SimpleBillCostStat) Complete() bool {
	return s.TotalRows > 0 && s.Rows == s.TotalRows
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
//	上游成本CNY = 官方刊例USD × 上游倍率 ÷ DiscountBaseFactor
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
		// 逐行成本是否都算得出来。任何一行缺 group_ratio / 缺渠道倍率就置 false，
		// 整桶的成本与利润随之留空——哪怕其余的 99 行都算得出来。
		allRows bool
		// 未参与成本核算的行数，按原因分开数：页面要能说清"是缺渠道倍率还是缺分组倍率"，
		// 因为这决定了用户该去哪补——前者去渠道倍率页，后者说明日志本身有问题。
		noRatioRows   int
		noChannelRows int
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
			costs[k] = &costAcc{allRows: true}
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
		}

		if !opts.CostColumns {
			continue
		}

		// 退款行也一起算：它冲抵的是某一行的额度，那一行的正负号变了，
		// 挂在它上面的刊例与成本按同一比例跟着变才是对的。跳过退款行会让
		// 成本与金额对不上——金额冲抵了、成本没冲抵，利润凭空变高。
		costRatio, ok := GroupRatioFromOther(other)
		if !ok || costRatio <= 0 {
			// 没有 group_ratio 就反推不出刊例。按 0 算会让刊例虚高到无穷，
			// 所以这一行**整体不计入成本**，并让整桶的成本留空（见 allRows）。
			ca.allRows = false
			ca.noRatioRows++
			continue
		}
		upstream, ok := rowUpstreamRatio(row, idxChannel, hasChannel, idxOther, hasOther, opts.UpstreamRatios)
		if !ok {
			ca.allRows = false
			ca.noChannelRows++
			continue
		}
		ca.rows++
		// 净额口径与 TotalQuota 一致（退款行为负），否则同一条退款在
		// 金额列与成本列上冲抵的方向会相反。
		ca.officialQuota += delta / costRatio
		// 成本逐行算再累加，而不是「汇总刊例 × 某个倍率」：
		// 一个 (分组, 模型) 横跨多个渠道时，各渠道倍率不同，只有逐行加权才对。
		//
		// 单位换算走两步，与 CostRow.UpstreamCostCNY 完全一致：
		//   ÷ QuotaPerCNY 把 quota 量纲换成刊例美金（见上面的恒等式）
		//   × 汇率         换成人民币
		//   ÷ DiscountBaseFactor 是上游折扣（上游倍率 / 7，见 group_ratio_source.md）
		ca.upstreamCNY += delta / costRatio / QuotaPerCNY * rate * upstream / DiscountBaseFactor
	}

	out := make([]SimpleBillRow, 0, len(buckets))
	for _, k := range order {
		r := buckets[k]
		// 金额保留 4 位：与手工 SQL 的 ROUND(..., 4) 一致，客户逐格比对时不会差在精度上。
		r.TotalCostCNY = round(r.TotalQuota/QuotaPerCNY, MoneyDecimals)

		if opts.CostColumns {
			ca := costs[k]
			r.CostRows = ca.rows
			r.MissingRatioRows = ca.noRatioRows
			r.MissingChannelRows = ca.noChannelRows
			// 只要有一行算不出来就不写成本：部分渠道算了、部分没算的成本看着像完整的，
			// 比留空更危险。官刊例同样要求算全——它虽然只要 group_ratio，
			// 但缺的那几行会让刊例偏小，客户拿去与上游对账时对不上。
			if ca.allRows && ca.rows > 0 {
				// 三项都 round 到金额精度：客户会拿计算器逐格复核，
				// 显示 4 位而内部多留几位，会让他手算的结果与表里差最后一位。
				officialUSD := round(ca.officialQuota/QuotaPerCNY, MoneyDecimals)
				upstreamCNY := round(ca.upstreamCNY, MoneyDecimals)
				profit := round(r.TotalCostCNY-upstreamCNY, MoneyDecimals)
				r.OfficialListUSD = &officialUSD
				r.UpstreamCostCNY = &upstreamCNY
				r.ProfitCNY = &profit
			} else {
				r.CostMissing = true
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

// WriteSimpleBill 写出模板二的表。
//
// 账单与汇总脱敏日志共用这一个函数：两者的列完全相同（客户已确认），
// 各写一份迟早会随时间走偏——那时同一笔账的两张表会对不上。
//
// 表头写在代码里而不是读模板文件：列是固定的七列，且没有任何一张现成的模板可复用；
// 从零建表比让部署方多维护一个二进制模板文件可靠（漏挂文件的报错很难自解释）。
func WriteSimpleBill(path string, rows []SimpleBillRow, sheetName string) error {
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

	header := make([]interface{}, len(SimpleBillColumns))
	for i, h := range SimpleBillColumns {
		header[i] = h
	}
	if err := f.SetSheetRow(sheet, "A1", &header); err != nil {
		return err
	}
	for i := range SimpleBillColumns {
		if err := f.SetCellStyle(sheet, axis(i+1, 1), axis(i+1, 1), styleHeader); err != nil {
			return err
		}
	}

	// 成本三列的样式按单列分别设：列数固定十列，若用循环写区间，
	// 将来插一列就会连样式一起串位。
	costStyles := []int{styleMoney, styleMoney, styleMoney}
	costMissing := false // 有任何一行没算出成本，就不写成本列的合计

	// 数据从第 2 行开始：这张表没有模板里那种「第二行写说明」的约定，
	// 多留一行空白只会让客户以为是漏填。
	firstDataRow := 2
	for i, r := range rows {
		row := firstDataRow + i
		values := []interface{}{r.Group, r.Model, r.HitCount, r.TotalPrompt, r.TotalCompletion, r.TotalQuota, r.TotalCostCNY}
		// 成本列**先补齐成空串**再逐格赋值：SetSheetRow 收的是一个定长切片，
		// 少给几格会让后面的列整体左移。空串与「没写」在 Excel 里都是空单元格。
		values = append(values, "", "", "")
		if r.CostMissing {
			costMissing = true
		}
		costVals := []*float64{r.OfficialListUSD, r.UpstreamCostCNY, r.ProfitCNY}
		for j, v := range costVals {
			if v != nil {
				values[SimpleBillCostColFirst+j] = *v
			}
		}
		if err := f.SetSheetRow(sheet, axis(1, row), &values); err != nil {
			return err
		}
		for _, col := range []int{4, 5, 6} {
			if err := f.SetCellStyle(sheet, axis(col, row), axis(col, row), styleAccounting); err != nil {
				return err
			}
		}
		if err := f.SetCellStyle(sheet, axis(7, row), axis(7, row), styleMoney); err != nil {
			return err
		}
		for j, st := range costStyles {
			col := SimpleBillCostColFirst + j + 1 // 列号是 1 基
			if err := f.SetCellStyle(sheet, axis(col, row), axis(col, row), st); err != nil {
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
			col   int
			style int
		}{
			{3, styleAccountingBold}, // 次数
			{4, styleAccountingBold}, // 输入
			{5, styleAccountingBold}, // 输出
			{6, styleMoneyBold},      // 额度
			{7, styleMoneyBold},      // 金额
			{8, styleMoneyBold},      // 官方刊例USD
			{9, styleMoneyBold},      // 上游成本
			{10, styleMoneyBold},     // 利润
		}
		for _, sc := range sumCols {
			// 成本三列只要有行没算出来就不写合计：SUM 会**跳过空单元格**，
			// 于是合计看起来是个正常数字，实际只加了有成本的那部分。
			// 那比留空更糟——留空至少看得出来"没算"，一个偏小的合计看不出来。
			isCostCol := sc.col >= SimpleBillCostColFirst+1 && sc.col <= SimpleBillCostColLast+1
			if isCostCol && costMissing {
				continue
			}
			letter, _ := excelize.ColumnNumberToName(sc.col)
			formula := fmt.Sprintf("SUM(%s%d:%s%d)", letter, firstDataRow, letter, lastDataRow)
			if err := f.SetCellFormula(sheet, axis(sc.col, totalRow), formula); err != nil {
				return err
			}
			if err := f.SetCellStyle(sheet, axis(sc.col, totalRow), axis(sc.col, totalRow), sc.style); err != nil {
				return err
			}
		}

		// 成本算不全时在表末写一行说明。不写的话，收件人看到成本列是空的，
		// 只会以为是漏填或程序出错；写清楚了才知道是"这几个渠道还没维护上游倍率"，
		// 而且要去找谁补。这条说明与成本利润表末尾的处理一致。
		if costMissing {
			noteRow := totalRow + 1
			// 按原因分开报：只说"有 3 行没算"会让人去查日志，
			// 说清是渠道倍率还是分组倍率，用户直接知道去哪儿补。
			noRatio, noChannel := 0, 0
			for _, r := range rows {
				noRatio += r.MissingRatioRows
				noChannel += r.MissingChannelRows
			}
			var reasons []string
			if noChannel > 0 {
				reasons = append(reasons, fmt.Sprintf("%d 行的渠道未维护上游倍率", noChannel))
			}
			if noRatio > 0 {
				reasons = append(reasons, fmt.Sprintf("%d 行缺少分组倍率", noRatio))
			}
			note := "注：有行的上游成本未计入——" + strings.Join(reasons, "；")
			var subject []string
			if noChannel > 0 {
				subject = append(subject, "请在渠道倍率维护页补齐后重新出账")
			}
			if noRatio > 0 {
				subject = append(subject, "缺分组倍率的行请检查日志来源")
			}
			note += "；" + strings.Join(subject, "，") + "。"
			if err := f.SetCellValue(sheet, axis(1, noteRow), note); err != nil {
				return err
			}
		}
	}

	widths := map[string]float64{
		"A": 20, "B": 28, "C": 10, "D": 16, "E": 16, "F": 16, "G": 16,
		"H": 18, "I": 18, "J": 16,
	}
	for col, w := range widths {
		if err := f.SetColWidth(sheet, col, col, w); err != nil {
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
	TotalQuota      float64
	TotalCostCNY    float64

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
	costed := true
	for _, r := range rows {
		t.HitCount += r.HitCount
		t.TotalPrompt += r.TotalPrompt
		t.TotalCompletion += r.TotalCompletion
		t.TotalQuota += r.TotalQuota
		t.TotalCostCNY += r.TotalCostCNY

		// 覆盖情况对**每一行**都累计，包括没算出成本的那些——
		// 只在有成本的行上累加的话，「有 2 行没算成本」这件事根本进不了合计，
		// 调用方也就无从判断这个成本口径完整不完整。
		t.Cost.TotalRows += r.TotalRows
		t.Cost.MissingRatioRows += r.MissingRatioRows
		t.Cost.MissingChannelRows += r.MissingChannelRows

		// 三列要么同时有值要么同时为空（见 AggregateSimpleBill），
		// 所以只看其中一个就够，不必三处都判。
		if r.UpstreamCostCNY == nil || r.OfficialListUSD == nil || r.ProfitCNY == nil {
			// 一行都没启用成本列时，这里也会把 costed 置 false——
			// 于是合计里根本没有成本，调用方据此不显示成本区，符合预期。
			costed = false
			continue
		}
		t.Cost.Rows += r.CostRows
		official += *r.OfficialListUSD
		upstream += *r.UpstreamCostCNY
		profit += *r.ProfitCNY
		t.AmountCoveredCNY += r.TotalCostCNY
	}
	if costed && len(rows) > 0 {
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
