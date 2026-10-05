package billing

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// 账单导出任务 = 一个**可维护的计划** + 它的**最近一次执行结果**。
//
// 为什么不是「一次执行的结果」：一个客户往往要多个计划——月度对账一条，
// 按周导的那些每周一条。若按 (客户, 账期) 唯一，同一客户同一月只留得下一条，
// 周度任务跑四次就互相覆盖，只剩最后一次。所以计划的唯一性来自它自己。
//
// 产物本身仍是临时的（jobDir 里 6 小时后清掉），表里只沉淀数字。

// BillTask 一条账单导出计划（含最近一次执行结果）。
//
// 计划字段（名称/客户/时段/勾选）与结果字段（结算额/成本/利润）是两组东西：
// 编辑计划不该清掉上次的结果，执行只该更新结果。
type BillTask struct {
	ID           int64  `json:"id"`
	CustomerID   int64  `json:"customerId"`
	CustomerName string `json:"customerName"` // 冗余快照：客户改名后计划列表仍读得懂
	// Name 计划名，如「9月第1周」。空则页面用「客户 + 时段」兜底显示。
	Name string `json:"name"`

	// PeriodYear/PeriodMonth 归属账期：由 StartTime 推导（按 +08:00 取开始日所在月），
	// 跨月任务整个计到开始月。仅用于「按月汇总」归集，不代表实际时段。
	PeriodYear  int `json:"periodYear"`
	PeriodMonth int `json:"periodMonth"`

	// StartTime/EndTime 计划的导出时段（北京时间闭区间）。**未执行的计划这两个值为零**
	// （库里是 NULL），所以是 *time.Time 而不是 time.Time。
	StartTime *time.Time `json:"startTime"`
	EndTime   *time.Time `json:"endTime"`

	// GenerateSanitized/GenerateCost 执行时的勾选，存在计划上——
	// 否则每次执行都要重选，「先建好计划、之后批量执行」就没意义了。
	GenerateSanitized bool `json:"generateSanitized"`
	GenerateCost      bool `json:"generateCost"`

	// ---- 以下为最近一次执行结果。未执行时全为零值 ----

	// SettleCNY 账单结算额合计（**全部行**），对应账单 V 列合计，是客户实际要付的钱。
	SettleCNY *float64 `json:"settleCny"`
	// ListCNY 总金额合计（刊例），对应账单 S 列合计。
	ListCNY         *float64 `json:"listCny"`
	OverallDiscount *float64 `json:"overallDiscount"`

	// CostedSettleCNY 参与成本核算的那部分结算额，即 CostTotals.SettleCNY。
	//
	// **它和 SettleCNY 不是一个数**：渠道没维护上游倍率时，那些行既没有成本、
	// 也不该计入用于算利润的结算额。利润必须用 costed_settle − cost 算，
	// 用 settle_cny − cost 会把「没算成本的那部分」当成零成本，利润虚高。
	CostedSettleCNY *float64 `json:"costedSettleCny"`
	// CostCNY 上游成本合计。nil = 未生成成本利润表（或被渠道倍率拦下）。
	CostCNY *float64 `json:"costCny"`
	// ProfitCNY 利润 = CostedSettleCNY − CostCNY。
	ProfitCNY *float64 `json:"profitCny"`
	// CostComplete 为假表示有渠道没维护倍率，成本只覆盖了一部分行，
	// 此时的利润**不是整体毛利**，页面必须说明。
	CostComplete  bool `json:"costComplete"`
	PricedRows    int  `json:"pricedRows"`
	TotalCostRows int  `json:"totalCostRows"`

	RowCount    int        `json:"rowCount"`  // 源日志行数
	LogPath     string     `json:"logPath"`   // 源日志（在 dataDir，可见可手动清理）
	JobID       string     `json:"jobId"`     // 内存 job 登记，6 小时内可下载产物
	RunCount    int        `json:"runCount"`  // 执行过几次
	LastRunAt   *time.Time `json:"lastRunAt"` // 最近一次执行时间；NULL = 从未执行
	GeneratedAt time.Time  `json:"generatedAt"`
}

// HasRun 该计划是否执行过。
//
// 用它区分「还没跑」与「跑了但没成本」——这两种在金额列上都是空，
// 但含义完全不同：前者不该计入月度汇总，后者要计入并标注成本不全。
func (t BillTask) HasRun() bool { return t.LastRunAt != nil }

