package billing

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 一键出账任务的编排：按计划的时段导出客户日志 → 出账 →（可选）成本利润表。
//
// 这里只做编排，不碰全局状态（不登记 job、不删文件）——那些属于 HTTP 层的职责。
// 保持纯编排的好处是这条链路能用测试直接跑，不必起 HTTP 服务。

// DisplayName 计划的展示名。用户没起名时用「客户 + 时段」兜底，
// 让列表里每条都读得懂，而不是一排空白。
func (t BillTask) DisplayName() string {
	if t.Name != "" {
		return t.Name
	}
	if t.StartTime != nil && t.EndTime != nil {
		return fmt.Sprintf("%s %s~%s", t.CustomerName,
			t.StartTime.Format("01-02"), t.EndTime.Format("01-02"))
	}
	return t.CustomerName
}

// summaryHeader 拼出成本利润摘要开头的定位行：客户、账号、时段。
//
// 只在这里拼，因为这是唯一同时握着 customer 与 start/end 的地方；
// billing 包不认识「客户」，也不知道时段从哪来，只负责把这几行原样印出来。
//
// 时段用北京时间墙上时间、精确到秒，与页面 datetime-local 的口径一致。
// 不要图省事用 task.StartAt()——那是给输入框用的 "2026-09-01T00:00:00"，
// T 分隔符不是给人读的。
func summaryHeader(customer Customer, start, end time.Time) []string {
	lines := []string{fmt.Sprintf("客户：%s", customer.Name)}

	// 账号可能配了多个（换行/逗号分隔），全部写出来：收件人据此确认覆盖范围，
	// 少写一个就可能被当成「这部分没算进去」。
	if usernames := customer.UsernameList(); len(usernames) > 0 {
		lines = append(lines, fmt.Sprintf("账号：%s", strings.Join(usernames, "、")))
	}

	lines = append(lines, fmt.Sprintf("时段：%s ~ %s",
		start.In(cstLocation).Format("2006-01-02 15:04:05"),
		end.In(cstLocation).Format("2006-01-02 15:04:05")))

	return lines
}

// TaskRunDeps 一次任务执行需要的外部依赖。
//
// 全部由调用方（handler）注入，而不是在函数里去读全局变量：
// 这样测试可以传临时目录与假配置，也避免 billing 包反向依赖 main 包的全局状态。
type TaskRunDeps struct {
	DB               DBConfig // 业务库（只读），用来导出日志
	PG               PGConfig // 本地库，读计划、客户、默认参数与渠道倍率
	DataDir          string   // 导出日志落这里（与「已导出文件」列表同目录）
	JobDir           string   // 出账产物落这里（6 小时后由 cleanupOldJobs 清理）
	TemplatePath     string   // data/bill_template.xlsx
	PriceTablePath   string   // data/price_table.xlsx
	DBPriceCachePath string   // data/db_price_cache.json

	// TaskID 要执行的**计划** ID。客户、时段、勾选全部来自这条计划，
	// 不再由调用方拼参数——那样「先建好计划、之后批量执行」就失去意义了。
	TaskID int64

	// SkipUpstreamReview 跳过「核对上游倍率与国模标识」这一步（即便计划勾了 ReviewUpstream）。
	//
	// 用户在核对弹窗里点了「继续」之后的那次重跑必须置位，否则每次重跑又会停在同一个
	// 弹窗上，永远出不了账。它是**一次性**的：只属于这次调用，不落到计划上——
	// 下一次从列表里点执行，计划勾了核对就照样会再弹。
	SkipUpstreamReview bool
}

