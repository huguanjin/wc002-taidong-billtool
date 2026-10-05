package billing

import (
	"database/sql"
	"fmt"
	"time"
)

// 账单导出任务：一次「某客户某账期」的出账结果沉淀。
//
// 产物本身是临时的（在 jobDir 里 6 小时后自动清掉），这里**只存数字**——
// 账期、结算额、成本、利润。目的是回答「这个月给客户导了几张账单、成本多少利润多少」，
// 这个答案不该依赖任何文件是否还在磁盘上。

// BillTask 一条已完成的账单导出任务。
//
// 只记录**成功**的任务：失败的执行不写库。否则一次失败（该账期没消费记录、
// 渠道倍率没维护被拦、业务库连不上）会 UPSERT 掉上一次的好数字，
// 而页面上看起来「任务跑过了、有数」，实际上数已经没了。
type BillTask struct {
	ID           int64  `json:"id"`
	CustomerID   int64  `json:"customerId"`
	CustomerName string `json:"customerName"` // 冗余快照：客户改名后历史账单仍读得懂
	PeriodYear   int    `json:"periodYear"`
	PeriodMonth  int    `json:"periodMonth"`

	// SettleCNY 账单结算额合计（**全部行**），对应账单 V 列合计，是客户实际要付的钱。
	SettleCNY float64 `json:"settleCny"`
	// ListCNY 总金额合计（刊例），对应账单 S 列合计。
	ListCNY         float64 `json:"listCny"`
	OverallDiscount float64 `json:"overallDiscount"`

	// CostedSettleCNY 参与成本核算的那部分结算额，即 CostTotals.SettleCNY。
	//
	// **它和 SettleCNY 不是一个数**：渠道没维护上游倍率时，那些行既没有成本、
	// 也不该计入用于算利润的结算额。利润必须用 costed_settle − cost 算，
	// 用 settle_cny − cost 会把「没算成本的那部分」当成零成本，利润虚高。
	CostedSettleCNY *float64 `json:"costedSettleCny"`
	// CostCNY 上游成本合计。nil = 未生成成本利润表。
	CostCNY *float64 `json:"costCny"`
	// ProfitCNY 利润 = CostedSettleCNY − CostCNY。nil = 未生成成本利润表。
	ProfitCNY *float64 `json:"profitCny"`
	// CostComplete 为假表示有渠道没维护倍率，成本只覆盖了一部分行，
	// 此时的利润**不是整体毛利**，页面必须说明。
	CostComplete  bool `json:"costComplete"`
	PricedRows    int  `json:"pricedRows"`
	TotalCostRows int  `json:"totalCostRows"`

	RowCount    int       `json:"rowCount"` // 源日志行数
	StartTime   time.Time `json:"startTime"`
	EndTime     time.Time `json:"endTime"`
	LogPath     string    `json:"logPath"` // 源日志（在 dataDir，可见可手动清理）
	JobID       string    `json:"jobId"`   // 内存 job 登记，6 小时内可下载产物
	GeneratedAt time.Time `json:"generatedAt"`
}

// EnsureBillTaskSchema 建表（幂等），启动时调用一次。
func EnsureBillTaskSchema(cfg PGConfig) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS bill_export_tasks (
			id BIGSERIAL PRIMARY KEY,
			customer_id BIGINT NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			customer_name TEXT NOT NULL,
			period_year INTEGER NOT NULL,
			period_month INTEGER NOT NULL,
			settle_cny DOUBLE PRECISION NOT NULL,
			list_cny DOUBLE PRECISION NOT NULL DEFAULT 0,
			overall_discount DOUBLE PRECISION NOT NULL DEFAULT 0,
			costed_settle_cny DOUBLE PRECISION,
			cost_cny DOUBLE PRECISION,
			profit_cny DOUBLE PRECISION,
			cost_complete BOOLEAN NOT NULL DEFAULT false,
			priced_rows INTEGER NOT NULL DEFAULT 0,
			total_cost_rows INTEGER NOT NULL DEFAULT 0,
			row_count INTEGER NOT NULL DEFAULT 0,
			start_time TIMESTAMPTZ NOT NULL,
			end_time TIMESTAMPTZ NOT NULL,
			log_path TEXT NOT NULL DEFAULT '',
			job_id TEXT NOT NULL DEFAULT '',
			generated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (customer_id, period_year, period_month)
		)
	`); err != nil {
		return fmt.Errorf("创建 bill_export_tasks 表失败: %w", err)
	}
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_bill_export_tasks_period
		ON bill_export_tasks (period_year DESC, period_month DESC)
	`); err != nil {
		return fmt.Errorf("创建 bill_export_tasks 索引失败: %w", err)
	}
	return nil
}

