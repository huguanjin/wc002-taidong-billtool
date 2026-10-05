package billing

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMysqlBatchEscape 复刻 mysql 批处理模式的取值转义。
// 这是整个导出功能最容易翻车的地方：转义写错，导出的文件读回来时字段会被拆错，
// 而 billtool 不会报错，只会静默把缓存算成 0。
func TestMysqlBatchEscape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"制表符", "a\tb", `a\tb`},
		{"换行", "a\nb", `a\nb`},
		{"回车", "a\rb", `a\rb`},
		{"反斜杠", `a\b`, `a\\b`},
		{"NUL", "a\x00b", `a\0b`},
		// other 列是 JSON，双引号必须原样保留——mysql 不转义也不加引号包裹，
		// 下游 detectDelimiter 与 csv.Reader{LazyQuotes:true} 依赖这个形态。
		{"双引号原样保留", `{"a":1,"b":"x"}`, `{"a":1,"b":"x"}`},
		// 反斜杠逐个翻倍：json 里的 `\\` 出去变成 `\\\\`。
		// 这是 mysql 批处理模式的行为，人工导出同样如此（见文件末尾的说明）。
		{"JSON 里的反斜杠要翻倍", `{"p":"a\\b"}`, `{"p":"a\\\\b"}`},
		{"中文原样", "国产模型", "国产模型"},
		{"空串", "", ""},
		{"多个混合", "a\tb\nc\\d\x00e", `a\tb\nc\\d\0e`},
		{"普通文本不动", "deepseek-v4.1-flash", "deepseek-v4.1-flash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mysqlBatchEscape(tc.in))
		})
	}
}

// TestLogExportColumnsMatchManualSQL 导出列必须与人工 SQL 的 14 列一致，末尾追加 other
// 与 channel_id，user_id 只在勾选时加。顺序是有意义的：前 14 列与历史手动导出文件对齐。
func TestLogExportColumnsMatchManualSQL(t *testing.T) {
	manual := []string{
		"id", "username", "type", "created_at", "token_id", "token_name",
		"model_name", "group", "prompt_tokens", "completion_tokens",
		"quota", "use_time", "is_stream", "request_id",
	}
	cols := LogExportColumns(false)
	require.Len(t, cols, len(manual)+2, "应是人工 14 列 + other + channel_id")
	assert.Equal(t, manual, cols[:len(manual)], "前 14 列必须与人工 SQL 完全一致且同序")
	// other 与 channel_id 都追加在人工列之后：other 带计费信息，
	// channel_id 是成本估算按渠道展开的依据（脱敏日志会丢掉 other，
	// 渠道号必须是独立列才能活下来）。
	assert.Equal(t, []string{"other", "channel_id"}, cols[len(manual):])

	// group 是保留字，SQL 里要反引号；对外暴露的列名不带反引号。
	assert.Equal(t, "group", cols[7])

	withUserID := LogExportColumns(true)
	require.Len(t, withUserID, len(cols)+1)
	assert.Equal(t, "user_id", withUserID[len(withUserID)-1], "user_id 追加在最末")

	// SELECT 清单里 group 必须带反引号，否则 SQL 语法错误。
	sel := buildLogExportSelect(false)
	assert.Contains(t, sel, "`group`", "保留字 group 在 SQL 里必须反引号包裹")
	assert.Contains(t, sel, "other")
	assert.Contains(t, sel, "channel_id")
	assert.NotContains(t, buildLogExportSelect(false), "user_id", "未勾选时不导出 user_id")
	assert.Contains(t, buildLogExportSelect(true), "user_id")
}

