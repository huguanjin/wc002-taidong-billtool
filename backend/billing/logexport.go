package billing

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 日志导出的列清单。前 14 列与人工导出脚本逐字对齐（顺序也一致），
// 末尾追加 other —— 缓存计费、阶梯表达式、工具调用、web_search 的信息全在 other 里，
// 人工 SQL 里没有它，所以那份导出的日志在 billtool 里算出来缓存永远是 0。
var (
	// logBaseColumns 与人工 SQL 的 SELECT 列完全相同，顺序保持不变：
	// 这样导出的文件前 14 列在视觉上与历史手动导出文件对齐。
	logBaseColumns = []string{
		"id", "username", "type", "created_at", "token_id", "token_name",
		"model_name", "`group`", "prompt_tokens", "completion_tokens",
		"quota", "use_time", "is_stream", "request_id",
	}
	// logRequiredColumns 人工 SQL 之外必需的列，紧跟基础列之后。
	logRequiredColumns = []string{"other"}
	// logOptionalColumns 勾选才加的可选列（跨账号排查用）。
	logOptionalColumns = []string{"user_id"}
)

// LogExportParams 一次日志导出的参数。
//
// 时间语义是**双闭区间**，与人工导出的 `BETWEEN a AND b` 一致：
// StartTime 与 EndTime 两个时刻本身都算在内，边界那一秒的日志不会被漏掉。
// 内部查询仍写成半开区间 `created_at >= start AND created_at < end+1s`——
// created_at 是整数秒，这个写法与闭区间完全等价（见 buildLogExportQuery）。
type LogExportParams struct {
	Usernames     []string  // 账号列表，与 UserIDs 取并集；两者不能同时为空
	UserIDs       []int     // 可选
	IncludeUserID bool      // 是否在输出里追加 user_id 列
	StartTime     time.Time // 起始时刻（含），由 ParseExportTime 按 +08:00 解析
	EndTime       time.Time // 结束时刻（含）
}

// ExportQueryEnd 返回查询用的半开区间右端点：EndTime + 1 秒。
//
// created_at 是整数秒，`< EndTime+1s` 等价于 `<= EndTime`，也就是闭区间。
// 这样既保持了「结束时刻本身包含在内」符合直觉的语义，又不必在 SQL 里
// 为秒粒度写 `<=`（下游一律走半开区间，判断逻辑只有一处）。
func (p LogExportParams) ExportQueryEnd() time.Time {
	return p.EndTime.Add(time.Second)
}

// ParseExportTime 按北京时间（+08:00）解析前端传来的时间字符串。
//
// 容器时区通常是 UTC，用 time.Local 解析会整体偏 8 小时、把客户账期错切一天，
// 所以这里一律用固定的 +08:00，与 aggregate.go 的 cstLocation 同一口径。
//
// 接受的输入形态（前端是 datetime-local，浏览器在秒为 0 时会省略秒段）：
//   - 2026-09-24T00:00:05  完整时刻
//   - 2026-09-24T00:00     缺秒，按 :00 处理
//   - 2026-09-24 00:00:05  空格分隔（部分浏览器/手填）
//   - 2026-09-24           只有日期，见 isDateOnly
func ParseExportTime(s string) (time.Time, bool, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return time.Time{}, false, fmt.Errorf("时间不能为空")
	}
	dateOnly := isDateOnly(v)
	normalized := strings.Replace(v, " ", "T", 1)

	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, normalized, cstLocation); err == nil {
			return t, dateOnly, nil
		}
	}
	if dateOnly {
		if t, err := time.ParseInLocation("2006-01-02", normalized, cstLocation); err == nil {
			return t, true, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("时间格式无法识别：%q（应为 YYYY-MM-DD HH:MM:SS）", s)
}

// isDateOnly 判断是否只给了日期没给时刻。
func isDateOnly(v string) bool {
	return !strings.ContainsAny(v, "T: ")
}

// ResolveExportRange 把用户填写的起止时间解析成导出区间。
//
// 只填日期的写法按整段处理，保持与旧版「按天导出」完全一致的行为：
// 起始 = 当天 00:00:00，结束 = 当天 23:59:59（含）。
// 给了时刻就按秒精确，结束时刻本身也算在内。
func ResolveExportRange(startRaw, endRaw string) (start, end time.Time, err error) {
	start, _, err = ParseExportTime(startRaw)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("开始时间：%w", err)
	}
	var endDateOnly bool
	end, endDateOnly, err = ParseExportTime(endRaw)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("结束时间：%w", err)
	}

	// 只给日期时起点就是当天 00:00:00，ParseExportTime 已经这么解析；
	// 终点要补到当天 23:59:59（含），才与旧的「按整段导出」行为一致。
	if endDateOnly {
		end = end.Add(24*time.Hour - time.Second)
	}

	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("结束时间不能早于开始时间")
	}
	return start, end, nil
}

// LogExportResult 导出结果摘要。
type LogExportResult struct {
	Path       string
	RowCount   int64
	StartedAt  time.Time
	FinishedAt time.Time
}

