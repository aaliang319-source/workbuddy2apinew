# -*- coding: utf-8 -*-
"""
以 dept_id=1960 的 26 条标准用药字典为模板，
为其余 18 个 dept_id 各生成一份 INSERT。

规则：
  - dept_id 替换为目标科室
  - pham_code 保持 P001~P026 原样（科室字典，按 dept_id 作用域）
  - create_time / update_time 按科室平移（第 N 个科室 +N 分钟）
  - 其余字段（pham_name / pham_spec / dosage / pathway_name 等）原样继承
"""
from datetime import datetime, timedelta

BASE_SQL = """
('Gn', 'P001', '注射用重组人促卵泡激素（果纳芬）', NULL, NULL, '75IU/支', '一日一次', NULL, '皮下注射', NULL, NULL, '150~225', 'IU', NULL, '8-12', '是', '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:14:57', '13100001111', '2026-09-24 15:14:57', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('Gn', 'P002', '注射用重组人促卵泡激素（果纳芬笔）', NULL, NULL, '300IU/0.5ml/支', '一日一次', NULL, '皮下注射', NULL, NULL, '150~225', 'IU', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:15:37', '13100001111', '2026-09-24 15:15:37', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('Gn', 'P003', '重组促卵泡素β注射液（普丽康）', NULL, NULL, '300IU/0.36ml/支', '一日一次', NULL, '皮下注射', NULL, NULL, '150~225', 'IU', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:53:16', '13100001111', '2026-09-24 15:53:16', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('Gn', 'P004', '注射用尿促卵泡素（丽申宝）', NULL, NULL, '75IU/支', '一日一次', NULL, '肌肉注射', NULL, NULL, '150~225', 'IU', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:53:48', '13100001111', '2026-09-24 15:53:48', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('Gn', 'P005', '注射用尿促性素（hMG）', NULL, NULL, '75IU/支', '一日一次', NULL, '肌肉注射', NULL, NULL, '75IU/支', 'IU', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:54:18', '13100001111', '2026-09-24 15:54:18', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('Gn', 'P006', '注射用高纯度尿促性素（贺美奇）', NULL, NULL, '75IU FSH+75IU LH/支', '一日一次', NULL, '肌肉注射', NULL, NULL, '150~225', 'IU', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:54:59', '13100001111', '2026-09-24 15:54:59', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('Gn', 'P007', '注射用重组人促卵泡激素（金赛恒）', NULL, NULL, '75IU/支', '一日一次', NULL, '皮下注射', NULL, NULL, '150~225', 'IU', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:56:33', '13100001111', '2026-09-24 15:56:33', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('其他', 'P008', '注射用曲普瑞林（达必佳）短效', NULL, NULL, '0.1mg/支', '一日一次', NULL, '皮下注射', NULL, NULL, '0.1', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:57:12', '13100001111', '2026-09-24 15:57:12', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('其他', 'P009', '注射用双羟萘酸曲普瑞林（达菲林）长效', NULL, NULL, '3.75mg/支', '1次', NULL, '肌肉注射', NULL, NULL, '3.75', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:57:45', '13100001111', '2026-09-24 15:57:45', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('其他', 'P010', '注射用醋酸曲普瑞林（达必佳）长效', NULL, NULL, '3.75mg/支', '1次', NULL, '肌肉注射', NULL, NULL, '3.75', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:58:21', '13100001111', '2026-09-24 15:58:21', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('GnRHanti', 'P011', '注射用西曲瑞克（思则凯）', NULL, NULL, '0.25mg/支', '一日一次', NULL, '皮下注射', NULL, NULL, '0.25', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:58:54', '13100001111', '2026-09-24 15:58:54', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('GnRHanti', 'P012', '注射用西曲瑞克（思则凯）', NULL, NULL, '3mg/支', '1次', NULL, '皮下注射', NULL, NULL, '3', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:59:21', '13100001111', '2026-09-24 15:59:21', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('GnRHanti', 'P013', '注射用加尼瑞克', NULL, NULL, '0.25mg/0.5ml/支', '一日一次', NULL, '皮下注射', NULL, NULL, '0.25', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 15:59:53', '13100001111', '2026-09-24 15:59:53', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('HMG', 'P014', '注射用绒促性素（hCG）', NULL, NULL, '2000IU/支', '1次', NULL, '肌肉注射', NULL, NULL, '5000~10000', 'IU', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:00:34', '13100001111', '2026-09-24 16:00:34', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('HMG', 'P015', '注射用重组人绒促性素（艾泽）', NULL, NULL, '250μg/0.5ml/支', '1次', NULL, '皮下注射', NULL, NULL, '250', 'ug', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:01:10', '13100001111', '2026-09-24 16:01:10', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('黄体酮', 'P016', '黄体酮注射液', NULL, NULL, '20mg/支', '一日一次', NULL, '肌肉注射', NULL, NULL, '40-60', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:01:57', '13100001111', '2026-09-24 16:01:57', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('黄体酮', 'P017', '地屈孕酮片（达芙通）', NULL, NULL, '10mg/片', '一日两次', NULL, '口服', NULL, NULL, '10', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:02:32', '13100001111', '2026-09-24 16:02:32', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('黄体酮', 'P018', '黄体酮软胶囊（安琪坦）', NULL, NULL, '100mg/粒', '一日两次', NULL, '阴塞', NULL, NULL, '200-300', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:03:02', '13100001111', '2026-09-24 16:03:02', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('其他', 'P019', '黄体酮阴道缓释凝胶（雪诺同）', NULL, NULL, '90mg/支', '一日一次', NULL, '阴塞', NULL, NULL, '90', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:03:32', '13100001111', '2026-09-24 16:03:32', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('其他', 'P020', '黄体酮栓（来婷）', NULL, NULL, '25mg/枚', '一日两次', NULL, '阴塞', NULL, NULL, '25-50', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:04:05', '13100001111', '2026-09-24 16:04:05', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('其他', 'P021', '戊酸雌二醇片（补佳乐）', NULL, NULL, '1mg/片', '一日一次', NULL, '口服', NULL, NULL, '1-4', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:04:39', '13100001111', '2026-09-24 16:04:39', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('其他', 'P022', '雌二醇片/雌二醇地屈孕酮片复合（芬吗通）', NULL, NULL, '1mg/片', '一日一次', NULL, '口服', NULL, NULL, '1', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:05:06', '13100001111', '2026-09-24 16:05:06', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('其他', 'P023', '来曲唑片（弗隆）', NULL, NULL, '2.5mg/片', '一日一次', NULL, '口服', NULL, NULL, '2.5-5', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:05:33', '13100001111', '2026-09-24 16:05:33', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('CC', 'P024', '枸橼酸氯米芬胶囊（克罗米芬）', NULL, NULL, '50mg/片', '一日一次', NULL, '口服', NULL, NULL, '500-100', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:05:58', '13100001111', '2026-09-24 16:05:58', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('其他', 'P025', '重组人生长激素（rhGH）', NULL, NULL, '4IU/支', '一日一次', NULL, '皮下注射', NULL, NULL, '4-5', 'IU', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:06:30', '13100001111', '2026-09-24 16:06:30', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL),
('其他', 'P026', '戊酸雌二醇片（补佳乐）内膜准备', NULL, NULL, '1mg/片', '一日一次', NULL, '口服', NULL, NULL, '2-6', 'mg', NULL, NULL, NULL, '1', NULL, 1960, 25090316333823410, '13100001111', '2026-09-24 16:07:00', '13100001111', '2026-09-24 16:07:00', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)
"""