// TestLogExportColumnsWithoutChannelID 老库没有 channel_id 列时要能降级导出，
// 且列清单与 SELECT 清单必须同步少这一列——两边不一致会让写出的行错位。
func TestLogExportColumnsWithoutChannelID(t *testing.T) {
	cols := LogExportColumnsFor(false, false)
	assert.NotContains(t, cols, "channel_id")
	assert.Contains(t, cols, "other", "other 与 channel_id 是两件事，缺一列不该影响另一列")

	sel := buildLogExportSelectWith(false, false)
	assert.NotContains(t, sel, "channel_id")
	assert.Contains(t, sel, "other")

	// 有该列时两份清单都要带上，且顺序一致。
	withCol := LogExportColumnsFor(false, true)
	assert.Equal(t, []string{"other", "channel_id"}, withCol[len(withCol)-2:])
	assert.Contains(t, buildLogExportSelectWith(false, true), "channel_id")

	// 无论哪种情况，列名清单与 SELECT 清单的列数必须相同。
	for _, hasCh := range []bool{false, true} {
		for _, uid := range []bool{false, true} {
			assert.Equal(t, len(LogExportColumnsFor(uid, hasCh)),
				strings.Count(buildLogExportSelectWith(uid, hasCh), ",")+1,
				"hasChannelID=%v includeUserID=%v 时两份清单列数必须一致", hasCh, uid)
		}
	}
}

// TestPlaceholders 账号/用户 ID 走占位符，个数必须与输入个数一致。
func TestPlaceholders(t *testing.T) {
	assert.Equal(t, "", placeholders(0))
	assert.Equal(t, "?", placeholders(1))
	assert.Equal(t, "?,?", placeholders(2))
	assert.Equal(t, "?,?,?,?,?", placeholders(5))
}

// TestSanitizeTableName 表名来自配置，仍要挡住非法字符，避免拼进 SQL 时注入。
func TestSanitizeTableName(t *testing.T) {
	name, err := sanitizeTableName("")
	require.NoError(t, err)
	assert.Equal(t, "logs", name, "未配置时默认 logs")

	name, err = sanitizeTableName("logs_2026")
	require.NoError(t, err)
	assert.Equal(t, "logs_2026", name)

	for _, bad := range []string{"logs`", "logs; DROP TABLE x", "logs-2", "logs 表", "logs\"\"", "logs.name"} {
		_, err := sanitizeTableName(bad)
		assert.Error(t, err, "%q 应被拒绝", bad)
	}
}

// TestExportLogsValidation 参数校验：账号为空、区间倒置都必须报错且提示可读。
func TestExportLogsValidation(t *testing.T) {
	cfg := DBConfig{Host: "127.0.0.1", DBName: "new-api"}

	_, err := ExportLogsFromDB(cfg, t.TempDir(), LogExportParams{
		StartTime: time.Now(), EndTime: time.Now().Add(time.Hour),
	})
	assert.Error(t, err, "没有任何账号或用户 ID 必须报错")
	assert.Contains(t, err.Error(), "用户名", "提示要能指导使用者")

	_, err = ExportLogsFromDB(cfg, t.TempDir(), LogExportParams{
		Usernames: []string{"u1"},
		StartTime: time.Now(), EndTime: time.Now().Add(-time.Hour),
	})
	assert.Error(t, err, "结束早于开始必须报错")
	assert.Contains(t, err.Error(), "结束时间")
}

// TestExportStartEndUseCSTBoundaries 日期边界按北京时间解释：
// 起始是当天 00:00:00 的 Unix 秒，结束是次日 00:00:00（半开区间）。
// 容器多为 UTC，用 time.Local 切日期会整体偏 8 小时、把账期错切一天。
func TestExportStartEndUseCSTBoundaries(t *testing.T) {
	start, err := time.ParseInLocation("2006-01-02", "2026-09-24", cstLocation)
	require.NoError(t, err)
	end, err := time.ParseInLocation("2006-01-02", "2026-09-29", cstLocation)
	require.NoError(t, err)
	end = end.AddDate(0, 0, 1) // 半开区间：结束日期次日 00:00

	// 文档第 1 节给出的换算基准（date -d "2026-09-24 00:00:00 +08:00" +%s → 1790179200）。
	assert.Equal(t, int64(1790179200), start.Unix(),
		"2026-09-24 00:00:00 +08:00 的 Unix 秒")
	assert.Equal(t, int64(1790697600), end.Unix(),
		"结束时刻应是 2026-09-30 00:00:00 +08:00")
	// 含 09-24 至 09-29 共 6 天整。
	assert.Equal(t, start.Unix()+6*24*3600, end.Unix())

	// 与 BETWEEN 语义等价：start 含、end 不含，中间没有整数秒被漏掉。
	assert.True(t, end.After(start))

	// 容器时区是 UTC 时也不能偏移：显式用 cstLocation 解析，与 time.Local 无关。
	utcSame, err := time.ParseInLocation("2006-01-02", "2026-09-24", time.UTC)
	require.NoError(t, err)
	assert.Equal(t, int64(-8*3600), start.Unix()-utcSame.Unix(),
		"按 UTC 解析会比 +08:00 晚 8 小时——这正是必须显式指定 +08:00 的原因")
}

