# -*- coding: utf-8 -*-
"""
以 dept_id=1960 的 31 条标准科室项目为模板，
为其余 18 个 dept_id 各生成一份 INSERT。

规则：
  - 业务唯一码 code = yyMMddHHmmss(12) + 2位随机段 + 3位序号 + 1位尾号
    · 前置时间戳随科室平移（每个科室整体 +N 分钟），保证跨科室不重复
    · 2 位随机段按科室序号做偏移，进一步避免碰撞
  - 名称/类型/时长/创建人等字段原样继承
"""
from datetime import datetime, timedelta

BASE_SQL = """
('基础激素', '26092410004416901', 1960, 25090316333823410, '13100001111', '2026-09-24 10:00:44', '13100001111', '2026-09-24 10:00:44', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('AMH', '26092410010072102', 1960, 25090316333823410, '13100001111', '2026-09-24 10:01:01', '13100001111', '2026-09-24 10:01:01', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('输血筛查', '26092410011309203', 1960, 25090316333823410, '13100001111', '2026-09-24 10:01:13', '13100001111', '2026-09-24 10:01:13', NULL, 'hy', 180, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('乙肝两对半', '26092410012397204', 1960, 25090316333823410, '13100001111', '2026-09-24 10:01:24', '13100001111', '2026-09-24 10:01:24', NULL, 'hy', 180, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('甲功', '26092410013613705', 1960, 25090316333823410, '13100001111', '2026-09-24 10:01:36', '13100001111', '2026-09-24 10:01:36', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('肝功', '26092410015873206', 1960, 25090316333823410, '13100001111', '2026-09-24 10:01:59', '13100001111', '2026-09-24 10:01:59', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('肾功', '26092410020819807', 1960, 25090316333823410, '13100001111', '2026-09-24 10:02:08', '13100001111', '2026-09-24 10:02:08', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('血糖', '26092410021924408', 1960, 25090316333823410, '13100001111', '2026-09-24 10:02:19', '13100001111', '2026-09-24 10:02:19', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('同型半胱氨酸', '26092410022991809', 1960, 25090316333823410, '13100001111', '2026-09-24 10:02:30', '13100001111', '2026-09-24 10:02:30', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('CA-125', '26092410024129210', 1960, 25090316333823410, '13100001111', '2026-09-24 10:02:41', '13100001111', '2026-09-24 10:02:41', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('抗心磷脂抗体', '26092410025537911', 1960, 25090316333823410, '13100001111', '2026-09-24 10:02:55', '13100001111', '2026-09-24 10:02:55', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('血沉', '26092410030756812', 1960, 25090316333823410, '13100001111', '2026-09-24 10:03:08', '13100001111', '2026-09-24 10:03:08', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('血型', '26092410032003213', 1960, 25090316333823410, '13100001111', '2026-09-24 10:03:20', '13100001111', '2026-09-24 10:03:20', NULL, 'hy', 99999, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('血常规', '26092410033782414', 1960, 25090316333823410, '13100001111', '2026-09-24 10:03:38', '13100001111', '2026-09-24 10:03:38', NULL, 'hy', 90, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('凝血六项', '26092410034778415', 1960, 25090316333823410, '13100001111', '2026-09-24 10:03:48', '13100001111', '2026-09-24 10:03:48', NULL, 'hy', 90, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('尿常规', '26092410273006616', 1960, 25090316333823410, '13100001111', '2026-09-24 10:27:30', '13100001111', '2026-09-24 10:27:30', NULL, 'hy', 180, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('白带常规', '26092410274081817', 1960, 25090316333823410, '13100001111', '2026-09-24 10:27:41', '13100001111', '2026-09-24 10:27:41', NULL, 'hy', 30, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('胸片/结核', '26092410275819418', 1960, 25090316333823410, '13100001111', '2026-09-24 10:27:58', '13100001111', '2026-09-24 10:27:58', NULL, 'jc', 180, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('心电图', '26092410280904819', 1960, 25090316333823410, '13100001111', '2026-09-24 10:28:09', '13100001111', '2026-09-24 10:28:09', NULL, 'jc', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('输卵管造影', '26092410282137120', 1960, 25090316333823410, '13100001111', '2026-09-24 10:28:21', '13100001111', '2026-09-24 10:28:21', NULL, 'jc', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('TCT', '26092410374752221', 1960, 25090316333823410, '13100001111', '2026-09-24 10:37:48', '13100001111', '2026-09-24 10:37:48', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('UU+CT+NG', '26092410380193322', 1960, 25090316333823410, '13100001111', '2026-09-24 10:38:02', '13100001111', '2026-09-24 10:38:02', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('B超', '26092410381355323', 1960, 25090316333823410, '13100001111', '2026-09-24 10:38:14', '13100001111', '2026-09-24 10:38:14', NULL, 'jc', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('HPV', '26092410383011024', 1960, 25090316333823410, '13100001111', '2026-09-24 10:38:30', '13100001111', '2026-09-24 10:38:30', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('宫腔镜', '26092410384911225', 1960, 25090316333823410, '13100001111', '2026-09-24 10:38:49', '13100001111', '2026-09-24 10:38:49', NULL, 'jc', 180, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('腹腔镜', '26092410385654026', 1960, 25090316333823410, '13100001111', '2026-09-24 10:38:57', '13100001111', '2026-09-24 10:38:57', NULL, 'jc', 180, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('染色体', '26092410390989027', 1960, 25090316333823410, '13100001111', '2026-09-24 10:39:10', '13100001111', '2026-09-24 10:39:10', NULL, 'hy', 99999, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('胰岛素抵抗', '26092410392211728', 1960, 25090316333823410, '13100001111', '2026-09-24 10:39:22', '13100001111', '2026-09-24 10:39:22', NULL, 'hy', 180, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('精液常规', '26092410394218129', 1960, 25090316333823410, '13100001111', '2026-09-24 10:39:42', '13100001111', '2026-09-24 10:39:42', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('精子形态分析', '26092410395183330', 1960, 25090316333823410, '13100001111', '2026-09-24 10:39:52', '13100001111', '2026-09-24 10:39:52', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('精液白细胞', '26092410400179531', 1960, 25090316333823410, '13100001111', '2026-09-24 10:40:02', '13100001111', '2026-09-24 10:40:02', NULL, 'hy', 360, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'local', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)
"""

