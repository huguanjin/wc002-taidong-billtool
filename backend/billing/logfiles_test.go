package billing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildDataDir 造一个与真实 data 目录同构的临时目录：
// 既有可删的导出日志，也有不该被列/被删的模板与报价表。
func buildDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	mustWrite := func(name, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	// 可删的导出日志
	mustWrite("日志查询_2026-09-01_2026-09-30_ab12cd34.tsv", "a")
	mustWrite("日志查询_2026-08-01_2026-08-31_ef56gh78.tsv", "b")
	mustWrite("日志查询_2026-07-01_2026-07-31_ij90kl12.csv", "c")
	// 不该出现在列表里，更不该被删
	mustWrite("bill_template.xlsx", "template")
	mustWrite("price_table.xlsx", "prices")
	mustWrite("db_price_cache.json", "{}")
	mustWrite("账单_2026-09-01_2026-09-30.xlsx", "bill")
	mustWrite("notes.txt", "x")
	return dir
}

// TestListDataLogsOnlyExported 列表只应包含本工具导出的日志。
// data 目录里还放着账单模板与报价表，把它们混进「可删除列表」会让人误删。
func TestListDataLogsOnlyExported(t *testing.T) {
	dir := buildDataDir(t)

	files, err := ListDataLogs(dir)
	require.NoError(t, err)
	require.Len(t, files, 3, "只应列出 3 个导出日志")

	for _, f := range files {
		assert.True(t, strings.HasPrefix(f.Name, ExportFilePrefix), "%s 应带导出前缀", f.Name)
		assert.True(t, dataLogExts[strings.ToLower(filepath.Ext(f.Name))], "%s 应是允许的扩展名", f.Name)
		assert.Greater(t, f.SizeBytes, int64(0))
		assert.False(t, f.ModifiedAt.IsZero())
		assert.Equal(t, filepath.Join(dir, f.Name), f.Path, "Path 应是可直接回填的绝对路径")
	}

	// 模板与报价表不能出现。
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name)
	}
	for _, forbidden := range []string{"bill_template.xlsx", "price_table.xlsx", "db_price_cache.json", "账单_2026-09-01_2026-09-30.xlsx"} {
		assert.NotContains(t, names, forbidden, "%s 不应出现在可删列表里", forbidden)
	}
}

// TestListDataLogsSortedByTimeDesc 按修改时间倒序，最近的排在最前面，便于清理。
func TestListDataLogsSortedByTimeDesc(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"日志查询_2026-07-01_2026-07-31_aaaaaaaa.tsv",
		"日志查询_2026-09-01_2026-09-30_bbbbbbbb.tsv",
		"日志查询_2026-08-01_2026-08-31_cccccccc.tsv",
	}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, n := range names {
		p := filepath.Join(dir, n)
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
		// 依次设置递增的修改时间：第 i 个文件比前一个晚一天。
		mt := base.AddDate(0, 0, i)
		require.NoError(t, os.Chtimes(p, mt, mt))
	}

	files, err := ListDataLogs(dir)
	require.NoError(t, err)
	require.Len(t, files, 3)
	assert.Equal(t, names[2], files[0].Name, "最新修改的应排最前")
	assert.Equal(t, names[1], files[1].Name)
	assert.Equal(t, names[0], files[2].Name)
}

// TestListDataLogsMissingDir 目录不存在时返回空列表而不是报错——
// 首次部署时 data 目录可能还没建。
func TestListDataLogsMissingDir(t *testing.T) {
	files, err := ListDataLogs(filepath.Join(t.TempDir(), "not-exist"))
	require.NoError(t, err)
	assert.Empty(t, files)
}

// TestDeleteDataLog 正常删除，且只删指定的那一个。
func TestDeleteDataLog(t *testing.T) {
	dir := buildDataDir(t)
	target := "日志查询_2026-09-01_2026-09-30_ab12cd34.tsv"

	removed, err := DeleteDataLog(dir, target)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, target), removed)

	_, statErr := os.Stat(filepath.Join(dir, target))
	assert.True(t, os.IsNotExist(statErr), "目标文件应已被删除")

	// 其余文件不受影响。
	assert.FileExists(t, filepath.Join(dir, "日志查询_2026-08-01_2026-08-31_ef56gh78.tsv"))
	assert.FileExists(t, filepath.Join(dir, "bill_template.xlsx"), "模板不能被误删")
	assert.FileExists(t, filepath.Join(dir, "price_table.xlsx"))
}

// TestDeleteDataLogGuardrails 删除的保护条件。
// 这是破坏性操作，任何一条失效都可能删掉不该删的东西。
func TestDeleteDataLogGuardrails(t *testing.T) {
	dir := buildDataDir(t)

	cases := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"空文件名", "", "不能为空"},
		{"只有空格", "   ", "不能为空"},
		{"目录穿越", "../bill_template.xlsx", "不合法"},
		{"子路径", "sub/日志查询_x.tsv", "不合法"},
		{"绝对路径", filepath.Join(dir, "日志查询_2026-09-01_2026-09-30_ab12cd34.tsv"), "不合法"},
		{"反斜杠路径", `sub\日志查询_x.tsv`, "不合法"},
		// 前缀不对：账单模板、报价表都不该能被删。
		{"模板文件", "bill_template.xlsx", "只能删除本工具导出的日志"},
		{"报价表", "price_table.xlsx", "只能删除本工具导出的日志"},
		{"价格缓存", "db_price_cache.json", "只能删除本工具导出的日志"},
		{"账单文件", "账单_2026-09-01_2026-09-30.xlsx", "只能删除本工具导出的日志"},
		{"txt 文件", "日志查询_x.txt", "只能删除本工具导出的日志"},
		{"无扩展名", "日志查询_x", "只能删除本工具导出的日志"},
		{"前缀对但不存在的扩展名", "日志查询_x.exe", "只能删除本工具导出的日志"},
		{"文件不存在", "日志查询_2020-01-01_2020-01-02_zzzzzzzz.tsv", "文件不存在"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DeleteDataLog(dir, tc.input)
			require.Error(t, err, "必须被拒绝：%s", tc.input)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	// 所有受保护文件都还在。
	for _, keep := range []string{"bill_template.xlsx", "price_table.xlsx", "db_price_cache.json", "账单_2026-09-01_2026-09-30.xlsx", "notes.txt"} {
		assert.FileExists(t, filepath.Join(dir, keep), "%s 不应被删除", keep)
	}
}

// TestDeleteDataLogRejectsDirectory 目标是目录时必须拒绝。
func TestDeleteDataLogRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, ExportFilePrefix+"adir.tsv")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	_, err := DeleteDataLog(dir, filepath.Base(sub))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "目录")
	assert.DirExists(t, sub, "目录不能被删掉")
}

// TestDeleteDataLogAllowsCSVAndXLSX 导出格式不止 tsv，csv/xlsx 同样可删。
func TestDeleteDataLogAllowsCSVAndXLSX(t *testing.T) {
	dir := t.TempDir()
	for _, ext := range []string{".tsv", ".csv", ".xlsx"} {
		n := ExportFilePrefix + "2026-09-01_2026-09-30_abc12345" + ext
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644))
		_, err := DeleteDataLog(dir, n)
		assert.NoError(t, err, "%s 应可删除", ext)
	}
}
