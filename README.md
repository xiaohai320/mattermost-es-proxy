# mm-es-substring-proxy

Mattermost 与 Elasticsearch 之间的透明反向代理,为 Mattermost 消息搜索提供任意位置子串匹配能力。无需修改 Mattermost 源码。

## 简介

Mattermost 的消息搜索基于 Elasticsearch 分词索引:消息文本按 standard 分词器切分为完整 token 存储,搜索时按词匹配。因此:

- 完整词可以命中
- 词的中段子串无法命中:消息包含 IP `192.168.231.42` 时,搜索 `231.42` 无结果;邮箱 `wang.wzhang@example.com` 中搜索 `zhang@example` 无结果;中文文本按单字切分,跨字子串依赖短语匹配

运维场景常需要按 IP 中段、主机名片段、工单号、邮箱片段检索聊天记录。

解决方案分两部分:

1. **索引侧**:为 `message` 字段添加子串检索子字段,由 `SUBSTRING_MODE` 选择实现:
   - `wildcard`(默认):`wildcard` 类型子字段 `message.wildcard`
   - `ngram`:ngram 分析器子字段 `message.ngram`
2. **查询侧**:透明代理拦截 mm 发往 ES 的搜索请求,将每条 `message` 子句原地改写为局部 OR:

```json
{
 "bool": {
  "should": [
   { "simple_query_string": { "query": "231.42", "fields": ["message"] } },
   { "wildcard": { "message.wildcard": { "value": "*231.42*", "case_insensitive": true } } }
  ],
  "minimum_should_match": 1
 }
}
```

原句(词级精确)命中或子串命中任一即返回。`bool.filter` 中的频道、用户、日期等权限过滤不受改写影响,权限边界保持不变。

## 子串方案原理

### wildcard 子字段(默认)

`wildcard` 字段类型(ES 7.9 引入)解决了普通 `text` 字段上通配查询的性能问题:普通倒排索引只包含完整 token,`*231.42*` 这类通配查询无法利用词典前缀裁剪,退化为全词典线性扫描。`wildcard` 字段的实现:

- 写入时存储一份按固定块切分、小写化的文本,构建为紧凑索引结构,支持任意偏移的子串匹配,并在块级别快速剪枝
- 查询时逐文档执行确定性的子串包含判断,复杂度与文档长度相关,与词典规模无关
- 语义为精确子串匹配,无相关性打分(恒定分),排序由 `sort` 字段决定(mm 默认 `create_at desc`)
- 查询词中的 `*`、`?`、`\` 由代理自动转义,仅外围两个 `*` 作为通配符
- 版本要求:ES >= 7.9;大小写不敏感需要 wildcard 查询的 `case_insensitive` 参数(ES 7.10 引入,7.9 的字段类型无此参数)

### ngram 子字段

为 `message` 添加使用 ngram 分析器的子字段 `message.ngram`,索引与查询两侧均切分为长度 2~20 的字符窗口:

```
substring_analyzer = ngram_tokenizer(min_gram=2, max_gram=20) + lowercase