// WallClockLayout 北京时间墙上时间的展示格式，精确到秒。
//
// 用它而不是 RFC3339：前端的 <input type="datetime-local"> 是没有时区概念
// 的墙上时间，若把带偏移的 RFC3339 直接喂给它，浏览器会按**本地时区**换算——
// 服务端在 UTC 时，来回编辑一次计划就整体偏 8 小时，而用户看不到任何提示。
// 所以接口统一给出「北京时间看上去是什么样」的字符串，前端原样绑定。
const WallClockLayout = "2006-01-02T15:04:05"

// StartAt 开始时刻的北京时间墙上时间，形如 "2026-09-01T00:00:00"。
// 未设置时段时是空串——不能给零值的 "0001-01-01T00:00:00"，
// 那会让前端的日期输入框显示一个荒唐的日期。
func (t BillTask) StartAt() string {
	if t.StartTime == nil {
		return ""
	}
	return t.StartTime.In(cstLocation).Format(WallClockLayout)
}

// EndAt 结束时刻的北京时间墙上时间。同 StartAt。
func (t BillTask) EndAt() string {
	if t.EndTime == nil {
		return ""
	}
	return t.EndTime.In(cstLocation).Format(WallClockLayout)
}

// billTaskWire 是 BillTask 的镜像类型：同样的字段与 JSON tag，但**没有方法**。
//
// 必须有它，否则会栈溢出：若直接嵌入 BillTask，后者的 MarshalJSON 会被提升到
// taskJSON 上，json.Marshal(taskJSON) 又调回 BillTask.MarshalJSON，无限递归。
// Go 的定义类型不继承方法，所以这里断得干净。
type billTaskWire BillTask

// taskJSON 是 BillTask 对外序列化的形态：原始字段 + 两个墙上时间字符串。
//
// 为什么不给 BillTask 直接加 StartAt/EndAt 字段：StartTime/EndTime 是 *time.Time，
// 参与全部业务逻辑；墙上时间是**同一份数据的另一种表示**，只给前端用。
// 两套字段并存就会有两个真相来源，迟早有人改了一个忘了另一个。
type taskJSON struct {
	billTaskWire
	StartAt string `json:"startAt"`
	EndAt   string `json:"endAt"`
}

// MarshalJSON 让 BillTask 直接序列化成带墙上时间的形态。
//
// 这样所有 writeJSON(task) 的地方都自动带上 startAt/endAt，
// 不必在每个 handler 里各拼一次——漏一处前端就会拿到空值，
// 而空值在编辑框里表现为「时段被清空了」，很难查。
func (t BillTask) MarshalJSON() ([]byte, error) {
	return json.Marshal(taskJSON{
		billTaskWire: billTaskWire(t),
		StartAt:      t.StartAt(),
		EndAt:        t.EndAt(),
	})
}

