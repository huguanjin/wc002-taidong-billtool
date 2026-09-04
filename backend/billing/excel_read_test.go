package billing

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadLogRowsBareQuoteInField 覆盖 other 列携带原始 JSON（含裸引号）时的宽松解析。
func TestLoadLogRowsBareQuoteInField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "8月日志.tsv")
	content := "model_name\tgroup\tprompt_tokens\tcompletion_tokens\tquota\tother\tcreated_at\n" +
		`claude-sonnet-5` + "\t" + `default` + "\t" + `1000000` + "\t" + `200000` + "\t" + `50000` + "\t" +
		`{"cache_tokens":10000,"model_price":0.5}` + "\t" + `1755000000` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 tsv 失败: %v", err)
	}

	_, rows, err := LoadLogRows(path, "", "")
	if err != nil {
		t.Fatalf("LoadLogRows 失败: %v", err)
	}
	if len(rows) != 1 || rows[0][5] != `{"cache_tokens":10000,"model_price":0.5}` {
		t.Fatalf("other 列未按原始文本解析: %v", rows)
	}
}

func TestLoadLogRowsTSV(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "8月日志.tsv")
	content := "model_name\tgroup\tprompt_tokens\tcompletion_tokens\tquota\tother\tcreated_at\n" +
		"claude-sonnet-5\tdefault\t1000000\t200000\t50000\t{}\t1755000000\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 tsv 失败: %v", err)
	}

	headers, rows, err := LoadLogRows(path, "", "")
	if err != nil {
		t.Fatalf("LoadLogRows 失败: %v", err)
	}
	wantHeaders := []string{"model_name", "group", "prompt_tokens", "completion_tokens", "quota", "other", "created_at"}
	if len(headers) != len(wantHeaders) {
		t.Fatalf("表头数量不符，期望 %d 实际 %d: %v", len(wantHeaders), len(headers), headers)
	}
	for i, h := range wantHeaders {
		if headers[i] != h {
			t.Errorf("表头[%d] 期望 %q 实际 %q", i, h, headers[i])
		}
	}
	if len(rows) != 1 {
		t.Fatalf("数据行数期望 1，实际 %d", len(rows))
	}
	if rows[0][0] != "claude-sonnet-5" || rows[0][2] != "1000000" {
		t.Errorf("解析出的行内容不符: %v", rows[0])
	}
}

// TestExtractDistinctModels 覆盖去重、排序、以及缺少 model_name 列时报错。
func TestExtractDistinctModels(t *testing.T) {
	headers := []string{"model_name", "group"}
	rows := [][]string{
		{"gpt-5.4", "vip"},
		{"claude-sonnet-5", "default"},
		{"gpt-5.4", "default"},
		{" claude-sonnet-5 ", "vip"},
		{"", "vip"},
	}
	models, err := ExtractDistinctModels(headers, rows)
	if err != nil {
		t.Fatalf("ExtractDistinctModels 失败: %v", err)
	}
	want := []string{"claude-sonnet-5", "gpt-5.4"}
	if len(models) != len(want) {
		t.Fatalf("期望 %v，实际 %v", want, models)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Errorf("期望 %v，实际 %v", want, models)
			break
		}
	}
}

func TestExtractDistinctModelsMissingColumn(t *testing.T) {
	headers := []string{"group", "quota"}
	if _, err := ExtractDistinctModels(headers, [][]string{{"default", "100"}}); err == nil {
		t.Fatal("期望缺少 model_name 列时报错，实际未报错")
	}
}

// TestDetectDelimiterMisnamedCSV 覆盖「扩展名是 .csv 但内容其实是 Tab 分隔」的兼容场景。
func TestDetectDelimiterMisnamedCSV(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "日志.csv")
	content := "model_name\tgroup\tprompt_tokens\tcompletion_tokens\n" +
		"gpt-5.4\tvip\t1000\t200\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 csv 失败: %v", err)
	}

	headers, rows, err := LoadLogRows(path, "", "")
	if err != nil {
		t.Fatalf("LoadLogRows 失败: %v", err)
	}
	if len(headers) != 4 || headers[1] != "group" {
		t.Fatalf("误命名为 .csv 的 tsv 内容未被正确按 Tab 拆分: %v", headers)
	}
	if len(rows) != 1 || rows[0][1] != "vip" {
		t.Fatalf("数据行解析不符: %v", rows)
	}
}