// UpsertBillTask 写入任务结果：同一客户同一账期只保留一条，重跑即覆盖。
//
// 唯一键是 (customer_id, period_year, period_month)。重跑同一个客户同一个月的账，
// 语义上就是「刷新这次的数字」，而不是产生第二份账单——若追加写，
// 月度汇总里「导了 N 张账单」和利润都会被重复累加，汇总直接失真。
func UpsertBillTask(cfg PGConfig, t BillTask) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		INSERT INTO bill_export_tasks
			(customer_id, customer_name, period_year, period_month,
			 settle_cny, list_cny, overall_discount,
			 costed_settle_cny, cost_cny, profit_cny, cost_complete,
			 priced_rows, total_cost_rows, row_count,
			 start_time, end_time, log_path, job_id, generated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, now())
		ON CONFLICT (customer_id, period_year, period_month) DO UPDATE SET
			customer_name = EXCLUDED.customer_name,
			settle_cny = EXCLUDED.settle_cny,
			list_cny = EXCLUDED.list_cny,
			overall_discount = EXCLUDED.overall_discount,
			costed_settle_cny = EXCLUDED.costed_settle_cny,
			cost_cny = EXCLUDED.cost_cny,
			profit_cny = EXCLUDED.profit_cny,
			cost_complete = EXCLUDED.cost_complete,
			priced_rows = EXCLUDED.priced_rows,
			total_cost_rows = EXCLUDED.total_cost_rows,
			row_count = EXCLUDED.row_count,
			start_time = EXCLUDED.start_time,
			end_time = EXCLUDED.end_time,
			log_path = EXCLUDED.log_path,
			job_id = EXCLUDED.job_id,
			generated_at = now()
	`, t.CustomerID, t.CustomerName, t.PeriodYear, t.PeriodMonth,
		t.SettleCNY, t.ListCNY, t.OverallDiscount,
		t.CostedSettleCNY, t.CostCNY, t.ProfitCNY, t.CostComplete,
		t.PricedRows, t.TotalCostRows, t.RowCount,
		t.StartTime, t.EndTime, t.LogPath, t.JobID)
	if err != nil {
		return fmt.Errorf("写入账单任务失败: %w", err)
	}
	return nil
}

// BillTaskFilter 任务列表的筛选条件，零值表示不筛。
type BillTaskFilter struct {
	Year       int
	Month      int
	CustomerID int64
}

// ListBillTasks 任务列表，按账期倒序、同账期按客户名排序。
func ListBillTasks(cfg PGConfig, f BillTaskFilter) ([]BillTask, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	// 用 $n 占位 + 动态条件，而不是把零值当「筛 0 月」——
	// 那样筛选条件一为空就查不到任何行。
	where := "WHERE 1=1"
	args := []interface{}{}
	n := 0
	if f.Year > 0 {
		n++
		where += fmt.Sprintf(" AND period_year = $%d", n)
		args = append(args, f.Year)
	}
	if f.Month > 0 {
		n++
		where += fmt.Sprintf(" AND period_month = $%d", n)
		args = append(args, f.Month)
	}
	if f.CustomerID > 0 {
		n++
		where += fmt.Sprintf(" AND customer_id = $%d", n)
		args = append(args, f.CustomerID)
	}

	rows, err := db.Query(`
		SELECT id, customer_id, customer_name, period_year, period_month,
		       settle_cny, list_cny, overall_discount,
		       costed_settle_cny, cost_cny, profit_cny, cost_complete,
		       priced_rows, total_cost_rows, row_count,
		       start_time, end_time, log_path, job_id, generated_at
		FROM bill_export_tasks
		`+where+`
		ORDER BY period_year DESC, period_month DESC, customer_name
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("查询账单任务失败: %w", err)
	}
	defer rows.Close()

	out := []BillTask{}
	for rows.Next() {
		var t BillTask
		var costedSettle, cost, profit sql.NullFloat64
		if err := rows.Scan(&t.ID, &t.CustomerID, &t.CustomerName, &t.PeriodYear, &t.PeriodMonth,
			&t.SettleCNY, &t.ListCNY, &t.OverallDiscount,
			&costedSettle, &cost, &profit, &t.CostComplete,
			&t.PricedRows, &t.TotalCostRows, &t.RowCount,
			&t.StartTime, &t.EndTime, &t.LogPath, &t.JobID, &t.GeneratedAt); err != nil {
			return nil, fmt.Errorf("读取账单任务失败: %w", err)
		}
		if costedSettle.Valid {
			v := costedSettle.Float64
			t.CostedSettleCNY = &v
		}
		if cost.Valid {
			v := cost.Float64
			t.CostCNY = &v
		}
		if profit.Valid {
			v := profit.Float64
			t.ProfitCNY = &v
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取账单任务失败: %w", err)
	}
	return out, nil
}

