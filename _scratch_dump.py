import openpyxl

TIER = "data/按照阶梯计费计算价格.xlsx"
OLD = "data/未按照阶梯计费计算价格.xlsx"

COLS = [1, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 19, 20, 22, 23, 24, 28]
LETTERS = ['A', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'J', 'K', 'L', 'M', 'S', 'T', 'V', 'W', 'X', 'AB']


def dump(path, label):
    wb = openpyxl.load_workbook(path)
    ws = wb[wb.sheetnames[0]]
    print("#### %s sheet=%s max_row=%d max_col=%d" % (label, ws.title, ws.max_row, ws.max_column))
    for r in range(3, ws.max_row + 1):
        parts = []
        for L, c in zip(LETTERS, COLS):
            v = ws.cell(row=r, column=c).value
            if v is None:
                v = ''
            parts.append("%s=%s" % (L, v))
        print("r%-3d %s" % (r, " | ".join(parts)))
    print()


dump(TIER, "TIERED")
dump(OLD, "OLD")
