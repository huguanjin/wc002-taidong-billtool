package billing

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuri/excelize/v2"
)

// MergeParams 一次日志合并的参数。
type MergeParams struct {
	Sheet    string // xlsx 源文件的工作表名；为空时按 LoadLogRows 的默认规则选择
	Encoding string // csv/tsv 源文件的编码；为空时自动探测
	Format   string // 合并结果格式："xlsx"（默认）| "csv" | "tsv"
	Dedupe   bool   // 是否丢弃完全相同的行（同一份日志被重复上传时使用）
	OutDir   string // 合并结果输出目录（通常为 data 目录）
}

// MergeResult 一次合并的结果摘要。
type MergeResult struct {
	Path        string
	Headers     []string
	InputCount  int
	InputRows   int // 各源文件数据行合计（去重前）
	RowCount    int // 实际写入合并文件的数据行数
	DroppedRows int // 因去重丢弃的行数
	Format      string
}

// mergeRowWriter 合并结果的写出目标：xlsx 或 csv/tsv。
type mergeRowWriter interface {
	WriteRow(row []string) error
	Close() error
}

// ExcelMergeWriter 把合并结果流式写为 xlsx；行数超过单个 sheet 上限时自动拆分到
// 「合并日志_2」「合并日志_3」……多个 sheet。
type ExcelMergeWriter struct {
	file       *excelize.File
	headers    []string
	sheetBase  string
	sheetIndex int
	stream     *excelize.StreamWriter
	rowNum     int
	path       string
}

func NewExcelMergeWriter(path string, headers []string) (*ExcelMergeWriter, error) {
	f := excelize.NewFile()
	sheetBase := "合并日志"
	if err := f.SetSheetName(f.GetSheetName(0), sheetBase); err != nil {
		return nil, err
	}
	w := &ExcelMergeWriter{file: f, headers: headers, sheetBase: sheetBase, path: path}
	if err := w.startSheet(sheetBase); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *ExcelMergeWriter) startSheet(name string) error {
	sw, err := w.file.NewStreamWriter(name)
	if err != nil {
		return err
	}
	headerRow := make([]interface{}, len(w.headers))
	for i, h := range w.headers {
		headerRow[i] = h
	}
	if err := sw.SetRow("A1", headerRow); err != nil {
		return err
	}
	w.sheetIndex++
	w.stream = sw
	w.rowNum = 1
	return nil
}

func (w *ExcelMergeWriter) WriteRow(row []string) error {
	if w.rowNum >= ExcelMaxRowsPerSheet {
		if err := w.stream.Flush(); err != nil {
			return err
		}
		next := fmt.Sprintf("%s_%d", w.sheetBase, w.sheetIndex+1)
		if _, err := w.file.NewSheet(next); err != nil {
			return err
		}
		if err := w.startSheet(next); err != nil {
			return err
		}
	}

	values := make([]interface{}, len(row))
	for i, v := range row {
		values[i] = cellValueForSanitized(v)
	}
	w.rowNum++
	axis, err := excelize.CoordinatesToCellName(1, w.rowNum)
	if err != nil {
		return err
	}
	return w.stream.SetRow(axis, values)
}

func (w *ExcelMergeWriter) Close() error {
	if err := w.stream.Flush(); err != nil {
		return err
	}
	return w.file.SaveAs(w.path)
}

// CSVMergeWriter 把合并结果流式写为 csv/tsv，没有 xlsx 单 sheet 的行数上限。
type CSVMergeWriter struct {
	f       *os.File
	w       *csv.Writer
	headers []string
	path    string
}

func NewCSVMergeWriter(path string, headers []string, delimiter rune) (*CSVMergeWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := csv.NewWriter(f)
	w.Comma = delimiter
	if err := w.Write(headers); err != nil {
		f.Close()
		return nil, err
	}
	return &CSVMergeWriter{f: f, w: w, headers: headers, path: path}, nil
}

func (w *CSVMergeWriter) WriteRow(row []string) error {
	return w.w.Write(row)
}