// EnsureBillTaskSchema 建表（幂等）+ 迁移（幂等），启动时调用一次。
//
// 迁移走 ALTER 而不是 DROP 重建：库里的数字是客户的历史统计，删了就没了。
// 所有语句都带 IF NOT EXISTS / IF EXISTS，重复执行安全。
func EnsureBillTaskSchema(cfg PGConfig) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	// 全新部署：直接建成新结构。
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS bill_export_tasks (
			id BIGSERIAL PRIMARY KEY,
			customer_id BIGINT NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			customer_name TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			period_year INTEGER NOT NULL DEFAULT 0,
			period_month INTEGER NOT NULL DEFAULT 0,
			start_time TIMESTAMPTZ,
			end_time TIMESTAMPTZ,
			generate_sanitized BOOLEAN NOT NULL DEFAULT true,
			generate_cost BOOLEAN NOT NULL DEFAULT true,
			settle_cny DOUBLE PRECISION,
			list_cny DOUBLE PRECISION,
			overall_discount DOUBLE PRECISION,
			costed_settle_cny DOUBLE PRECISION,
			cost_cny DOUBLE PRECISION,
			profit_cny DOUBLE PRECISION,
			cost_complete BOOLEAN NOT NULL DEFAULT false,
			priced_rows INTEGER NOT NULL DEFAULT 0,
			total_cost_rows INTEGER NOT NULL DEFAULT 0,
			row_count INTEGER NOT NULL DEFAULT 0,
			log_path TEXT NOT NULL DEFAULT '',
			job_id TEXT NOT NULL DEFAULT '',
			run_count INTEGER NOT NULL DEFAULT 0,
			last_run_at TIMESTAMPTZ,
			generated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("创建 bill_export_tasks 表失败: %w", err)
	}

	// 老部署升级：加列 + 放开旧约束。每句都幂等，已升级过的再跑一遍是空操作。
	// 注意顺序——先加列再动约束，中途失败留下的是「列已加、约束还在」的中间态，
	// 而约束那步下次启动会补上，比反过来安全。
	migrations := []string{
		// 唯一键从 (客户, 账期) 换成计划自身：同一客户同一账期现在可以有多条计划。
		// 这是本次改造的核心——旧约束下根本建不出「一个月度 + 四个周度」这组计划。
		`ALTER TABLE bill_export_tasks
		   DROP CONSTRAINT IF EXISTS bill_export_tasks_customer_id_period_year_period_month_key`,
		`ALTER TABLE bill_export_tasks ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE bill_export_tasks ADD COLUMN IF NOT EXISTS generate_sanitized BOOLEAN NOT NULL DEFAULT true`,
		`ALTER TABLE bill_export_tasks ADD COLUMN IF NOT EXISTS generate_cost BOOLEAN NOT NULL DEFAULT true`,
		`ALTER TABLE bill_export_tasks ADD COLUMN IF NOT EXISTS run_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE bill_export_tasks ADD COLUMN IF NOT EXISTS last_run_at TIMESTAMPTZ`,
		// 未执行的计划还没有金额，这几列必须可空。
		`ALTER TABLE bill_export_tasks ALTER COLUMN settle_cny DROP NOT NULL`,
		`ALTER TABLE bill_export_tasks ALTER COLUMN list_cny DROP NOT NULL`,
		`ALTER TABLE bill_export_tasks ALTER COLUMN overall_discount DROP NOT NULL`,
		// 时段对未执行的计划也是空的。
		`ALTER TABLE bill_export_tasks ALTER COLUMN start_time DROP NOT NULL`,
		`ALTER TABLE bill_export_tasks ALTER COLUMN end_time DROP NOT NULL`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			return fmt.Errorf("迁移 bill_export_tasks 失败 (%s): %w", firstLine(m), err)
		}
	}

	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_bill_export_tasks_period
		ON bill_export_tasks (period_year DESC, period_month DESC)
	`); err != nil {
		return fmt.Errorf("创建 bill_export_tasks 索引失败: %w", err)
	}
	return nil
}

// firstLine 取 SQL 的第一行，用于报错时说明是哪条迁移挂了。
func firstLine(sql string) string {
	for i, r := range sql {
		if r == '\n' {
			return sql[:i]
		}
	}
	return sql
}

// CreateBillTask 新建一条计划，返回带 ID 的完整记录。
//
// 只写计划字段：金额列一律留 NULL。计划刚建出来还没执行，
// 写 0 会被读成「这个月导了 0 元的账单」，与「还没跑」混为一谈。
func CreateBillTask(cfg PGConfig, t BillTask) (BillTask, error) {
	if t.CustomerID <= 0 {
		return BillTask{}, fmt.Errorf("缺少客户")
	}
	if err := validatePlanRange(t); err != nil {
		return BillTask{}, err
	}

	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return BillTask{}, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	err = db.QueryRow(`
		INSERT INTO bill_export_tasks
			(customer_id, customer_name, name, period_year, period_month,
			 start_time, end_time, generate_sanitized, generate_cost)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, generated_at
	`, t.CustomerID, t.CustomerName, t.Name, t.PeriodYear, t.PeriodMonth,
		t.StartTime, t.EndTime, t.GenerateSanitized, t.GenerateCost,
	).Scan(&t.ID, &t.GeneratedAt)
	if err != nil {
		return BillTask{}, fmt.Errorf("新建账单计划失败: %w", err)
	}
	return t, nil
}

// UpdateBillTask 编辑计划字段。
//
// **不碰金额列**：改个时段或改个名字，不该把上次跑出来的结算额/成本/利润清掉——
// 那些数字是已经出过的账，仍然有效。要清空只能删了重建。
func UpdateBillTask(cfg PGConfig, t BillTask) error {
	if t.ID <= 0 {
		return fmt.Errorf("缺少任务 ID")
	}
	if err := validatePlanRange(t); err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	res, err := db.Exec(`
		UPDATE bill_export_tasks SET
			name = $2,
			period_year = $3,
			period_month = $4,
			start_time = $5,
			end_time = $6,
			generate_sanitized = $7,
			generate_cost = $8
		WHERE id = $1
	`, t.ID, t.Name, t.PeriodYear, t.PeriodMonth,
		t.StartTime, t.EndTime, t.GenerateSanitized, t.GenerateCost)
	if err != nil {
		return fmt.Errorf("保存账单计划失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("保存账单计划失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("任务不存在（可能已被删除）")
	}
	return nil
}

// validatePlanRange 校验计划的时段。两种合法形态：两个都空（还没定时段），
// 或两个都有且起 ≤ 止、跨度不超上限。
//
// 不允许只填一个：只有开始没有结束的时段没法查库，而漏填时若不拦住，
// 执行阶段才报错就晚了一步。
func validatePlanRange(t BillTask) error {
	bothEmpty := t.StartTime == nil && t.EndTime == nil
	bothSet := t.StartTime != nil && t.EndTime != nil
	if !bothEmpty && !bothSet {
		return fmt.Errorf("开始时间与结束时间必须同时填写")
	}
	if !bothSet {
		return nil
	}
	if t.EndTime.Before(*t.StartTime) {
		return fmt.Errorf("结束时间不能早于开始时间")
	}
	if span := t.EndTime.Sub(*t.StartTime); span > time.Duration(MaxExportDays)*24*time.Hour {
		return fmt.Errorf("时段跨度 %.1f 天超过上限 %d 天，请拆成多个计划",
			span.Hours()/24, MaxExportDays)
	}
	return nil
}

// GetBillTask 按 ID 取计划。
func GetBillTask(cfg PGConfig, id int64) (BillTask, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return BillTask{}, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT `+billTaskColumns+` FROM bill_export_tasks WHERE id = $1`, id)
	if err != nil {
		return BillTask{}, fmt.Errorf("查询账单计划失败: %w", err)
	}
	defer rows.Close()

	list, err := scanBillTasks(rows)
	if err != nil {
		return BillTask{}, err
	}
	if len(list) == 0 {
		return BillTask{}, sql.ErrNoRows
	}
	return list[0], nil
}

