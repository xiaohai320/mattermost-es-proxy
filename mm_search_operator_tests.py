#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Mattermost 搜索操作符回归测试（验证经 mm-es-proxy 注入 ngram 子串搜索）。

覆盖操作符：FROM: / IN: / ON: / BEFORE: / AFTER: / -(排除) / ""(短语)
以及它们与 ngram 子串匹配的组合场景。

前置条件（本脚本会自动准备，也可手动发好）：
  team qf9qrgw4s3nkdx9mz7fa3qqhjh 下两个频道里共 6 条 ops-test-* 消息，
  消息文本见下方 TEST_MESSAGES。

已知局限（非 bug，是 mm 9.9.1 本身行为，已用例固化）：
  - on:/before:/after: 只认 ISO 日期（2026-11-03），中文"今天/昨天"和
    英文 today/yesterday 解析失败 → 生成 range from:0 to:0 → 恒 0 条
  - in: 只认频道 name（URL slug，英文），中文 display_name 不转 ID
  - in: 中的频道必须属于当前搜索的 team（DM/GM 不在 team 搜索范围）

运行: python3 mm_search_operator_tests.py
"""
import json
import sys
import time
import urllib.error
import urllib.request

MM = 'http://192.168.248.34:8065'
TEAM = 'qf9qrgw4s3nkdx9mz7fa3qqhjh'
TOKEN_FILE = '/tmp/mm_admin_token'

TEST_MESSAGES = {
    'alpha':   'ops-test-alpha 部署完成 10.55.66.77',          # 频道"测试"
    'beta':    'ops-test-beta 记录联系人 wang.wzhang@example.com',  # 频道"测试"
    'gamma':   'ops-test-gamma 含引号短语 "磁盘阵列检查"',       # 频道"测试"
    'delta':   'ops-test-delta 普通 alpha 重复词',              # 频道"测试"
    'epsilon': 'ops-test-epsilon 最后一条测试消息',              # 频道"测试"
    'zeta':    'ops-test-zeta 闲聊频道部署 alpha 10.99.88.77',   # 频道"闲聊"(off-topic)
}
MSG_BY_KEY = TEST_MESSAGES
KEY_BY_MSG = {v: k for k, v in TEST_MESSAGES.items()}

TOKEN = open(TOKEN_FILE).read().strip()


def api(path, body=None, method='GET'):
    req = urllib.request.Request(MM + path, method=method)
    req.add_header('Authorization', 'Bearer ' + TOKEN)
    req.add_header('Content-Type', 'application/json')
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data=data, timeout=15) as resp:
            return json.load(resp)
    except urllib.error.HTTPError as e:
        return json.load(e)


def search(terms):
    r = api(f'/api/v4/teams/{TEAM}/posts/search', {
        'terms': terms, 'time_zone_offset': 480, 'is_or_search': False,
        'page': 0, 'per_page': 50, 'include_deleted_channels': True,
    }, 'POST')
    if isinstance(r, dict) and r.get('status_code'):
        raise RuntimeError(f"API error: {r.get('message')}")
    return r


def channel_by_display(name):
    for c in api(f'/api/v4/users/me/teams/{TEAM}/channels'):
        if c.get('display_name') == name:
            return c['id']
    raise RuntimeError(f'channel {name!r} not found')


def seed_messages():
    """幂等发种子消息：已存在（按文本匹配）则跳过。"""
    channels = {'测试': channel_by_display('测试'),
                '闲聊': channel_by_display('闲聊')}
    existing = set()
    r = search('from:admin ops-test')
    for pid in r.get('order', []):
        p = r['posts'].get(pid)
        if p:
            existing.add(p.get('message', ''))
    for key, text in TEST_MESSAGES.items():
        if text in existing:
            continue
        ch = channels['闲聊'] if key == 'zeta' else channels['测试']
        posted = api('/api/v4/posts', {'channel_id': ch, 'message': text}, 'POST')
        if 'id' not in posted:
            raise RuntimeError(f'seed {key} failed: {posted}')
    # 等 ES 实时索引落盘
    time.sleep(2)


CASES = []


def case(name, terms, want=frozenset(), absent=frozenset()):
    CASES.append((name, terms, want, absent))


# ---- 子串（ngram 核心场景）----
case('子串: IP 中段 55.66', '55.66', {'alpha'})
case('子串: 邮箱中段 zhang@example', 'zhang@example', {'beta'})
case('子串: 中文中段 盘阵列', '盘阵列', {'gamma'})
case('阴性: 不存在子串无结果', 'in:off-topic zzz999', set(),
     set(TEST_MESSAGES))
# ---- 短语 "" ----
case('短语: "磁盘阵列检查" 连续命中', '"磁盘阵列检查"', {'gamma'})
case('短语+排除: "磁盘阵列检查" -gamma', '"磁盘阵列检查" -gamma', set(),
     {'gamma'})
# ---- 排除 - ----
case('排除: alpha -部署（zeta 含部署也被排除）', 'alpha -部署', {'delta'},
     {'alpha', 'zeta'})
case('排除: alpha -磁盘阵列（3字排除词不误伤）', 'alpha -磁盘阵列',
     {'alpha', 'delta', 'zeta'}, {'gamma'})
# ---- FROM: ----
case('from: 命中 admin 全部测试消息', 'from:admin ops-test',
     set(TEST_MESSAGES))
case('from:+排除后为空', 'from:admin -ops-test', set(), set(TEST_MESSAGES))
# ---- IN:（只认英文 slug）----
case('in:off-topic 命中 zeta', 'in:off-topic zeta', {'zeta'})
case('in:+子串: off-topic 内 88.77', 'in:off-topic 88.77', {'zeta'})
case('in:+from: 组合', 'in:off-topic from:admin zeta', {'zeta'})
case('in:+排除: zeta 含部署被排除', 'in:off-topic alpha -部署', set(),
     {'zeta'})
# ---- ON:/BEFORE:/AFTER:（只认 ISO 日期）----
case('on: 今天的消息全命中', 'on:2026-11-03 ops-test', set(TEST_MESSAGES))
case('on: 越界日期无结果', 'on:2026-10-01 ops-test', set(), set(TEST_MESSAGES))
case('after: 昨天起全命中', 'after:2026-11-02 ops-test', set(TEST_MESSAGES))
case('before: 明天前全命中', 'before:2026-11-04 ops-test', set(TEST_MESSAGES))
case('before: 昨天前无结果', 'before:2026-11-02 ops-test', set(),
     set(TEST_MESSAGES))
# ---- 多操作符组合 ----
case('三操作符: in:+on:+子串', 'in:off-topic on:2026-11-03 88.77', {'zeta'})
case('组合: on:+短语+排除', 'on:2026-11-03 "磁盘阵列检查" -zeta', {'gamma'})
case('组合: on:+子串精确到单条', 'on:2026-11-03 55.66', {'alpha'})


def run():
    print('=== mm 搜索操作符回归（经 mm-es-proxy）===')
    seed_messages()
    passed = failed = 0
    for name, terms, want, absent in CASES:
        r = search(terms)
        got = set()
        for pid in r.get('order', []):
            p = r['posts'].get(pid)
            if p and p.get('message') in KEY_BY_MSG:
                got.add(KEY_BY_MSG[p['message']])
        bad = ([f'应命中未命中: {k} ({MSG_BY_KEY[k][:30]})' for k in want if k not in got] +
               [f'不应命中却命中: {k} ({MSG_BY_KEY[k][:30]})' for k in absent if k in got])
        if bad:
            print(f'✗ FAIL  {name}  terms={terms!r}')
            for b in bad:
                print('        ' + b)
            print(f'        实际命中: {sorted(got)}')
            failed += 1
        else:
            print(f'✓ PASS  {name}')
            passed += 1
    print(f'\n结果: {passed} passed, {failed} failed / {len(CASES)}')
    return failed


if __name__ == '__main__':
    raise SystemExit(1 if run() else 0)