DEPTS = [1961, 1996, 2001, 2006, 2002, 2003, 2004, 1988, 2022,
         2005, 1986, 2007, 1870, 2008, 2009, 2011, 2020, 2019]

COLUMNS = ("`name`, `code`, `dept_id`, `create_id`, `create_by`, `create_time`, "
           "`update_by`, `update_time`, `remark`, `type`, `duration`, `b_type`, "
           "`name_py`, `type_child`, `department`, `location`, `check_req`, `price`, "
           "`from_s`, `item_num`, `item_code`, `item_name`, `item_namepy`, `specimenno`, "
           "`specimenname`, `usedstate`, `valideday`, `printorder`, `printoneself`, `item_type`")

OUT_PATH = r"D:\workspace\magic\mz_laboratory_seed.sql"


def parse_base_rows():
    rows = []
    for raw in BASE_SQL.strip().splitlines():
        line = raw.strip().rstrip(',')
        if not line.startswith('('):
            continue
        # 字段内不含逗号，直接按逗号切分
        rows.append([p.strip() for p in line[1:-1].split(',')])
    return rows


def main():
    base = parse_base_rows()
    assert len(base) == 31, '模板行数应为 31，实际 %d' % len(base)

    out = ['-- 科室项目（mz_laboratory）标准数据批量生成',
           '-- 模板：dept_id = 1960（31 项）；本文件生成其余 %d 个科室，共 %d 行' % (len(DEPTS), len(DEPTS) * 31),
           'SET NAMES utf8mb4;',
           '']

    seen_codes = set()
    for di, dept in enumerate(DEPTS):
        offset = timedelta(minutes=di + 1)   # 每个科室整体后移 di+1 分钟
        body = []
        for f in base:
            code0 = f[1].strip("'")
            assert len(code0) == 17, '模板 code 长度异常: %s' % code0
            rnd2 = int(code0[12:14])     # 前 12 位为 yyMMddHHmmss
            seq3 = code0[14:17]          # 后 3 位在科室内唯一

            dt = datetime.strptime(f[5].strip("'"), '%Y-%m-%d %H:%M:%S') + offset
            ts12 = dt.strftime('%y%m%d%H%M%S')
            ct = dt.strftime('%Y-%m-%d %H:%M:%S')

            code = '%s%02d%s' % (ts12, (rnd2 + di) % 100, seq3)
            assert len(code) == 17, '生成 code 长度异常: %s' % code
            seen_codes.add(code)

            g = list(f)
            g[1] = "'%s'" % code
            g[2] = str(dept)
            g[5] = "'%s'" % ct
            g[7] = "'%s'" % ct
            body.append('(' + ', '.join(g) + ')')

        out.append('-- dept_id = %d' % dept)
        out.append('INSERT INTO `mz_laboratory` (%s) VALUES' % COLUMNS)
        out.append(',\n'.join(body) + ';')
        out.append('')

    total = len(DEPTS) * 31
    assert len(seen_codes) == total, '业务码存在重复：%d/%d' % (len(seen_codes), total)

    with open(OUT_PATH, 'w', encoding='utf-8', newline='\n') as fp:
        fp.write('\n'.join(out))

    print('OK -> %s' % OUT_PATH)
    print('科室数: %d, 总行数: %d, 业务码唯一: %s' % (len(DEPTS), total, len(seen_codes) == total))


if __name__ == '__main__':
    main()