DEPTS = [1961, 1996, 2001, 2006, 2002, 2003, 2004, 1988, 2022,
         2005, 1986, 2007, 1870, 2008, 2009, 2011, 2020, 2019]

COLUMNS = ("`pham_type`, `pham_code`, `pham_name`, `pham_good_name`, `pham_py`, "
           "`pham_spec`, `frequ_name`, `frequ_code`, `pathway_name`, `pham_sample_py`, "
           "`count`, `dosage`, `dosage_unit`, `dosage_unit_name`, `use_day`, `shot_flag`, "
           "`is_enable`, `is_del`, `dept_id`, `create_id`, `create_by`, `create_time`, "
           "`update_by`, `update_time`, `advice_flag`, `advice_type`, `base_flag`, "
           "`bag_spec`, `bag_unit`, `produce_factory`, `approval_no`, `drug_Code`, "
           "`antibiotics`, `pham_b_type`, `price`, `hivf_json`, `infusion_flag`, `days`, `useflag`")

NCOL = 39
OUT_PATH = r"D:\workspace\magic\mz_web_reproductive_medicine_seed.sql"


def parse_base_rows():
    rows = []
    for raw in BASE_SQL.strip().splitlines():
        line = raw.strip().rstrip(',')
        if not line.startswith('('):
            continue
        fields = [p.strip() for p in line[1:-1].split(',')]
        assert len(fields) == NCOL, '字段数 %d != %d: %s' % (len(fields), NCOL, line[:60])
        rows.append(fields)
    return rows


def main():
    base = parse_base_rows()
    assert len(base) == 26, '模板行数应为 26，实际 %d' % len(base)

    out = ['-- 辅助生殖用药字典（mz_web_reproductive_medicine）批量生成',
           '-- 模板：dept_id = 1960（26 项）；本文件生成其余 %d 个科室，共 %d 行' % (len(DEPTS), len(DEPTS) * 26),
           'SET NAMES utf8mb4;',
           '']

    for di, dept in enumerate(DEPTS):
        offset = timedelta(minutes=di + 1)   # 每个科室整体后移 di+1 分钟
        body = []
        for f in base:
            ct = datetime.strptime(f[21].strip("'"), '%Y-%m-%d %H:%M:%S') + offset
            ct = ct.strftime('%Y-%m-%d %H:%M:%S')

            g = list(f)
            g[18] = str(dept)          # dept_id
            g[21] = "'%s'" % ct        # create_time
            g[23] = "'%s'" % ct        # update_time
            body.append('(' + ', '.join(g) + ')')

        out.append('-- dept_id = %d' % dept)
        out.append('INSERT INTO `mz_web_reproductive_medicine` (%s) VALUES' % COLUMNS)
        out.append(',\n'.join(body) + ';')
        out.append('')

    with open(OUT_PATH, 'w', encoding='utf-8', newline='\n') as fp:
        fp.write('\n'.join(out))

    print('OK -> %s' % OUT_PATH)
    print('科室数: %d, 总行数: %d' % (len(DEPTS), len(DEPTS) * 26))


if __name__ == '__main__':
    main()
