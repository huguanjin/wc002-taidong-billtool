package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"taidong-bill-backend/billing"
)

// TestHandleDeleteLogFileAcceptsMultipart 前端用 FormData 提交，content-type 是
// multipart/form-data。曾经这里用 r.ParseForm() 读参数，而 ParseForm 不解析
// multipart body，结果读出来永远是空串，接口稳定返回「文件名不能为空」。
// 这个测试钉住「multipart 提交必须能被正确解析」。
func TestHandleDeleteLogFileAcceptsMultipart(t *testing.T) {
	dir := t.TempDir()
	oldDataDir := dataDir
	dataDir = dir
	defer func() { dataDir = oldDataDir }()

	const name = "日志查询_2026-09-01_2026-09-30_ab12cd34.tsv"
	target := filepath.Join(dir, name)
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	// 按前端的方式构造 multipart 请求。
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("name", name); err != nil {
		t.Fatalf("写字段失败: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("关闭 writer 失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/delete-log-file", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()

	handleDeleteLogFile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，响应体 %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Deleted string `json:"deleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Deleted != name {
		t.Errorf("期望删除 %q，实际 %q", name, resp.Deleted)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("文件应已被删除")
	}
}

// TestHandleDeleteLogFileRejectsProtectedFiles 通过 HTTP 层再确认一次保护条件：
// 模板与报价表不能删，路径穿越要被挡。
func TestHandleDeleteLogFileRejectsProtectedFiles(t *testing.T) {
	dir := t.TempDir()
	oldDataDir := dataDir
	dataDir = dir
	defer func() { dataDir = oldDataDir }()

	protected := []string{"bill_template.xlsx", "price_table.xlsx", "db_price_cache.json"}
	for _, n := range protected {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("keep"), 0o644); err != nil {
			t.Fatalf("准备文件失败: %v", err)
		}
	}

	cases := []struct {
		label   string
		name    string
		missing bool // 是否完全不传 name 字段
	}{
		{"模板文件", "bill_template.xlsx", false},
		{"报价表", "price_table.xlsx", false},
		{"价格缓存", "db_price_cache.json", false},
		{"路径穿越", "../bill_template.xlsx", false},
		{"不传文件名", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			if !tc.missing {
				if err := mw.WriteField("name", tc.name); err != nil {
					t.Fatalf("写字段失败: %v", err)
				}
			}
			_ = mw.Close()

			req := httptest.NewRequest(http.MethodPost, "/api/delete-log-file", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			rec := httptest.NewRecorder()

			handleDeleteLogFile(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("期望 400，实际 %d，响应体 %s", rec.Code, rec.Body.String())
			}
		})
	}

	for _, n := range protected {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s 不应被删除: %v", n, err)
		}
	}
}

// TestHandleDataLogsListsExportedFiles 列表接口只返回可删的导出日志。
func TestHandleDataLogsListsExportedFiles(t *testing.T) {
	dir := t.TempDir()
	oldDataDir := dataDir
	dataDir = dir
	defer func() { dataDir = oldDataDir }()

	for _, n := range []string{
		"日志查询_2026-09-01_2026-09-30_ab12cd34.tsv",
		"bill_template.xlsx",
		"price_table.xlsx",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("准备文件失败: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/data-logs", nil)
	rec := httptest.NewRecorder()
	handleDataLogs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var resp struct {
		Files   []billing.DataLogFile `json:"files"`
		DataDir string                `json:"dataDir"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if len(resp.Files) != 1 {
		t.Fatalf("期望只列出 1 个导出日志，实际 %d 个: %+v", len(resp.Files), resp.Files)
	}
	if resp.Files[0].Name != "日志查询_2026-09-01_2026-09-30_ab12cd34.tsv" {
		t.Errorf("列出的文件不对: %s", resp.Files[0].Name)
	}
	if resp.DataDir != dir {
		t.Errorf("期望 dataDir=%s，实际 %s", dir, resp.DataDir)
	}
}
