# mm-es-substring-proxy

[![Go Version](https://img.shields.io/badge/go-1.22-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/license-internal-lightgrey)](#许可)

一个介于 [Mattermost](https://mattermost.com) 与 Elasticsearch 之间的透明反向代理,为 mm 的消息搜索注入**任意位置子串匹配**能力——不修改 mm 源码,对 mm 完全无感知。

## 简介

Mattermost 的消息搜索是**分词级别**的:消息文本由 Elasticsearch 按 standard 分词器切成完整 token 存储,搜索时也按词匹配。这意味着:

- 搜消息里的完整词 ✅
- 搜**词的中段** ❌:消息里有 IP `192.168.231.42`,搜 `231.42` 无结果;邮箱 `wang.wzhang@example.com`,搜 `zhang@example` 无结果;中文「盘阵列检查」按单字切分,短语以外的子串也搜不到

而运维场景恰恰最需要按 IP 中段、主机名片段、工单号、邮箱片段来查聊天记录。

本项目的解法是在索引侧给 `message` 字段加一个 ngram 子字段(`message.ngram`,所有长度 2~20 的字符窗口都成为可查 token),在查询侧用一个透明代理拦截 mm 发往 ES 的搜索请求,把打到 `message` 字段的每条子句**原地**包成局部 OR:

```json
{
  "bool": {
    "should": [
      { "simple_query_string": { "query": "231.42", "fields": ["message"] } },
      { "match": { "message.ngram": { "query": "231.42", "minimum_should_match": "75%" } } }
    ],
    "minimum_should_match": 1
  }
}
```

原句(词级精确)命中**或** ngram 子串命中即返回;频道/用户/日期等权限过滤在 bool 的 `filter` 里,原地替换后自动继续生效,权限边界不变。

为什么不用其他方案:

| 方案 | 结论 |
|---|---|
| mm 插件 | ❌ 插件 API 没有搜索拦截点,改不了搜索 DSL |
| 改 mm 源码 | ❌ 需 fork、重编译,每次升级重打补丁 |
| **ES 查询重写代理(本项目)** | ✅ mm 的 `ConnectionURL` 是配置项,指向代理即可,mm 升级无感知 |

## 功能说明

- **子串搜索注入**:拦截 `POST {index}/_search` 且索引名匹配 `POSTS_PATTERN` 前缀的请求,解析 DSL 并为每条 message 子句注入 ngram 分支。覆盖 mm 实际使用的查询类型:`simple_query_string`、`query_string`、`match`、`match_phrase`、`match_phrase_prefix`、`multi_match`;`attachments`/`urls`/`hashtags` 等其他字段的子句不碰。
- **排除词子串化**:搜索 `alpha -部署` 时,mm 生成 must + must_not 两个子句,代理给每条子句注入**它自己的**词条——否定分支同样按子串排除,且正面词条绝不会泄漏进否定分支(否则会把含 "alpha" 的消息全误排除)。
- **每日新索引自动补字段**(`AUTO_PATCH_NGRAM=1`):mm 自建索引模板会丢弃外部的 `message.ngram` 子字段定义,代理在首个请求到达具体索引时检查 mapping,缺子字段就自动 `PUT _mapping` 补上(每个索引只做一次,失败带 2 分钟冷却重试)。通配符(`posts_*`)和逗号分隔的多索引路径同样覆盖。
- **认证透传**:代理不存任何凭证,mm 发来的 `Authorization` 头原样转发给 ES;错误凭证语义与直连完全一致。
- **其余请求透传**:非搜索请求(索引写入、集群健康检查等)按原样流式转发,不改不存。
- **操作符全兼容**:mm 搜索框的 `from:` / `in:` / `on:` / `before:` / `after:` / `""` / `-` 全部正常(22 条回归用例,见 `mm_search_operator_tests.py`)。

工作流程:

```
mm ──(ConnectionURL)──▶ 代理 :9210 ──▶ ES :9200
                        │
                        └─ 拦截 POST posts*/_search:
                           1. 解析 DSL,遍历查询树
                           2. 每条 message 子句原地包成 should:[原句, ngram-match]
                           3. 转发;其余请求流式透传
```

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `ES_UPSTREAM` | `http://1.1.1.1:9200` | 真实 Elasticsearch 地址,mm 流量全部转发到这里 |
| `LISTEN_ADDR` | `:9210` | 代理监听地址(mm 的 `ConnectionURL` 指向它) |
| `POSTS_PATTERN` | `posts` | 需要改写的索引名前缀,逗号分隔;请求里的索引名(去尾部 `*` 后)以任一前缀开头即触发注入。索引固定 `posts` 开头时默认即可;若存在不想增强的 `posts*` 系索引,改用 `posts_` 收窄 |
| `MSM` | `75%` | 注入的 ngram match 的 `minimum_should_match`:100% 最严格(易漏),越低召回越宽(易带噪音) |
| `AUTO_PATCH_NGRAM` | `1` | `1`=对每日索引自动补 `message.ngram` 子字段(best-effort,失败仅记日志) |
| `DUMP_DSL` | `0` | `1`=把改写前后的 DSL 打进日志,排查注入问题时用 |

## 快速开始

```bash
docker compose up -d --force-recreate
```

mm 侧三要素(`config.json`):

```json
"ElasticsearchSettings": {
    "ConnectionURL": "http://<代理IP>:9210",   // ① 指向代理
    "Username": "elastic",                      // ② 真实凭证(代理纯透传)
    "Password": "********",
    "EnableIndexing": true,
    "EnableSearching": true,
    "Sniff": false                              // ③ 必须关闭!
}
```

> **`Sniff: false` 是最常见的失效原因**:true 时 mm 会向 ES 请求 `_nodes/http` 拿真实节点地址,之后绕过代理直连 ES,注入静默失效(搜索一切正常,只是没有子串增强)。

另需:

- ES 安装 **analysis-icu** 插件(mm 官方依赖,没有它 mm 的 ES 集成激活失败,搜索回退数据库)
- `substring_analyzer`(ngram 分析器)已定义在 ES 侧的索引模板中,供 `message.ngram` 使用

## 构建与运维

```
mm-es-proxy/
├── main.go                        # 全部逻辑(~600 行)
├── go.mod / Dockerfile            # Go 1.22 静态编译,alpine 运行
├── docker-compose.yml             # 运行配置(env 全部带注释)
└── mm_search_operator_tests.py    # 22 条操作符回归测试(幂等 seed,python3 直接跑)
```

```bash
# 改代码后重建部署
docker compose build && docker compose up -d --force-recreate

# 看日志(每条请求都有访问日志)
docker compose logs -f mm-es-proxy

# 跑回归测试(需 /tmp/mm_admin_token 里有 mm admin token)
python3 mm_search_operator_tests.py
```

## 排查手册

| 症状 | 原因 | 验证方法 |
|---|---|---|
| 代理日志空,搜索却"正常" | mm 绕过代理直连 ES(Sniff=true 或配置未生效) | `docker logs mm-es-proxy`;对比 ES `_stats/search` 计数 |
| 搜索走数据库(中文要全词) | `EnableSearching=false` 或 mm ES 集成未激活 | mm 日志 grep elastic |
| mm 启动报 `Failed to create Elasticsearch template ... icu_tokenizer` | ES 缺 analysis-icu 插件 | `_cat/plugins` |
| 搜索经代理但无结果(日志有 rewrote) | 搜索词提取不到(mm 用了未覆盖的查询类型)或注入结构错误 | `DUMP_DSL=1` 看改写后 DSL |
| 新的一天搜不到子串 | 新索引缺 ngram 子字段 | 查 `_mapping`;确认 `AUTO_PATCH_NGRAM=1` |