// TestExportedTSVRoundTrip 核心契约：导出写出的 tsv 必须能被 billtool 读回来，
// 且 other 里的缓存字段能被 ParseCacheTokens 解析出非 0 值。
// 如果这一步坏了，客户账单的缓存读/写会静默变成 0。
func TestExportedTSVRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "日志查询_2026-09-01_2026-09-30.tsv")

	otherJSON := `{"model_ratio":1,"completion_ratio":4,"cache_ratio":0.1,"cache_tokens":57216,"cache_creation_tokens_5m":1200,"usage_semantic":"openai","admin_info":{"use_channel":["615"]}}`
	record := []string{
		"425258232", "zhongkang2026", "2", "1789470821", "4337", "国产模型",
		"deepseek-v4.1-flash", "国产模型", "57945", "914",
		"7580", "3", "0", "202609151113385444841498268d9d6J2aSe3OH", otherJSON, "1165",
	}

	f, err := os.Create(path)
	require.NoError(t, err)
	w := bufio.NewWriter(f)
	require.NoError(t, writeTSVRecord(w, LogExportColumns(false)))
	require.NoError(t, writeTSVRecord(w, record))
	require.NoError(t, w.Flush())
	require.NoError(t, f.Close())

	headers, rows, err := LoadLogRows(path, "", "")
	require.NoError(t, err, "导出的 tsv 必须能被 LoadLogRows 读回来")
	require.Len(t, rows, 1)
	assert.Equal(t, LogExportColumns(false), headers, "表头应一致")
	assert.Len(t, rows[0], len(LogExportColumns(false)), "字段数应与列数一致")

	// other 列读回来必须与原文逐字节一致（双引号不能被改动、转义不能残留）。
	idxOther := indexOfHeader(headers, "other")
	require.NotEqual(t, -1, idxOther)
	assert.Equal(t, otherJSON, rows[0][idxOther],
		"other 里的 JSON 双引号必须原样保留")

	// 核心契约：缓存字段能解析出来，而不是全 0。
	cacheRead, w5, w1 := ParseCacheTokens(rows[0][idxOther])
	assert.Equal(t, 57216.0, cacheRead, "缓存读必须解析出来")
	assert.Equal(t, 1200.0, w5, "5 分钟缓存创建必须解析出来")
	assert.Equal(t, 0.0, w1)

	// 聚合一遍，确认整条链路（读文件→解析→聚合）缓存列不为 0。
	agg, aggErr := AggregateFromRows(rows, headers, nil, 7.0, false, nil, false, nil)
	require.NoError(t, aggErr)
	require.Len(t, agg.Rows, 1)
	assert.Equal(t, 57216.0, agg.Rows[0].CacheRead, "聚合后的缓存读不能是 0")
	assert.Equal(t, 1200.0, agg.Rows[0].CacheWrite5m)
}

