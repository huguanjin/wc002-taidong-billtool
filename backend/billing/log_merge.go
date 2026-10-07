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
	// DroppedColumns 合并时从输入里剔除的列。目前只有脱敏日志形态的合并会剔除
	// channel_id（见 MergeLogs）。列出来是为了让调用方能告诉用户「输入里有这一列、
	// 结果里没有」，而不是悄悄少一列——这个函数的原则本来就是不默默丢数据。
	DroppedColumns []string
	Format         string
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

// channelIDColumn 渠道号列名。脱敏日志不得带它（见 SanitizedDropColumns）。
const channelIDColumn = "channel_id"

// headerIndex 表头里某列的下标，找不到返回 -1。
// 与 mergeColumnPermutation 一样按去掉首尾空白后的列名比对。
func headerIndex(headers []string, name string) int {
	for i, h := range headers {
		if strings.TrimSpace(h) == name {
			return i
		}
	}
	return -1
}

// isSanitizedLogHeaders 表头是不是脱敏日志的形态：没有 other，且带脱敏才会展开的明细列。
//
// 判据必须**只命中脱敏日志**——原始导出日志合并后还要拿去估成本，channel_id 得留着：
//   - 原始导出一定带 other（它是必选导出列，见 logRequiredColumns），脱敏日志一定没有；
//   - 只看「没有 other」还不够：手工 SQL 导出的日志同样没有 other，
//     所以再要求带 uncached_input_tokens，那是脱敏日志才会展开出来的明细列。
//
// 这条判据对「带 channel_id 的脱敏日志」没有漏网：channel_id 是在脱敏明细列之后
// 才进导出列的，凡带 channel_id 的脱敏日志必然已经带着 uncached_input_tokens。
func isSanitizedLogHeaders(headers []string) bool {
	return headerIndex(headers, "other") < 0 && headerIndex(headers, "uncached_input_tokens") >= 0
}

// dropColumn 把某一列从表头和所有行里去掉，返回是否真的去掉了。找不到该列时原样返回。
// 行是就地改短的：合并的源文件可能有几十万行，再复制一份只会把内存占用翻倍。
func dropColumn(headers []string, rows [][]string, name string) ([]string, [][]string, bool) {
	idx := headerIndex(headers, name)
	if idx < 0 {
		return headers, rows, false
	}
	outHeaders := make([]string, 0, len(headers)-1)
	outHeaders = append(outHeaders, headers[:idx]...)
	outHeaders = append(outHeaders, headers[idx+1:]...)
	for i, row := range rows {
		// 比表头短的行本来就没有这一列的值，不用动。
		if idx < len(row) {
			rows[i] = append(row[:idx], row[idx+1:]...)
		}
	}
	return outHeaders, rows, true
}

// MergeLogs 把多个日志文件按行拼成一个文件，表头取首个文件，后续文件按列名对齐。
// 列名与首个文件不一致、或出现首个文件没有的列时直接报错，避免默默拼出错位的日志。
//
// 唯一的例外是脱敏日志形态的合并：结果里不带 channel_id，输入里有也一律剔除
// （剔除了哪些列见 MergeResult.DroppedColumns）。
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

	// 脱敏日志形态的合并，结果里不得带 channel_id（渠道号不给客户看，见 SanitizedDropColumns）。
	//
	// 不处理的话有两种坏结果，都出在「旧版脱敏日志（那时还带 channel_id）」与新版混着合并时：
	//   · 新版在前当基准：旧版多出一列 channel_id，mergeColumnPermutation 直接报错，合并失败；
	//   · 旧版在前当基准：结果表头保留 channel_id，旧文件的行填着真实渠道号、新文件的行是空的——
	//     等于借合并把已经脱掉的渠道号又交了出去。
	// 判据只认脱敏形态（见 isSanitizedLogHeaders），原始日志的合并照旧保留 channel_id。
	sanitizedMerge := isSanitizedLogHeaders(baseHeaders)
	var ignoreColumns map[string]bool
	droppedChannel := false
	if sanitizedMerge {
		ignoreColumns = map[string]bool{channelIDColumn: true}
		baseHeaders, baseRows, droppedChannel = dropColumn(baseHeaders, baseRows, channelIDColumn)
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
		perm, err := mergeColumnPermutation(baseHeaders, headers, ignoreColumns)
		if err != nil {
			writer.Close()
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		// 后续文件里带着 channel_id 也要记下来：它在 mergeColumnPermutation 里被忽略了，
		// 不记的话调用方只会看到基准文件那一列，漏报「别的文件里也被剔除过」。
		if sanitizedMerge && headerIndex(headers, channelIDColumn) >= 0 {
			droppedChannel = true
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
	if droppedChannel {
		result.DroppedColumns = []string{channelIDColumn}
	}
	if result.RowCount == 0 {
		return nil, fmt.Errorf("合并结果没有任何数据行")
	}
	return result, nil
}

// mergeColumnPermutation 返回把 src 表的列重排成 base 表顺序的映射：
// perm[i] 是 base 第 i 列在 src 中的下标，-1 表示 src 缺少这一列（按空值补齐）。
// src 里出现 base 没有的列（基准没有的新列）仍然报错，避免默默丢弃数据——
// 这专门用于兼容「脱敏日志新增列后，旧版/新版产物混合合并」的场景：
// 旧版缺的新列允许留空，但不允许新版文件反过来悄悄缺列。
//
// ignore 里的列是例外：src 有、base 没有时不报错，也不进结果（perm 只按 base 的列建）。
// 这是给「该列是故意从结果里剔除的」用的（见 MergeLogs 对 channel_id 的处理），
// 不是放宽对错位的检查——除这几列之外的陌生列照样报错。
func mergeColumnPermutation(base, src []string, ignore map[string]bool) ([]int, error) {
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
	baseSet := make(map[string]bool, len(base))
	for _, h := range base {
		if key := strings.TrimSpace(h); key != "" {
			baseSet[key] = true
		}
	}
	for key := range index {
		if !baseSet[key] && !ignore[key] {
			return nil, fmt.Errorf("表头存在基准没有的列: %s", key)
		}
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
			perm[i] = -1 // 该文件缺少这一列，合并时按空值补齐
			continue
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
