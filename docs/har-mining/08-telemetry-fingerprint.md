# HAR 逆向报告 08：遥测体系、客户端指纹与反检测特征

> **本文档回答什么问题**：服务端靠什么识别"正常客户端"——三路遥测流（/events、OneCollector、client_health_ping）的结构与节奏、四层会话 ID 关联链、variants 动态派生的三方一致性校验机制，以及 Go 客户端的拟真度评分卡与最小拟真改动清单。

数据源：本地抓包文件 har1.har ~ har6.har（2026-08-05 ~ 2026-08-23，M365 Copilot web）。
方法：Python 全量解析 2229 条 entry，交叉比对 `internal/chathub/client.go` 现有实现。
证据标注 `harN[特征]`；token/身份值已脱敏。**本篇核心结论：服务端识别"正常客户端"的最强信号不是 header，而是三路遥测流（/events、OneCollector、client_health_ping）+ Trouter 长连接的存在性；我们当前全部缺失。**

---

## 1. m365.cloud.microsoft/events 埋点全解析

### 1.1 端点行为

- `POST https://m365.cloud.microsoft/events`，同源请求，`Content-Type: application/json`
- 请求体：`{"events":[...], "context":{"sessionId":"…","officeHomeEcsRing":"StandardRelease","featureFlags":{…}}}`
- 响应体：`{"success":true,"received":N}`（89 个 HTTP 200 样本）
- har1 中另有 **328 条 status=0** 的同名请求：页面卸载期 `navigator.sendBeacon` 发出后被取消，随后**相同 payload 以 XHR 重发**（har1 中同一 10984B payload 在 09:55:11~09:55:14 出现 4 次）。→ 服务端对**重复事件幂等去重**，客户端重试是正常行为，模拟时可安全重发。
- 触发时机（har1 时序）：页面加载后 0~9s 内爆发式上报 SSR 阶段积压事件（单批最大 127KB），之后转为每轮对话结束时一批。**没有固定心跳周期**——它是事件驱动批量上传，不是定时器。

### 1.2 _formatterKey 枚举（12 种，共 6712 事件）

| formatterKey | 数量 | 用途 |
|---|---|---|
| EventV2 | 3505 | 主诊断通道（Sydney 连接生命周期、API 调用、性能） |
| CopilotResultsRender | 1737 | 渲染性能指标（ChatE2ELatency 等 30 项/轮） |
| DiagnosticEvent | 764 | debug 级诊断（WebVitals、StoreStateInfo） |
| CopilotAction | 351 | 用户交互行为（QuerySubmit、InputFocused…） |
| 3sEvent | 288 | 页面加载 3 秒检查点 |
| ErrorEvent | 42 | error 级（ClientProxyError、SSRClientAuthError） |
| CopilotSessionEvent | 19 | EventLoggerClientCreated（每个 route 一个 logger 实例） |
| APIResponseReceived / Request / PageView / Performance / Impression | 少量 | 辅助 |

### 1.3 eventId 枚举（关键子集）

**EventV2（93 种）中对拟真最关键的：**

```
SydneyClient.WarmupStarted / StartConnection / ConnectionBuilt / Connected / ConnectionClosed
SydneyVariants            ← 记录本次会话生效的全部 variants（见 §6 对照）
SydneyOptionsSets         ← 记录 optionsSets 全文
SendingRequestToSydney / ReceivedResponseFromSydney(Success) / FirstServiceResponse
ChatFirstChunkReceived / ChatFirstControlReceived / StreamingLatency / ResponseTokenRate
TrouterTelemetry          ← "[Trouter-Wrapper] TrouterClient_13_26: Trouter started"
COTReceivedOverTrouter    ← CoT 经 trouter 下发的证明
SydneyGetChatApiTokensStart/Finish (fluxVersion:"3")
SydneyReconnectAttempt    ← {attempt, maxAttempts:3, backoffDelayMs:500}
ClientProxySuccess / ClientProxyError / APIRequestAborted
DiscoverMCPServers (72次) / GetHeaderObjectAxios / GetMSALToken
UXDiagnostic(702) / AgentPerformanceMeasure(494) / IrisFetchDecision(173)
```

**CopilotAction 用户行为序列（每轮对话的标准轨迹）**：
`InputFocused → InputChanged → QuerySubmit → USR → GPT5ChatModelUpdated`；历史操作 `HistoryChatDeleted`。