// TestExportedTSVEscapesEmbeddedControlChars 值里含 Tab/换行时，转义的首要作用是
// **不让它破坏列结构**：值必须仍是同一个字段，而不是被拆成多个。
//
// 注意这是单向变换：mysql 批处理模式把制表符写成 `\t` 两字符，读回来不会被还原
// （encoding/csv 不做反斜杠反转义），人工 `mysql -e` 导出也一样。
// 这个损失不影响 billtool 的取数：other 列是 json.Marshal 的产物，
// 控制字符在写库时已经被 JSON 转义成两字符序列，DB 里不存在裸的控制字符。
func TestExportedTSVEscapesEmbeddedControlChars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roundtrip.tsv")

	tokenName := "带\t制表符\n和换行"
	record := []string{
		"1", "u", "2", "1789470821", "1", tokenName,
		"m", "g", "1", "1", "1", "1", "0", "req-1", `{"cache_tokens":7}`, "1165",
	}

	f, err := os.Create(path)
	require.NoError(t, err)
	w := bufio.NewWriter(f)
	require.NoError(t, writeTSVRecord(w, LogExportColumns(false)))
	require.NoError(t, writeTSVRecord(w, record))
	require.NoError(t, w.Flush())
	require.NoError(t, f.Close())

	headers, rows, err := LoadLogRows(path, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0], len(LogExportColumns(false)),
		"含制表符的值不能被拆成额外字段——这是转义的首要目的")

	idx := indexOfHeader(headers, "token_name")
	assert.Equal(t, `带\t制表符\n和换行`, rows[0][idx],
		"控制字符按 mysql 约定写成两字符序列，读回时保持字面量（单向变换，与人工导出一致）")

	// 关键：后面的列没有被串位，other 仍能正确解析。
	cacheRead, _, _ := ParseCacheTokens(rows[0][indexOfHeader(headers, "other")])
	assert.Equal(t, 7.0, cacheRead, "前面的值含控制字符也不能影响 other 的解析")
}

// TestEscapeRoundTripIsLossyForControlChars 固定「转义是单向的」这一性质。
//
// mysql 批处理模式写出 \t / \n / \\ ，而读回时走的是 encoding/csv，
// 它不还原反斜杠序列。所以含控制字符的文本列「写→读」一轮后不是原值，
// 人工 `mysql -e` 导出也是同样结果——这是既有导出流程的行为，不是本实现的缺陷。
//
// 之所以不影响 billtool：other 列是 json.Marshal 的产物，控制字符在写库时
// 已被 JSON 转义，DB 里不存在裸的制表符/换行；其余受影响的只有 token_name
// 一类的展示字段，而它们不参与计费。
func TestEscapeRoundTripIsLossyForControlChars(t *testing.T) {
	original := "a\tb"
	escaped := mysqlBatchEscape(original)
	assert.Equal(t, `a\tb`, escaped, "制表符写出为两字符序列")

	r := csv.NewReader(strings.NewReader(escaped))
	r.Comma = '\t'
	r.LazyQuotes = true
	fields, err := r.Read()
	require.NoError(t, err)
	require.Len(t, fields, 1, "转义的首要目的是不让值破坏列结构")
	assert.NotEqual(t, original, fields[0], "读回不还原转义——这是 mysql 批处理模式的既有行为")
	assert.Equal(t, `a\tb`, fields[0])
}

// TestRealisticOtherSurvivesRoundTrip other 列的真实形态必须无损往返：
// 这是计费数据，缓存字段丢了账单就少算钱。
//
// 用 json.Marshal 的真实产物构造，不手写病理输入：JSON 里本身带已转义反斜杠的
// 嵌套结构在 mysql 批处理模式下是「写出去翻倍、读回来不还原」的单向变换，
// 人工导出同样如此，且 billtool 的读取路径从不做反还原——两边行为一致。
func TestRealisticOtherSurvivesRoundTrip(t *testing.T) {
	// 真实的 other：计费字段 + 缓存 + admin_info 里带渠道数组与中文。
	payload := map[string]interface{}{
		"model_ratio":    1.5,
		"cache_tokens":   57216,
		"usage_semantic": "openai",
		"admin_info": map[string]interface{}{
			"use_channel": []string{"615"},
			"token_name":  "国产模型",
		},
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	other := string(raw)

	dir := t.TempDir()
	path := filepath.Join(dir, "other.tsv")
	record := []string{"1", "u", "2", "1", "1", "t", "m", "g", "1", "1", "1", "1", "0", "r", other, "1165"}

	f, err := os.Create(path)
	require.NoError(t, err)
	w := bufio.NewWriter(f)
	require.NoError(t, writeTSVRecord(w, LogExportColumns(false)))
	require.NoError(t, writeTSVRecord(w, record))
	require.NoError(t, w.Flush())
	require.NoError(t, f.Close())

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	// 原样落盘：引号不加包裹、不被转义。
	assert.Contains(t, string(content), `"cache_tokens":57216`, "JSON 双引号不应被包裹或转义")

	headers, rows, err := LoadLogRows(path, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0], len(LogExportColumns(false)), "字段数不能变")

	got := rows[0][indexOfHeader(headers, "other")]
	assert.Equal(t, other, got, "other 应逐字节往返")

	cacheRead, _, _ := ParseCacheTokens(got)
	assert.Equal(t, 57216.0, cacheRead, "缓存字段必须能从导出文件里解析出来")
	assert.Equal(t, "openai", UsageSemanticFromOther(got))

	// 整条链路：聚合后缓存列不为 0（这正是「人工导出的日志缓存全是 0」的修复点）。
	agg, aggErr := AggregateFromRows(rows, headers, nil, 7.0, false, nil, false, nil)
	require.NoError(t, aggErr)
	require.Len(t, agg.Rows, 1)
	assert.Equal(t, 57216.0, agg.Rows[0].CacheRead)
}