"192.168.231.42" → ["19", "192", "192.", ..., "23", "231", "231.", ...]
```

任意位置的子串均成为可查 token,查询词切分后的窗口在倒排索引中正常匹配。

实现要点:

- 注入的查询必须使用 `match`,不能使用 `match_phrase`:ngram 查询串切分出的 token 在索引端与查询端位置密度不同,短语匹配恒为 0 命中
- `minimum_should_match`(环境变量 `MSM`,默认 75%)控制容错:查询词超过 `max_gram` 时按部分窗口匹配;调低召回更宽,调高更严格
- `min_gram=2` 支持两字符中文子串;单字符搜索由原句的 standard 分支覆盖
- 依赖:索引 settings 中必须定义 `substring_analyzer`(参考 ES 侧 `mm-posts-ngram` 模板),缺失时 mapping 补丁失败、子串搜索降级
- 代价:索引体积膨胀约 3~5 倍

### 两种方案对比

| | `wildcard` 字段(默认) | `ngram` 子字段 |
|---|---|---|
| 匹配语义 | 精确子串,逐字符 | token 窗口,MSM 容错 |
| 索引体积 | 小 | 大(膨胀 3~5 倍) |
| 大小写 | 查询带 `case_insensitive: true`(ES 7.10+) | 由 lowercase filter 处理 |
| 依赖 | ES >= 7.10,无分析器 | 索引 settings 需定义 `substring_analyzer` |
| 漏召回 | 无 | 有(切分边界差异,MSM 兜底) |
| 误召回 | 无 | 低(MSM 放宽导致) |

默认使用 wildcard:索引体积小、语义精确。若数据以中文长文本为主或出现精确匹配漏召回,切换 ngram 模式。

## 改写示例

以搜索 `unique99` 为例(`DUMP_DSL=1` 采集的实际请求,`POST /posts*/_search`,有省略)。

处理前(mm 原始请求):

```json
{
 "query": {
  "bool": {
   "filter": [
    { "terms": { "channel_id": ["wr9xe7gtb3bt7xn9w39deooasa", "..."] } },
    { "bool": { "should": [ { "terms": { "type": ["default"] } }, { "terms": { "type": ["slack_attachment"] } } ] } }
   ],
   "must": {
    "bool": {
     "must": {
      "bool": {
       "should": [
        { "simple_query_string": { "default_operator": "and", "fields": ["message"], "query": "unique99" } },
        { "simple_query_string": { "default_operator": "and", "fields": ["attachments"], "query": "unique99" } },
        { "terms": { "urls": ["unique99"] } }
       ]
      }
     }
    }
   }
  }
 }
}
```

处理后(wildcard 模式,转发给 ES):

```json
{
 "query": {
  "bool": {
   "filter": [
    { "terms": { "channel_id": ["wr9xe7gtb3bt7xn9w39deooasa", "..."] } },
    { "bool": { "should": [ { "terms": { "type": ["default"] } }, { "terms": { "type": ["slack_attachment"] } } ] } }
   ],
   "must": {
    "bool": {
     "must": {
      "bool": {
       "should": [
        { "bool": {
         "minimum_should_match": 1,
         "should": [
          { "simple_query_string": { "default_operator": "and", "fields": ["message"], "query": "unique99" } },
          { "wildcard": { "message.wildcard": { "value": "*unique99*", "case_insensitive": true } } }
         ]
        } },
        { "simple_query_string": { "default_operator": "and", "fields": ["attachments"], "query": "unique99" } },
        { "terms": { "urls": ["unique99"] } }
       ]
      }
     }
    }
   }
  }
 }
}
```

改动仅一处:`fields:["message"]` 的 `simple_query_string` 子句被原地改写为 `bool{should:[原子句, 子串匹配], minimum_should_match:1}`。`attachments`/`urls` 子句、`filter` 中的权限过滤、`sort` 均保持不变。

ngram 模式下同一请求的差异仅在注入分支:

```json
{ "match": { "message.ngram": { "query": "unique99", "minimum_should_match": "75%" } } }
```

排除操作符的处理:搜索 `alpha -部署` 时 mm 生成 must 与 must_not 两个子句,代理为每条子句注入其各自的词条,否定分支同样按子串排除,正面词条不会进入否定分支。

## 功能说明

- **子串搜索注入**:拦截 `POST {index}/_search` 且索引名匹配 `POSTS_PATTERN` 前缀的请求,为每条 message 子句注入子串分支。支持的查询类型:`simple_query_string`、`query_string`、`match`、`match_phrase`、`match_phrase_prefix`、`multi_match`;其他字段(`attachments`、`urls`、`hashtags`)的子句不改写。
- **排除词子串化**:must_not 子句按其自身词条注入,正面词条不泄漏进否定分支。
- **每日新索引自动补字段**(`AUTO_PATCH_NGRAM=1`):mm 的索引模板会在新索引上丢弃外部子字段定义,代理在首个请求到达具体索引时检查 mapping,缺失 `message.<子字段>`(随 `SUBSTRING_MODE`)时自动 `PUT _mapping` 补上,每索引一次,失败按 2 分钟冷却重试;通配符路径(`posts_*`)与逗号分隔的多索引路径同样覆盖。ngram 模式下同时确保索引 settings 中存在 `substring_analyzer` 定义(注意:analysis settings 为 non-dynamic,open 状态索引上该 PUT 会被 ES 拒绝,仅记录日志)。
- **认证透传**:代理不存储凭证,`Authorization` 头原样转发。
- **其余请求透传**:非搜索请求流式转发,不修改不存储。

工作流程:

```
mm ──(ConnectionURL)──▶ 代理 :9210 ──▶ ES :9200
                        │
                        └─ 拦截 POST {prefix}*/_search:
                           1. 解析 DSL,遍历查询树
                           2. 每条 message 子句原地改写为
                              should:[原子句, 子串匹配(按 SUBSTRING_MODE)]
                           3. 转发;其余请求流式透传
