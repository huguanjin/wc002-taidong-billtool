package billing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

func writeTSVLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}

func writeXLSXLog(t *testing.T, path string, header []interface{}, rows [][]interface{}) {
	t.Helper()
	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	_ = f.SetSheetRow(sheet, "A1", &header)
	for i, row := range rows {
		axis := cellRef(i + 2)
		r := row
		_ = f.SetSheetRow(sheet, axis, &r)
	}
	if err := f.SaveAs(path); err != nil {
		t.Fatalf("保存 %s 失败: %v", path, err)
	}
}

const logHeader = "model_name\tgroup\tprompt_tokens\tcompletion_tokens\tquota\tother\tcreated_at\n"

// TestMergeLogsTSVToXLSX 覆盖最基本的场景：两个 tsv 拼成一个 xlsx，行数为两者之和。
func TestMergeLogsTSVToXLSX(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "data")
	a := filepath.Join(dir, "8月日志.tsv")
	b := filepath.Join(dir, "8月下旬日志.tsv")
	writeTSVLog(t, a, logHeader+"claude-sonnet-5\tdefault\t100\t20\t5\t{}\t1755000000\n")
	writeTSVLog(t, b, logHeader+"gpt-5.4\tvip\t200\t40\t10\t{}\t1755000001\n")

	result, err := MergeLogs([]string{a, b}, MergeParams{Format: "xlsx", OutDir: outDir})
	if err != nil {
		t.Fatalf("MergeLogs 失败: %v", err)
	}
	if result.RowCount != 2 || result.InputRows != 2 || result.InputCount != 2 {
		t.Fatalf("行数统计不符: %+v", result)
	}
	if filepath.Dir(result.Path) != outDir {
		t.Errorf("合并结果未输出到指定目录: %s", result.Path)
	}
	if filepath.Base(result.Path) != "合并日志.xlsx" {
		t.Errorf("合并结果文件名不符: %s", filepath.Base(result.Path))
	}

	headers, rows, err := LoadLogRows(result.Path, "", "")
	if err != nil {
		t.Fatalf("读取合并结果失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("合并结果行数期望 2，实际 %d", len(rows))
	}
	if headers[0] != "model_name" || headers[6] != "created_at" {
		t.Errorf("合并结果表头不符: %v", headers)
	}
	if rows[0][0] != "claude-sonnet-5" || rows[1][0] != "gpt-5.4" {
		t.Errorf("合并结果行内容/顺序不符: %v", rows)
	}
	// 纯数字列在 xlsx 里应仍是数字，避免后续出账解析出问题
	if rows[0][2] != "100" {
		t.Errorf("数字列被写成非数字: %q", rows[0][2])
	}
}

// TestMergeLogsColumnOrderDiffers 覆盖源文件列顺序不同时的按列名对齐。
func TestMergeLogsColumnOrderDiffers(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "日志A.tsv")
	b := filepath.Join(dir, "日志B.tsv")
	writeTSVLog(t, a, "model_name\tgroup\tquota\nmodel-a\tdefault\t100\n")
	writeTSVLog(t, b, "group\tquota\tmodel_name\ndefault\t200\tmodel-b\n")

	result, err := MergeLogs([]string{a, b}, MergeParams{Format: "tsv", OutDir: dir})
	if err != nil {
		t.Fatalf("MergeLogs 失败: %v", err)
	}
	_, rows, err := LoadLogRows(result.Path, "", "")
	if err != nil {
		t.Fatalf("读取合并结果失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("合并结果行数期望 2，实际 %d", len(rows))
	}
	if rows[1][0] != "model-b" || rows[1][1] != "default" || rows[1][2] != "200" {
		t.Errorf("第二份日志未按列名对齐: %v", rows[1])
	}
}

// TestMergeLogsHeaderMismatch 覆盖列名缺失时报错，而不是默默地拼出错位的日志。
func TestMergeLogsHeaderMismatch(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "日志A.tsv")
	b := filepath.Join(dir, "日志B.tsv")
	writeTSVLog(t, a, "model_name\tgroup\tquota\nmodel-a\tdefault\t100\n")
	writeTSVLog(t, b, "model_name\tgroup\tprompt_tokens\nmodel-b\tdefault\t50\n")

	if _, err := MergeLogs([]string{a, b}, MergeParams{Format: "xlsx", OutDir: dir}); err == nil {
		t.Fatal("期望列名不一致时报错，实际未报错")
	}
}

// TestMergeLogsDedupe 覆盖去重开关：完全相同的行只保留一次。
func TestMergeLogsDedupe(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "日志A.tsv")
	b := filepath.Join(dir, "日志B.tsv")
	row := "claude-sonnet-5\tdefault\t100\t20\t5\t{}\t1755000000\n"
	writeTSVLog(t, a, logHeader+row)
	writeTSVLog(t, b, logHeader+row+"gpt-5.4\tvip\t200\t40\t10\t{}\t1755000001\n")

	kept, err := MergeLogs([]string{a, b}, MergeParams{Format: "csv", OutDir: dir})
	if err != nil {
		t.Fatalf("MergeLogs 失败: %v", err)
	}
	if kept.RowCount != 3 || kept.DroppedRows != 0 {
		t.Fatalf("未去重时行数不符: %+v", kept)
	}

	dedupedDir := filepath.Join(dir, "dedup")
	deduped, err := MergeLogs([]string{a, b}, MergeParams{Format: "csv", OutDir: dedupedDir, Dedupe: true})
	if err != nil {
		t.Fatalf("MergeLogs 失败: %v", err)
	}
	if deduped.RowCount != 2 || deduped.DroppedRows != 1 {
		t.Fatalf("去重后行数不符: %+v", deduped)
	}
}

// TestMergeLogsMixedFormatAndGBK 覆盖 xlsx + GBK 编码 csv 混合来源。
func TestMergeLogsMixedFormatAndGBK(t *testing.T) {
	dir := t.TempDir()
	xlsxPath := filepath.Join(dir, "8月日志.xlsx")
	writeXLSXLog(t, xlsxPath,
		[]interface{}{"model_name", "group", "quota"},
		[][]interface{}{{"model-a", "默认", 100}},
	)

	// GBK 编码的 csv（含中文分组名），不显式指定 encoding 时自动探测
	csvPath := filepath.Join(dir, "8月日志2.csv")
	gbkContent := "model_name,group,quota\nmodel-b,默认,200\n"
	gbkBytes, _, err := transform.Bytes(simplifiedchinese.GBK.NewEncoder(), []byte(gbkContent))
	if err != nil {
		t.Fatalf("构造 GBK 内容失败: %v", err)
	}
	if err := os.WriteFile(csvPath, gbkBytes, 0o644); err != nil {
		t.Fatalf("写入 %s 失败: %v", csvPath, err)
	}

	result, err := MergeLogs([]string{xlsxPath, csvPath}, MergeParams{Format: "tsv", OutDir: dir})
	if err != nil {
		t.Fatalf("MergeLogs 失败: %v", err)
	}
	_, rows, err := LoadLogRows(result.Path, "", "")
	if err != nil {
		t.Fatalf("读取合并结果失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("合并结果行数期望 2，实际 %d", len(rows))
	}
	if rows[1][1] != "默认" {
		t.Errorf("GBK 源文件的中文未正确解码: %v", rows[1])
	}
}
