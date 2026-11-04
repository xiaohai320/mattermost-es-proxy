# mm-es-substring-proxy

一个介于 Mattermost 与 Elasticsearch 之间的透明反向代理,为 mm 的消息搜索注入**任意位置子串匹配**能力——不修改 mm 源码,对 mm 完全无感知。

## 简介

Mattermost 的消息搜索是**分词级别**的:消息文本由 Elasticsearch 按 standard 分词器切成完整 token 存储,搜索时也按词匹配。这意味着:

- 搜消息里的完整词 ✅
- 搜**词的中段** ❌:消息里有 IP `192.168.231.42`,搜 `231.42` 无结果;邮箱 `wang.wzhang@example.com`,搜 `zhang@example` 无结果;中文「盘阵列检查」按单字切分,短语以外的子串也搜不到

而运维场景恰恰最需要按 IP 中段、主机名片段、工单号、邮箱片段来查聊天记录。

本项目的解法是在索引侧给 `message` 字段加一个子串检索用的子字段(两种方案,由 `SUBSTRING_MODE` 选择,下文详述),在查询侧用一个透明代理拦截 mm 发往 ES 的搜索请求,把打到 `message` 字段的每条子句**原地**包成局部 OR:

```json
{
 "bool": {
  "should": [
   { "simple_query_string": { "query": "231.42", "fields": ["message"] } },
   { "wildcard": { "message.wildcard": { "value": "*231.42*" } } }  // wildcard 模式注入的分支
      // { "match": { "message.ngram": { "query": "231.42", "minimum_should_match": "75%" } } }  // ngram 模式注入的分支
  ],
  "minimum_should_match": 1
 }
}
```

原句(词级精确)命中**或**子串命中即返回(具体语义随模式:见下);频道/用户/日期等权限过滤在 bool 的 `filter` 里,原地替换后自动继续生效,权限边界不变。

## 两种子串方案原理

### wildcard 子字段(默认)

在普通 `text` 字段上跑 `*231.42*` 这种前缀通配查询是 ES 的性能灾难:倒排索引里只有**完整 token**(`192`、`168`…),通配符无法利用词典做前缀裁剪,只能退化成对全部 term 的线性扫描,大索引上直接拖垮查询。`wildcard` 字段类型(ES 7.9+ 引入)就是为解决这个问题设计的:

- **写入时**:除了原文本,额外存一份按固定块切分、**小写化**的文本,每个块都构建成类似 FST 的紧凑结构(可按任意偏移做子串匹配,也能快速剪枝"不可能包含该子串的块")
- **查询时**:`*term*` 不再扫描倒排词典,而是在该结构上逐文档做**确定性的子串包含判断**——不命中可以在块级别直接排除,复杂度与文档长度相关而非词典规模相关
- **语义**:所见即所得的**精确子串**——文档里逐字符包含查询词(大小写不敏感)才命中,没有模糊匹配、没有相关性打分(恒定分),排序靠 `sort` 字段(mm 本来就按 `create_at desc` 排)
- **依赖**:ES >= 7.9,不需要任何自定义分析器

### ngram 子字段

索引侧给 `message` 挂一个用 ngram 分析器的子字段 `message.ngram`,搜索词和文档都被切成所有长度 2~20 的字符窗口:

```
substring_analyzer = ngram_tokenizer(min_gram=2, max_gram=20) + lowercase

索引/查询时:"192.168.231.42" → ["19", "192", "192.", ..., "23", "231", "231.", ...]
```

于是**任意位置、任意长度的子串都成为一个可查 token**,查询词切出的窗口在倒排里正常匹配即可。

实现要点(实测踩过的坑):

- **必须用 `match`,不能用 `match_phrase`**:ngram 查询串自身会被切成一串位置连续的小 token,`match_phrase` 要求它们在索引里严格相邻,而索引端 token 流密度完全不同,短语匹配恒 0 命中
- `minimum_should_match`(环境变量 `MSM`,默认 75%)控制容错:查询词超过 max_gram=20 时按部分窗口匹配;调低召回更宽(噪音多),调到 100% 最严格(易漏)
- `min_gram=2` 支持两字中文子串(单字搜索走原句的 standard 分支,不依赖 ngram)
- **依赖**:索引 settings 里必须定义 `substring_analyzer`(参考 ES 侧 `mm-posts-ngram` 模板);缺失时新索引的 mapping 补丁失败,子串搜索静默降级
- **代价**:索引体积膨胀 3~5 倍

两种方案对比(`SUBSTRING_MODE` 切换):

| | `wildcard` 字段(默认) | `ngram` 子字段 |
|---|---|---|
| 匹配语义 | 精确子串,逐字符 | token 窗口,MSM(默认 75%)容错近匹配 |
| 索引体积 | 小(按需结构) | 大(2~20 字符窗口全量索引,膨胀 3~5 倍) |
| 大小写 | 字段内建不敏感 | 靠 analyzer 的 lowercase filter |
| 依赖 | ES >= 7.9,无分析器 | 索引 settings 里需定义 `substring_analyzer` |
| 漏召回风险 | 无(查询词确实存在就命中) | 有(切分边界差异,靠 MSM 兜) |
| 误召回风险 | 无 | 低(MSM 放宽导致的部分匹配) |