// TestBuildLogExportQueryParameterized SQL 必须参数化：账号个数变化时占位符个数
// 与参数个数严格对应，且恶意输入不改变 SQL 结构（只作为参数值出现）。
func TestBuildLogExportQueryParameterized(t *testing.T) {
	start, err := time.ParseInLocation("2006-01-02", "2026-09-24", cstLocation)
	require.NoError(t, err)
	end, err := time.ParseInLocation("2006-01-02", "2026-09-30", cstLocation)
	require.NoError(t, err)
	end = end.AddDate(0, 0, 1)

	t.Run("两个账号", func(t *testing.T) {
		q, args := buildLogExportQuery("logs", LogExportParams{
			Usernames: []string{"a37836323", "test02"},
			StartTime: start, EndTime: end,
		}, true)
		assert.Contains(t, q, "username IN (?,?)")
		assert.Contains(t, q, "type = 2", "type 必须写死为消费日志")
		assert.Contains(t, q, "created_at >= ? AND created_at < ?", "半开区间")
		assert.Contains(t, q, "ORDER BY created_at, id", "刻意排序，保证结果可复现")
		assert.NotContains(t, q, "a37836323", "账号不能出现在 SQL 文本里")

		require.Len(t, args, 4, "2 个时间 + 2 个账号")
		assert.Equal(t, start.Unix(), args[0])
		assert.Equal(t, end.Unix()+1, args[1], "右端点是 end+1s（闭区间语义）")
		assert.Equal(t, "a37836323", args[2])
		assert.Equal(t, "test02", args[3])
	})

	t.Run("账号个数按输入生成占位符", func(t *testing.T) {
		for _, n := range []int{1, 3, 7} {
			names := make([]string, n)
			for i := range names {
				names[i] = fmt.Sprintf("u%d", i)
			}
			q, args := buildLogExportQuery("logs", LogExportParams{
				Usernames: names, StartTime: start, EndTime: end,
			}, true)
			assert.Contains(t, q, fmt.Sprintf("username IN (%s)", placeholders(n)),
				"n=%d 时应生成 %d 个占位符", n, n)
			assert.Equal(t, 2+n, strings.Count(q, "?"),
				"n=%d 时问号总数应为 2 个时间 + %d 个账号", n, n)
			assert.Len(t, args, 2+n)
		}
	})

	t.Run("恶意输入只作为参数值", func(t *testing.T) {
		evil := []string{"a'; DROP TABLE logs; --", "b\" OR \"1\"=\"1", "c`x", "  ", "很长的账号名" + strings.Repeat("x", 300)}
		q, args := buildLogExportQuery("logs", LogExportParams{
			Usernames: evil, StartTime: start, EndTime: end,
		}, true)
		assert.Contains(t, q, "username IN (?,?,?,?,?)", "结构不变，仍是对应个数的占位符")
		assert.NotContains(t, q, "DROP TABLE", "输入不得进入 SQL 文本")
		assert.NotContains(t, q, "a37836323", "输入不得进入 SQL 文本")
		// 只应有一个 ORDER BY（来自实现），输入里的 " 不能拼出新的子句。
		assert.Equal(t, 1, strings.Count(q, "ORDER BY"))
		require.Len(t, args, 7)
		for i, e := range evil {
			assert.Equal(t, e, args[2+i], "原样作为参数传递")
		}
	})

	t.Run("账号与用户ID并集", func(t *testing.T) {
		q, args := buildLogExportQuery("logs", LogExportParams{
			Usernames: []string{"u1"}, UserIDs: []int{42, 43},
			StartTime: start, EndTime: end,
		}, true)
		assert.Contains(t, q, "username IN (?)")
		assert.Contains(t, q, "user_id IN (?,?)")
		require.Len(t, args, 5)
		assert.Equal(t, 42, args[3])
		assert.Equal(t, 43, args[4])
	})

	t.Run("勾选时带出user_id列", func(t *testing.T) {
		q, _ := buildLogExportQuery("logs", LogExportParams{
			Usernames: []string{"u1"}, IncludeUserID: true,
			StartTime: start, EndTime: end,
		}, true)
		assert.Contains(t, q, "user_id", "勾选后 SELECT 清单要含 user_id")
	})
}

