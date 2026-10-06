package billing

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/xuri/excelize/v2"
)

// GenerateResult 一次出账运行的完整结果：文件路径 + 摘要数据。
type GenerateResult struct {
	BillPath      string
	SanitizedPath string // 为空表示未生成
	// CostPath 成本利润表路径；为空表示未生成（未勾选、或倍率未维护被拦下）。
	CostPath string
	// CostBlocked 为真表示用户勾了生成成本利润表，但有渠道尚未维护上游倍率，
	// 于是没有生成成本利润表。**这不是错误**——硬报错会让用户丢掉已填好的出账参数。
	// 调用方应把 MissingChannelInfos / UnknownChannelIDs 交给页面就地补录。
	CostBlocked bool
	// MissingChannelInfos 未维护倍率的渠道（渠道表里查得到，可以补录）。
	MissingChannelInfos []ChannelInfo
	// UnknownChannelIDs 日志里有、但渠道表里查不到的渠道号（多半已被硬删除，无法补录）。
	UnknownChannelIDs []int
	// CostTotals 成本利润表合计；为空表示成本利润表没生成。前端据此拼可复制的说明文字。
	CostTotals *CostTotals
	// CostSummary 结果区那段可复制的文字，由后端按与表内公式同源的口径生成。
	CostSummary string
	// BillSummary 简易账单（模板二）的可复制文字。
	//
	// 与 CostSummary 分开而不是复用同一个字段：两者的内容与口径完全不同
	// （成本利润 vs 站点实收额度），合成一个字段后前端只凭「有没有值」判断该显示
	// 哪种口径的说明，迟早会在一处显示错。模板二不产成本表，CostSummary 始终为空。
	BillSummary string
	// SimpleCostStat 简易账单的成本覆盖情况（参与核算的行数 / 全部行数）。
	//
	// 只有模板二会填。它与 CostTotals 不同：那是模板一那张独立成本利润表的合计，
	// 这是同一张汇总账单里三列成本的覆盖面，前端据此决定要不要写那句
	// 「利润只覆盖 N/M 行」——不写的话，一个偏小的成本会被读成整体毛利。
	SimpleCostStat SimpleBillCostStat
	Summary        Summary
}

// loadBillingPriceBook 按计价来源装载价表。
//
// 抽出来是因为「预览分组折扣」也要读同一份价表——两处各写一遍的话，
// 页面上显示的折扣会与实际出账时用的不一致，而那正是这个功能要消灭的问题。
func loadBillingPriceBook(params Params, priceTablePath, dbPriceCachePath string) (*PriceBook, *BillingExprSetting, error) {
	switch params.PriceSource {
	case PriceSourceDB:
		book, exprSetting, _, err := LoadDBPriceCache(dbPriceCachePath)
		if err != nil {
			return nil, nil, err
		}
		// LoadDBPriceCache 只读数据库实时价格，不含 price_table.xlsx 人工维护的厂商家族
		// 折扣 sheet（如「国产模型」sheet 里 DeepSeek=6折）；不补上的话这些分组会整组掉进
		// ComputeGroupDiscounts 的「Σ结算/Σ总金额」反推——对走 billing_expr 阶梯表达式计费
		// 的自定义模型，反推出来的折扣不可信（反推用的"官方刊例"并非真正的官方对标价）。
		// 加载失败不影响出账，只是这些分组会退回反推。
		if discounts, derr := LoadPriceTableDiscounts(priceTablePath); derr == nil {
			for family, d := range discounts {
				if _, exists := book.Discounts[family]; !exists {
					book.Discounts[family] = d
				}
			}
		}
		return book, exprSetting, nil
	default:
		book, err := LoadPriceBook(priceTablePath)
		if err != nil {
			return nil, nil, fmt.Errorf("加载报价表失败: %w", err)
		}
		return book, nil, nil
	}
}

// PreferPriceTableFor 该计价来源下是否优先用报价表里的价格。
func PreferPriceTableFor(source PriceSource) bool {
	return source == PriceSourcePriceTable || source == PriceSourceDB
}