## 改写示例

以在 mm 搜索框输入 `unique99` 为例(代理 `DUMP_DSL=1` 抓取的真实请求,`POST /posts*/_search`,有省略):

**处理前**(mm 原始请求):

```json
{
 "from": 0,
 "highlight": {
  "encoder": "html",
  "fields": { "attachments": {}, "hashtags": {}, "message": {}, "urls": {} },
  "highlight_query": {
   "bool": {
    "must": {
     "bool": {
      "should": [
       { "simple_query_string": { "default_operator": "and", "fields": ["message"], "query": "unique99" } },
       { "simple_query_string": { "default_operator": "and", "fields": ["attachments"], "query": "unique99" } },
       { "terms": { "urls": ["unique99"] } },
       { "simple_query_string": { "default_operator": "and", "fields": ["hashtags"], "query": "#unique99" } }
      ]
     }
    }
   }
  }
 },
 "query": {
  "bool": {
   "filter": [
    { "terms": { "channel_id": ["wr9xe7gtb3bt7xn9w39deooasa", "swfnojctjigqzx7birswirxcda", "..."] } },
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
 },
 "size": 50,
 "sort": [ { "create_at": { "order": "desc" } } ]
}
```

**处理后**(代理注入 wildcard 分支后转发给 ES):

```json
{
 "from": 0,
 "highlight": {
  "encoder": "html",
  "fields": { "attachments": {}, "hashtags": {}, "message": {}, "urls": {} },
  "highlight_query": {
   "bool": {
    "must": {
     "bool": {
      "should": [
       { "simple_query_string": { "default_operator": "and", "fields": ["message"], "query": "unique99" } },
       { "simple_query_string": { "default_operator": "and", "fields": ["attachments"], "query": "unique99" } },
       { "terms": { "urls": ["unique99"] } },
       { "simple_query_string": { "default_operator": "and", "fields": ["hashtags"], "query": "#unique99" } }
      ]
     }
    }
   }
  }
 },
 "query": {
  "bool": {
   "filter": [
    { "terms": { "channel_id": ["wr9xe7gtb3bt7xn9w39deooasa", "swfnojctjigqzx7birswirxcda", "..."] } },
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
          { "wildcard": { "message.wildcard": { "value": "*unique99*" } } }
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
 },
 "size": 50,
 "sort": [ { "create_at": { "order": "desc" } } ]
}
```

改动点只有一处:`fields:["message"]` 的那条 `simple_query_string` 被原地包成
`bool{should:[原句, 子串匹配], minimum_should_match:1}`——词级命中或子串命中任一即返回。
`attachments`/`urls`/`hashtags` 的子句(含 `highlight_query` 里的)、`filter` 里的频道/类型权限过滤、排序全部原样保留。
另注:`query.must` 中 mm 本来就不生成 hashtags 子句(hashtag 搜索走 `terms`/`highlight_query`),所以前后对比 query 部分只有 message 子句一处差异。

以上示例是 `SUBSTRING_MODE=wildcard`(默认)时的输出;ngram 模式下同样的请求,唯一差别是注入分支变成:

```json
{ "match": { "message.ngram": { "query": "unique99", "minimum_should_match": "75%" } } }
```

## 功能说明

- **子串搜索注入**:拦截 `POST {index}/_search` 且索引名匹配 `POSTS_PATTERN` 前缀的请求,解析 DSL 并为每条 message 子句注入子串分支(按 `SUBSTRING_MODE`:`wildcard` 注入 `*词*` 精确子串,`ngram` 注入带 MSM 的 ngram match)。覆盖 mm 实际使用的查询类型:`simple_query_string`、`query_string`、`match`、`match_phrase`、`match_phrase_prefix`、`multi_match`;`attachments`/`urls`/`hashtags` 等其他字段的子句不碰。
- **排除词子串化**:搜索 `alpha -部署` 时,mm 生成 must + must_not 两个子句,代理给每条子句注入**它自己的**词条——否定分支同样按子串排除,且正面词条绝不会泄漏进否定分支(否则会把含 "alpha" 的消息全误排除)。
- **每日新索引自动补字段**(`AUTO_PATCH_NGRAM=1`):mm 自建索引模板会丢弃外部的子字段定义,代理在首个请求到达具体索引时检查 mapping,缺 `message.<子字段>`(随 `SUBSTRING_MODE`)就自动 `PUT _mapping` 补上(每个索引只做一次,失败带 2 分钟冷却重试)。通配符(`posts_*`)和逗号分隔的多索引路径同样覆盖。
- **认证透传**:代理不存任何凭证,mm 发来的 `Authorization` 头原样转发给 ES;错误凭证语义与直连完全一致。
- **其余请求透传**:非搜索请求(索引写入、集群健康检查等)按原样流式转发,不改不存。
- **操作符全兼容**:mm 搜索框的 `from:` / `in:` / `on:` / `before:` / `after:` / `""` / `-` 全部正常(22 条回归用例,见 `mm_search_operator_tests.py`)。