// TestBuildLogExportQueryInclusiveInterval 区间是**双闭**的，与人工导出的
// BETWEEN a AND b 一致：起止两个时刻本身都算在内，边界那一秒的日志不会被漏掉。
// SQL 里仍写半开 `< end+1s`，因为 created_at 是整数秒，两者完全等价。
func TestBuildLogExportQueryInclusiveInterval(t *testing.T) {
	// 模拟用户填「2026-09-24 00:00:00」到「2026-09-29 23:59:59」。
	start, _, err := ParseExportTime("2026-09-24 00:00:00")
	require.NoError(t, err)
	end, _, err := ParseExportTime("2026-09-29 23:59:59")
	require.NoError(t, err)
	require.Equal(t, int64(1790697599), end.Unix(), "该时刻正是 09-29 23:59:59 +08:00")

	_, args := buildLogExportQuery("logs", LogExportParams{
		Usernames: []string{"u"}, StartTime: start, EndTime: end,
	}, true)
	lo := args[0].(int64)
	hi := args[1].(int64)
	assert.Equal(t, int64(1790179200), lo, "起点是 09-24 00:00:00")
	assert.Equal(t, int64(1790697600), hi, "右端点是 end+1s，即 09-30 00:00:00（不含）")

	inRange := func(ts int64) bool { return ts >= lo && ts < hi }
	assert.True(t, inRange(start.Unix()), "起始时刻本身必须在内")
	assert.True(t, inRange(end.Unix()), "结束时刻本身必须在内（闭区间，这是关键）")
	assert.True(t, inRange(end.Unix()-1))
	assert.False(t, inRange(end.Unix()+1), "结束时刻之后一秒必须在外")
	assert.False(t, inRange(start.Unix()-1), "起始时刻之前一秒必须在外")
}

// TestResolveExportRangeSecondsPrecision 起止时间要精确到秒，而不只是按天。
// 排查某个具体事故时往往只要几个小时，按天切会把无关日志一起导出来。
func TestResolveExportRangeSecondsPrecision(t *testing.T) {
	t.Run("完整到秒", func(t *testing.T) {
		start, end, err := ResolveExportRange("2026-09-24 10:30:05", "2026-09-24 10:30:08")
		require.NoError(t, err)
		assert.Equal(t, int64(3), int64(end.Sub(start).Seconds()), "3 秒的窗口要能表达出来")
		assert.Equal(t, 5, start.Second())
		assert.Equal(t, 8, end.Second())
	})

	t.Run("T分隔与空格分隔等价", func(t *testing.T) {
		a, _, err := ResolveExportRange("2026-09-24T10:30:05", "2026-09-24T10:30:08")
		require.NoError(t, err)
		b, _, err := ResolveExportRange("2026-09-24 10:30:05", "2026-09-24 10:30:08")
		require.NoError(t, err)
		assert.Equal(t, a.Unix(), b.Unix(), "datetime-local 用 T，手填常用空格，两者应一致")
	})

	t.Run("缺秒段按0秒", func(t *testing.T) {
		// datetime-local 在秒为 0 时会省略秒段。
		start, _, err := ResolveExportRange("2026-09-24T10:30", "2026-09-24T10:31")
		require.NoError(t, err)
		assert.Equal(t, 0, start.Second())
	})

	t.Run("只填日期仍是整段", func(t *testing.T) {
		start, end, err := ResolveExportRange("2026-09-24", "2026-09-29")
		require.NoError(t, err)
		assert.Equal(t, int64(1790179200), start.Unix(), "起点是当天 00:00:00")
		assert.Equal(t, int64(1790697599), end.Unix(), "终点补到当天 23:59:59（含）")
		// 与旧的「按天导出」行为完全一致：含 24~29 共 6 天整。
		assert.Equal(t, int64(6*24*3600-1), int64(end.Sub(start).Seconds()))
	})

	t.Run("时间倒置报错", func(t *testing.T) {
		_, _, err := ResolveExportRange("2026-09-24 10:00:00", "2026-09-24 09:00:00")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "不能早于")
	})

	t.Run("同一时刻可用", func(t *testing.T) {
		// 起止相同时表示「就看这一秒」，不是错误。
		start, end, err := ResolveExportRange("2026-09-24 10:00:00", "2026-09-24 10:00:00")
		require.NoError(t, err)
		assert.True(t, end.Equal(start))
	})

	t.Run("非法格式报错", func(t *testing.T) {
		_, _, err := ResolveExportRange("9/24/2026", "2026-09-29")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "开始时间")
	})

	t.Run("按北京时间解析而非UTC", func(t *testing.T) {
		start, _, err := ResolveExportRange("2026-09-24 00:00:00", "2026-09-24 01:00:00")
		require.NoError(t, err)
		assert.Equal(t, int64(1790179200), start.Unix(),
			"与文档第 1 节 date -d \"2026-09-24 00:00:00 +08:00\" +%s 一致")
	})
}