func (w *CSVMergeWriter) Close() error {
	w.w.Flush()
	if err := w.w.Error(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// MergeLogs 把多个日志文件按行拼成一个文件，表头取首个文件，后续文件按列名对齐。
// 列名与首个文件不一致、或出现首个文件没有的列时直接报错，避免默默拼出错位的日志。
func MergeLogs(inputPaths []string, params MergeParams) (*MergeResult, error) {
	if len(inputPaths) == 0 {
		return nil, fmt.Errorf("请至少提供一个日志文件")
	}
	format := strings.ToLower(params.Format)
	if format == "" {
		format = "xlsx"
	}
	if format != "xlsx" && format != "csv" && format != "tsv" {
		return nil, fmt.Errorf("不支持的合并结果格式: %s", params.Format)
	}

	baseHeaders, baseRows, err := readLogRows(inputPaths[0], params.Sheet, params.Encoding)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", filepath.Base(inputPaths[0]), err)
	}
	if len(baseHeaders) == 0 {
		return nil, fmt.Errorf("%s 没有表头", filepath.Base(inputPaths[0]))
	}

	outPath, err := resolveMergeOutputPath(params.OutDir, format)
	if err != nil {
		return nil, err
	}
	var writer mergeRowWriter
	switch format {
	case "csv":
		writer, err = NewCSVMergeWriter(outPath, baseHeaders, ',')
	case "tsv":
		writer, err = NewCSVMergeWriter(outPath, baseHeaders, '\t')
	default:
		writer, err = NewExcelMergeWriter(outPath, baseHeaders)
	}
	if err != nil {
		return nil, fmt.Errorf("初始化合并结果写出失败: %w", err)
	}

	result := &MergeResult{Path: outPath, Headers: baseHeaders, InputCount: len(inputPaths), Format: format}
	seen := map[string]bool{}
	writeRows := func(rows [][]string) error {
		for _, row := range rows {
			aligned, ok := normalizeMergeRow(row, baseHeaders)
			if !ok {
				continue // 整行为空（常见于日志末尾的空行），跳过
			}
			if params.Dedupe {
				key := strings.Join(aligned, "\x00")
				if seen[key] {
					result.DroppedRows++
					continue
				}
				seen[key] = true
			}
			if err := writer.WriteRow(aligned); err != nil {
				return err
			}
			result.RowCount++
		}
		return nil
	}

	if err := writeRows(baseRows); err != nil {
		writer.Close()
		return nil, err
	}
	result.InputRows = len(baseRows)

	for _, path := range inputPaths[1:] {
		headers, rows, err := readLogRows(path, params.Sheet, params.Encoding)
		if err != nil {
			writer.Close()
			return nil, fmt.Errorf("读取 %s 失败: %w", filepath.Base(path), err)
		}
		perm, err := mergeColumnPermutation(baseHeaders, headers)
		if err != nil {
			writer.Close()
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		aligned := make([][]string, 0, len(rows))
		for _, row := range rows {
			aligned = append(aligned, permuteMergeRow(row, perm))
		}
		if err := writeRows(aligned); err != nil {
			writer.Close()
			return nil, err
		}
		result.InputRows += len(rows)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("写出合并结果失败: %w", err)
	}
	if result.RowCount == 0 {
		return nil, fmt.Errorf("合并结果没有任何数据行")
	}
	return result, nil
}

// mergeColumnPermutation 返回把 src 表的列重排成 base 表顺序的映射：
// perm[i] 是 base 第 i 列在 src 中的下标。列名集合必须完全一致。
func mergeColumnPermutation(base, src []string) ([]int, error) {
	if len(base) != len(src) {
		return nil, fmt.Errorf("表头列数不一致（基准 %d 列，该文件 %d 列）", len(base), len(src))
	}
	index := make(map[string]int, len(src))
	for i, h := range src {
		key := strings.TrimSpace(h)
		if key == "" {
			continue
		}
		if _, dup := index[key]; dup {
			return nil, fmt.Errorf("表头存在重复列名: %s", key)
		}
		index[key] = i
	}
	perm := make([]int, len(base))
	for i, h := range base {
		key := strings.TrimSpace(h)
		if key == "" {
			perm[i] = -1
			continue
		}
		j, ok := index[key]
		if !ok {
			return nil, fmt.Errorf("缺少列: %s（与首个日志的列名不一致）", key)
		}
		perm[i] = j
	}
	return perm, nil
}

func permuteMergeRow(row []string, perm []int) []string {
	out := make([]string, len(perm))
	for i, j := range perm {
		if j >= 0 {
			out[i] = cellAt(row, j)
		}
	}
	return out
}

// normalizeMergeRow 把一行补齐/截断到表头列数；整行为空时返回 false。
func normalizeMergeRow(row []string, headers []string) ([]string, bool) {
	empty := true
	for _, v := range row {
		if strings.TrimSpace(v) != "" {
			empty = false
			break
		}
	}
	if empty {
		return nil, false
	}
	out := make([]string, len(headers))
	for i := range headers {
		out[i] = cellAt(row, i)
	}
	return out, true
}

// resolveMergeOutputPath 生成 data 目录下的合并结果文件名：沿用首个日志的文件名，
// 把「日志」换成「合并日志」；重名时追加 -2、-3 序号，避免覆盖上一次的合并结果。
// resolveMergeOutputPath 生成合并结果的落盘路径。合并的是多个文件，用第一个文件的名字命名
// 既没意义又容易撞名（上传文件还会带内部序号前缀），因此固定叫「合并日志」，
// 目录里已有同名文件时依次追加 -2、-3……，不覆盖已有文件。
func resolveMergeOutputPath(outDir string, format string) (string, error) {
	if strings.TrimSpace(outDir) == "" {
		return "", fmt.Errorf("未指定合并结果的输出目录")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	base := "合并日志"
	ext := "." + format
	for i := 1; i <= 100; i++ {
		name := base + ext
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", base, i, ext)
		}
		path := filepath.Join(outDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path, nil
		}
	}
	return "", fmt.Errorf("输出目录下同名文件过多: %s", base)
}