// billTaskColumns 是查询用的列清单，抽出来是因为有三处查询要用同一份，
// 列顺序必须与 scanBillTasks 的 Scan 一一对应。
const billTaskColumns = `
	id, customer_id, customer_name, name, period_year, period_month,
	start_time, end_time, generate_sanitized, generate_cost,
	settle_cny, list_cny, overall_discount,
	costed_settle_cny, cost_cny, profit_cny, cost_complete,
	priced_rows, total_cost_rows, row_count,
	log_path, job_id, run_count, last_run_at, generated_at`

// scanBillTasks 把结果集扫成 []BillTask。可空列一律走 sql.NullXxx 再转指针，
// 这样「NULL」与「0」在 Go 侧是两种不同的值：前者是「没有」，后者是「真的是 0」。
func scanBillTasks(rows *sql.Rows) ([]BillTask, error) {
	out := []BillTask{}
	for rows.Next() {
		var t BillTask
		var start, end, lastRun sql.NullTime
		var settle, list, discount, costedSettle, cost, profit sql.NullFloat64
		if err := rows.Scan(
			&t.ID, &t.CustomerID, &t.CustomerName, &t.Name, &t.PeriodYear, &t.PeriodMonth,
			&start, &end, &t.GenerateSanitized, &t.GenerateCost,
			&settle, &list, &discount,
			&costedSettle, &cost, &profit, &t.CostComplete,
			&t.PricedRows, &t.TotalCostRows, &t.RowCount,
			&t.LogPath, &t.JobID, &t.RunCount, &lastRun, &t.GeneratedAt,
		); err != nil {
			return nil, fmt.Errorf("读取账单计划失败: %w", err)
		}
		t.StartTime = nullTimePtr(start)
		t.EndTime = nullTimePtr(end)
		t.LastRunAt = nullTimePtr(lastRun)
		t.SettleCNY = nullFloatPtr(settle)
		t.ListCNY = nullFloatPtr(list)
		t.OverallDiscount = nullFloatPtr(discount)
		t.CostedSettleCNY = nullFloatPtr(costedSettle)
		t.CostCNY = nullFloatPtr(cost)
		t.ProfitCNY = nullFloatPtr(profit)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取账单计划失败: %w", err)
	}
	return out, nil
}

func nullTimePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

func nullFloatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

// SaveBillTaskResult 写入一次执行结果。
//
// **只动结果列**，计划字段（名称/时段/勾选）保持用户设置的样子；
// 同时累加 run_count 并刷新 last_run_at——后者是「这个计划执行过没有」的唯一判据。
func SaveBillTaskResult(cfg PGConfig, t BillTask) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		UPDATE bill_export_tasks SET
			customer_name = $2,
			settle_cny = $3,
			list_cny = $4,
			overall_discount = $5,
			costed_settle_cny = $6,
			cost_cny = $7,
			profit_cny = $8,
			cost_complete = $9,
			priced_rows = $10,
			total_cost_rows = $11,
			row_count = $12,
			log_path = $13,
			job_id = $14,
			run_count = run_count + 1,
			last_run_at = now()
		WHERE id = $1
	`, t.ID, t.CustomerName,
		t.SettleCNY, t.ListCNY, t.OverallDiscount,
		t.CostedSettleCNY, t.CostCNY, t.ProfitCNY, t.CostComplete,
		t.PricedRows, t.TotalCostRows, t.RowCount,
		t.LogPath, t.JobID)
	if err != nil {
		return fmt.Errorf("写入账单任务结果失败: %w", err)
	}
	return nil
}

// BillTaskFilter 计划列表的筛选条件，零值表示不筛。
type BillTaskFilter struct {
	Year       int
	Month      int
	CustomerID int64
}

// ListBillTasks 计划列表，按账期倒序、同账期按客户名与时段排序。
func ListBillTasks(cfg PGConfig, f BillTaskFilter) ([]BillTask, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

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

	rows, err := db.Query(`SELECT `+billTaskColumns+` FROM bill_export_tasks `+where+`
		ORDER BY period_year DESC, period_month DESC, customer_name, start_time NULLS LAST, id`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("查询账单计划失败: %w", err)
	}
	defer rows.Close()
	return scanBillTasks(rows)
}

// DeleteBillTask 删一条计划。**不删任何文件**——
// 产物在 jobDir 里由 6 小时清理兜底，源日志在 dataDir 里由「已导出文件」列表管理。
func DeleteBillTask(cfg PGConfig, id int64) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	res, err := db.Exec(`DELETE FROM bill_export_tasks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("删除账单计划失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("删除账单计划失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("任务不存在（可能已被删除）")
	}
	return nil
}

// PeriodSummary 一个账期的汇总。
type PeriodSummary struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	// TaskCount 该账期的**已执行**任务数（一计划一行，所以也是「导了几份账单」）。
	//
	// 只数已执行的：没跑过的计划没有金额，算进去会让「这个月导了几张账单」虚高，
	// 而且与下面的结算额/成本对不上（0 张账单却有非零金额的观感）。
	TaskCount int `json:"taskCount"`
	// SettleCNY 账单结算额合计（全部行）。这是对外报的数。
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
	// UnrunCount 该账期还没执行过的计划数。不计入上面任何金额，
	// 单独报出来是为了让用户知道「这个月还有 N 个计划没跑」。
	UnrunCount int `json:"unrunCount"`
}

// SummarizeTasks 按月汇总计划，供「每月导了多少账单、成本多少利润多少」。
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
//
// 未执行的计划只进 UnrunCount，不进任何金额与 TaskCount。
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

		// 还没执行过的计划：只计数，不进金额，也不算「导了几张账单」。
		if !t.HasRun() {
			ps.UnrunCount++
			continue
		}

		ps.TaskCount++

		// 已执行但系列金额为空（老数据，或当时的写入路径没带上）。
		// 宁可当「没成本」处理，也不能用零值参与运算。
		if t.SettleCNY == nil {
			ps.MissingCostCount++
			continue
		}
		ps.SettleCNY += *t.SettleCNY

		if t.CostCNY == nil || t.CostedSettleCNY == nil {
			// 这个任务没有成本口径：只进 SettleCNY 与「缺成本」计数。
			// **绝不**把它的结算额加进利润的计算链路。
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

	// 账期倒序，与计划列表的排序一致。
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