// TestExportQueryEndIsInclusive 右端点是 end+1s：created_at 是整数秒，
// `< end+1s` 等价于 `<= end`，闭区间语义靠这一步实现。
func TestExportQueryEndIsInclusive(t *testing.T) {
	end, _, err := ParseExportTime("2026-09-29 23:59:59")
	require.NoError(t, err)

	p := LogExportParams{EndTime: end}
	assert.Equal(t, end.Unix()+1, p.ExportQueryEnd().Unix())
	assert.False(t, p.ExportQueryEnd().After(end.Add(2*time.Second)))
}

// TestExportFilenameUsesGivenDays 文件名取起止时刻的日期本身。
// 闭区间下 EndTime 已是最后一个算在内的时刻，若再减一秒，
// 「结束于 09-29 00:00:00」会被标成 09-28，与实际导出内容不符。
func TestExportFilenameUsesGivenDays(t *testing.T) {
	cases := []struct {
		name      string
		start     string
		end       string
		wantPrefix string
	}{
		{"整段", "2026-09-01 00:00:00", "2026-09-30 23:59:59", "日志查询_2026-09-01_2026-09-30_"},
		{"结束在当天零点", "2026-09-01 00:00:00", "2026-09-29 00:00:00", "日志查询_2026-09-01_2026-09-29_"},
		{"只填日期", "2026-09-24", "2026-09-29", "日志查询_2026-09-24_2026-09-29_"},
		{"秒级窗口", "2026-09-24 10:30:05", "2026-09-24 10:30:08", "日志查询_2026-09-24_2026-09-24_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, err := ResolveExportRange(tc.start, tc.end)
			require.NoError(t, err)
			got := ExportLogFileName(LogExportParams{
				Usernames: []string{"test02"}, StartTime: start, EndTime: end,
			})
			assert.True(t, strings.HasPrefix(got, tc.wantPrefix),
				"文件名应以 %q 开头（日期段），实际 %q", tc.wantPrefix, got)
			assert.True(t, strings.HasSuffix(got, ".tsv"))
			// 含「日志查询」，以便出账时被 defaultOutputName 改名为「账单」。
			assert.Contains(t, got, "日志查询")
		})
	}
}