// TaskRunResult 一次任务执行的结果。
type TaskRunResult struct {
	TaskID              int64
	Customer            Customer
	Task                BillTask // 结果字段已填好，调用方负责 SaveBillTaskResult
	BillPath            string
	SanitizedPath       string // 为空表示未生成
	CostPath            string // 为空表示未生成或被拦下
	CostBlocked         bool
	MissingChannelInfos []ChannelInfo
	UnknownChannelIDs   []int
	CostSummary         string // 可复制的成本利润摘要，为空表示无成本数据
	// BillSummary 简易账单（模板二）的可复制文字，为空表示本次用的是标准模板。
	// 与 CostSummary 互斥：简易账单不产成本利润表，标准模板不产账单摘要。
	BillSummary string
	// ChannelCheck 非 nil 表示本次执行**没出账**，而是被成本核算预检拦下了：
	// 日志里有渠道没维护上游倍率，需要用户就地补录后重跑。
	//
	// 它不是错误（所以没有走 error 返回）：调用方应把它当作一个可继续的中间态，
	// 去渲染补录界面，而不是报失败。此时 Task 里的结果字段全是零值，
	// **不能落库**——否则会把上一次跑出来的好数字覆盖成空。
	ChannelCheck *ChannelCheckResult
	Summary      Summary
	LogPath      string // 导出的源日志
	LogRowCount  int64
}

// needsUpstreamReview 这次执行要不要停在「核对上游倍率与国模标识」。
//
// 四个条件缺一不可，抽成函数是为了能单独测每一个：
//   - 计划勾了 ReviewUpstream，且确实在做成本核算（没有成本就没有上游数据可核对）；
//   - 本次没有 skip——这是防**无限弹窗**的那一道：用户在弹窗里点了继续，重跑时必须放行，
//     漏了这个判断，每次重跑又停回同一个弹窗，永远出不了账；
//   - 日志里观测到了渠道：一个渠道都识别不出时弹个空表只会让人困惑，
//     那种日志的问题（取不到渠道号）账单备注里会如实报出。
func needsUpstreamReview(task BillTask, skip bool, observedChannels int) bool {
	return (task.CheckCost || task.GenerateCost) &&
		task.ReviewUpstream && !skip && observedChannels > 0
}