工作流程:

```
mm ──(ConnectionURL)──▶ 代理 :9210 ──▶ ES :9200
 │
 └─ 拦截 POST posts*/_search:
  1. 解析 DSL,遍历查询树
  2. 每条 message 子句原地包成 should:[原句, 子串匹配(按 SUBSTRING_MODE)]
  3. 转发;其余请求流式透传
```

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `ES_UPSTREAM` | `http://1.1.1.1:9200` | 真实 Elasticsearch 地址,mm 流量全部转发到这里 |
| `LISTEN_ADDR` | `:9210` | 代理监听地址(mm 的 `ConnectionURL` 指向它) |
| `POSTS_PATTERN` | `posts` | 需要改写的索引名前缀,逗号分隔;请求里的索引名(去尾部 `*` 后)以任一前缀开头即触发注入。索引固定 `posts` 开头时默认即可;若存在不想增强的 `posts*` 系索引,改用 `posts_` 收窄 |
| `SUBSTRING_MODE` | `wildcard` | 子串匹配策略:`wildcard`(精确子串、大小写不敏感、索引体积小,ES >= 7.9)或 `ngram`(MSM 容错、索引膨胀 3~5 倍、需要索引 settings 里有 `substring_analyzer`) |
| `MSM` | `75%` | `ngram` 模式下注入 match 的 `minimum_should_match`;`wildcard` 模式忽略 |
| `AUTO_PATCH_NGRAM` | `1` | `1`=对每日索引自动补 `message.<子字段>` 子字段,字段名随 `SUBSTRING_MODE` 变化(best-effort,失败仅记日志) |
| `DUMP_DSL` | `0` | `1`=把改写前后的 DSL 打进日志,排查注入问题时用 |

## 快速开始

```bash
docker compose up -d --force-recreate
```

mm 侧三要素(`config.json`):

```json
"ElasticsearchSettings": {
 "ConnectionURL": "http://<代理IP>:9210", // ① 指向代理
 "Username": "elastic", // ② 真实凭证(代理纯透传)
 "Password": "********",
 "EnableIndexing": true,
 "EnableSearching": true,
 "Sniff": false // ③ 必须关闭!
}
```

> **`Sniff: false` 是最常见的失效原因**:true 时 mm 会向 ES 请求 `_nodes/http` 拿真实节点地址,之后绕过代理直连 ES,注入静默失效(搜索一切正常,只是没有子串增强)。

另需:

- ES 安装 **analysis-icu** 插件(mm 官方依赖,没有它 mm 的 ES 集成激活失败,搜索回退数据库)
- ES 版本 **>= 7.9**:`wildcard` 字段类型是 7.9 引入的;更老的版本上 mapping 补丁 PUT 会 400 失败,子串搜索静默降级(`SUBSTRING_MODE=ngram` 无此要求)
- `SUBSTRING_MODE=ngram` 时,ES 索引 settings 里必须定义 `substring_analyzer`(ngram 分析器,参考 `mm-posts-ngram` 模板),否则新索引的 mapping 补丁会失败、子串搜索静默降级;`wildcard` 模式无此要求

## 构建与运维

```
mm-es-proxy/
├── main.go # 全部逻辑(~600 行)
├── go.mod / Dockerfile # Go 1.22 静态编译,alpine 运行
├── docker-compose.yml # 运行配置(env 全部带注释)
└── mm_search_operator_tests.py # 22 条操作符回归测试(幂等 seed,python3 直接跑)
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
| --- | --- | --- |
| 代理日志空,搜索却"正常" | mm 绕过代理直连 ES(Sniff=true 或配置未生效) | `docker logs mm-es-proxy`;对比 ES `_stats/search` 计数 |
| 搜索走数据库(中文要全词) | `EnableSearching=false` 或 mm ES 集成未激活 | mm 日志 grep elastic |
| mm 启动报 `Failed to create Elasticsearch template ... icu_tokenizer` | ES 缺 analysis-icu 插件 | `_cat/plugins` |
| 搜索经代理但无结果(日志有 rewrote) | 搜索词提取不到(mm 用了未覆盖的查询类型)、注入结构错误,或文档写入早于字段补丁(补丁只对新写入文档生效) | `DUMP_DSL=1` 看改写后 DSL |
| 新的一天搜不到子串 | 新索引缺子字段(或 ES < 7.9 不支持 wildcard 类型) | 查 `_mapping`;确认 `AUTO_PATCH_NGRAM=1` 与 ES 版本 |