// TestExportFileNameDistinguishesAccounts 同名覆盖回归。
//
// 修复前文件名只有日期段：同一时间段给不同客户导出会得到完全相同的名字，
// 而落盘走 os.Rename——后一次导出静默覆盖前一次，数据直接丢且无任何提示。
func TestExportFileNameDistinguishesAccounts(t *testing.T) {
	start, end, err := ResolveExportRange("2026-09-01 00:00:00", "2026-09-30 23:59:59")
	require.NoError(t, err)

	nameOf := func(usernames []string, ids []int, includeUID bool) string {
		return ExportLogFileName(LogExportParams{
			Usernames: usernames, UserIDs: ids,
			IncludeUserID: includeUID, StartTime: start, EndTime: end,
		})
	}

	accA := nameOf([]string{"a37836323"}, nil, false)
	accB := nameOf([]string{"test02"}, nil, false)
	accAB := nameOf([]string{"a37836323", "test02"}, nil, false)

	assert.NotEqual(t, accA, accB, "不同账号必须得到不同文件名，否则会互相覆盖")
	assert.NotEqual(t, accA, accAB, "账号集合不同也必须区分开")
	assert.NotEqual(t, accB, accAB)

	// 同一小段日期前缀，便于人工按时间排序查找。
	assert.Contains(t, accA, "2026-09-01_2026-09-30")

	// 账号输入顺序不同视为同一组，不产生多余文件。
	assert.Equal(t, accAB, nameOf([]string{"test02", "a37836323"}, nil, false),
		"账号顺序不同应得到同一个名字（先排序再算指纹）")
	assert.Equal(t, nameOf([]string{"Test02"}, nil, false), nameOf([]string{"test02"}, nil, false),
		"大小写不同视为同一账号")

	// 用户 ID、是否带 user_id 列、时间区间变化都要体现在名字里。
	assert.NotEqual(t, nameOf(nil, []int{42}, false), nameOf(nil, []int{43}, false),
		"不同用户 ID 必须区分")
	assert.NotEqual(t, accA, nameOf([]string{"a37836323"}, nil, true),
		"是否追加 user_id 列会影响列结构，名字也要不同")
	otherStart, _, err := ResolveExportRange("2026-08-01 00:00:00", "2026-08-31 23:59:59")
	require.NoError(t, err)
	assert.NotEqual(t, accA, ExportLogFileName(LogExportParams{
		Usernames: []string{"a37836323"}, StartTime: otherStart, EndTime: otherStart.Add(24 * time.Hour),
	}), "不同时间段必须区分")

	// 指纹不带账号明文——文件名会出现在日志与页面里，不该直接暴露客户账号。
	assert.NotContains(t, accA, "a37836323")
	assert.NotContains(t, accB, "test02")

	// 指纹定长 8 位，名字长度稳定。
	parts := strings.Split(strings.TrimSuffix(accA, ".tsv"), "_")
	assert.Len(t, parts[len(parts)-1], 8, "指纹应为 8 位十六进制")
}

func indexOfHeader(headers []string, name string) int {
	for i, h := range headers {
		if h == name {
			return i
		}
	}
	return -1
}

// TestNullValuesRenderAsLiteralNULL NULL 按 mysql 客户端约定输出字面量 NULL。
func TestNullValuesRenderAsLiteralNULL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nulls.tsv")

	// group / token_name 为 NULL 的老数据行。
	record := []string{"1", "u", "2", "1789470821", "1", "NULL", "m", "NULL",
		"1", "1", "1", "1", "0", "req", "NULL", "NULL"}

	f, err := os.Create(path)
	require.NoError(t, err)
	w := bufio.NewWriter(f)
	require.NoError(t, writeTSVRecord(w, LogExportColumns(false)))
	require.NoError(t, writeTSVRecord(w, record))
	require.NoError(t, w.Flush())
	require.NoError(t, f.Close())

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	// 这一行有三处 NULL：token_name、group、other；表头行不含 NULL。
	// 四个 NULL：token_name、group、other、channel_id（表头行不含 NULL）。
	assert.Equal(t, 4, strings.Count(string(content), "NULL"), "NULL 应写成字面量")

	headers, rows, err := LoadLogRows(path, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	for _, col := range []string{"token_name", "group", "other"} {
		assert.Equal(t, "NULL", rows[0][indexOfHeader(headers, col)], "%s 列的 NULL 应保留", col)
	}
	// 非 NULL 的列不受影响。
	assert.Equal(t, "u", rows[0][indexOfHeader(headers, "username")])
	assert.Equal(t, "0", rows[0][indexOfHeader(headers, "is_stream")], "is_stream 应是 0/1 而不是 true/false")
}