**DiagnosticEvent**：`WebVitals(metricName:TTFB)、OAIKeyResolverSuccess、SubmitDisabled/Enabled、SbsEligibilityDecision、SafeLinksInitialization、FloodgateSetupSkipped`（Floodgate=NPS 问卷系统，8 个会话中 8 次全部 skipped）。

### 1.4 session 关联方式（服务端串联视图的关键）

一次会话中四层 ID 并存，全部可被服务端关联：

| ID 层 | 生命周期 | 出现位置 | 样例(har1) |
|---|---|---|---|
| `sessionId`（浏览器会话） | 整个标签页生命周期 | body.context.sessionId == 请求头 `X-Session-Id` == WS URL `X-SessionId` == Taos `Session.Id` | `<SESSION_ID_A_HEX>` |
| `clientCorrelationId` | **每轮对话一个**（=requestId） | events properties + substrate `client-request-id` + WS `chatsessionid/clientrequestid/XRoutingParameterSessionKey` | `<REQ_ID_EVENT_SAMPLE>` |
| `BrowserSessionId` / InteractionSessionId | 页面加载周期 | Taos.Hub.* 事件内 | `<BROWSER_SESSION_ID>` |
| MSAL `sesId` | 每个 1DS SDK 实例一个 | OneCollector ext.app.sesId | `<MSAL_SESID>`(base64ish) |

格式规律：sessionId 有两种形态 —— 带 连字符的 GUID 与 **32 位无连字符 hex**（如 `<SESSION_ID_A_HEX>`），后者实为同一 GUID 去 `-`。`traceId` 字段 = clientCorrelationId（去连字符形式）。**我们的 Go 客户端 SessionID/requestID 生成方式与此一致，无需改**。

### 1.5 context.featureFlags —— 客户端过滤配置回显

```json
{"enableCalipsoEventPropertyFromCode":true,"enableUnifiedTelemetryIntegration":true,
 "enableSydneyBlockList":true,"enableServerSideEcsConfigIds":true,
 "enableBizChatConfigIdsForMcaSessionFlights":true,
 "enableClientEventBlockList":true,
 "clientEventBlockList":"{\"eventId\":[\"HybridAuth_GetToken\",\"^TokenAcquisitionSuccess$\",\"^TokenAcquisitionFailure$\"],\"message\":[\"No text block found in adaptive card\"]}"}
```

含义：ECS 下发的 blocklist 正则决定哪些 eventId **不上报**。服务端知道自己给这个用户下发了什么 blocklist → 如果我们伪造 /events 却上报了被 blocklist 过滤的事件类型，反而暴露。`officeHomeEcsRing` 在 417 批次中恒为 `"StandardRelease"`。

---

## 2. OneCollector 遥测（browser.events.data.microsoft.com）

### 2.1 请求结构

```
POST /OneCollector/1.0/?cors=true&content-type=application/x-json-stream&w={0|2}
Headers:
  Content-Type: application/x-json-stream
  Client-Id: NO_AUTH                          ← 固定值（未登录态采集器）
  client-version: 1DS-Web-JS-{3.2.15|3.2.18|4.2.1}   ← 按 SDK 实例区分
  apikey: {iKeyGuid}-{uuid}-{port}             ← 见映射表
  upload-time: {epoch ms}
  time-delta-to-apply-millis: use-collector-delta
Body: NDJSON 流（多个事件一行一个）
```

### 2.2 apikey ↔ iKey 映射表（apikey 前 12 位即 iKey）

| iKey (o:) | 归属 | 请求数 |
|---|---|---|
| d634483c… | MSAL.js 鉴权遥测 (clientId 4765445b) | 153 |
| eba12008… | Office.Taos.Hub.*（M365 门户 OTel） | 149 |
| 983f1bcd… | Designer 应用 OTel | 104 |
| 6a8929bc… | Designer 性能 | 47 |
| 70ed233f… | Office.Dime.Sdk.*（flight 分配） | 20 |
| b8ffe739… | MeControl（头像控件） | 18 |
| 3de4087d / 86e373bb / e5ad7601 | Designer 子组件 | 少量 |

### 2.3 ext 公共指纹段（每事件必带）

```json
"ext": {
  "sdk": {"ver":"1DS-Web-JS-4.2.1","seq":N,"epoch":"4026885819"},   ← epoch 每批次随机 10 位数字
  "app": {"locale":"zh-hans","sesId":"…"},
  "user":{"locale":"zh-CN"},
  "web": {"domain":"m365.cloud.microsoft","screenRes":"1920X1080","userConsent":false},
  "intweb": {}, "utc":{"popSample":100}, "loc":{"tz":"+08:00"}
}
```