// LogExportColumns 返回本次导出的列名（不带反引号），供测试与文档核对。
func LogExportColumns(includeUserID bool) []string {
	cols := make([]string, 0, len(logBaseColumns)+2)
	for _, c := range logBaseColumns {
		cols = append(cols, strings.Trim(c, "`"))
	}
	cols = append(cols, logRequiredColumns...)
	if includeUserID {
		cols = append(cols, logOptionalColumns...)
	}
	return cols
}

// buildLogExportSelect 拼出导出用的 SELECT 列清单。
func buildLogExportSelect(includeUserID bool) string {
	cols := make([]string, 0, len(logBaseColumns)+2)
	cols = append(cols, logBaseColumns...)
	cols = append(cols, logRequiredColumns...)
	if includeUserID {
		cols = append(cols, logOptionalColumns...)
	}
	return strings.Join(cols, ", ")
}

// sanitizeTableName 校验表名只含字母数字下划线，防止配置里的表名被拼进 SQL 时
// 带入反引号或分号。表名来自配置而非用户输入，这里是纵深防御。
func sanitizeTableName(name string) (string, error) {
	if name == "" {
		return "logs", nil
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_'
		if !ok {
			return "", fmt.Errorf("日志表名 %q 含非法字符，只允许字母、数字与下划线", name)
		}
	}
	return name, nil
}

// mysqlBatchEscape 复刻 mysql 客户端批处理模式的取值转义：
// 值内部不整体加引号，只把控制字符与反斜杠转成两字符序列。
//
// 这是「导出结果能否被 billtool 读回来」的关键：other 列本身是 JSON、里面全是双引号，
// mysql 不转义也不包裹它们，下游 detectDelimiter（按首行 Tab 计数判分隔符）与
// csv.Reader{LazyQuotes: true} 正是为这种未加引号的形态设计的。
// 若改用 encoding/csv 写出（双引号包裹 + "" 转义），读回来时字段会被拆错。
func mysqlBatchEscape(v string) string {
	// 绝大多数值不含需要转义的字符，先扫一遍避免无谓的 Builder 分配。
	needsEscape := false
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\t', '\n', '\r', '\\', 0:
			needsEscape = true
		}
		if needsEscape {
			break
		}
	}
	if !needsEscape {
		return v
	}

	var b strings.Builder
	b.Grow(len(v) + 8)
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\\':
			b.WriteString(`\\`)
		case 0:
			b.WriteString(`\0`)
		default:
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

// buildLogExportQuery 拼出导出用的 SQL 与参数。
//
// 抽成纯函数便于测试：账号与用户 ID 一律走占位符，参数个数与顺序必须与
// 占位符严格对应，不能有任何字符串拼接（账号名里带引号/分号也不能破坏结构）。
func buildLogExportQuery(table string, params LogExportParams) (string, []interface{}) {
	// 账号与用户 ID 取并集。
	//
	// 区间写成半开 `< end+1s`：created_at 是整数秒，这与「结束时刻也包含在内」
	// 的闭区间语义完全等价（`< T+1s` 即 `<= T`）。
	conds := []string{"type = 2", "created_at >= ?", "created_at < ?"}
	args := []interface{}{params.StartTime.Unix(), params.ExportQueryEnd().Unix()}
	if len(params.Usernames) > 0 {
		conds = append(conds, fmt.Sprintf("username IN (%s)", placeholders(len(params.Usernames))))
		for _, u := range params.Usernames {
			args = append(args, u)
		}
	}
	if len(params.UserIDs) > 0 {
		conds = append(conds, fmt.Sprintf("user_id IN (%s)", placeholders(len(params.UserIDs))))
		for _, id := range params.UserIDs {
			args = append(args, id)
		}
	}

	// 人工导出没有 ORDER BY，数据库返回顺序并不保证稳定；这里刻意按
	// (created_at, id) 排序，让同样的输入总是得到同样的文件，便于比对与复查。
	query := fmt.Sprintf("SELECT %s FROM `%s` WHERE %s ORDER BY created_at, id",
		buildLogExportSelect(params.IncludeUserID), table, strings.Join(conds, " AND "))
	return query, args
}

// ExportLogFileName 导出文件名：日志查询_<起始日>_<结束日>.tsv。
//
// 取起止时刻本身的日期，不做任何偏移：区间是双闭的，EndTime 已经是最后一个
// 算在内的时刻，减一秒会把「结束于某天 00:00:00」错标成前一天。
// 含「日志查询」字样，出账时 defaultOutputName 会据此把名字换成「账单」。
func ExportLogFileName(start, end time.Time) string {
	return fmt.Sprintf("日志查询_%s_%s.tsv",
		start.Format("2006-01-02"), end.Format("2006-01-02"))
}