// RunBillExportTask 执行一条计划。
//
// 失败一律返回 error 且**不落库**——调用方只在成功时 SaveBillTaskResult。
// 这个约定很重要：执行结果是覆盖写的，一次失败如果也写库，
// 会把上一次跑出来的好数字覆盖成空值，页面上看起来「跑过了」但数已经没了。
// 批量执行时这条同样成立：单个任务失败不能影响其他任务已成功的记录。
func RunBillExportTask(deps TaskRunDeps) (*TaskRunResult, error) {
	// ---- 1. 读计划并校验时段 ----
	task, err := GetBillTask(deps.PG, deps.TaskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("任务不存在（可能已被删除）")
		}
		return nil, fmt.Errorf("读取任务失败: %w", err)
	}
	if task.StartTime == nil || task.EndTime == nil {
		return nil, fmt.Errorf("任务「%s」还没有设置时段，请先编辑填写起止日期", task.DisplayName())
	}
	start, end := *task.StartTime, *task.EndTime

	// 计划保存时已校验过，这里再挡一道：库里的行可能被直接改过，
	// 而这条上限守的是业务库的索引约束（logs 表在 (username, created_at) 上没组合索引）。
	if span := end.Sub(start); span > time.Duration(MaxExportDays)*24*time.Hour {
		return nil, fmt.Errorf("时段跨度 %.1f 天超过上限 %d 天，请拆成多个计划",
			span.Hours()/24, MaxExportDays)
	}

	// ---- 2. 客户与账号 ----
	customer, err := GetCustomer(deps.PG, task.CustomerID)
	if err != nil {
		return nil, fmt.Errorf("读取客户失败: %w", err)
	}
	usernames := customer.UsernameList()
	if len(usernames) == 0 {
		return nil, fmt.Errorf("客户「%s」还没有配置业务库账号，请先在客户信息页填写", customer.Name)
	}

	// ---- 3. 导出客户日志（只读业务库）----
	exported, err := ExportLogsFromDB(deps.DB, deps.DataDir, LogExportParams{
		Usernames: usernames,
		StartTime: start,
		EndTime:   end,
	})
	if err != nil {
		return nil, fmt.Errorf("导出该客户日志失败: %w", err)
	}
	// 0 行不是错误，但也没法出账——直接告诉用户这个时段没有消费，
	// 而不是生成一张全是 0 的账单让他自己发现。
	if exported.RowCount == 0 {
		return nil, fmt.Errorf("客户「%s」在 %s ~ %s 没有消费记录（已按 %d 个账号查询）",
			customer.Name, start.In(cstLocation).Format("2006-01-02"),
			end.In(cstLocation).Format("2006-01-02"), len(usernames))
	}

	// 两个预检都要读一遍日志，这里做一次惰性加载共享结果。
	//
	// 不能各读各的：日志常是几百 MB（实测 548MB），多读一遍就是多几十秒，
	// 而且两个预检本来就该看同一份数据，各自读还可能读到不一致的中间状态。
	var preHeaders []string
	var preRows [][]string
	preLoaded := false
	loadForCheck := func() ([]string, [][]string, error) {
		if preLoaded {
			return preHeaders, preRows, nil
		}
		h, r, err := LoadLogRows(exported.Path, "", "")
		if err != nil {
			return nil, nil, err
		}
		preHeaders, preRows, preLoaded = h, r, true
		return h, r, nil
	}

	// 上游配置（倍率 + 国模标识 + 渠道清单）只读一次，预检、核对弹窗、出账三处共用。
	//
	// 不能各读各的：核对弹窗给用户看的、预检判断的、出账实际用的，必须是同一份数——
	// 分几次读，中间被别处改一下，就会出现「弹窗里看着是新的、账单按旧的算」。
	type upstreamConfig struct {
		ratios   map[int]float64
		domestic map[int]bool
		channels []ChannelWithRatio
	}
	var upCfg *upstreamConfig
	loadUpstream := func() (*upstreamConfig, error) {
		if upCfg != nil {
			return upCfg, nil
		}
		ratios, domestic, err := ChannelUpstreamConfig(deps.PG)
		if err != nil {
			return nil, err
		}
		channels, err := ListChannels(deps.PG)
		if err != nil {
			return nil, err
		}
		upCfg = &upstreamConfig{ratios: ratios, domestic: domestic, channels: channels}
		return upCfg, nil
	}

	// ---- 3.5 成本核算预检：日志用到的渠道是否都维护了上游倍率 ----
	//
	// 放在这里（导出之后、出账之前）是唯一可选的时点：要判断缺哪些倍率，
	// 必须先知道这份日志用到哪些渠道，而渠道集合只有导出后才知道。
	//
	// 缺倍率时**不是错误**，而是返回一个可继续的中间态（同 CostBlocked 的哲学）：
	// 用户已经填好计划参数，硬报错会让他白填一遍。页面拿到 blocked 结果后就地补录，
	// 保存后重新执行即可。
	var blocked *ChannelCheckResult
	if task.CheckCost || task.GenerateCost {
		headers, rows, rerr := loadForCheck()
		if rerr != nil {
			return nil, fmt.Errorf("读取已导出的日志失败: %w", rerr)
		}

		cfg, cerr := loadUpstream()
		if cerr != nil {
			return nil, cerr
		}
		ratios, domestic := cfg.ratios, cfg.domestic
		infoMap := make(map[int]ChannelInfo, len(cfg.channels))
		for _, c := range cfg.channels {
			infoMap[c.ChannelID] = c.ChannelInfo
		}

		// 按**行**判据统计（与出账侧同一个 RowCostReason），而不是按渠道。
		//
		// 这是修一个真实故障：从前这里用 ExtractChannelUsage，而它会把
		// 「没有渠道号的行」直接跳过。于是这些行对预检完全隐形，预检放行，
		// 出账时它们却被判为缺失——用户看到「预检通过」却在账单上读到
		// 「392 行缺少渠道倍率或分组倍率」，而且重跑再也不会弹出补录界面。
		counts, missingRatios, rowsPerChannel := CountRowCostReasons(headers, rows, ratios)

		// 用同一份行级统计填结果，无论走不走 CheckChannelRatios 都是这几个数——
		// 免得同一个「缺多少行」在两条分支上有两个来源。
		fill := func(c *ChannelCheckResult) {
			c.UncostableRows = map[string]int{}
			c.UncostableTotal = 0
			for reason, n := range counts {
				// zero_delta 不计入：额度为 0 的行不影响成本，
				// 算进去会让用户去补一批无关的倍率。
				if reason == SkipNone || reason == SkipZeroDelta || n == 0 {
					continue
				}
				c.UncostableRows[string(reason)] = n
				c.UncostableTotal += n
			}
			c.MissingGroupRatioRows = counts[SkipNoGroupRatio]
			c.TotalRows = len(rows)
		}

		check := ChannelCheckResult{}
		fill(&check)

		// 渠道观测事实（行数、金额、分组、模型）：核对弹窗与待补录面板都要，只扫一次。
		var obs map[int]*ChannelObservation
		observe := func() map[int]*ChannelObservation {
			if obs == nil {
				obs = ObserveChannels(headers, rows)
			}
			return obs
		}

		// ---- 3.55 核对上游倍率与国模标识（计划勾了 ReviewUpstream 时）----
		//
		// 放在「缺倍率就拦」**之前**：核对弹窗里已经包含了缺倍率的渠道（高亮、必须填），
		// 用户在那里一次改完，就不会在核对之后又撞上第二道拦截。
		//
		// 日志里一个渠道都识别不出来时跳过：没有东西可核对，弹一个空表只会让人困惑，
		// 那种日志的问题（取不到渠道号）账单备注里会如实报出。
		if needsUpstreamReview(task, deps.SkipUpstreamReview, len(observe())) {
			review := check
			review.NeedsUpstreamReview = true
			review.ReviewChannels = BuildChannelReview(observe(), ratios, domestic, infoMap)
			return &TaskRunResult{TaskID: task.ID, Task: task, Customer: customer, ChannelCheck: &review}, nil
		}

		// 待补录渠道：**含渠道清单里查不到的**。倍率表以 channel_id 为主键，
		// 与清单无关，所以清单没拉到的渠道照样能填——把它们排除在外，
		// 就是那 392 行永远算不出成本、界面上还没地方可填的原因。
		//
		// 只有当确实存在「有渠道号但没倍率」的行时才走这条路径（missingRatios 非空）。
		if len(missingRatios) > 0 {
			usage, _ := ExtractChannelUsage(headers, rows)
			check = CheckChannelRatios(usage, ratios, domestic, infoMap, rowsPerChannel)
			fill(&check)
			// 补录面板里填倍率的同时要能判断「这是不是国模渠道」，所以带上模型信息。
			EnrichIssues(check.Missing, observe())
		}

		// 拦下的判据：只要存在**没维护倍率的渠道**就拦。
		//
		//	渠道没填倍率（无论它在不在本地渠道清单里）→ 拦住，填完就能算
		//	日志缺 group_ratio / 没渠道号 / 多渠道路由 → 不拦，补倍率也没用，
		//	    它们在账单上如实报出「N 行未计入成本」
		//
		// 「在不在渠道清单里」**不参与这个判断**：倍率表以 channel_id 为主键，
		// 清单里没有的渠道照样能填。从前把清单外的渠道归成"补不了"而不拦，
		// 结果是一批新渠道永远算不出成本、页面上还没地方可填——
		// 用户只看到账单上那句「392 行…」，却找不到任何入口。
		if len(check.Missing) > 0 {
			blocked = &check
		}
		if blocked != nil {
			return &TaskRunResult{TaskID: task.ID, Task: task, Customer: customer, ChannelCheck: blocked}, nil
		}
		// 没被拦下时不另外传话给出账侧：出账侧用同一个 RowCostReason 自己算，
		// 两边的数必然一致。多传一份反而多一个可能不同步的地方。
	}

	// ---- 4. 装载默认出账参数 ----
	settings, err := GetSettings(deps.PG)
	if err != nil {
		return nil, err
	}

	// 该客户手工维护的「分组 → 折扣」：线下谈定、new-api 里没及时更新的那些。
	//
	// 只在勾了「使用自定义折扣」时才装载：不勾就该完全按机器算出来的折扣出账，
	// 连读都不必读——读进来又不用，只会让人误以为它生效了。
	manualDiscounts := map[string]float64{}
	if task.UseManualDiscount {
		var err error
		manualDiscounts, err = CustomerGroupDiscountMap(deps.PG, task.CustomerID)
		if err != nil {
			// 读失败**不能静默降级**——降级的表现是照常出一张按过期倍率算出来的账单，
			// 金额是错的却不报错，比直接失败危险得多。这里让本次任务失败。
			return nil, fmt.Errorf("读取客户手工折扣失败: %w", err)
		}

		// ---- 3.6 线下折扣预检：勾了「使用自定义折扣」，日志里的分组就都得填过 ----
		//
		// 缺了照样拦下（不报错），让用户就地补：一半按线下折扣、一半按反推的账单
		// 是最坏的结果——客户会拿着两个不同口径的折扣来问为什么。
		headers, rows, rerr := loadForCheck()
		if rerr != nil {
			return nil, fmt.Errorf("读取已导出的日志失败: %w", rerr)
		}
		if missing := MissingGroupDiscounts(headers, rows, manualDiscounts); len(missing) > 0 {
			return &TaskRunResult{
				TaskID: task.ID, Task: task, Customer: customer,
				ChannelCheck: &ChannelCheckResult{MissingDiscountGroups: missing},
			}, nil
		}
	}

	params := Params{
		// 账期用计划上存的**归属账期**，不靠推断：
		// 导出的日志文件名是「日志查询_<起>_<止>_<指纹>.tsv」，不含「N月」字样，
		// GenerateBill 的 monthFromFilename 认不出来，会落到日志内容的月份——
		// 跨月或跨月补录时那个推断就错了。
		Year:                 task.PeriodYear,
		Month:                task.PeriodMonth,
		Discount:             settings.Discount,
		ManualDiscounts:      manualDiscounts,
		ExchangeRate:         settings.ExchangeRate,
		PriceSource:          PriceSource(settings.PriceSource),
		SanitizedLog:         task.GenerateSanitized,
		SanitizedFormat:      settings.SanitizedFormat,
		IncludeBillingParams: settings.IncludeBillingParams,
		DomesticMarkers:      settings.DomesticMarkerList(),
		GenerateCost:         task.GenerateCost,
		CheckCost:            task.CheckCost,
		BillTemplate:         task.BillTemplate,
		SummaryHeader:        summaryHeader(customer, start, end),
		// 产物文件名带上客户名：一个 job 目录里可能同时躺着好几个客户的表，
		// 下载到本地后全叫「账单_xxx.xlsx」就分不清了。
		CustomerName: customer.Name,
	}

	// 渠道上游倍率：两条模板路径都要，但用途不同——
	//
	//	模板一：GenerateCost 时用来算那张独立的成本利润表
	//	模板二：CheckCost 时用来填账单上的成本三列（见 AggregateSimpleBill）
	//
	// 两者的门槛写在一起，是为了让「成本列什么时候有值」与「预检什么时候跑」
	// 保持同一个条件。分开写迟早会漂移，表现是账单上的成本列全空而预检明明跑过了。
	if (task.GenerateCost && !IsSimpleBillTemplate(task.BillTemplate)) ||
		(task.CheckCost && IsSimpleBillTemplate(task.BillTemplate)) {
		// 与预检用的是同一次读出来的配置（见 loadUpstream），预检看到的与账单用的不会两样。
		cfg, err := loadUpstream()
		if err != nil {
			return nil, err
		}
		params.ChannelUpstreamRatios = cfg.ratios
		// 国模标识与倍率成对：折扣 = f(倍率, 是否国模)。只传倍率不传标识，
		// 国模渠道就会按海外口径多除一个 7，成本整体偏低 7 倍而毫无提示。
		params.ChannelDomestic = cfg.domestic

		// 模板二只用这份清单来判断「这个缺倍率的渠道还有没有救」：
		// 清单里有的可以补录，没有的（业务库已删）补不了，账单备注里要分开说。
		params.ChannelKnownIDs = make(map[int]bool, len(cfg.channels))
		for _, c := range cfg.channels {
			params.ChannelKnownIDs[c.ChannelID] = true
		}
		// 渠道名与渠道信息只有模板一写成本利润表时才用得上。
		if !IsSimpleBillTemplate(task.BillTemplate) {
			params.ChannelNames = make(map[int]string, len(cfg.channels))
			params.ChannelInfos = make(map[int]ChannelInfo, len(cfg.channels))
			for _, c := range cfg.channels {
				params.ChannelNames[c.ChannelID] = c.Name
				params.ChannelInfos[c.ChannelID] = c.ChannelInfo
			}
		}
	}

	// ---- 5. 出账 ----
	gen, err := GenerateBill(exported.Path, deps.TemplatePath, deps.PriceTablePath,
		deps.DBPriceCachePath, deps.JobDir, params)
	if err != nil {
		return nil, err
	}

	// ---- 6. 组装结果（只填结果字段，计划字段原样保留）----
	settle := gen.Summary.SettleCNYTotal
	list := gen.Summary.ListCNYTotal
	discount := gen.Summary.OverallDiscount
	task.SettleCNY = &settle
	task.ListCNY = &list
	task.OverallDiscount = &discount
	task.RowCount = int(exported.RowCount)
	task.LogPath = exported.Path
	task.CustomerName = customer.Name // 刷新冗余快照，客户改过名的话跟上

	// 可复制摘要一并落库：两种模板只会有一段，谁非空存谁。
	//
	// 不能存两列（成本摘要 + 账单摘要）：它们互斥，存两列就要在每个读取点
	// 各判一次「该看哪个」，漏一处就显示成空白或显示成另一种口径的文字。
	task.SummaryText = gen.BillSummary
	if task.SummaryText == "" {
		task.SummaryText = gen.CostSummary
	}

	// 只有成本利润表真的生成出来了，才有成本口径可落。
	// 被拦下（渠道倍率没维护）时三个成本字段保持 nil，页面据此显示「未核算成本」——
	// 这比写 0 安全：0 会被读成「上游免费」，利润虚高。
	if gen.CostTotals != nil {
		costedSettle := gen.CostTotals.SettleCNY
		cost := gen.CostTotals.CostCNY
		profit := gen.CostTotals.ProfitCNY
		task.CostedSettleCNY = &costedSettle
		task.CostCNY = &cost
		task.ProfitCNY = &profit
		task.CostComplete = gen.CostTotals.PricedRows == gen.CostTotals.TotalRows
		task.PricedRows = gen.CostTotals.PricedRows
		task.TotalCostRows = gen.CostTotals.TotalRows
	}

	return &TaskRunResult{
		TaskID:              task.ID,
		Customer:            customer,
		Task:                task,
		BillPath:            gen.BillPath,
		SanitizedPath:       gen.SanitizedPath,
		CostPath:            gen.CostPath,
		CostBlocked:         gen.CostBlocked,
		MissingChannelInfos: gen.MissingChannelInfos,
		UnknownChannelIDs:   gen.UnknownChannelIDs,
		CostSummary:         gen.CostSummary,
		BillSummary:         gen.BillSummary,
		Summary:             gen.Summary,
		LogPath:             exported.Path,
		LogRowCount:         exported.RowCount,
	}, nil
}