// GenerateBill 对应 log_to_bill.py 的 main()：读日志→提取缓存→聚合定价→写账单模板→（可选）写脱敏日志。
// dbPriceCachePath 是「拉取最新数据库价格」写出的本地 JSON 文件路径，出账时只读此文件，不连接数据库。
func GenerateBill(inputPath, templatePath, priceTablePath, dbPriceCachePath, outputDir string, params Params) (*GenerateResult, error) {
	// 简易账单在**读价表之前**分流：它完全不参与定价，价表缺失的部署也该能出这张表。
	// 放到后面分流的话，一个没挂 price_table.xlsx 的环境会先在加载价表时失败。
	if IsSimpleBillTemplate(params.BillTemplate) {
		return generateSimpleBill(inputPath, outputDir, params)
	}

	book, exprSetting, err := loadBillingPriceBook(params, priceTablePath, dbPriceCachePath)
	if err != nil {
		return nil, err
	}
	// official 模式下内置官方价优先；price_table/db 模式下报价表/数据库价格优先。
	preferPriceTable := PreferPriceTableFor(params.PriceSource)
	mergeManualPrices(book, params.ManualPrices)

	headers, rows, err := LoadLogRows(inputPath, params.Sheet, params.Encoding)
	if err != nil {
		return nil, fmt.Errorf("读取日志失败: %w", err)
	}

	stem := strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))
	billPath := filepath.Join(outputDir, withCustomerSuffix(defaultOutputName(stem), params.CustomerName)+".xlsx")

	var sanitizedWriter SanitizedWriter
	var sanitizedPath string
	if params.SanitizedLog {
		ext, delimiter, isDelimited := sanitizedFormatInfo(params.SanitizedFormat)
		sanitizedPath = filepath.Join(outputDir, withCustomerSuffix(defaultSanitizedName(stem), params.CustomerName)+ext)
		if isDelimited {
			sanitizedWriter, err = NewCSVSanitizedWriter(sanitizedPath, headers, delimiter, params.IncludeBillingParams)
		} else {
			sanitizedWriter, err = NewExcelSanitizedWriter(sanitizedPath, headers, params.IncludeBillingParams)
		}
		if err != nil {
			return nil, fmt.Errorf("初始化脱敏日志写出失败: %w", err)
		}
	}

	exchangeRate := params.ExchangeRate
	if exchangeRate <= 0 {
		exchangeRate = DefaultExchangeRate
	}

	var writerIface SanitizedRowWriter
	if sanitizedWriter != nil {
		writerIface = sanitizedWriter
	}

	agg, aggErr := AggregateFromRows(rows, headers, book, exchangeRate, preferPriceTable, exprSetting, params.IncludeBillingParams, writerIface)
	if sanitizedWriter != nil {
		if closeErr := sanitizedWriter.Close(); closeErr != nil && aggErr == nil {
			return nil, fmt.Errorf("写出脱敏日志失败: %w", closeErr)
		}
	}
	if aggErr != nil {
		return nil, aggErr
	}

	month := params.Month
	if month == 0 {
		if m, ok := monthFromFilename(stem); ok {
			month = m
		} else {
			month = agg.Month
		}
	}
	if month < 1 || month > 12 {
		return nil, fmt.Errorf("无效月份: %d", month)
	}
	year := params.Year
	if year == 0 {
		year = agg.Year
	}

	// 折扣覆盖只在这里拼装一次，再往下传给账单、成本利润表与前端摘要三处。
	// 三处各拼各的话，迟早出现「账单用了手工折扣、页面摘要没用」这种不一致。
	ov := DiscountOverrides{Forced: params.Discount, Manual: params.ManualDiscounts}

	missingPrices, err := WriteBillFromTemplate(templatePath, billPath, agg.Rows, year, month, book, ov, exchangeRate, preferPriceTable, params.DomesticMarkers)
	if err != nil {
		return nil, fmt.Errorf("写出账单失败: %w", err)
	}

	if params.KeepLog && !IsDelimitedText(inputPath) {
		if err := attachLogSheet(billPath, headers, rows); err != nil {
			return nil, fmt.Errorf("附带原日志失败: %w", err)
		}
	}

	summary := buildSummary(agg, book, exchangeRate, ov, missingPrices, params.DomesticMarkers)

	result := &GenerateResult{BillPath: billPath, SanitizedPath: sanitizedPath, Summary: summary}

	// 成本利润表是增量产物：不生成时账单与改动前逐格一致，不影响既有客户。
	if params.GenerateCost {
		costPath, totals, summaryText, blocked, missing, unknown, cerr := generateCostTable(
			inputPath, templatePath, billPath, rows, headers, book, params, ov, exchangeRate, preferPriceTable, year, month)
		if cerr != nil {
			return nil, cerr
		}
		result.CostBlocked = blocked
		result.MissingChannelInfos = missing
		result.UnknownChannelIDs = unknown
		if !blocked {
			result.CostPath = costPath
			result.CostTotals = totals
			result.CostSummary = summaryText
			// 合计同时挂进 Summary：前端结果区的「结算/成本/利润」与可复制文字
			// 都读这一份，避免两处各取一套数。
			result.Summary.CostTotals = totals
		}
	}

	return result, nil
}