### 2.4 高价值指纹事件

**① Office.Taos.Hub.Session（会话启动，14KB 单事件）** — 服务端画像的核心：

```
App.Version = 2.20260731.36.0        ClientBuildVersion = 同     ← 前端 build 号（随部署更新！）
User.PrimaryIdentityHash = <PUID_HASH>   Space = OrgIdPuid
User.TenantId = <TENANT_ID>           User.TenantGroup = Commercial
UserLicenseType = AAD_Paid           Geo = koreacentral
FlightsHash = <FLIGHTS_HASH>
EcsRing = StandardRelease            Data.Session.Flights = "P-R-1755750-1-1,P-R-1088282-1-1,…(60+)"
Session.SamplingValue = 0.179        SamplingKey = PrimaryIdentityHash
```

**② Office.Dime.Sdk.Flight** — 设备级持久标识 + flight 配额：

```
Device.Id = <DEVICE_ID>      ← localStorage 持久 GUID，跨会话稳定
PersistentId = 同 Device.Id
CorrelationVector = <CV_VALUE>                 ← cV 向量，标准格式 base64+序号
EcsETag = <ECS_ETAG>
EcsConfigIds = P-E-1851971-2-7,P-R-1905776-2-9,…(50+)  ← 该设备命中的 config id 全集
PartnerId = cwc   EcsCountry = CN   Market = cn
UserAgent = Mozilla/5.0 …Edg/139.0.0.0                ← 遥测内再报一遍 UA 供交叉校验
```

**③ 事件名枚举（60+，节选高频）**：`acquireTokenSilent(32)/acquireTokenByCode、localStorageUpdated(114)、Office.Taos.Hub.{Request,Impression,Performance,PageView.BizChat,Feature.HybridAuthGetToken}、Office.Fluid.EcsClient.Generic、MeControlWeb_{PageView,OutgoingRequest}`。MSAL 事件含完整鉴权参数（authority/libraryVersion=5.9.0/cacheLookupPolicy=4）。

### 2.5 上报节奏（har1 时间线）

加载后 11s 内 10 批（含重复重发），此后每 ~30-90s 一批；每批 1-66KB。**登录后的前 30 秒是遥测密度最高窗口**——若只模拟稳态而缺启动风暴，分布异常。

---

## 3. 浏览器指纹收集点与会话标识链

> ⚠️ 本批 HAR 由新版 Chrome/Edge 导出，**Cookie/Set-Cookie 头已被 DevTools 隐私机制剥离**，无法直接枚举 MUID/PPAuth/SCC 值。但通过遥测载荷可确认以下标识的存在与作用域：

| 标识 | 作用域 | 刷新机制 | HAR 证据 |
|---|---|---|---|
| MUID | 设备级（.microsoft.com 域族） | 1 年滚动 | 未见于本批 HAR（剥离）；Dime `Device.Id` 为其 localStorage 补位 |
| Device.Id / PersistentId | 浏览器 profile 级 | localStorage 持久，不过期 | har1 Office.Dime.Sdk.Flight |
| SSO cookie（ESTSAUTHPERSISTENT 等 login.live.com 族） | 租户级 | 随 OAuth 刷新 | 03 篇已析 |
| SCC/Substrate token | 每资源 | expires_in≈4300-5300s，refresh_token 静默续 | 03 篇 §1 |
| FlightsHash | 会话级 | 每页加载由服务端注入 HTML 重算 | Taos.Session |
| EcsETag/EcsConfigIds | 设备级缓存 | ECS 响应携带变更 | Dime.Flight |
| sec-ms-gec | 请求级反爬令牌 | Edge 自动计算（版本 1-139.0.3405.102） | 仅 arc/bing 域 3 次 |

**一致性校验链（服务端可做的交叉验证）**：
UA ↔ sec-ch-ua ↔ sec-ms-gec-version ↔ 遥测内 UserAgent ↔ screenRes/tz/locale 必须自洽。真实样本：Edge139 会话全套 `Chromium/139 + Edg/139 + gec-version 1-139.x`；Chrome151 会话全套 `151` 且无 gec。**混搭（如 Chrome UA + Edge gec）即为破绽**。

---

## 4. 自定义头取值规律

