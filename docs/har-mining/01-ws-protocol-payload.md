# HAR 逆向报告 01：Chathub WebSocket 协议与 chatPayload 字段级还原

> **本文档回答什么问题**：浏览器到底如何构造 Chathub WebSocket 连接与 chat 载荷——升级 URL 的每个 query 参数从哪来、SignalR 帧按什么顺序发送、chatPayload 每个字段的浏览器真实取值是什么，以及 Go 实现（internal/chathub/client.go）与浏览器行为的逐项差异与改动建议。
>
> 挖掘对象：`wss://substrate.office.com/m365Copilot/Chathub/{oid}@{tid}` 升级请求、SignalR 帧序列、
> `chatPayload`（type:4 target:chat）字段构造，以及 m365.cloud.microsoft 前端 JS chunk 中的
> BuildWSURL / Sydney client 构造代码。
>
> 对照代码：`internal/chathub/client.go`（BuildWSURLWithOptions / chatPayload）
>
> 数据源：
> | 文件 | 大小 | entries | Chathub WS 升级 | _webSocketMessages |
> |---|---|---|---|---|
> | har1.har | 79 MB | 1105 | 6 (#364,#590,#650,#789,#873,#976) | 373 帧 |
> | har2.har | 14 MB | 172 | 0 | 0 |
> | har3.har | 24 MB | 230 | 0 | 0 |
> | har4.har | 69 MB | 560 | 7 (#4,#163,#194,#201,#211,#246,#546) | 530 帧 |
> | har5.har | 37 MB | 127 | 0 | 0 |
> | har6.har | 1.1 MB | 36 | 0 | 0 |
>
> 关键幸运点：har1/har4 的 HAR **包含 `_webSocketMessages`**（共 903 帧），
> 因此本文所有 payload 结论均来自**浏览器实际发送的字节**，而非推测。
> JS 结论来自 HAR 内嵌的 response.content.text（1023 个 JS entry，约 87 MB）。

---

## 目录

1. [WS 升级请求 query 参数全表](#1-ws-升级请求-query-参数全表)
2. [JS 源码还原：浏览器端 BuildWSURL](#2-js-源码还原浏览器端-buildwsurl)
3. [variants 精确 diff（HAR vs Go）](#3-variants-精确-diffhar-vs-go)
4. [SignalR 帧序列协议（含 AttachToSession 与双 Metrics）](#4-signalr-帧序列协议)
5. [chatPayload 字段级对比（argument 层 / message 层）](#5-chatpayload-字段级对比)
6. [tone / streamingMode / optionsSets 枚举与机制](#6-tone--streamingmode--optionssets)
7. [CreateConversation 与 conversationId 来源](#7-createconversation-与-conversationid-来源)
8. [UploadFile 上传协议补充](#8-uploadfile-上传协议补充)
9. [Go 改动建议汇总（伪代码/diff 级）](#9-go-改动建议汇总)
10. [附录：证据索引](#10-附录证据索引)

---

## 1. WS 升级请求 query 参数全表

样本：13 个升级请求全部 101 Switching Protocols。
URL 形态：`wss://substrate.office.com/m365Copilot/Chathub/{oid}@{tid}?{query}`
（har1 六次为 oid `<USER_A_OID>@<TENANT_ID>`；har4 七次为 `<USER_B_OID>@<TENANT_ID>`）

### 1.1 参数出现统计

| 参数 | 出现次数/13 | 示例值 | Go BuildWSURL 是否有 | 差异说明 |
|---|---|---|---|---|
| chatsessionid | 13 | `<REQ_ID_RECONNECT>` | ✅ = requestID | 一致。但见 §2：仅 reconnect 模式才发 |
| clientrequestid | 13 | 同上 | ✅ = requestID | 一致（小写驼峰形态） |
| XRoutingParameterSessionKey | 13 | 同上 | ✅ = requestID | 一致。三值始终相同 |
| X-SessionId | 13 | `<SESSION_ID_B>` | ✅ = sessionID | **页面级会话 ID**，跨全部 13 次连接复用（har1 内恒为 `<SESSION_ID_A>`）；Go 每次 Chat 随机生成 |
| ConversationId | 13 | `<CONVERSATION_ID_B>` | ✅ | 多轮复用同一 ID（har4 中 7 连接共用）；Go 单轮即换 |
| access_token | 13 | `<REDACTED_TOKEN>` len≈3471 | ✅ | 一致，token 只走 query，无 Authorization 头 |
| source | 13 | `"officeweb"`（带引号） | ✅ | 一致，引号原样传输未转义 |
| product | 13 | `Office` | ✅ | 一致 |
| agentHost | 13 | `Bizchat.FullScreen` | ✅ | 一致 |
| licenseType | 13 | `Starter` | ✅ | 一致（默认 Starter） |
| agent | 13 | `web` | ✅ | 一致 |
| scenario | 13 | `OfficeWebIncludedCopilot` | ✅ | 一致（默认 OfficeWebIncludedCopilot） |
| isEdu | 13 | `false` | ✅ | 一致 |
| disableMemory | 7（仅 har4 全部 7 次） | `1` | ✅ 条件 | **JS 语义是 isInPrivateChat（隐私聊天）**，非通用"关记忆"开关，见 §2 |
| variants | 13 | 2163 字符 CSV | ✅ 但内容过时 | 见 §3 diff：HAR +16 / −4 |

### 1.2 升级请求头要点

- `Origin: https://m365.cloud.microsoft`（无尾斜杠）— Go `NewClient()` 一致 ✓ (client.go:303)
- UA：抓包为 Chrome 系 `Mozilla/5.0 … AppleWebKit/537.36 (KHTML, like Gecko) Chrome/…`；
  **Go 硬编码 Firefox/148 UA** (client.go:304)。服务端不校验 UA 与 WS 握手成败无关，
  但与 HTTP API 侧 UA 不一致时构成指纹矛盾（建议全链路统一）。
- 无 `Sec-WebSocket-Protocol`、无 `Authorization` 头。

### 1.3 未观察但 JS 支持的参数（Go 全部缺失）

来自 BuildWSURL JS 源码（har1#17 pos≈8018–8700，见 §2）：

| 参数 | 触发条件 | 说明 |
|---|---|---|
| `ClientRequestId=<id>` | 默认形态（非 reconnect） | 注意大小写是 **ClientRequestId**；reconnect 时才降级为小写 `clientrequestid` 并加发 chatsessionid |
| `hostClientSessionContext=<ctx>` | host 提供 T | 宿主上下文透传 |
| `developerMode=Basic` | developer flag h 开启 | 调试通道 |
| `trafficType=<t>` | g 存在 | JS 里可见 `'test'` 值：`trafficType: R.enableSydneyTrafficTypeProp&&(0,o.C)()||(0,r.q)()?'test':void 0`（har1#97 pos≈157800） |
| `gptId=<enc>` | E（当前选中 agent/gpt） | URL 编码后拼接 |
| `X-TargetPartition=<p>` | fluxV4TargetPartition O | 分区路由 |

---

## 2. JS 源码还原：浏览器端 BuildWSURL

证据：har1#17（JS chunk，pos≈8018 起）。反混淆后的完整逻辑：

```js
// 输入解构: ring:n, urlOverride:o, objectId:s, tenantId:c, clientRequestId:l,
//   trafficType:g, puid:b, sessionId:_, isInPrivateChat:v,
//   isSydneyReconnectEnabled:x, isSydneyReconnectRoutingQueryParamEnabled:S,
//   enableFluxV4StreamHub:C, enableFluxV4SkipConversationId:w,
//   hostClientSessionContext:T, gptId:E, fluxV4FrontierEndpoint:D,
//   fluxV4TargetPartition:O, fluxV4UseSvcStreamHub:k

let j = base;                       // ring 表或 override
if (D) j = D;                       // fluxV4FrontierEndpoint:
//   j = 'https://substrate.office.com/m365copilotfrontier/streamhub'
else if (k) j = n==='frontier'
//   ? 'https://substrate.office.com/m365copilotfrontier/streamhub'
//   : j.replace(/\/chathub$/i, '/StreamHub');      // ★ chathub→StreamHub 替换

j += '/' + (s || b || '') + '@' + (c ?? '') + '?';    // path: objectId 或 puid 兜底

if (l) {
  let e = 'ClientRequestId';                        // 默认参数名（大写驼峰）
  if (x || C) {                                     // reconnect 或 FluxV4StreamHub
    j += `chatsessionid=${l}&`;
    e = 'clientrequestid';
    if (S || C) j += `XRoutingParameterSessionKey=${l}&`;
  }
  j += `${e}=${l}&`;
}
if (T) j += `hostClientSessionContext=${T}&`;
if (_) j += `X-SessionId=${_}&`;
if (h) j += 'developerMode=Basic&';                 // developer flag
if (g) j += `trafficType=${g}&`;
if (!(w && P) && f) j += `ConversationId=${f}&`;    // w&&P=StreamHub+skip flag 时省略
if (v) j += 'disableMemory=1&';                     // ★ v = isInPrivateChat
if (E) j += `gptId=${encodeURIComponent(E)}&`;
if (O) j += `X-TargetPartition=${O}&`;
```

直接结论：

- **[F1] `disableMemory=1` 的真实语义是"隐私聊天"**（变量名 `isInPrivateChat`）。
  Go 的 `DisableMemory` 字段语义兼容，但如果产品上把"关闭记忆个性化"映射到它，
  浏览器对应的是隐私窗口行为，两者在服务端可能有不同的计费/日志策略。
- **[F2] 三连 `chatsessionid/clientrequestid/XRoutingParameterSessionKey` 是重连模式特征**。
  抓包 13/13 全带 → 该租户 flight 下 `enableSydneyReconnect=true` 且
  `enableSydneyReconnectRoutingQueryParam=true`。Go 恒发三连，与抓包一致 ✓；
  若要伪装"普通首连"形态应只发 `ClientRequestId=`（大写）——目前无证据表明服务端区分。
- **[F3] StreamHub 变体端点存在**：`/m365Copilot/Chathub` → `/m365Copilot/StreamHub`
  （正则 `/\/chathub$/i` 替换），另有 frontier 专用
  `https://substrate.office.com/m365copilotfrontier/streamhub`。这是 FluxV4 新流式架构的入口。
  Go 目前写死 `wsBase = ".../Chathub"` (client.go:165)，未来 FluxV4 全量时需可切换。
- **[F4] path 的 oid 可用 puid 兜底**：`(s || b || '')`。Go 只用 OID，若某账号无 OID 可用 puid。

相关旁证（重连机制 flag 家族，har1#4 pos≈1078259 / har1#89 pos≈42398）：

```js
{ enableSydneyReconnect:P, disableTrouterStreaming:F,
  enableSydneyReconnectRoutingQueryParam:I, enableReconnectReplay:L,
  enablePreCalForSydney:R, enableSydneyReconnectBackOff:…,
  enableReconnectWarmupRaceFix:…, enableReconnectDisposalGuard:… }
```

---

## 3. variants 精确 diff（HAR vs Go）

HAR variants（13 个升级中实测，len=2163，63 项）vs Go `variants` 常量
(client.go:173，len=1705，51 项)：

**HAR 有而 Go 没有（16 项，建议全部补入）：**

```
feature.EnableCodeInterpreterConversion
agt_module_attr_enableReferencesForCodeInterpreter
agt_module_enableCodeInterpreterHallucinatedUrlFilter
SingletonEnvOn
cdxenablefccinmainline
EnableComposeWidget
feature.EnableContentApiandDocTypeHtmlInRichAnswers
cdxgrounding_api_v2_rich_web_answers_reference_bottom_force
cdxenablerenderforisocomp
feature.EnableSkipRehydrationForSpeCIdImages
feature.EnablePersonalization
rich_responses                                  ← 注意：这同时出现在 optionsSets 里
feature.EnableBase64DataInMessageAnnotations
feature.EnableSkipEmittingMessageOnFlush
feature.EnableRemoveEmptySourceAttributions
agt_researcheragent_enableMemoryRead   ← 带 '-' 前缀（见下）
```

**Go 有而 HAR 没有（4 项，疑似已下线，建议移除或保留观察）：**

```
feature.turnOnWorkTabRecommendation
turnOffWorkTabUpsellFromClient
feature.EnableCuaTakeControlApi
feature.EnableConversationShareApisForMsa
```

**负前缀开关语法**：13 个升级实际有两种 variants 串，唯一差别是一组 researcher 相关项翻转，
其中一项以 `-agt_researcheragent_enableMemoryRead` 形式出现——
**`-` 前缀表示显式关闭该 flight**。另一种形态还多出
`feature.EnableResearchSteering / feature.EnableResearcherTodoListObserver /
feature.EnableResearcherTodoObserverSlim / feature.EnableResearcherTodoSummarizerPacing`
（同会话前后两次连接间被 ECS 热更）。Go 的常量字符串无法表达负开关与热更新。

> 证据：`ws_upgrades.json` param_examples.variants；两组串分别来自
> har1#364 等 12 个升级与其中 1 个升级（A/B 组 diff 见挖掘脚本输出）。

---

## 4. SignalR 帧序列协议

一次完整对话连接（以 har4#4 为例，45 帧）的实际序列：

```
S→ {"protocol":"json","version":1}\x1e          (f0)
R← {}\x1e                                        (f1, handshake ack)
S→ {"type":6}\x1e                                (f2, 应答服务器 ping)
S→ <chat type:4>\x1e<Metrics type:1>\x1e         (f3, ★两帧合并一次发送!)
R← {"type":1,"target":"update",...} × N          (流式响应)
S→ {"type":6}\x1e                                (按需心跳)
S→ <第二 Metrics type:1>\x1e                     (f44, 响应结束后补报)
```

### 4.1 [F5] chat + Metrics 合并帧

浏览器把 chat payload 和第一个 Metrics 帧用 `\x1e` 连接后**一次性 write**
（har4#4 f3 数据 = chat JSON + `\x1e` + Metrics JSON + `\x1e`）。
Go 分两次 `wsWrite` (client.go:509 与 metrics 在 chatPayload 尾部一起拼好——实际上 Go 已在
同一个字符串里返回 chat+metrics 并一次写出 ✓)。核对 client.go:1455-1457：
`return string(b1)+rs+string(b2)+rs` —— **一致 ✓**。

### 4.2 [F6] 第一个 Metrics 帧的真实内容（Go 缺 RequestSent 且值为空）

```json
{"arguments":[{"Timestamps":{
  "ConnectionStart":"2026-08-23T14:25:26.822Z",
  "UserInputStart":"2026-08-23T14:25:27.022Z",
  "ConnectionEstablished":"2026-08-23T14:25:29.071Z",
  "UserInputSubmit":"2026-08-23T14:25:36.673Z",
  "RequestSent":"2026-08-23T14:25:37.590Z"     ← Go 没有此键
}}],"target":"Metrics","type":1}
```

Go (client.go:1441-1454) 发送的 Timestamps 只有 4 键且全是空字符串 `""`。
浏览器发真实 ISO 时间戳且多一个 `RequestSent`。
空字符串 vs 真实时间戳是一个明显的非人类指纹。

### 4.3 [F7] 第二个 Metrics 帧（Go 完全没有）

响应完成后浏览器补发遥测帧（har4#4 f44、har4#546 f6）：

```json
{"arguments":[{
  "Timestamps":{
    "RequestSent":"2026-08-23T14:25:37.590Z",
    "FirstServiceResponseReceived":"2026-08-23T14:25:38.041Z",
    "FirstServiceResponseRendered":"2026-08-23T14:25:38.047Z",
    "FirstTokenReceived":"2026-08-23T14:25:38.794Z",
    "LastTokenReceived":"2026-08-23T14:25:41.206Z",
    "FirstTokenRendered":"2026-08-23T14:25:38.797Z",
    "SuggestionsRendered":"2026-08-23T14:25:41.210Z",
    "SuggestionsReceived":"2026-08-23T14:25:41.585Z"},
  "ReceivedTokenMetrics":{
    "TokenCount":12,"CharCount":57,
    "TimeBetweenTokens":{"Mean":113.8,"Median":5,"Sd":184.7,
                         "Variance":34105.3,"Max":498.7,"Min":1},
    "AverageChunkSize":4.75,"BurstRate":0.727,"StallRate":0}
}],"target":"Metrics","type":1}
```

JS 侧开关名 `enableReceivedTokenMetricsInSydneyMetrics`（har1#97 pos≈157800）。
该帧携带 token 到达间隔统计，是服务端质量分析输入；缺失不一定致命，
但对"会话完整性画像"是一个可观测缺口。Go 已有全部原始时间戳（Timestamps struct），
补发成本极低。

### 4.4 [F8] AttachToSession 重连帧（Go 未实现）

har4#546 是一次断线重连（用户刷新页面后续接同一会话）：

```json
{"arguments":["<REQ_ID_RECONNECT>"],
 "invocationId":"0","target":"AttachToSession","type":4}
```

- 参数就是原连接的 `chatsessionid`；
- WS URL 本身与常规升级完全相同（同样带三连 session 参数与 disableMemory）；
- 之后不再发 target:chat，而是等服务端回放/继续 `update` 流；
- 随后照常发第二个 Metrics 帧。

Go 有 `FeatureFlags.SydneyReconnect` 开关位 (client.go:213) 但没有任何实现消费它。
实现 AttachToSession 后可以做：进程重启/网络闪断后接管仍在生成的响应。

### 4.5 心跳

服务器 ping `{"type":6}\x1e` → 客户端立即回 `{"type":6}\x1e`。Go 一致 ✓ (client.go:716-718)。

---

## 5. chatPayload 字段级对比

基准样本：`chatpayload_har4.har_4_3.json`（完整 dump，12 个 chat payload 结构完全一致，
仅 text/tone/sessionId 不同；har4#246 因长文本略大）。

### 5.1 argument 层（arguments[0]）

| 字段 | 浏览器值（har4#4） | Go chatPayload | 判定 |
|---|---|---|---|
| source | `"officeweb"` | 同 | ✓ |
| clientCorrelationId | `<REQ_ID_1>`（=chatsessionid=requestId=traceId，**四值一体**） | `uuid.NewString()` 每次随机 (client.go:1389) | ✗ 见 F9 |
| sessionId | `<SESSION_ID_B>`（=X-SessionId，带连字符） | req.SessionID | ✓（但生成策略见 F10） |
| optionsSets | 33 项（见 §6.3） | 31+flag 项 | 内容基本一致，顺序不同 |
| streamingMode | `"ConciseWithPadding"` | 同硬编码 | ✓（可选 ConciseV2，§6.2） |
| options | `{}` | `{}` | ✓ |
| **extraExtensionParameters** | `{}`（skills 场景注入 `enabled-skills`，见下） | **缺失** | ✗ F11 |
| allowedMessageTypes | 30 项 | 30 项逐字一致 | ✓ |
| sliceIds | `[]` | `[]` | ✓ |
| threadLevelGptId | `{}` | `{}` | ✓ |
| traceId | `<REQ_ID_1>`（=requestId 同值） | `uuid.NewString()` 独立随机 (client.go:1408) | ✗ F9 |
| isStartOfSession | **`false`**（包括新对话第一条！12/12 样本均 false） | firstTurn（新会话=true）(client.go:1409) | ✗ F12 |
| clientInfo | 9 键 | 9 键逐字一致 | ✓ |
| message | 见 §5.2 | 见 §5.2 | 部分 |
| plugins | `[{"Id":"BingWebSearch","Source":"BuiltIn"}]` 默认必带 | 无工具时 `[]` (tools.go:10-33) | ✗ F13 |
| **isSbsSupported** | `true` | **缺失** | ✗ F11 |
| tone | `"Magic"` 等 | req.Tone 默认 "magic" | ⚠ 大小写：浏览器 `Magic`，Go 默认 `magic` (client.go:164) |
| **renderReferencesBehindEOS** | `true` | **缺失** | ✗ F11 |
| **disconnectBehavior** | `"continue"` | **缺失** | ✗ F11 |
| conversationId | **不存在** | 发送 (client.go:1407) | ✗ 冗余 F14 |
| productThreadType | **不存在** | 发送 `"Office"` (client.go:1410) | ✗ 冗余 F14 |
| toolChoice | **不存在**（即使带工具场景也未见于样本） | 恒发送（可能 null）(client.go:1428) | ✗ 冗余 F14 |
| previousMessages / conversationSignature | 样本中未出现（多轮靠服务端会话状态） | 条件发送 | 中性保留 |

### 5.2 message 层

| 字段 | 浏览器值 | Go | 判定 |
|---|---|---|---|
| author/inputMethod/text/entityAnnotationTypes/requestId/locationInfo/locale/messageType/experienceType/adaptiveCards/clientPreferences/connectedFederatedConnections | 逐字对齐 | 同 | ✓ |
| **clientInfo（内嵌副本）** | message 里再嵌一份与 argument 层相同的 clientInfo (dump L117-128) | **缺失** | ✗ F15 |
| market | **不存在** | 恒发送 (client.go:1275) | ✗ 冗余 F14 |
| requestId | `=chatsessionid 同值` | requestID（=chatsessionid ✓） | ✓ 值来源一致 |
| locale | `"zh-cn"`（小写） | 默认 en-us | ⚠ 可配置即可 |
| locationInfo | `{"timeZoneOffset":8,"timeZone":"Asia/Shanghai"}` | 默认 UTC/0 | ⚠ 建议跟随调用方 |
| text 尾部 | 长文本样本身带 `<br aria-hidden="true">`（富文本编辑器残留，har4#246） | 无 | 忽略，勿模仿 |

### 5.3 [F9] ID 体系：浏览器"四值一体"，Go "一值三随机"

浏览器（har4 全部 7 连接）：

```
chatsessionid = clientrequestid = XRoutingParameterSessionKey
              = message.requestId = arguments.traceId = arguments.clientCorrelationId
              = <同一个 UUID>            （X-SessionId/sessionId/clientInfo.clientSessionId 为另一页面级常量）
```

即：每条消息生成 1 个新 UUID（request 级），页面生命周期内 1 个固定 UUID（session 级）。
traceId/clientCorrelationId 不是独立随机数，而是复用 request UUID。

Go 现状：requestID 用于 chatsessionid/clientrequestid/XRouting/message.requestId（✓ 对），
但 traceId 与 clientCorrelationId 各自 `uuid.NewString()`（✗ 引入两个多余随机源）。

旁证（SSR 状态里 clientCorrelationId 是持久字段）：
har1#0 pos≈636351 `"clientCorrelationId":"<SESSION_ID_A_HEX>"` 与该页所有 WS 的
chatsessionid 同值；har2#0 另一会话为 `"<CLIENT_CORRELATION_ID_HEX>"`（32 hex 无连字符也合法）。

### 5.4 [F12] isStartOfSession 恒 false

12/12 个 chat payload 均 `"isStartOfSession":false`，包括每个对话的第一条消息
（har1#364 是 conversation `<CONVERSATION_ID_A1>` 的首条、har4#4 是 `<CONVERSATION_ID_B>` 首条）。
结合 §7（conversationId 客户端自造、无 CreateConversation 调用），说明 M365 Copilot 的
Chathub 已不依赖该标志做会话初始化。Go 把新会话置 true 属于旧 Bing Chat 习惯，
属于无证据冗余信号。

### 5.5 [F11] extraExtensionParameters 的注入语法

默认 `{}`；当启用 agent skills 时（har1#22 pos≈251600）：

```js
extraExtensionParameters: b ? {...y, "enabled-skills":
    {enabledSkillIds: l, ...(c.length>0?{expSkillIds:c}:{})}} : y
// 另有被剔除的 "prefetch-agent-skills" 键
```

配套 variants 联动：开启 skills 时追加
`feature.EnableForceSbsShown, feature.EnableSkillExperiment`（har1#22 pos≈251600 Xe 数组）。
Go 如需支持 agent/skills 场景，这是准确的字段形状。

### 5.6 [F13] plugins 默认值

浏览器无第三方工具时仍发 `[{"Id":"BingWebSearch","Source":"BuiltIn"}]`
（12/12 样本；SSR 历史记录里每个会话也存 `plugins:[{id:"BingWebSearch",source:"BuiltIn"}]`，
har1#0 pos≈571460）。Go 无工具时发 `[]`。空数组可能让服务端走"无搜索工具"分支，
影响联网能力与响应形态。

---

## 6. tone / streamingMode / optionsSets

### 6.1 [F16] tone 完整枚举（模型选择菜单 SSR dump，har1#0）

```
Magic               自动        —— section 1（默认，实测 10/12 样本）
Chat                快速答复     —— section 1
Reasoning           深度思考     —— section 1
── OpenAI group ──
Gpt_5_6_Reasoning   GPT 5.6 Think deeper (shortTitle: GPT 5.6 Think)
Gpt_5_5_Chat        GPT 5.5 快速响应   (shortTitle: GPT 5.5 快速)
```

实测 payload 中出现过：`Magic`（10 次）、`Gpt_5_6_Reasoning`（har1#590）、
`Gpt_5_5_Chat`（har1#789）。历史会话存储里大量 `"tone":"Gpt_5_6_Reasoning"`
（har1#0/#283 conversationPageHistoryList）。
**Go defaultTone=`"magic"` 小写** (client.go:164)——所有观测样本均为 `Magic` 首字母大写；
虽然服务端大概率大小写不敏感（Go 用 magic 显然能工作），但要最大化相似性应改 `Magic`。

### 6.2 [F17] streamingMode 由 flight 决定

```js
streamingMode: Y.enableConciseV2 ? 'ConciseV2' : 'ConciseWithPadding'
// 枚举: Full | Concise | ConciseWithPadding | None   (har1#431 pos≈2288)
```
（har1#89 pos≈36205、har1#97 pos≈157845、har3#163 pos≈36361 三处一致）
Go 硬编码 ConciseWithPadding 与本次抓包一致 ✓；`ConciseV2` 是灰度中的新值，
可作为 fallback 尝试项。

### 6.3 [F18] optionsSets 组装机制与顺序

JS：`optionsSets: [...ECS下发串.split(','), ...本地配置数组]`（har1#4 pos≈524543），
即服务端 flight 配置在前、客户端静态列表在后。

浏览器最终串（har4#4 dump L7-41，33 项）与 Go 列表的差异：
- 内容集合一致（当 MemoryV2=true 时）；
- **顺序不同**：浏览器把 `update_memory_plugin, add_custom_instructions` 插在
  `cwc_fileupload_odb` 之后、`cwc_flux_v3` 之前（第 22-23 位）；Go append 到尾部
  (client.go:1361-1363)。数组比较型风控会看出顺序差异；
- Go MemoryV2=false 时会比浏览器少两项（浏览器抓包账号记忆功能开启）。

---

## 7. CreateConversation 与 conversationId 来源

- **6 个 HAR、覆盖多次新建对话，零次 `/m365Copilot/CreateConversation` 调用**
  （全量扫描 method+url，脚本 conv_api.py 输出为空）。
- 结合 BuildWSURL JS 中 `enableSetConversationIdFromClient` 开关（har1#89 pos≈40199）
  与 WS URL/query 直接携带客户端自造 ConversationId，可证实：
  **客户端本地 `crypto.randomUUID()` 造 conversationId 直连 WS 是受支持的主路径**。
  Go 的 `req.ConversationID == "" → uuid.NewString()` (client.go:369-372) 与浏览器一致 ✓。
- CreateConversation REST API 确实存在（JS har1#144 pos≈20920），
  body 形状如下，供未来需要"服务端命名线程/转移会话"时使用：

```js
body = { source: t, productThreadType: f(t), threadType: 'bizchat',
         gptIdentifier: y, threadCreationType: v,
         conversationTransferToken: S?p(S):void 0, audiences: C }
url  = <avalon endpoint> + '/m365Copilot/...' + '?variants=' + w   // POST
```

注意：`productThreadType` 只在这个 REST body 里出现，**不在 chatPayload 里**——
进一步证明 Go 往 chatPayload 塞 `productThreadType` 是多余项（F14）。

- 多轮行为：浏览器同一对话跨消息复用 ConversationId（har4 七连一对话），
  每条消息新开一条 WS 连接（无跨消息连接复用）。Go 的连接池会把连接放回池中给
  下一条消息用——服务端允许（项目已验证可用），但这与浏览器形态不同，
  属于已知的性能/隐蔽性权衡，无需改动，记录在案。

---

## 8. UploadFile 上传协议补充

JS 源码（har1#22 pos≈161441）显示浏览器的 FormData 版本：

```js
d.append('scenario','UploadImage'); d.append('conversationId',t);
d.append('FileBase64',e); d.append('optionsSets','cwcgptvsan');
// 条件追加：
c && FileName=l, optionsSets+='flux_v3_image_normalize_file_name'
s && optionsSets+='flux_v3_gptv_enable_upload_multi_image_in_turn'
o && optionsSets+='flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch'
n && optionsSets+='gptvpng2048'      ← PNG 专用!
r && optionsSets+='gptvnorm2048'     ← 其他格式
i && …
```

Go (client.go:1153-1166) 用 x-www-form-urlencoded（项目实测 multipart 被 400 拒绝，
JS FormData 形态反而不可用——已由 live 验证注释记录），发
`cwcgptvsan + flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch` 两项，实测成功 ✓。
可借鉴的点：**PNG 文件追加 `gptvpng2048`** 可能改善 PNG 的预处理路径（未验证，低风险实验项）。

---

## 9. Go 改动建议汇总

按优先级排序；P0=高价值低风险，P2=实验性。

### P0-1 [F9] traceId/clientCorrelationId 复用 requestID

```go
// client.go chatPayload()
- "clientCorrelationId": uuid.NewString(),
+ "clientCorrelationId": requestID,
...
- "traceId": uuid.NewString(),
+ "traceId": requestID,
```

### P0-2 [F11/F14] argument 层字段增删

```go
chat := map[string]any{"arguments": []any{map[string]any{
    ...
+   "extraExtensionParameters": map[string]any{},
+   "isSbsSupported": true,
+   "renderReferencesBehindEOS": true,
+   "disconnectBehavior": "continue",
-   "conversationId": req.ConversationID,   // 仅保留在 WS URL query
-   "productThreadType": "Office",          // 浏览器从不在此发送
    ...
-   "toolChoice": req.ToolChoice,           // 仅在有工具时加入，否则整个键不发
}}}
```

### P0-3 [F14/F15] message 层增删

```go
message := map[string]any{
    ...
-   "market": market,          // 浏览器 message 层无 market（locale 足够）
+   "clientInfo": clientInfo,  // 复用 argument 层同一个 map（内嵌副本）
}
```

### P0-4 [F12] isStartOfSession 恒 false

```go
- "isStartOfSession": firstTurn,
+ _ = firstTurn
+ "isStartOfSession": false,   // 12/12 HAR 样本恒 false
```
（保守方案：加 FeatureFlags 开关默认 false。）

### P0-5 [F13] plugins 默认带 BingWebSearch

```go
func clientPlugins(tools []Tool, mcpServerURL string) []any {
    if mcpServerURL == "" && len(tools) == 0 {
        return []any{map[string]any{"Id": "BingWebSearch", "Source": "BuiltIn"}}
    }
    ...
}
```

### P0-6 [F6] Metrics 时间戳填真值 + RequestSent

```go
now := time.Now().UTC().Format(time.RFC3339Nano)  // 各阶段取真实时刻
metrics := map[string]any{"arguments": []any{map[string]any{
    "Timestamps": map[string]string{
        "ConnectionStart": connStart, "ConnectionEstablished": connEstab,
        "UserInputStart": userInputStart, "UserInputSubmit": submitAt,
        "RequestSent": sentAt,            // 新增键
    },
}}, "target": "Metrics", "type": 1}
```

### P1-7 [F18] optionsSets 顺序对齐 + tone 大小写

```go
- defaultTone = "magic"
+ defaultTone = "Magic"
// update_memory_plugin/add_custom_instructions 移到 cwc_fileupload_odb 之后：
..., "cwc_fileupload_odb",
+ "update_memory_plugin", "add_custom_instructions",   // 按 MemoryV2 flag
"cwc_flux_v3", ...   // 删除尾部 append 分支
```

### P1-8 [§3] variants 常量更新

+16 项（含 `rich_responses`、`feature.EnablePersonalization`、
`feature.EnableBase64DataInMessageAnnotations` 等）、−4 项（WorkTab/CuaTakeControl/
ShareApisForMsa/turnOffWorkTabUpsellFromClient）。直接替换 client.go:173 字符串。
如需表达负开关：`"-agt_researcheragent_enableMemoryRead"`。

### P1-9 [F7] 响应结束补发第二 Metrics 帧

在收到 type:3 completion 后、return 前（或 goroutine 异步）发送：
Timestamps 8 键（复用 ts struct 扩展 FirstServiceResponseRendered/FirstTokenRendered/
SuggestionsRendered/SuggestionsReceived）+ ReceivedTokenMetrics
（TokenCount=len(deltas)、CharCount=len(text)、TimeBetweenTokens 可由 delta 到达时刻算出，
AverageChunkSize/BurstRate/StallRate 公式从样本反推）。

### P1-10 [F5] UA 统一

`NewClient()` 的 Firefox UA 与抓包 Chrome UA 不一致；建议改为与 HTTP 侧相同的现代
Chrome UA，或从环境读取，保证 WS 与 REST 指纹一致。

### P2-11 [F8] AttachToSession 实现（SydneyReconnect 落地）

```go
if reuseSession {   // 断线重连：沿用上次 chatsessionid
    payload = `{"arguments":["` + prevChatSessionID + `"],"invocationId":"0",
               "target":"AttachToSession","type":4}` + rs
    wsWrite(payload)   // 之后直接进入读循环等待 update 回放
}
```

### P2-12 [F3] StreamHub 端点预留

```go
var wsBase = "wss://substrate.office.com/m365Copilot/Chathub"
// FeatureFlag: FluxV4 → strings.TrimSuffix(wsBase,"Chathub")+"StreamHub"
// frontier  → "wss://substrate.office.com/m365copilotfrontier/streamhub"
```

### P2-13 [F11] skills 注入（agent 场景）

```go
eep := map[string]any{}
if len(enabledSkills) > 0 {
    eep["enabled-skills"] = map[string]any{"enabledSkillIds": enabledSkills}
}
// 同时 variants 追加 feature.EnableForceSbsShown, feature.EnableSkillExperiment
```

---

## 10. 附录：证据索引

| # | 结论 | 证据位置 |
|---|---|---|
| 参数表 13 样本 | ws_upgrades.json（挖掘产物） |
| BuildWSURL JS | har1.har entry#17 pos≈8018-8700 |
| chat payload 完整 dump | chatpayload_har1.har_364_3.json 等 12 份（挖掘产物） |
| chat+Metrics 合并帧 / 双 Metrics | har4.har entry#4 `_webSocketMessages` f3/f44 |
| AttachToSession | har4.har entry#546 f3 |
| Trouter v4 认证层 | har1.har entry#263（user.authenticate/trouter.connected） |
| Designer RealTimeChannel | har4.har entry#477/#530（protobuf+bearer，非 Chathub） |
| streamingMode flag | har1#89 pos≈36205；har1#97 pos≈157845；har3#163 pos≈36361 |
| 枚举 StreamingMode | har1#431 pos≈2288 |
| extraExtensionParameters skills | har1#22 pos≈251600-252124 |
| sydney 配置上下文默认值 | har1#4 pos≈1076892-1078307；har2#4 pos≈1136701 |
| renderReferencesBehindEOS/isSbsSupported flag | har1#89 pos≈40199-40331；har1#97 pos≈159455-159587 |
| productThreadType/CreateConversation | har1#144 pos≈20920 |
| UpdateConversation(isStartOfSession:false) | har1#381 pos≈109849；har1#470 pos≈81322 |
| UploadFile FormData/optionsSets 分支 | har1#22 pos≈161441-161508 |
| tone 菜单 SSR | har1#0 availableModelSelectionOptions（pos≈550761 起） |
| clientCorrelationId 页面级常量 | har1#0 pos≈636351；har1#866 pos≈31854；har2#0 pos≈349127 |
| SignalR 库（invocationId 机制） | har1#85（@microsoft/signalr 完整源码） |
| variants A/B 两组 diff | 挖掘脚本 variants_diff.py 输出 |
| trafficType/mocksydney/debug flags | har1#97 pos≈157800-157900 |

*生成于 2026-08-25 HAR 深度挖掘任务；分析脚本存档于临时工作目录（未入库）。*

---

## 落地清单

> 本报告的 Go 改动建议汇总（优先级 P0 > P1 > P2），详细依据见上文 §9。

- [ ] **P0 [F9]** traceId/clientCorrelationId 复用 requestID，消除两个多余随机源（client.go:1389/:1408）
- [ ] **P0 [F11/F14]** argument 层增删：补 extraExtensionParameters/isSbsSupported/renderReferencesBehindEOS/disconnectBehavior；删 conversationId/productThreadType/toolChoice
- [ ] **P0 [F14/F15]** message 层增删：删 market；补 clientInfo 内嵌副本
- [ ] **P0 [F12]** isStartOfSession 恒 false（12/12 样本），移除 firstTurn 分支
- [ ] **P0 [F13]** plugins 默认带 `[{"Id":"BingWebSearch","Source":"BuiltIn"}]`
- [ ] **P0 [F6]** Metrics 帧时间戳填真实 ISO 时刻，新增 RequestSent 键
- [ ] **P1 [F18/F16]** optionsSets 顺序对齐（update_memory_plugin/add_custom_instructions 移到 cwc_fileupload_odb 之后）；defaultTone 改 `"Magic"`
- [ ] **P1 [§3]** variants 常量更新：+16 项 / −4 项，支持 `-` 前缀负开关语法
- [ ] **P1 [F7]** 响应结束补发第二个 Metrics 帧（Timestamps 8 键 + ReceivedTokenMetrics）
- [ ] **P1 [§1.2]** UA 全链路统一为现代 Chrome UA（WS 与 REST 一致）
- [ ] **P2 [F8]** 实现 AttachToSession 断线重连帧（消费 FeatureFlags.SydneyReconnect）
- [ ] **P2 [F3]** StreamHub 端点预留开关（Chathub→StreamHub / frontier streamhub）
- [ ] **P2 [F11]** agent skills 场景注入 extraExtensionParameters["enabled-skills"]