// generateSimpleBill 简易账单（模板二）的出账路径。
//
// 与模板一的流程有本质区别：不读价表、不逐行定价、不反推折扣、不算阶梯表达式，
// 只把日志按 (分组, 模型) 汇总并把额度折算成金额。因此它也不产出单独的**成本利润表**，
// 且忽略 params.GenerateCost —— 那张表的前提是「每个渠道有上游倍率」，与本模板无关。
//
// 但成本三列**不依赖价表**：它们是从额度里反推的（见 AggregateSimpleBill）。
// 所以这张表照样能给成本与利润，只是没有模板一那种逐渠道拆开的明细。
func generateSimpleBill(inputPath, outputDir string, params Params) (*GenerateResult, error) {
	headers, rows, err := LoadLogRows(inputPath, params.Sheet, params.Encoding)
	if err != nil {
		return nil, fmt.Errorf("读取日志失败: %w", err)
	}

	// 成本列只在开了成本核算时才填（见 Params.CheckCost）。关掉时列还在、值为空——
	// 列集合固定，否则同一份产物在两种开关下结构不同，下游脚本会莫名对不上。
	opts := SimpleBillOptions{
		UpstreamRatios: params.ChannelUpstreamRatios,
		KnownChannels:  params.ChannelKnownIDs,
		CostColumns:    params.CheckCost,
		// 汇率与模板一同一来源：两边的成本都经这一步换算，用不同的汇率会得出两个成本数。
		ExchangeRate: params.ExchangeRate,
	}
	summaryRows, err := AggregateSimpleBill(rows, headers, opts)
	if err != nil {
		return nil, err
	}

	stem := strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))

	// 三张表用的是**同一份 summaryRows**（行相同），差别只在列：
	//
	//	账单 / 脱敏日志	→ 客户版七列
	//	成本表			→ 客户版七列 + 官方刊例 / 上游成本 / 利润
	//
	// 行共用是为了两张表天然对得上（各聚一次迟早走偏）；列分开是因为成本三列
	// 是站点的采购价与单笔毛利，客户拿到的任何文件里都不该有。
	// 起初这三列挂在账单上，靠「发之前自己删列」兜——那要求人永远不忘、还得逐列
	// 看清删对了。挪进独立成本表后，客户版文件里**根本不存在**这些列。
	billPath := filepath.Join(outputDir, withCustomerSuffix(simpleOutputName(stem), params.CustomerName)+".xlsx")
	if err := WriteSimpleBill(billPath, summaryRows, "简易账单", SimpleBillWriteOptions{}); err != nil {
		return nil, fmt.Errorf("写出简易账单失败: %w", err)
	}

	var sanitizedPath string
	if params.SanitizedLog {
		sanitizedPath = filepath.Join(outputDir, withCustomerSuffix(simpleSanitizedName(stem), params.CustomerName)+".xlsx")
		if err := WriteSimpleBill(sanitizedPath, summaryRows, "汇总明细", SimpleBillWriteOptions{}); err != nil {
			return nil, fmt.Errorf("写出汇总脱敏日志失败: %w", err)
		}
	}

	// 成本表：带成本三列，文件名由账单名推出（账单二_xxx → 成本二_xxx）。
	//
	// 只在勾了成本核算时出：没勾就没有成本口径，出一张成本列全空的表
	// 比不出更让人困惑（看着像算错了）。
	var costPath string
	if params.CheckCost {
		costPath = filepath.Join(outputDir, withCustomerSuffix(simpleCostName(stem), params.CustomerName)+".xlsx")
		if err := WriteSimpleBill(costPath, summaryRows, "成本表", SimpleBillWriteOptions{CostTable: true}); err != nil {
			return nil, fmt.Errorf("写出成本表失败: %w", err)
		}
	}

	totals := SumSimpleBill(summaryRows)

	// 账期与模板一同口径：参数指定优先，其次从文件名推断，最后从日志的 created_at 推断。
	//
	// 第三层不能省：从「导出日志明细」导出的文件叫
	// 「日志查询_2026-09-01_2026-09-30_ab12cd.tsv」，不含「N月」字样，
	// monthFromFilename 认不出来。模板一在这时会退回日志内容，模板二同样要。
	// 三层都推不出来时留 0，前端显示为「—」而不是编一个当月糊上去。
	year, month := params.Year, params.Month
	if month == 0 {
		if m, ok := monthFromFilename(stem); ok {
			month = m
		} else if y, m := SimpleBillPeriod(headers, rows); m > 0 {
			year, month = y, m
		}
	}
	if month < 1 || month > 12 {
		month = 0
	}

	// 摘要只填最小集：模板二没有刊例与折扣，前端结果区的「总金额」用它，
	// 「综合折扣」留 0（模板二不打折，显示成 1.0 会让人以为真有个折扣）。
	// 逐行明细刻意留空：这张表给的就是汇总，把汇总行塞进「明细」表反而让人以为
	// 中间每一步的 token 都能在这里查到。
	summary := Summary{
		Year:            year,
		Month:           month,
		SettleCNYTotal:  totals.TotalCostCNY,
		ListCNYTotal:    totals.TotalCostCNY,
		OverallDiscount: 0,
		RowCount:        len(summaryRows),
	}

	result := &GenerateResult{
		BillPath:      billPath,
		SanitizedPath: sanitizedPath,
		CostPath:      costPath,
		Summary:       summary,
		BillSummary:   FormatSimpleBillSummary(summaryRows, totals, year, month, params.SummaryHeader),
		// 成本覆盖情况一起交出去：摘要里要写「利润只覆盖了 N/M 行」，
		// 而 totals 本身分不清「一行都没算」与「没开成本核算」。
		SimpleCostStat: totals.Cost,
	}

	// 成本合计也要挂进 CostTotals，否则计划列表里这两列永远显示「—」。
	//
	// 这是修一个真实故障：这个字段原先只有模板一那条路径会填（在
	// generateCostTable 之后赋值），而模板二算出了成本与利润、摘要里也印出来了，
	// 却因为落库那段代码是 `if gen.CostTotals != nil` 而被整个跳过——
	// 结果是同一次执行，摘要写着「上游成本 ¥36.9743、利润 ¥22.479」，
	// 计划列表却显示「未核算成本」，两处自相矛盾。
	//
	// 复用同一个结构体而不是给模板二另开一个落库分支：计划表的
	// costed_settle_cny / cost_cny / profit_cny / cost_complete / priced_rows /
	// total_rows 六列对两种模板是同一套语义，各自写一遍迟早会漂移。
	if totals.UpstreamCostCNY != nil || totals.ProfitCNY != nil {
		ct := CostTotals{
			// 用 AmountCoveredCNY 而不是总金额：两者在有行没算成本时不相等，
			// 而成本只覆盖了其中一部分，拿总金额去减会算出一个虚高的利润。
			SettleCNY:    totals.AmountCoveredCNY,
			CostCNY:      derefFloat(totals.UpstreamCostCNY),
			ProfitCNY:    derefFloat(totals.ProfitCNY),
			PricedRows:   totals.Cost.Rows,
			TotalRows:    totals.Cost.TotalRows,
			ChannelCount: 0, // 模板二不按渠道展开，这个数对它没有意义
			// 用 opts.Rate() 而不是 params.ExchangeRate：后者可能是 0（未设置），
			// 而实际算成本时用的是回退后的默认汇率。写 0 会让摘要里的美金换算失去依据。
			RateCNYPerUSD: opts.Rate(),
		}
		result.CostTotals = &ct
		// 与模板一同样挂进 Summary，让结果区与列表读同一份数。
		result.Summary.CostTotals = &ct
	}
	return result, nil
}