| Header | 值规律 | 变更频率 |
|---|---|---|
| x-session-id | GUID（连字符形），= telemetry sessionId | 每标签页 |
| x-host-context | `{"clientPlatform":"web","hostName":"officeweb"}`；SSR 内层加 `appName:"SSR","appMode":"default"` | 固定 |
| x-client-eligibility | 服务端下发后原样回传的 JSON（isCopilotEligible/m365CopilotAcquisitionState:"acquired"/featureSet.uxFeatures:[CodeInterpreter,Designer,GPTV,Threads,WebGroundingControls]/shareSetting:"Confidential"\|"All"/copilotAdminPinSetting:"Pinned"） | 每会话由 `/chat` SSR 响应刷新 |
| x-client-flights | `enablePromptAutoSuggestProvider,enableeipa,ReGroupHistoryPromptOnTop,EnableMtgSeriesSuggClient` | 版本级固定 |
| x-client-language / x-client-localtime | `zh-cn` / ISO8601 带时区当前时刻 | 每请求 |
| x-edge-shopping-flag | `0` | 固定 |
| x-anchormailbox | `Oid:{oid}@{tid}` | 用户级 |
| x-routingparameter-sessionkey | requestId（无连字符） | 每轮 |
| x-ms-mac-appid / hostingapp / version | `f6859b87-…` / `BizChatMetaOS` / `ocv-inapp-feedback-shared_local_build` | 仅反馈组件 |
| clientname / clientbuild | `CWC` / `1.0.20260819.6` | 版本级 |
| client-id / client-version (OneCollector) | `NO_AUTH` / `1DS-Web-JS-x.y.z` | SDK 级 |
| x-dc-hint | `WestUS2` | 数据中心提示 |
| x-scenario | `OfficeWebIncludedCopilot`（大写变体 `officeweb` 也出现） | 固定 |
| x-events-source | `recovered-localstorage`（仅离线恢复批次） | 特殊场景 |

**client_health_ping（未被文档化的健康探针）**：
`GET /client_health_ping?type={documentLoad|loggerInit|mainLoad}&route=%2Fchat&traceId={reqid}&sessionId={sid}` → 204。每次页面加载恰好 3 条、顺序固定。**零成本高收益的拟真项**。

---

## 5. ECS 配置下发分析

### 5.1 clients.config.office.net/user/v1.0/web/policies

带 `Authorization: Bearer <clients.config.office.net/.default>`（scope=UserPolicies.Read，见 03 篇 #3/#9/#24）。响应：

```json
{"policiesHash":"<POLICIES_HASH>",
 "value":[{"policyType":1,"priority":0,
   "policiesPayload":[{"app":"office16","platform":"Web","settingId":"office16;L_CopilotPinning",
                       "type":"REG_DWORD","value":"1","valueName":"CopilotPinning"},…]}]}
```

纯 IT 管理员策略（Copilot 固定），不参与 chat A/B。**GET 幂等、结果稳定**，可直接透传或硬编码 policiesHash。

### 5.2 ecs.office.com/config/v1/Fluid/0.0.0.1（15KB 配置）

Query：`agents=FluidExperiences,Segmentation&audience=Production&userId={oid}&tenantId={tid}&hostName=M365Chat`。返回数百个开关。**其中一项直接操纵 Sydney variants**：

```
loopApp.isNotebooksInfographicsSydneyVariantsOverride =
  "cdximage_gen_gpt_image_2_prod,cdiimage_gen_gpt_image_1_5,…,feature.EnableBypassHelix3PFlightAllowListWithRequestVariants"
```

### 5.3 A/B 分流机制与我们硬编码 variants 的暴露风险

分流链条：`ECS(EcsConfigIds per device) → 前端拼 variants → WS URL query.variants + 首帧 arguments[0].optionsSets`，同时 `Taos.Session.Flights + FlightsHash` 把命中集上报。**服务端因此能做三方一致性校验：该用户 EcsConfigIds 应推出的 variants ≡ 实际 WS query.variants ≡ 遥测 SydneyVariants 文本。**

对照实测 diff（browser har1-har6 vs internal/chathub/client.go:173 硬编码）：

- 浏览器有而我们缺 **22 项**，含高风险行为开关：
  `rich_responses, SingletonEnvOn, EnableComposeWidget, -agt_researcheragent_enableMemoryRead, agt_researcheragent_enableMemoryRead, feature.EnablePersonalization, feature.EnableResearchSteering, feature.EnableResearcherTodo{ObserverSlim,SummarizerPacing,Observer}, feature.EnableCodeInterpreterConversion, feature.EnableBase64DataInMessageAnnotations, feature.EnableSkipEmittingMessageOnFlush, feature.EnableSkipRehydrationForSpeCIdImages, feature.EnableRemoveEmptySourceAttributions, feature.EnableContentApiandDocTypeHtmlInRichAnswers, agt_module_attr_enableReferencesForCodeInterpreter, agt_module_enableCodeInterpreterHallucinatedUrlFilter, cdxenablefccinmainline, cdxenablerenderforisocomp, cdxgrounding_api_v2_rich_web_answers_reference_bottom_force`