// DeleteBillTask 删一条任务记录。**不删任何文件**——
// 产物在 jobDir 里由 6 小时清理兜底，源日志在 dataDir 里由「已导出文件」列表管理。
func DeleteBillTask(cfg PGConfig, id int64) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	res, err := db.Exec(`DELETE FROM bill_export_tasks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("删除账单任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("删除账单任务失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("任务记录不存在（可能已被删除）")
	}
	return nil
}

// PeriodSummary 一个账期的汇总。
type PeriodSummary struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	// TaskCount 该账期的账单任务数（一客户一账期一条，所以也是「客户数」）。
	TaskCount int `json:"taskCount"`
	// SettleCNY 帐单结算额合计（全部行）。这是对外报的数。
	SettleCNY float64 `json:"settleCny"`
	// CostCNY 上游成本合计，**只含已生成成本利润表且口径可用的任务**。
	CostCNY float64 `json:"costCny"`
	// ProfitCNY 利润合计 = CostedSettleCNY − CostCNY，只覆盖有成本的任务。
	ProfitCNY float64 `json:"profitCny"`
	// CostedSettleCNY 与 CostCNY/ProfitCNY 口径一致的结算额小计。页面展示毛利率要用它做分母。
	CostedSettleCNY float64 `json:"costedSettleCny"`
	// Margin 毛利率（百分比），= ProfitCNY / CostedSettleCNY × 100。
	Margin float64 `json:"margin"`
	// MissingCostCount 缺成本的任务数（未勾选生成成本利润表，或渠道倍率没维护被拦）。
	MissingCostCount int `json:"missingCostCount"`
	// PartialCostCount 有成本但不完整的任务数（部分渠道没维护倍率）。
	PartialCostCount int `json:"partialCostCount"`
}

// SummarizeTasks 按月汇总任务，供「每月导了多少账单、成本多少利润多少」。
//
// 口径是这里最关键的部分，三个金额各有各的用途，**不能混用**：
//
//	SettleCNY        —— Σ settle_cny，全部行的结算额，对外报账用
//	CostedSettleCNY  —— Σ costed_settle_cny，只含成本能对应上的那些行
//	CostCNY          —— Σ cost_cny
//	ProfitCNY        —— CostedSettleCNY − CostCNY
//
// 若把 ProfitCNY 写成 Σ settle_cny − Σ cost_cny，就等于把「没维护倍率、因而没算成本」
// 的那部分消费当成零成本，利润会虚高——渠道一多、漏维护一两个，这个数就明显偏大。
// 缺成本的任务数单独用 MissingCostCount/PartialCostCount 报出来，让用户知道覆盖率。
//
// 纯函数（不碰数据库），所以能直接测。
func SummarizeTasks(tasks []BillTask) []PeriodSummary {
	type key struct{ y, m int }
	order := []key{}
	byKey := map[key]*PeriodSummary{}

	for _, t := range tasks {
		k := key{t.PeriodYear, t.PeriodMonth}
		ps, ok := byKey[k]
		if !ok {
			ps = &PeriodSummary{Year: t.PeriodYear, Month: t.PeriodMonth}
			byKey[k] = ps
			order = append(order, k)
		}

		ps.TaskCount++
		ps.SettleCNY += t.SettleCNY

		if t.CostCNY == nil || t.CostedSettleCNY == nil {
			// 这个任务没有成本口径：只进 SettleCNY 与「缺成本」计数。
			// **绝不**把它的 settle_cny 加进利润的计算链路。
			ps.MissingCostCount++
			continue
		}

		ps.CostCNY += *t.CostCNY
		ps.CostedSettleCNY += *t.CostedSettleCNY
		if !t.CostComplete {
			ps.PartialCostCount++
		}
	}

	out := make([]PeriodSummary, 0, len(order))
	for _, k := range order {
		ps := byKey[k]
		ps.ProfitCNY = round(ps.CostedSettleCNY-ps.CostCNY, MoneyDecimals)
		ps.SettleCNY = round(ps.SettleCNY, MoneyDecimals)
		ps.CostCNY = round(ps.CostCNY, MoneyDecimals)
		ps.CostedSettleCNY = round(ps.CostedSettleCNY, MoneyDecimals)
		if ps.CostedSettleCNY > 0 {
			ps.Margin = round(ps.ProfitCNY/ps.CostedSettleCNY*100, 2)
		}
		out = append(out, *ps)
	}

	// 账期倒序，与任务列表的排序一致。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			if out[j].Year > out[j-1].Year ||
				(out[j].Year == out[j-1].Year && out[j].Month > out[j-1].Month) {
				out[j], out[j-1] = out[j-1], out[j]
				continue
			}
			break
		}
	}
	return out
}