// derefFloat 取指针的值，nil 当作 0。
//
// 只用在「已经确认过至少有一项非 nil」的地方（见上面的调用），
// 所以不存在把「没有数据」误当成 0 的风险。
func derefFloat(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// FormatSimpleBillSummary 生成简易账单那段可复制的文字。
//
// 与成本利润摘要一样，它是给人粘到聊天/邮件里的，所以数字口径必须与 xlsx 同源
// （都来自同一份 summaryRows），并且把「金额是站点实收额度折算」写在明处——
// 收件人若拿它和模板一的账单比，会看到两个不同的数（刊例×折扣 vs 额度折算），
// 不说清楚就会被当成算错了。
func FormatSimpleBillSummary(rows []SimpleBillRow, totals SimpleBillTotals, year, month int, header []string) string {
	var b strings.Builder
	for _, line := range header {
		b.WriteString(line)
		b.WriteByte('\n')
	}

	// 账期推断不出来时整行省略，而不是写「账期：0-0」。
	if month >= 1 && month <= 12 && year > 0 {
		fmt.Fprintf(&b, "账期：%d-%02d\n", year, month)
	} else if month >= 1 && month <= 12 {
		fmt.Fprintf(&b, "账期：%d月\n", month)
	}

	fmt.Fprintf(&b, "账单金额：¥%s\n", trimMoney(totals.TotalCostCNY))
	fmt.Fprintf(&b, "请求次数：%d 次；汇总行：%d 行\n", totals.HitCount, len(rows))

	// 成本三项：只要有行算出了成本就写，**并把覆盖率一起写出来**。
	//
	// 从前这里是「算不全就一个都不写」，结果是 504100 行里 392 行算不出来时，
	// 整段摘要里连成本两个字都没有。那既不诚实（明明算出来了 99.9%）也没用
	// （用户拿不到任何参考）。现在改成：给数 + 说清覆盖了多少行、没覆盖的是哪几类原因。
	//
	// 反过来，一行都没算出来时（倍率全没维护）仍然一个数都不给——
	// 那时报 0 会被读成「上游免费」，利润虚高，那是成本核算最不能出的错。
	if totals.UpstreamCostCNY != nil && totals.OfficialListUSD != nil && totals.ProfitCNY != nil {
		fmt.Fprintf(&b, "官方刊例：$%s\n", trimFixed(*totals.OfficialListUSD, 2))
		fmt.Fprintf(&b, "上游成本：¥%s\n", trimMoney(*totals.UpstreamCostCNY))
		margin := 0.0
		if totals.AmountCoveredCNY > 0 {
			// 分母用 AmountCoveredCNY 而不是总金额：两者在有行没算成本时不相等，
			// 用总金额会算出一个偏小的毛利率（成本只覆盖了一部分，金额却是全部的）。
			margin = *totals.ProfitCNY / totals.AmountCoveredCNY * 100
		}
		// 毛利率单独用逗号收尾，不套括号——与 FormatCostSummary 同一写法。
		fmt.Fprintf(&b, "利润：¥%s，毛利率 %s%%\n",
			trimMoney(*totals.ProfitCNY), trimPercent(margin))
		if skipped := totals.Cost.SkippedRows(); skipped > 0 {
			// 上面的毛利率分母只是「有成本的那部分金额」，所以它是**这部分**的，
			// 不是整张账单的。这条说明必须紧跟其后，否则会被当成整体毛利。
			fmt.Fprintf(&b, "成本覆盖：%d/%d 行，金额 ¥%s/¥%s\n",
				totals.Cost.Rows, totals.Cost.TotalRows,
				trimMoney(totals.AmountCoveredCNY), trimMoney(totals.TotalCostCNY))
			// 把「没覆盖的都是哪一类」写清楚：只说行数，用户还是要自己去翻日志。
			if desc := DescribeSkipReasons(totals.Cost.SkipReasons); desc != "" {
				fmt.Fprintf(&b, "未计入成本的原因：%s\n", desc)
			}
		}
	} else if totals.Cost.TotalRows > 0 && len(totals.Cost.SkipReasons) > 0 {
		// 一行都没算出来：如实说清是哪些行、什么原因，而不是静默省略——
		// 省略会让人以为这张表本来就不含成本，于是拿另一份有成本的账单去对，越对越乱。
		if desc := DescribeSkipReasons(totals.Cost.SkipReasons); desc != "" {
			fmt.Fprintf(&b, "成本：未能核算（%s）\n", desc)
		} else {
			b.WriteString("成本：未能核算\n")
		}
	}

	// 按分组给小计：客户通常按分组核对，给一份分组合计比只给总额更省一轮沟通。
	// 明细行数多时整段会很长，所以只列分组，不列到模型。
	groupTotals := map[string]float64{}
	var groupOrder []string
	for _, r := range rows {
		if _, seen := groupTotals[r.Group]; !seen {
			groupOrder = append(groupOrder, r.Group)
		}
		groupTotals[r.Group] += r.TotalCostCNY
	}
	if len(groupOrder) == 1 {
		// 只有一个分组时「分组小计」就是总额的复述，没有信息量。
		fmt.Fprintf(&b, "分组：%s\n", groupOrder[0])
	} else if len(groupOrder) > 1 {
		parts := make([]string, 0, len(groupOrder))
		for _, g := range groupOrder {
			parts = append(parts, fmt.Sprintf("%s ¥%s", g, trimMoney(groupTotals[g])))
		}
		fmt.Fprintf(&b, "分组小计：%s\n", strings.Join(parts, "；"))
	}

	// 口径说明写在最后：它是给人复制的，收件人必须知道这个金额是什么。
	// 与模板一的「刊例 × 折扣」是两个不同的数，不写清楚会被当成算错了。
	b.WriteString("注：金额为站点实际扣费额度 ÷ 500000，已扣除任务退款")
	return b.String()
}

// generateCostTable 生成成本利润表。若有渠道还没维护倍率，**不报错中断**，而是返回
// blocked=true 与待补录清单——用户此时已经填好了出账参数，硬报错会让他白填一遍。
//
// 返回 (成本利润表路径, 合计, 可复制文字, 是否被拦下, 未维护渠道, 未知渠道号, 错误)。
func generateCostTable(inputPath, templatePath, billPath string, rows [][]string, headers []string,
	book *PriceBook, params Params, ov DiscountOverrides, exchangeRate float64, preferPriceTable bool, year, month int) (
	string, *CostTotals, string, bool, []ChannelInfo, []int, error) {

	channelIDs, err := ExtractChannelIDs(headers, rows)
	if err != nil {
		return "", nil, "", false, nil, nil, err
	}
	status := CheckUpstreamRatios(channelIDs, params.ChannelUpstreamRatios, params.ChannelInfos)
	if len(status.Missing) > 0 {
		// 未维护倍率的渠道还**能**补录，先拦下把清单交给页面。
		return "", nil, "", true, status.Missing, status.UnknownChannelIDs, nil
	}
	// 未知渠道不拦：这类渠道业务库已查不到、填不了倍率，拦下来等于成本利润表永远出不来。
	// 它们的成本列留空且不计入合计，与页面提示、DEPLOY.md 的说法一致。

	costRows, err := AggregateCostByChannel(rows, headers, book, exchangeRate, preferPriceTable,
		nil, params.ChannelUpstreamRatios, params.ChannelNames)
	if err != nil {
		return "", nil, "", false, nil, nil, fmt.Errorf("成本聚合失败: %w", err)
	}

	outPath := costOutputPath(billPath)
	if err := WriteCostFromTemplate(templatePath, outPath, costRows, year, month, book,
		ov, exchangeRate, preferPriceTable, params.DomesticMarkers); err != nil {
		return "", nil, "", false, nil, nil, fmt.Errorf("写出成本利润表失败: %w", err)
	}
	// 合计与文字用与表内公式同源的口径算，避免结果区报的数与 xlsx 里的 SUM 对不上。
	totals, text := SummarizeCost(costRows, book, ov, exchangeRate,
		params.DomesticMarkers, year, month, params.SummaryHeader)
	// 未知渠道不拦生成，但必须如实报出：成本利润表里它们的成本列是空的，
	// 用户得知道是哪几个渠道号——否则会以为成本利润表已经算全了。
	return outPath, &totals, text, false, nil, status.UnknownChannelIDs, nil
}

// mergeManualPrices 把用户手动补全的价格写入 book.ByModel，作为「哪里都找不到定价」时的
// 最后兜底；已有官方价/报价表条目的模型不受影响（ResolvePrice 仍优先选官方价）。
func mergeManualPrices(book *PriceBook, manual map[string]ManualPriceInput) {
	for model, p := range manual {
		book.ByModel[model] = ModelPrice{
			InputPerM: p.InputPerM, OutputPerM: p.OutputPerM, Currency: "USD",
			Source: "manual_override", Category: "手动补全", Channel: "manual",
		}
	}
}

// buildSummary 生成给前端展示的逐行摘要。
//
// ov 必须与写账单时传的是同一份：这里算的是「结算额 = 刊例 × 结算系数」，
// 与账单 V 列同一公式。若两处用不同的折扣（比如这里固定不传覆盖），
// 页面上的折扣与金额会和刚下载的账单对不上——同一笔账两个数，比不显示更糟。
func buildSummary(agg *AggregateResult, book *PriceBook, exchangeRate float64, ov DiscountOverrides, missingPrices []string, manualMarkers []string) Summary {
	rowSummaries := make([]RowSummary, 0, len(agg.Rows))
	settleTotal, listTotal := 0.0, 0.0
	discountResult := ComputeGroupDiscounts(agg.Rows, book, exchangeRate, ov, manualMarkers)

	for _, a := range agg.Rows {
		list := 0.0
		// 阶梯表达式模型在价表里查不到条目，但刊例已由表达式算出，同样算「有价」。
		hasPrice := HasKnownListPrice(a)
		if hasPrice {
			list = OfficialListCNY(a, exchangeRate)
		}
		// 结算金额口径与账单 V 列一致：总金额 × 结算系数（按倍率结算的行用精确值）。
		// 日志 quota 折算出的金额只作为交叉校验，不再直接当作结算金额，
		// 否则账面上的 V 与这里报出的数会对不上。
		settle := round(list*discountResult.SettleFactor(a.Group), MoneyDecimals)
		settleTotal += settle
		listTotal += list
		rowSummaries = append(rowSummaries, RowSummary{
			// Group 用展示名（带倍率），与账单 C 列一致，页面摘要与账单不会对不上。
			Model: a.Model, Group: a.DisplayGroup(),
			Uncached: a.Uncached, CacheRead: a.CacheRead, Output: a.Output,
			CacheWrite5m: a.CacheWrite5m, CacheWrite1h: a.CacheWrite1h, Quota: a.Quota,
			SettleCNY: settle, ListCNY: list,
			Discount:     discountResult.Discounts[a.Group],
			SettleFactor: discountResult.SettleFactor(a.Group),
			Rows:         a.Rows, HasPrice: hasPrice,
		})
	}

	overallDisc := 0.0
	if listTotal > 0 {
		overallDisc = settleTotal / listTotal
	}

	return Summary{
		Year: agg.Year, Month: agg.Month, Rows: rowSummaries,
		SettleCNYTotal: settleTotal, ListCNYTotal: listTotal, OverallDiscount: overallDisc,
		MissingPriceModels: missingPrices, RowCount: agg.RowCount,
		CacheHitRows: agg.CacheHitRows, WebSearchRows: agg.WebSearchRows,
	}
}

var monthFilenameRe = regexp.MustCompile(`(\d{1,2})\s*月份?`)

func monthFromFilename(stem string) (int, bool) {
	m := monthFilenameRe.FindStringSubmatch(stem)
	if m == nil {
		return 0, false
	}
	var month int
	if _, err := fmt.Sscanf(m[1], "%d", &month); err != nil {
		return 0, false
	}
	if month < 1 || month > 12 {
		return 0, false
	}
	return month, true
}

func defaultOutputName(stem string) string {
	if strings.Contains(stem, "日志查询") {
		return strings.Replace(stem, "日志查询", "账单", 1)
	}
	if strings.Contains(stem, "日志") {
		return strings.Replace(stem, "日志", "账单", 1)
	}
	return stem + "_账单"
}

func defaultSanitizedName(stem string) string {
	if strings.Contains(stem, "日志查询") {
		return strings.Replace(stem, "日志查询", "脱敏日志", 1)
	}
	if strings.Contains(stem, "日志") {
		return strings.Replace(stem, "日志", "脱敏日志", 1)
	}
	return stem + "_脱敏日志"
}

// withCustomerSuffix 给产物文件名加客户名后缀，形如「账单_日志查询_..._钛动.xlsx」。
//
// 客户名放在**末尾**：成本利润表的名字是由账单名推出来的（见 costOutputPath，
// 把开头的「账单」换成「成本利润」），客户名插在中间会推出「成本利润_钛动_…」
// 这种读不通的形状，放末尾则三张表天然一致。
//
// 客户名是用户随手填的自由文本，可能含路径分隔符或 Windows 保留字符。
// 这里统一清洗：不清的话 filepath.Join 之后可能真正写到别的目录，
// 或者落盘直接失败（Windows 上 `:` `*` `?` 都是非法字符）。
// 清洗后没有实质内容（比如客户名就叫「///」）则不加后缀，退回原来的命名——
// 「账单___」这种文件名不提供任何信息，只是噪音。
func withCustomerSuffix(name, customer string) string {
	safe := sanitizeFileNamePart(customer)
	if strings.Trim(safe, "_") == "" {
		return name
	}
	return name + "_" + safe
}

// sanitizeFileNamePart 把一段自由文本洗成能安全做文件名的一部分。
func sanitizeFileNamePart(s string) string {
	// 路径分隔符与控制字符一律换成下划线；顺手压掉首尾空白。
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, s)
	s = strings.TrimSpace(s)

	// Windows 上文件名不能以点或空格结尾（会被默默截掉，或直接创建失败）。
	s = strings.TrimRight(s, ". ")

	// 客户名可能很长，也会被塞进 Content-Disposition。按**字符**截断而不是字节，
	// 否则中文会被砍成半个字变成乱码。
	const maxRunes = 40
	if runes := []rune(s); len(runes) > maxRunes {
		s = strings.TrimSpace(string(runes[:maxRunes]))
	}
	return s
}