- 我们多出 **3 项**（真实流量未见）：`feature.turnOnWorkTabRecommendation, turnOffWorkTabUpsellFromClient, feature.EnableCuaTakeControlApi`
- 注意 `-agt_researcheragent_enableMemoryRead` 带负号（禁用内存读取）——**它出现与否直接改变服务端记忆写入行为**，属于功能性差异而非纯标记。

结论：**variants 不是静态常量而是 per-user/per-flight 动态集合**。最低风险做法是从某次真实登录的 ECS/Taos.Flights 推导并按账号缓存；直接全局硬编码会在账号维度产生可聚类的异常签名。

---

## 6. Trouter / Chathub 心跳节奏与断线判定

### 6.1 Trouter v4/c 长连接（go.trouter.teams.microsoft.com）

```
WSS /v4/c?tc={"cv":"2025.30.01.1","ua":"BizChat","hr":"","v":"3639/1.0.0"}
       &timeout=40 &epid={sessionId} &dom=m365.cloud.microsoft
       &cor_id={guid} &con_num={epoch_ms}_0 &check={epoch_ms}
握手后立即: send  5:::{"name":"user.authenticate","args":[{"headers":{"Authorization":"Bearer …"}}]}
recv:  5:1::{"name":"trouter.connected","args":[{"id":"<TRouter_CONNECTION_ID>",…
         "url":"https://pub-ent-ince-10-f.trouter.teams.microsoft.com:8443/v4/f/{id}/",…}]}
recv:  trouter.message_loss ×N（断线期间 tag/etag 清单）
心跳:   每 40s  send 5:{n}+::{"name":"ping"}   ← 实测 09:55:52 / :92 / 56:32，严格 40s
               recv 6:::{n}+["pong"]
```

- `timeout=40` 即服务端判定断线的阈值：**40s 内无任何帧则判死**。应用层 ping 周期恰等于该值。
- 断线恢复后服务端补发 `message_loss`（tag="", messaging, messagingsync…），客户端据此拉增量——**跳过 message_loss 处理不影响 chat，但不处理会导致 COT 类消息丢失**（COTReceivedOverTrouter 事件证明 CoT 可走 trouter 下发）。

### 6.2 Chathub SignalR 心跳（substrate.office.com/m365Copilot/Chathub）

- 握手：send `{"protocol":"json","version":1}` → recv `{}` → **send `{"type":6}`（客户端主动首 ping）**
- 空闲期：服务端每 **15±1s** 发 `{"type":6}`，客户端立即回应；双向均有发起能力（har1 中 1785923758.5 server→1785923758.7 client 交错）
- 断线判定：SignalR 默认 ServerTimeout=30s 双向；实测流内最长静默 15s，从未逼近阈值
- Go 现状（client.go:716）：被动应答 type:6 ✓，但**缺少握手后主动首 ping**，且空闲连接依赖服务端 ping 保活——语义等价，时序指纹略异（低风险）。

### 6.3 Sydney 层重连参数

`SydneyReconnectAttempt metaData = {"attempt":1,"maxAttempts":3,"backoffDelayMs":500}` → **最多 3 次重连，500ms 指数退避**；`ConnectionClosed` metaData 记录全量 signalCounts（up: type6×1,type4×1,type1×2; down: type1×15,type2×1,type3×1; errorCount:0）供服务端对账。若我们的连接从不产生这些计数模式（例如从不发 type:4=stream invocation 完成），对账侧可发现不对称。

---

## 7. 拟真度评分卡（Go 客户端 vs 真实浏览器，按风险降序）

