package loader

import (
	"errors"
	"fmt"
	"strings"

	"github.com/extrame/xls"
	"github.com/xuri/excelize/v2"
)

// maxSheetRows 单个工作表最多转化的行数，避免超大表格把上下文撑爆
const maxSheetRows = 5000

// extractXLSX 解析 .xlsx/.xlsm：逐工作表转为「行 = 单元格 | 单元格」的文本。
// 表格按行转文本，天然保留表头与每行数据的语义，适合分块后做 BM25 + 向量检索。
func extractXLSX(path string) (string, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return "", fmt.Errorf("打开 Excel 失败: %w", err)
	}
	defer f.Close()

	var b strings.Builder
	for _, sheet := range f.GetSheetList() {
		rows, err := f.GetRows(sheet)
		if err != nil {
			continue // 单表失败不影响其他表
		}
		if writeSheetText(&b, sheet, rows) {
			continue
		}
	}

	out := cleanText([]byte(b.String()))
	if strings.TrimSpace(out) == "" {
		return "", errors.New("Excel 中没有可提取的文本（工作表为空或只含图片）")
	}
	return out, nil
}

// extractXLS 解析旧版 .xls（OLE2 + BIFF）。依赖 extrame/xls，个别异常行做兜底跳过。
func extractXLS(path string) (string, error) {
	wb, err := xls.Open(path, "utf-8")
	if err != nil {
		return "", fmt.Errorf("打开 .xls 失败（若是 xlsx 请改扩展名）: %w", err)
	}

	var b strings.Builder
	for i := 0; i < wb.NumSheets(); i++ {
		sheet := wb.GetSheet(i)
		if sheet == nil {
			continue
		}
		rows := xlsRows(sheet)
		writeSheetText(&b, sheet.Name, rows)
	}

	out := cleanText([]byte(b.String()))
	if strings.TrimSpace(out) == "" {
		return "", errors.New("Excel 中没有可提取的文本（工作表为空）")
	}
	return out, nil
}

// xlsRows 读取工作表所有行；库对稀疏行可能返回 nil，用 recover 兜住异常。
func xlsRows(sheet *xls.WorkSheet) (rows [][]string) {
	defer func() {
		if r := recover(); r != nil {
			// 保留已读取的部分
		}
	}()
	for i := 0; i <= int(sheet.MaxRow); i++ {
		row := sheet.Row(i)
		if row == nil {
			rows = append(rows, nil)
			continue
		}
		last := row.LastCol()
		if last > 200 {
			last = 200
		}
		cells := make([]string, 0, last)
		for c := 0; c < last; c++ {
			cells = append(cells, strings.TrimSpace(row.Col(c)))
		}
		rows = append(rows, cells)
	}
	return rows
}

// writeSheetText 把一张表写入 buffer；返回是否写入了有效内容
func writeSheetText(b *strings.Builder, sheet string, rows [][]string) bool {
	// 先裁剪尾部空行，判断是否有效
	end := len(rows)
	for end > 0 && isBlankRow(rows[end-1]) {
		end--
	}
	if end == 0 {
		return false
	}
	if end > maxSheetRows {
		end = maxSheetRows
	}

	fmt.Fprintf(b, "\n## 工作表：%s\n", sheet)
	for _, row := range rows[:end] {
		line := joinRow(row)
		if line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if len(rows) > maxSheetRows {
		fmt.Fprintf(b, "（该表共 %d 行，已截断前 %d 行）\n", len(rows), maxSheetRows)
	}
	return true
}

func joinRow(row []string) string {
	// 去掉行尾空单元格
	end := len(row)
	for end > 0 && strings.TrimSpace(row[end-1]) == "" {
		end--
	}
	if end == 0 {
		return ""
	}
	cells := make([]string, 0, end)
	for _, c := range row[:end] {
		cells = append(cells, strings.ReplaceAll(strings.TrimSpace(c), "\n", " "))
	}
	return strings.Join(cells, " | ")
}

func isBlankRow(row []string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}