// sanitizedFormatInfo 把「脱敏日志格式」参数解析为输出扩展名/分隔符；
// csv、tsv 是纯文本格式，没有 xlsx 单 sheet 104 万行的上限，适合超大日志。
func sanitizedFormatInfo(format string) (ext string, delimiter rune, isDelimited bool) {
	switch strings.ToLower(format) {
	case "csv":
		return ".csv", ',', true
	case "tsv":
		return ".tsv", '\t', true
	default:
		return ".xlsx", 0, false
	}
}

// simpleOutputName 简易账单的文件名：「日志查询_xxx」→「账单二_xxx」。
//
// 与模板一的「账单_xxx」刻意区分开：同一 job 目录里可能两种口径并存
// （比如对比核查），同名会互相覆盖，而且覆盖后光看文件名分不出是哪一种。
func simpleOutputName(stem string) string {
	if strings.Contains(stem, "日志查询") {
		return strings.Replace(stem, "日志查询", "账单二", 1)
	}
	if strings.Contains(stem, "日志") {
		return strings.Replace(stem, "日志", "账单二", 1)
	}
	return stem + "_账单二"
}

// simpleSanitizedName 简易账单配套的汇总脱敏日志文件名。
// simpleCostName 模板二成本表的文件名：账单二_xxx → 成本二_xxx。
//
// 沿用 simpleOutputName 那套「按前缀替换」的写法，而不是复用模板一的
// costOutputPath（那是从「账单」推「成本利润」）。两者产物不同：
// 模板一的成本表与账单同构（29 列 + 成本列），模板二的是汇总表（10 列）。
// 名字上区分开，用户一眼能看出这是哪种成本表。
func simpleCostName(stem string) string {
	if strings.Contains(stem, "日志查询") {
		return strings.Replace(stem, "日志查询", "成本二", 1)
	}
	if strings.Contains(stem, "日志") {
		return strings.Replace(stem, "日志", "成本二", 1)
	}
	return stem + "_成本二"
}