| # | 维度 | 真实浏览器 | 我们现状 | 服务端可观测性 | 风险 | 建议 |
|---|---|---|---|---|---|---|
| 1 | **遥测存在性** | 三路全开（/events + OneCollector + health_ping），登录后 30s 密集爆发 | 全部为零 | 「有 chat WS 流量但零遥测」是最简单的全局聚类异常 | **P0** | 最小方案：登录后发 3×client_health_ping + 2 批 /events（复制真实 payload 结构，eventId 从 §1.3 取） |
| 2 | **variants 集合** | ECS 动态派生，含 22 项行为开关 | 硬编码 51 项，22 缺 3 多 | WS query 直查 + 与 EcsConfigIds 对账 | **P0** | 按账号从首次真实会话抓取缓存；负号项（memoryRead）单独核对 |
| 3 | **Trouter 长连接** | 登录必建，40s ping | 无 | 连接级存在性查询，一查一个准 | **P1** | 至少在活跃会话期维持一条 trouter 连接（认证+ping 即可，不必消费消息） |
| 4 | UA 生态自洽 | Edge139↔sec-ch-ua139↔gec139 或 Chrome150/151 全套 | Firefox/148 裸 UA（无 sec-ch-ua，Firefox 语义自洽） | header 组合统计 | P1 | Firefox 方案技术上自洽；若求分布相似可切 Chrome 全套+伪 sec-ch-ua |
| 5 | x-client-\* 头（m365 域） | eligibility/host-context/session-id/flights/localtime 全带 | 仅访问 m365.cloud.microsoft 静态资源时不带 | SSR/eligibility 校验日志 | P1 | eligibility JSON 可硬编码 §4 样本；host-context/session-id 成本极低建议补 |
| 6 | ECS/policies 拉取 | 启动时 GET policies + Fluid config | 无（也不需要其内容） | 「该租户用户从未拉过策略」弱信号 | P2 | 低优先；可在会话初始化时顺手 GET 一次 policies |
| 7 | SignalR 时序 | 主动首 ping + 15s 双向互发 + type:4 完成 帧 | 被动 pong | ConnectionClosed signalCounts 对账 | P2 | 加握手后首 ping；type:4 是否必须待协议组确认（01 篇） |
| 8 | 会话 ID 格式 | GUID/去连字符双形态，四层关联 | 一致 ✓ | — | ✅ | 保持 |
| 9 | Chathub WS 参数 | 13 个 query 参数逐字一致 | 一致 ✓（BuildWSURLWithOptions） | — | ✅ | 保持 |
| 10 | optionsSets | 33 项（§1.3 SydneyOptionsSets 全文） | 基础集 + 场景附加 | WS 首帧 | P2 | 以 har1 SydneyOptionsSets message 全文为准校准 |

---

## 8. 附：最小拟真改动清单（收益/成本比排序）

1. **health_ping 三连**（~20 行 Go）：登录成功后依次 GET documentLoad/loggerInit/mainLoad，参数用现有 sessionID/requestID。
2. **variants 替换**：以 har1 WS URL 中完整 variants 串替换 client.go:173 常量（含 22 个新增项，删除 3 个多余项）。
3. **/events 两批**：`EventLoggerClientCreated + StoreStateInfo + WebVitals(TTFB)` 启动批 + 每轮对话结束 `QuerySubmit/CopilotSuccessResponseRendered/StreamingLatency` 批；context.sessionId=X-Session-Id 对齐。
4. **trouter 保活线程**：WSS v4/c + authenticate + 40s ping 循环，会话空闲 5 分钟后可断。
5. （可选）Chrome UA 全套切换，需同步改 plugins.go/m365cloud.go/server.go 四处硬编码。

---
*生成：opencode har-mining 任务 08；解析脚本留存于临时目录（tel_scan/tel_events/tel_ext/tel_syd/tel_cookies/ecs_ws/variants_diff.py）*

---

## 落地清单

> 最小拟真改动清单（按收益/成本比排序），详细依据见上文 §8 与 §7 评分卡。

- [ ] health_ping 三连（~20 行）：登录成功后依次 GET documentLoad/loggerInit/mainLoad，参数复用现有 sessionID/requestID
- [ ] variants 替换：以真实会话抓取的 variants 串替换 client.go:173 常量（+22 项 / −3 项，负号项 memoryRead 单独核对）
- [ ] /events 两批最小遥测：启动批（EventLoggerClientCreated+StoreStateInfo+WebVitals）+ 每轮结束批（QuerySubmit/CopilotSuccessResponseRendered/StreamingLatency），context.sessionId 与 X-Session-Id 对齐
- [ ] trouter 保活线程：WSS v4/c + user.authenticate + 40s ping 循环，会话空闲 5 分钟后可断
- [ ] （可选）Chrome UA 全套切换，需同步 plugins.go/m365cloud.go/server.go 等四处硬编码
- [ ] SignalR 握手后主动首 ping（时序指纹对齐，低风险）