// ExportLogsFromDB 连业务库把消费日志导出为 mysql 批处理模式的 tsv，
// 列与列顺序对齐人工导出（末尾追加 other），流式写入 outDir 下的文件。
//
// 只读：全程只有 SELECT，不对业务库做任何写操作。
func ExportLogsFromDB(cfg DBConfig, outDir string, params LogExportParams) (*LogExportResult, error) {
	if len(params.Usernames) == 0 && len(params.UserIDs) == 0 {
		return nil, fmt.Errorf("必须至少指定一个用户名或用户 ID")
	}
	if params.EndTime.Before(params.StartTime) {
		return nil, fmt.Errorf("结束时间不能早于开始时间")
	}
	table, err := sanitizeTableName(cfg.LogTableName())
	if err != nil {
		return nil, err
	}
	query, args := buildLogExportQuery(table, params)

	db, err := sql.Open("mysql", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询 %s 表失败: %w", table, err)
	}
	defer rows.Close()

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录失败: %w", err)
	}
	// 文件名里的日期直接取起止时刻本身（见 ExportLogFileName）。
	name := ExportLogFileName(params.StartTime, params.EndTime)
	finalPath := filepath.Join(outDir, name)
	tmp, err := os.CreateTemp(outDir, ".logexport-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	started := time.Now()
	w := bufio.NewWriterSize(tmp, 1<<20)
	headers := LogExportColumns(params.IncludeUserID)
	if err := writeTSVRecord(w, headers); err != nil {
		return nil, fmt.Errorf("写入表头失败: %w", err)
	}

	var count int64
	for rows.Next() {
		record, err := scanLogRecord(rows, params.IncludeUserID)
		if err != nil {
			return nil, fmt.Errorf("读取第 %d 行失败: %w", count+1, err)
		}
		if err := writeTSVRecord(w, record); err != nil {
			return nil, fmt.Errorf("写入第 %d 行失败: %w", count+1, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取 %s 表数据失败: %w", table, err)
	}
	if err := w.Flush(); err != nil {
		return nil, fmt.Errorf("刷新输出缓冲失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("落盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("关闭临时文件失败: %w", err)
	}
	// 先写临时文件再改名，避免半截文件被当成有效日志读取。
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return nil, fmt.Errorf("重命名输出文件失败: %w", err)
	}
	committed = true

	return &LogExportResult{
		Path: finalPath, RowCount: count, StartedAt: started, FinishedAt: time.Now(),
	}, nil
}

// scanLogRecord 读取一行并按导出列顺序返回单元格文本。
//
// 整数列用 sql.NullInt64、字符串列用 sql.NullString：老数据里 group / token_name
// 可能是 NULL，直接 Scan 进 string/int 会报错。NULL 统一输出字面量 NULL，
// 与 mysql 客户端批处理模式一致。
func scanLogRecord(rows *sql.Rows, includeUserID bool) ([]string, error) {
	var (
		id               sql.NullInt64
		username         sql.NullString
		logType          sql.NullInt64
		createdAt        sql.NullInt64
		tokenID          sql.NullInt64
		tokenName        sql.NullString
		modelName        sql.NullString
		group            sql.NullString
		promptTokens     sql.NullInt64
		completionTokens sql.NullInt64
		quota            sql.NullInt64
		useTime          sql.NullInt64
		isStream         sql.NullInt64
		requestID        sql.NullString
		other            sql.NullString
		userID           sql.NullInt64
	)

	dest := []interface{}{
		&id, &username, &logType, &createdAt, &tokenID, &tokenName,
		&modelName, &group, &promptTokens, &completionTokens,
		&quota, &useTime, &isStream, &requestID, &other,
	}
	if includeUserID {
		dest = append(dest, &userID)
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, err
	}

	record := []string{
		nullInt64Text(id),
		nullStringText(username),
		nullInt64Text(logType),
		nullInt64Text(createdAt),
		nullInt64Text(tokenID),
		nullStringText(tokenName),
		nullStringText(modelName),
		nullStringText(group),
		nullInt64Text(promptTokens),
		nullInt64Text(completionTokens),
		nullInt64Text(quota),
		nullInt64Text(useTime),
		// is_stream 在 MySQL 里是 tinyint(1)，mysql 客户端输出 0/1；
		// 用 NullInt64 扫而不是 bool，避免写成 true/false。
		nullInt64Text(isStream),
		nullStringText(requestID),
		nullStringText(other),
	}
	if includeUserID {
		record = append(record, nullInt64Text(userID))
	}
	return record, nil
}

// writeTSVRecord 按 mysql 批处理模式的约定写一行：字段间单个 Tab，行尾 \n。
func writeTSVRecord(w *bufio.Writer, record []string) error {
	for i, cell := range record {
		if i > 0 {
			if err := w.WriteByte('\t'); err != nil {
				return err
			}
		}
		if _, err := w.WriteString(mysqlBatchEscape(cell)); err != nil {
			return err
		}
	}
	return w.WriteByte('\n')
}

// placeholders 生成 n 个逗号分隔的 ? 占位符。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func nullInt64Text(v sql.NullInt64) string {
	if !v.Valid {
		return "NULL"
	}
	return fmt.Sprintf("%d", v.Int64)
}

func nullStringText(v sql.NullString) string {
	if !v.Valid {
		return "NULL"
	}
	return v.String
}