func simpleSanitizedName(stem string) string {
	if strings.Contains(stem, "日志查询") {
		return strings.Replace(stem, "日志查询", "脱敏日志二", 1)
	}
	if strings.Contains(stem, "日志") {
		return strings.Replace(stem, "日志", "脱敏日志二", 1)
	}
	return stem + "_脱敏日志二"
}

// attachLogSheet 把原始日志作为「日志查询」工作表附加到账单文件末尾。// 行数超过 ExcelMaxRowsPerSheet 时自动拆分到「日志查询_2」「日志查询_3」……多个 sheet。
func attachLogSheet(billPath string, headers []string, rows [][]string) error {
	f, err := excelize.OpenFile(billPath)
	if err != nil {
		return err
	}
	defer f.Close()

	const sheetBase = "日志查询"
	if idx, _ := f.GetSheetIndex(sheetBase); idx != -1 {
		if err := f.DeleteSheet(sheetBase); err != nil {
			return err
		}
	}

	headerRow := make([]interface{}, len(headers))
	for i, h := range headers {
		headerRow[i] = h
	}

	sheetName := sheetBase
	sheetIndex := 1
	if _, err := f.NewSheet(sheetName); err != nil {
		return err
	}
	if err := f.SetSheetRow(sheetName, "A1", &headerRow); err != nil {
		return err
	}

	rowInSheet := 1 // 已写入当前 sheet 的行数（含表头）
	for _, row := range rows {
		if rowInSheet >= ExcelMaxRowsPerSheet {
			sheetIndex++
			sheetName = fmt.Sprintf("%s_%d", sheetBase, sheetIndex)
			if _, err := f.NewSheet(sheetName); err != nil {
				return err
			}
			if err := f.SetSheetRow(sheetName, "A1", &headerRow); err != nil {
				return err
			}
			rowInSheet = 1
		}

		values := make([]interface{}, len(row))
		for j, v := range row {
			values[j] = cellValueForSanitized(v)
		}
		rowInSheet++
		axis, _ := excelize.CoordinatesToCellName(1, rowInSheet)
		if err := f.SetSheetRow(sheetName, axis, &values); err != nil {
			return err
		}
	}

	return f.SaveAs(billPath)
}
