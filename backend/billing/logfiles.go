package billing

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ExportFilePrefix 导出文件名的前缀。删除接口只认这个前缀，
// 避免把 data 目录下的账单模板、报价表、价格缓存一并删掉。
const ExportFilePrefix = "日志查询_"

// dataLogExts 允许列出与删除的扩展名。
var dataLogExts = map[string]bool{
	".tsv":  true,
	".csv":  true,
	".xlsx": true,
}

// DataLogFile 一条已导出的日志文件记录。
type DataLogFile struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"` // 绝对路径，可直接回填给「服务器路径」输入框
	SizeBytes  int64     `json:"sizeBytes"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

// ListDataLogs 列出 outDir 下由本工具导出的日志文件，按修改时间倒序。
//
// 只列「日志查询_」前缀 + 已知扩展名的文件：data 目录里还放着账单模板与报价表，
// 把它们混进列表会让人误以为可以删。
func ListDataLogs(outDir string) ([]DataLogFile, error) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取数据目录失败: %w", err)
	}

	files := make([]DataLogFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !looksLikeExportedLog(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // 单个文件取不到信息不影响整体列表
		}
		files = append(files, DataLogFile{
			Name:       e.Name(),
			Path:       filepath.Join(outDir, e.Name()),
			SizeBytes:  info.Size(),
			ModifiedAt: info.ModTime(),
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].ModifiedAt.After(files[j].ModifiedAt)
	})
	return files, nil
}

// looksLikeExportedLog 文件名是否符合「本工具导出的日志」这一形态。
func looksLikeExportedLog(name string) bool {
	return strings.HasPrefix(name, ExportFilePrefix) && dataLogExts[strings.ToLower(filepath.Ext(name))]
}

// DeleteDataLog 删除 outDir 下一个已导出的日志文件。
//
// 两道防线，缺一不可：
//  1. 只接受纯文件名（不含路径分隔符），杜绝 ../ 之类的目录穿越；
//  2. 解析出的绝对路径必须仍在 outDir 内，且文件名符合导出文件形态。
//
// 这里**不做**「删除前先确认」的交互——那是页面的责任，接口保持确定性。
func DeleteDataLog(outDir, name string) (string, error) {
	base := strings.TrimSpace(name)
	if base == "" {
		return "", fmt.Errorf("文件名不能为空")
	}
	// 只允许纯文件名：前端传过来的应该是列表里那一项的名字。
	if base != filepath.Base(base) || strings.ContainsAny(base, `/\`) {
		return "", fmt.Errorf("文件名不合法，只能填写文件名本身")
	}
	if !looksLikeExportedLog(base) {
		return "", fmt.Errorf("只能删除本工具导出的日志文件（%s*.tsv/csv/xlsx）", ExportFilePrefix)
	}

	target := filepath.Join(outDir, base)
	// 再核一次归属：即使符号链接之类让路径跑出 outDir，也挡住。
	rootAbs, err := filepath.Abs(outDir)
	if err != nil {
		return "", fmt.Errorf("解析数据目录失败: %w", err)
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("解析文件路径失败: %w", err)
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径超出数据目录范围")
	}

	info, err := os.Stat(targetAbs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("文件不存在：%s", base)
		}
		return "", fmt.Errorf("读取文件失败: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("目标是目录，不是文件：%s", base)
	}

	if err := os.Remove(targetAbs); err != nil {
		return "", fmt.Errorf("删除失败: %w", err)
	}
	return targetAbs, nil
}
