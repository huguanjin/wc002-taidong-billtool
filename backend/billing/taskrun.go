package billing

import (
	"fmt"
	"time"
)

// 一键出账任务的编排：导出客户日志 → 出账 →（可选）成本利润表 → 汇总落库。
//
// 这里只做编排，不碰全局状态（不登记 job、不删文件）——那些属于 HTTP 层的职责。
// 保持纯编排的好处是这条链路能用测试直接跑，不必起 HTTP 服务。

// TaskRunDeps 一次任务执行需要的外部依赖。
//
// 全部由调用方（handler）注入，而不是在函数里去读全局变量：
// 这样测试可以传临时目录与假配置，也避免 billing 包反向依赖 main 包的全局状态。
type TaskRunDeps struct {
	DB               DBConfig // 业务库（只读），用来导出日志
	PG               PGConfig // 本地库，读默认参数与渠道倍率
	DataDir          string   // 导出日志落这里（与「已导出文件」列表同目录）
	JobDir           string   // 出账产物落这里（6 小时后由 cleanupOldJobs 清理）
	TemplatePath     string   // data/bill_template.xlsx
	PriceTablePath   string   // data/price_table.xlsx
	DBPriceCachePath string   // data/db_price_cache.json

	CustomerID int64
	Year       int // 账期，必填且必须是单个月
	Month      int
	// GenerateCost 是否生成成本利润表（同时决定成本/利润是否有值）。
	GenerateCost bool
	// GenerateSanitized 是否生成脱敏日志。
	GenerateSanitized bool
}

// TaskRunResult 一次任务执行的结果。
type TaskRunResult struct {
	Customer            Customer
	Task                BillTask // 已算好待落库的记录（调用方负责 UpsertBillTask）
	BillPath            string
	SanitizedPath       string // 为空表示未生成
	CostPath            string // 为空表示未生成或被拦下
	CostBlocked         bool
	MissingChannelInfos []ChannelInfo
	UnknownChannelIDs   []int
	CostSummary         string // 可复制的成本利润摘要，为空表示无成本数据
	Summary             Summary
	LogPath             string // 导出的源日志
	LogRowCount         int64
}

// RunBillExportTask 执行一次「某客户某账期」的完整出账链路。
//
// 失败一律返回 error 且**不落库**——调用方只在成功时 UpsertBillTask。
// 这个约定很重要：任务是按 (客户, 账期) 覆盖写的，一次失败如果也写库，
// 会把上一次跑出来的好数字覆盖成空值，页面上看起来「跑过了」但数已经没了。
func RunBillExportTask(deps TaskRunDeps) (*TaskRunResult, error) {
	// ---- 1. 客户与账期校验 ----
	customer, err := GetCustomer(deps.PG, deps.CustomerID)
	if err != nil {
		return nil, fmt.Errorf("读取客户失败: %w", err)
	}
	usernames := customer.UsernameList()
	if len(usernames) == 0 {
		return nil, fmt.Errorf("客户「%s」还没有配置业务库账号，请先在客户信息页填写", customer.Name)
	}

	start, end, err := MonthRange(deps.Year, deps.Month)
	if err != nil {
		return nil, err
	}
	// 单月跨度必然 ≤31 天，正常不会触发；但账期是靠 year/month 传入的，
	// 这里再挡一道，将来若有人放开成任意区间，上限仍守着业务库的索引约束。
	if span := end.Sub(start); span > time.Duration(MaxExportDays)*24*time.Hour {
		return nil, fmt.Errorf("账期跨度 %.1f 天超过上限 %d 天",
			span.Hours()/24, MaxExportDays)
	}

	// ---- 2. 导出客户日志（只读业务库）----
	exported, err := ExportLogsFromDB(deps.DB, deps.DataDir, LogExportParams{
		Usernames: usernames,
		StartTime: start,
		EndTime:   end,
	})
	if err != nil {
		return nil, fmt.Errorf("导出该客户日志失败: %w", err)
	}
	// 0 行不是错误，但也没法出账——直接告诉用户这个账期没有消费，
	// 而不是生成一张全是 0 的账单让他自己发现。
	if exported.RowCount == 0 {
		return nil, fmt.Errorf("客户「%s」在 %s 没有消费记录（已按 %d 个账号查询）",
			customer.Name, PeriodLabel(deps.Year, deps.Month), len(usernames))
	}

	// ---- 3. 装载默认出账参数 ----
	settings, err := GetSettings(deps.PG)
	if err != nil {
		return nil, err
	}

	params := Params{
		// 账期显式传入，不靠推断：导出的日志文件名是
		// 「日志查询_<起>_<止>_<指纹>.tsv」，不含「N月」字样，
		// GenerateBill 的 monthFromFilename 认不出来，会落到日志内容的月份——
		// 跨月或有跨月补录时那个推断就错了。
		Year:                 deps.Year,
		Month:                deps.Month,
		Discount:             settings.Discount,
		ExchangeRate:         settings.ExchangeRate,
		PriceSource:          PriceSource(settings.PriceSource),
		SanitizedLog:         deps.GenerateSanitized,
		SanitizedFormat:      settings.SanitizedFormat,
		IncludeBillingParams: settings.IncludeBillingParams,
		DomesticMarkers:      settings.DomesticMarkerList(),
		GenerateCost:         deps.GenerateCost,
	}

	// 成本利润表需要渠道上游倍率；从本地 PG 读好传进去（billing 的算账逻辑不连 PG）。
	if deps.GenerateCost {
		ratios, err := ChannelRatioMap(deps.PG)
		if err != nil {
			return nil, err
		}
		channels, err := ListChannels(deps.PG)
		if err != nil {
			return nil, err
		}
		params.ChannelUpstreamRatios = ratios
		params.ChannelNames = make(map[int]string, len(channels))
		params.ChannelInfos = make(map[int]ChannelInfo, len(channels))
		for _, c := range channels {
			params.ChannelNames[c.ChannelID] = c.Name
			params.ChannelInfos[c.ChannelID] = c.ChannelInfo
		}
	}

	// ---- 4. 出账 ----
	gen, err := GenerateBill(exported.Path, deps.TemplatePath, deps.PriceTablePath,
		deps.DBPriceCachePath, deps.JobDir, params)
	if err != nil {
		return nil, err
	}

	// ---- 5. 组装任务记录 ----
	task := BillTask{
		CustomerID:      customer.ID,
		CustomerName:    customer.Name,
		PeriodYear:      deps.Year,
		PeriodMonth:     deps.Month,
		SettleCNY:       gen.Summary.SettleCNYTotal,
		ListCNY:         gen.Summary.ListCNYTotal,
		OverallDiscount: gen.Summary.OverallDiscount,
		RowCount:        int(exported.RowCount),
		StartTime:       start,
		EndTime:         end,
		LogPath:         exported.Path,
	}
	// 只有成本利润表真的生成出来了，才有成本口径可落。
	// 被拦下（渠道倍率没维护）时三个成本字段保持 nil，页面据此显示「缺成本」——
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
		Customer:            customer,
		Task:                task,
		BillPath:            gen.BillPath,
		SanitizedPath:       gen.SanitizedPath,
		CostPath:            gen.CostPath,
		CostBlocked:         gen.CostBlocked,
		MissingChannelInfos: gen.MissingChannelInfos,
		UnknownChannelIDs:   gen.UnknownChannelIDs,
		CostSummary:         gen.CostSummary,
		Summary:             gen.Summary,
		LogPath:             exported.Path,
		LogRowCount:         exported.RowCount,
	}, nil
}