```

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `ES_UPSTREAM` | `http://1.1.1.1:9200` | 上游 Elasticsearch 地址 |
| `LISTEN_ADDR` | `:9210` | 代理监听地址,mm 的 `ConnectionURL` 指向此处 |
| `POSTS_PATTERN` | `posts` | 触发改写的索引名前缀,逗号分隔;请求中的索引名(去除尾部 `*` 后)以任一前缀开头即触发注入 |
| `SUBSTRING_MODE` | `wildcard` | 子串匹配策略:`wildcard` 或 `ngram` |
| `MSM` | `75%` | ngram 模式的 `minimum_should_match`;wildcard 模式忽略 |
| `AUTO_PATCH_NGRAM` | `1` | 对每日索引自动补 `message.<子字段>`(字段名随 `SUBSTRING_MODE`),失败仅记录日志 |
| `DUMP_DSL` | `0` | 输出改写前后的 DSL 到日志 |

## 快速开始

```bash
docker compose up -d --force-recreate
```

mm 侧配置(`config.json`):

```json
"ElasticsearchSettings": {
 "ConnectionURL": "http://<代理IP>:9210",
 "Username": "elastic",
 "Password": "********",
 "EnableIndexing": true,
 "EnableSearching": true,
 "Sniff": false
}
```

注意:

- `Sniff` 必须为 `false`:为 `true` 时 mm 通过 `_nodes/http` 获取真实节点地址后绕过代理直连 ES,注入失效
- ES 需安装 **analysis-icu** 插件(mm 建索引模板时依赖,缺失则 mm 的 ES 集成无法激活)
- ES 版本要求:>= 7.10(`wildcard` 字段 7.9 引入,`case_insensitive` 参数 7.10 引入);`SUBSTRING_MODE=ngram` 无此要求
- `SUBSTRING_MODE=ngram` 要求索引 settings 中定义 `substring_analyzer`(参考 `mm-posts-ngram` 模板),wildcard 模式无此要求

## 构建与运维

```
mm-es-proxy/
├── main.go                        # 全部逻辑
├── go.mod / Dockerfile            # Go 1.22 静态编译,alpine 运行
├── docker-compose.yml             # 运行配置
└── mm_search_operator_tests.py    # 22 条操作符回归测试
```

```bash
# 重建部署
docker compose build && docker compose up -d --force-recreate

# 查看日志(含每条请求的访问日志)
docker compose logs -f mm-es-proxy

# 回归测试(需 /tmp/mm_admin_token 中的 mm admin token)
python3 mm_search_operator_tests.py
```

## 排查手册

| 症状 | 原因 | 验证方法 |
| --- | --- | --- |
| 代理无日志,搜索仍"正常" | mm 绕过代理直连 ES(`Sniff=true` 或配置未生效) | `docker logs mm-es-proxy`;对比 ES `_stats/search` 计数 |
| 搜索回退数据库 | `EnableSearching=false` 或 mm ES 集成未激活 | mm 日志 grep elastic |
| mm 启动报 `Failed to create Elasticsearch template ... icu_tokenizer` | ES 缺 analysis-icu 插件 | `_cat/plugins` |
| 搜索经代理但无结果(日志有 rewrote) | 查询类型未被覆盖、注入结构错误,或文档写入早于字段补丁(补丁仅对新写入文档生效) | `DUMP_DSL=1` 查看改写后 DSL |
| 新日期索引搜不到子串 | 索引缺子字段,或 ES < 7.10 不支持 wildcard/`case_insensitive` | 查 `_mapping`;确认 `AUTO_PATCH_NGRAM=1` 与 ES 版本 |
