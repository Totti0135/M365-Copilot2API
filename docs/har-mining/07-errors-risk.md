# HAR 逆向报告 07：错误处理、风控信号与封号边界

> **本文档回答什么问题**：上游的风控到底长什么样——图片限额软拒藏在 type:2 成功帧里的实证、"零 429"结论与三级限制分层、ErrorDisallowedAADUser 等错误码全景、浏览器真实重试行为与我们 failover 的对照，以及账号健康管理预警指标的设计。
>
> 数据源：`har5.har`（37MB，Designer 图片编辑+限额场景，127 entries）为主，har1~har4/har6 交叉验证
> 分析方式：Python 全量解析 HTTP entries + `_webSocketMessages`（SignalR `\x1e` 分隔帧逐帧解析）+ OneCollector 遥测 NDJSON + RemoteUls 前端日志 + JS chunk 反混淆
> 账号样本：
> - 账号 A（har1/har2/har3）：`user-a@example.com`，licenseType=**Starter**，**无 SPE 存储 license**（copilotuploads 403）
> - 账号 B（har4/har5/har6）：`user-b@example.com`，**可生成图片但 SPE 容器被禁**（GetStorageInfo 403）
>
> 本批 HAR **零 429、零 5xx、零 Retry-After 限流头** —— 上游对 Copilot 配额的风控几乎不走 HTTP 层，这是本报告最重要的结论。

---

## 目录

1. [图片限额完整触发链路：正常→警告→拒绝](#1)
2. [结构化错误全解析：ErrorDisallowedAADUser 及错误码全景](#2)
3. [浏览器重试行为 vs 我们 failover 策略的差异](#3)
4. [全部非 200 响应的风控相关响应头](#4)
5. [token 失效与静默刷新流程](#5)
6. [给 M365-Copilot2API 的账号健康预警指标](#6)
7. [软限制 / 硬限制分界信号总表（P0 结论）](#7)

---

<a id="1"></a>
## 1. 图片限额完整触发链路：正常 → 警告 → 拒绝

### 1.1 关键结论先行

**图片生成限额的"拒绝"不在 HTTP 层，也不在 WebSocket 错误帧里，而是藏在 `type:2` 成功结果帧的 `result.meteringInformation[]` 数组中**。HTTP 状态全程 101/200，`result.value === "Success"`，唯一异常标记是：

```json
"meteringInformation":[{"meterError":"ImageGenInsufficientTokensThrottled","hasAccess":false}]
```

### 1.2 链路逐步复盘（账号 A，har1，2026-08-05）

每条用户消息新建一条 Chathub WebSocket（har1 共 7 条 WS 连接：#263 trouter、#364/#590/#650/#789 四轮对话、#873/#976 新对话），服务端最终回复统一走 `type:2` 帧（SignalR StreamItem），payload 位于 `item` 字段：

| 轮次 | entry | 用户意图 | throttling.metering 关键值 | result | 判定 |
|---|---|---|---|---|---|
| msg#1 | har1#364 | 打招呼 | `ImageGeneration:100, LLMOnly:100, WXPAgentMode:7, FileReference:3, CostQuota:0` | Success + 正常 message | ✅ |
| msg#2 | har1#590 | 问模型 | 同上 | Success + 正常 message | ✅ |
| msg#3 | har1#650 | **"你能生成图像吗？画一只猫咪吧"** | 同上（ImageGeneration 仍显示 **100**！） | `value:"Success"` **无 message 字段**，仅 `meteringInformation:[{"meterError":"ImageGenInsufficientTokensThrottled","hasAccess":false}]` | 🚫 **软拒绝** |
| msg#4 | har1#789 | （用户追问） | 同上 | Success + **文字降级道歉**："虽然这次图像生成功能没有成功…送你一只文字版小猫 ASCII" | ⚠️ 模型侧已知失败并降级 |

四轮完整 metering 快照（15 个能力槽位，账号 A）：

```json
{"TenantDataAccess":0,"ImageAnalysis":0,"LLMOnly":100,"VisualCreator":0,
 "PersonalDataAccess":0,"GraphicArt":0,"CodeInterpreter":0,"FileReference":3,
 "DeepResearch":0,"CopilotTuning":0,"DeepWork":0,"WXPAgentMode":7,
 "NotebookCowork":0,"CostQuota":0,"ImageGeneration":100}
```

**核心悖论（P0 级发现）**：msg#3 被 throttle 时 `ImageGeneration.remainingAllowance` 依然返回 **100**。证明：
1. 下发给客户端的 `throttling.metering` 是**缓存快照，不是实时余额**；
2. 真实扣减判定在服务端 metering 服务，客户端拿到的 `remainingAllowance` **不能**作为可用性判据；
3. 唯一可靠的拒绝信号就是 `meteringInformation[].meterError` + `hasAccess:false`。

对照：账号 B（har4，2026-08-23）msg#2「生成一张图片」成功返回"已为你生成图片"，其 metering 多出 Claude 系列配额（`ClaudeOpusQuery:100, ClaudeOpusQuery75:75, ClaudeOpusQueryDaily:40, ClaudeOpusQueryHourlyDev:2, ArtifactGeneration:3, ReasoningModelTurnUsage:10`）——两账号 flight/配额模板不同。

### 1.3 conversationExpiryTime 观察

- har1 msg#1：startTime 09:55:21 → expiry `2026-08-05T15:55:24Z`（**仅 +6h**）
- har1 msg#2 起：expiry `2026-09-03T09:55:46Z`（变为 **+29 天**）
- har4 msg#1 起：一律 +29 天

首轮 +6h 后续 +29d 的跳变机制不明，但说明 `conversationExpiryTime` 是动态调整的，做会话保活时应以最新帧为准。

### 1.4 聊天 → Designer 的域跳转链路（har4/har5）

```
m365.cloud.microsoft/chat (WS Chathub 生成图片成功)
  → 跳转 https://designer.svc.cloud.microsoft/editor?designerhost=&clientName=CWC&hostAppName=officeweb&lng=zh-cn&correlationId={uuid}   (har4#273)
    → designerapp.officeapps.live.com/designerapp/*.ashx 业务面:
       mediaprocessor.ashx (上传处理)          har5#3 200
       document.ashx?path=/{oid}/SAM/{id}.jpg  (原图读取, 需 fileToken)  har5#8 200 / #12+ 401
       designai.ashx (Action:GetOCR, multipart/mixed)   har5#13 200
       sam.ashx?Action=SAM (图像分割 embedding, 5.5MB fp32)  har5#15 200
       Export.ashx?forceUseDRS=true (protobuf body "+design-{uuid}\timage/png")  har5#52-59 200
       Media.ashx/?id={uuid}.png&fileToken=... (成品图读取)  har5#61-69 200
       Account.ashx?action=GetStorageInfo (SPE 存储查询)  har5#19/114/122/124 **403**
    → m365.cloud.microsoft/library/visuals (LibraryModule, 复用同一 designerapp 后端)  har5#108/115
```

fileToken 为 base64 JSON（无签名！）：`{"TokenPrefix":"<TOKEN_PREFIX>","UserObjectId":"<USER_B_OID>","ClientName":"CWC"}`（har5#12 URL 解码）。Export.ashx 每次导出都会签发新的 TokenPrefix。

### 1.5 「警告」态说明

本批 HAR 未捕捉到显式"警告"中间态（如 remainingAllowance=10 的帧）。前端 JS（har5#97 chunk）存在 `aiHubCreateModuleWXPCreateWXPErrorToastQuotaLimitEnabled` 等 toast 开关，说明产品定义了"接近限额弹 toast"路径，但其触发由服务端配置驱动，HAR 中未出现实例。**工程含义：不要指望有"警告"信号可以搭车——从正常到拒绝可能一步发生。**

---

<a id="2"></a>
## 2. 结构化错误全解析

### 2.1 ErrorDisallowedAADUser —— 错误信息全在响应头，无 body

四个实例（har5#19/#114/#122/#124、har4#452/#491），完整头部指纹：

```
GET /designerapp/Account.ashx?action=GetStorageInfo  →  403
x-errorcode:        ErrorDisallowedAADUser          ★ 机器可读错误码
x-failurereason:    User not allowed to access SPE container
access-control-allow-origin: https://designer.svc.cloud.microsoft   (Designer 页内)
                    或 https://m365.cloud.microsoft  (LibraryModule 页内)
x-correlation:      {uuid}
x-dc-hint:          WestUS2 / WestUS
sessionid:          {uuid}
x-req-start:        {ms}
CSP: require-trusted-types-for 'script'; report-uri .../OfficeDesignerApp-Production
```

**触发条件**：AAD（工作账号）身份请求 SPE（SharePoint Embedded）容器信息被拒。账号 B 能正常聊天和生成图片，唯独此接口 403 —— 即**租户/账号未开通 SPE 存放资格 ≠ Copilot 能力被封**，两者完全独立。
**注意**：401 版本同构：`x-errorcode: ErrorInvalidFileToken` + `x-failurereason: Auth token not present for validation`（har5#12/#14/#16/#18/#23）。

### 2.2 Graph 侧孪生错误（账号 A，上传通道死）

har1#238 / har3#194：`GET graph.microsoft.com/v1.0/me/drive/special/copilotuploads` → **403**，JSON body：

```json
{"error":{"code":"notAllowed",
 "innerError":{"code":"provisioningNotAllowed"},
 "message":"You do not have access to create this personal site or you do not have a valid license"}}
```

附带 `SPLogId`、`x-ms-ags-diagnostic`（DataCenter/Slice/Ring/RoleInstance 定位信息）。含义：OneDrive 个人站点无法 provision → copilotuploads 虚拟目录不可用 → **文件上传链路整体不可用**，但聊天不受影响（账号 A 全程可聊）。

### 2.3 前端错误码枚举表（JS chunk 反混淆，风控相关的完整清单）

**终态错误码列表**（har5#103 `m365-copilot-client-module-library.chunk.js`，命中即不重试、归为 ExpectedFailure）：

```
ErrorBadDriveState, ErrorGraphStorageFull, ErrorGraphItemNotFound,
ErrorRaaSContainerProvisioning, ErrorSchemaVersionNotSupported,
ErrorProfileImageNotFound, ErrorClientClosedConnection,
ErrorFailedODAccountStorageFull, ErrorFailedItemNotFoundInOD,
ErrorViolateRAIGuidelines,
ErrorUserBanned,            ← ★ 封号终态
ErrorUserThrottled,         ← ★ 限流终态
ErrorDisallowedAADUser,     ← ★ 本报告实测命中
InsufficientTokens          ← ★ 令牌耗尽终态
```

**Designer 图片生成错误枚举**（har5#99 chunk）：

```
BackendOrUnexpectedError, RAIBlocklistError, RAIGenericError,
RAISuspensionWarningError, RAISuspendedError, HarmfulContentError,
AuthenticationError, CharacterLimitReachedError, UnsuccessfulLoginError,
StorageRehydrationError, UserBannedError, TooManyRequestsError,
UnsupportedDalleImage, UnsupportedMediaError, HumanFaceInInputError,
DesignerUserLimitThrottlingError, BICRequestInProgressError,
DallERequestTimeoutError, RegionBlockedError, TenantAccessBlocked,
ProtectedDocError,
AADImageOutOfCreditsError, AADPosterOutOfCreditsError, AADBannerOutOfCreditsError,
MSAFamilyOrPersonalOutOfCreditsError, MSAFreeOrBasicOutOfCreditsError,
OutOfCreditsError, BlankChatCardError, ThirdPartyRequestValidationError,
BrandificationError, RetrieveGroundingData*Error(4种), GenerationStoppedByClient/DueToTimeout...
```

**轮询子状态**（同 chunk）：`EmptyResponse, PollRequestFailed, GenerationStoppedDueToTimeout, ClientCanceled, MissingAuthToken, NoResponse, TooManyRequests, RAIError, BackendOrUnexpectedError`。

**meterError 家族**（结合我们已在用的 variants flight `feature.EnableImageGenInsufficientTokensThrottled` / `feature.EnableImageGenSystemCapacityThrottled`，见 internal/chathub/client.go:173）：至少存在 `ImageGenInsufficientTokensThrottled`（个人额度，本批实测）与 `ImageGenSystemCapacityThrottled`（全局容量，flight 已预留，未捕获实例）两类。

### 2.4 遥测侧的错误归档结构（OneCollector 完整 schema）

403 发生后，前端上报 `Office.DesignerApp.Performance.OutgoingRequest`（har5#123 完整事件）：

```json
"Data.StatusCode": 403,
"Data.RetryCounter": 0,                      // ★ 403 零重试的直接证据
"Data.ServerHost": "designerapp.officeapps.live.com",
"Data.ServerPath": "/designerapp/account.ashx",
"Data.ExpectedNegativeImpact": "FeatureUnusable",
"Data.ErrorsFromAllAttempts": "{\"DataError-attempt-1\":\"StatusCode: 403\"}",
"Data.Error": "Status code 403, X-ErrorCode: ErrorDisallowedAADUser",
"Activity.Result.Type": "ExpectedFailure",
"Activity.Result.IsExpected": true           // ★ 命中终态错误码列表 → 归为预期失败
```

---

<a id="3"></a>
## 3. 浏览器重试行为 vs 我们的 failover 差异

### 3.1 实测时间线

**(a) document.ashx 401 ×5 —— 自动重试，近似指数退避**

| attempt | 时刻(har5) | 间隔 |
|---|---|---|
| 1 | 14:37:24.868 | – |
| 2 | 14:37:25.327 | 0.46s |
| 3 | 14:37:26.198 | 0.87s |
| 4 | 14:37:27.390 | 1.19s |
| 5 | 14:37:31.018 | 3.63s |

间隔 ≈ 0.46→0.87→1.19→3.63s，符合 jitter 指数退避形态。5 连败后放弃，随后走重新取 token 流程（§5）。注意这是 `<img>`/媒体加载场景的重试；401 的 `x-errorcode` 明确指出缺 fileToken。

**(b) GetStorageInfo 403 ×4 —— 零自动重试**

时刻：14:37:27.802 → 14:38:25.553 → 14:38:29.857 → 14:38:32.926。第一次属于 Designer 编辑器页，后三次属于 LibraryModule 页面三次路由切换（每次挂载都发起新查询，tanstack-react-query `queryFn`）。遥测 `Data.RetryCounter: 0` + ULS 日志（RemoteUls，har5#22）直接写着：

```
Not retryable error: AxiosError: Request failed with status code 403
```

**(c) 前端通用重试引擎**（har5#103 反混淆还原）：

```js
// 伪代码还原
retryPolicy = { maxRetries, waitBetweenTriesMs, retryOnStatusCodes }
for (; attempt <= maxRetries;) {
  resp = await fetch(...)
  if (attempt > 0) {
    if (!retryOnStatusCodes.includes(status)) break
    await sleep(retryAfterHeaderValue || waitBetweenTriesMs)   // ★ Retry-After 优先于固定间隔
  }
  if (resp.ok || isExpectedError(...) && !keepRetryingExpected) return resp
}
// 特殊 header：X-SkipRetry（服务端可强制禁重试）、X-Dc-Hint（回传机房提示）
```

可重试条件白名单：`status < 300 || status === 422 || status === 429 || 网络层错误(ECONNABORTED/ERR_CANCELED/ERR_NETWORK/offline/document.hidden)`。
MSAL 层自报能力：token 请求体携带 `x-ms-lib-capability=retry-after%2C%20h429`（支持 Retry-After 与 HTTP 429 处理，har5#109）。

**(d) WebSocket 层**：每条消息独立建连（Chathub），单连接内不做消息级重试；socket.io RealTimeChannel 用 `__ping__/__pong__` 保活（har4#530）；augloop 会话由服务端下发 `forceReconnect:false` 与 `sliceUrl` 决定重连目标（har5#113）。

### 3.2 与本项目 failover 的差异对照（internal/chathub/client.go / internal/web/errors.go）

| 维度 | 浏览器真实行为 | 我们现状 | 建议 |
|---|---|---|---|
| 429 | 重试白名单成员，Retry-After 优先退避 | ✅ QUOTA_429 + RetryAfter 传递 | 一致 |
| 403 | **终态，绝不重试**（RetryCounter=0 实证） | client.go:415 将 403 计入 dial 失败并 failover | 保持 failover（换账号），但**不要对同账号原地重试 403**；且需区分 `ErrorDisallowedAADUser`（存储禁用≠聊天死） |
| 401 | 媒体类 5 次指数退避后触发静默续约 | 已归 auth 类 | 补充：401 后先刷新 token 再判死刑 |
| 422 | **可重试白名单成员**（出乎意料） | 未特殊处理 | 上游若回 422 应纳入重试 |
| 文本通道限流 | Copilot 把软拒写在 message 里（§1.2 msg#4 话术） | ✅ ErrRateLimitNotice 已识别 "please retry"+"later" | 追加特征串："图像生成功能没有成功"、`meterError` 关键词 |
| meteringInformation | 浏览器靠它判定软拒 | **缺失** —— 我们目前不解析 type:2 帧的 `item.result.meteringInformation` | **P0：解析并暴露该字段，作为账号额度探针**（见 §6） |

---

<a id="4"></a>
## 4. 非 200 响应的风控相关响应头全集

六份 HAR 全量扫描（排除遥测 status=0 噪音）后的完整清单：

| entry | status | 端点 | 风控相关头 |
|---|---|---|---|
| har5#12/14/16/18/23 | 401 | document.ashx | `x-errorcode: ErrorInvalidFileToken`、`x-failurereason`、`x-dc-hint: WestUS`、`x-correlation`；**无 WWW-Authenticate、无 Set-Cookie、无 Retry-After** |
| har5#19/114/122/124, har4#452/491 | 403 | Account.ashx?GetStorageInfo | `x-errorcode: ErrorDisallowedAADUser`、`x-failurereason`、`x-dc-hint`、`sessionid`；无 Set-Cookie 变化 |
| har1#238, har3#194 | 403 | graph copilotuploads | JSON error body（§2.2）；`SPLogId`、`client-request-id`、`request-id`、`x-ms-ags-diagnostic`；ACAExposeHeaders 声明 `Retry-After,WWW-Authenticate` **可出现在该端点**（本次未触发） |
| har4#132/224, har4#356 | 200/304 | ecs.office.com/config | `retry-after: 900`（配置缓存指令，**与限流无关**，勿误读） |

要点：
- 整个数据集 **没有任何 X-RateLimit-\*** 头；Copilot 主链路限流不走 header。
- 401/403 响应**不翻动任何 Cookie**（会话状态纯靠 Bearer token），封禁/限流判定无法从 Set-Cookie 观察到。
- `x-errorcode`/`x-failurereason` 是 designerapp 系错误的标准载体；graph 系则是 `{error:{code,innerError:{code}}}` JSON。两套体系并存，解析器需要双轨。
- `x-ms-ags-diagnostic` 泄露上游拓扑（Korea Central/E/Ring 4/SE1PEPF000110B7），可用于故障归因与区域漂移检测。

---

<a id="5"></a>
## 5. token 失效与静默刷新流程

### 5.1 双 token 体系

- **fileToken**（资源级，designerapp 专用）：base64(JSON)，无签名，随 Media/Export URL 下发；缺失 → 401 ErrorInvalidFileToken。有效期观察：har5 内同一 TokenPrefix 跨多次请求复用。
- **OAuth access_token**（用户级）：MSAL.js 5.9.0 管理，per-resource scope 分离。

### 5.2 静默刷新标准流程（har5#109/#117 实录）

```
POST https://login.microsoftonline.com/{tenantId}/oauth2/v2.0/token?client-request-id={uuid}
Content-Type: application/x-www-form-urlencoded

client_id=4765445b-32c6-49b0-83e6-1d93765276ca        ← M365 Web 固定 clientId
&scope=https://designerappservice.officeapps.live.com//.default openid profile offline_access
&grant_type=refresh_token
&client_info=1&x-client-SKU=msal.js.browser&x-client-VER=5.9.0
&x-ms-lib-capability=retry-after,h429                  ← MSAL 自报支持 Retry-After/429
&refresh_token=1.AT4A...                               ← 每次 rotate（#109 与 #117 的 RT 不同）
&X-AnchorMailbox=Oid:<USER_B_OID>@<TENANT_ID>         ← 路由锚点，Oid@Tid 格式
```

响应（200）：`token_type:Bearer`、`scope:"https://...//designerappservice.all ..."`, `expires_in:5330, ext_expires_in:5330`（≈89 分钟）；augloop 资源则 `expires_in:3875`（≈65 分钟）。augloop WS 握手后服务端再下发 `{"tokenExpirationTime":1787499783,"tokenExpirationSeconds":3875}`（har5#113 帧）确认寿命。

### 5.3 刷新触发时机

har5 中两次刷新（14:38:23 designerappservice、14:38:26 augloop）发生在 401 风暴之后、LibraryModule 加载之时——即**进入新模块时 MSAL acquireTokenSilent 按 resource 逐个续约**，而非严格由 401 回调驱动。六份 HAR 共 16 次 token 端点调用全部 200，无 refresh_token 失效样本（无 `interaction_required` / invalid_grant 案例）。har2#79 是 authorization_code 兑换（登录起点），其余均为 refresh_token。

**工程含义**：我们的 PKCE 网关（pkce_auth_gateway.py）应模拟同样的 per-resource scope 拆分与 AnchorMailbox 参数；refresh_token 必须持久化每次 rotate 后的新值。

---

<a id="6"></a>
## 6. 账号健康管理「预警指标」设计

按信号到达顺序与可信度分级（全部可直接从我们代理的上游流量中提取）：

### P0 —— 已经出事的确定性信号

| 指标 | 提取位置 | 含义 | 动作 |
|---|---|---|---|
| `item.result.meteringInformation[].meterError` | Chathub WS type:2 帧 | 该账号该能力**已触顶**（实测值 `ImageGenInsufficientTokensThrottled`；姊妹值 `ImageGenSystemCapacityThrottled` 由 flight 预留） | 给账号打标：图片能力冷却至次日；继续放行纯文本 |
| `hasAccess:false` | 同上数组元素 | 能力级准入被否 | 同上 |
| `x-errorcode ∈ {ErrorUserBanned, ErrorUserThrottled, InsufficientTokens}` | HTTP 响应头 | 终态封禁/限流 | 立即下线账号池中该账号 |
| graph 403 `error.innerError.code=provisioningNotAllowed` | copilotuploads 响应 | 账号存储从未 provision，上传链路永久废 | 账号标记 no-upload，避免反复撞 403 加深风控画像 |

### P1 —— 额度水位（趋势预警）

| 指标 | 提取位置 | 说明 |
|---|---|---|
| `throttling.metering.*.remainingAllowance` | 每个 type:2 帧 | 注意 §1.2 悖论：它是**滞后的快照**，只可做趋势线（连续多轮下降才可信），不可做实时闸门 |
| `numUserMessagesInConversation` vs `maxNumUserMessagesInConversation`(600) | type:1/type:2 帧 | 单对话计数器；逼近 600 时主动换会话 |
| `ClaudeOpusQueryDaily/HourlyDev` 等细分槽 | metering 快照 | 高价值模型独立配额，耗尽速度远快于主额度（har4 实测 Daily=40、HourlyDev=2） |
| `conversationExpiryTime` 突然收缩 | item 字段 | har1 实测出现过 +6h 异常值；骤缩可能预示会话级干预 |

### P2 —— 环境与画像信号

| 指标 | 提取位置 | 说明 |
|---|---|---|
| `x-errorcode: ErrorDisallowedAADUser` | designerapp 403 | SPE 容器禁用。**单独出现≠账号危险**（账号 B 实测聊天完好），但与其它信号叠加时可视为低权限画像 |
| `x-ms-ags-diagnostic` DataCenter/Ring 漂移 | graph 响应头 | 同一账号短时间内数据中心大幅跳动=出口 IP 漂移，是风控画像输入 |
| `ecs.office.com` 配置里 QuotaLimit 相关 flag 翻转 | ECS 配置响应 | 前端 toast/降级开关由服务端下发，flag 变化先于用户体验变化 |
| 遥测分类 `Activity.Result.IsExpected` | OneCollector | 学习微软自己把哪些错误当"预期"，用于校准我们的重试白名单 |

### 最小实现建议

在现有 `internal/web/stream.go` 的 MarkFailure 路径旁增加一个 `ParseMetering(item []byte)` 钩子：正则提取 `meterError|hasAccess|maxNumUserMessagesInConversation|remainingAllowance` 写入账号 ledger（internal/web/agent_ledger.go 已有账本骨架），即可让 §6 表格全部落地，改动集中在 chathub/client.go 消息分发处一处。

---

<a id="7"></a>
## 7. 软限制 / 硬限制分界信号总表

| 维度 | 软限制（可恢复） | 硬限制（终态） |
|---|---|---|
| HTTP 状态 | **200/101 全绿** | 403/401（header 携带 x-errorcode） |
| 载体 | WS type:2 帧 `result.meteringInformation` | 响应头 / graph JSON error body |
| 实测错误值 | `ImageGenInsufficientTokensThrottled` + `hasAccess:false` | `ErrorDisallowedAADUser`、`provisioningNotAllowed`；枚举中的 `ErrorUserBanned/ErrorUserThrottled/InsufficientTokens` |
| 客户端反应 | 模型文字降级（道歉+替代方案），UI 不弹错 | ULS 记 `Not retryable error`，遥测标 `IsExpected:true`，组件直接放弃 |
| remainingAllowance | 可能仍显示满额 100（快照滞后） | — |
| 对账号池的含义 | 单能力冷却（小时级~次日），账号保留 | 账号级剔除或功能级永久降级 |
| 重试语义 | 可在下一轮对话试探（新对话新计费） | 同参数重试只会加深坏画像 |

**一句话结论**：Copilot 的风控分层为 *metering 软拒（WS 成功流内） → 能力终态码（x-errorcode） → 账号/租户硬封（枚举存在但本批未捕获实例）* 三级；监控必须深入 WebSocket 应用帧，仅看 HTTP 状态码会 100% 漏掉第一级。

---

## 附：证据索引

| 发现 | 证据位置 |
|---|---|
| ImageGenInsufficientTokensThrottled | har1#650 IN 帧（prompt 见 OUT 帧"你能生成图像吗？画一只猫咪吧"）；降级话术 har1#789 |
| 两账号 metering 全量快照 | har1#364/#590/#650/#789/#873/#976；har4#4/#163/#194/#201/#211/#546 |
| ErrorDisallowedAADUser | har5#19/#114/#122/#124、har4#452/#491 响应头 |
| ErrorInvalidFileToken | har5#12/#14/#16/#18/#23 响应头 |
| provisioningNotAllowed | har1#238、har3#194 body |
| 401 重试退避序列 | har5#12→#23 startedDateTime |
| 403 零重试 | har5#123 遥测 `Data.RetryCounter:0`；har5#20 RemoteUls "Not retryable error" |
| 前端终态错误码列表 | har5#103 chunk @offset 2893 |
| Designer 错误枚举 | har5#99 chunk @offset 28082 |
| 重试引擎实现 | har5#103 chunk retry loop（maxRetries/waitBetweenTriesMs/Retry-After 优先/X-SkipRetry） |
| MSAL 刷新参数 | har5#109/#117 请求体 |
| fileToken 明文结构 | har5#12 URL 参数 base64 解码 |
| Retry-After=900（非限流） | har4#132/#224/#354 ecs.office.com |
| Designer 跳转入口 | har4#273 `/editor?clientName=CWC&hostAppName=officeweb` |
| augloop token 寿命 | har5#113 WS TokenProvisionResponse |

---

## 落地清单

> 本报告的风控与错误处理改动建议汇总，详细依据见上文 §3.2 对照表与 §6 预警指标。

- [ ] **P0** 在 chathub/client.go 消息分发处加 ParseMetering 钩子：提取 item.result.meteringInformation 的 meterError/hasAccess 写入账号 ledger
- [ ] meterError=ImageGenInsufficientTokensThrottled → 该账号图片能力冷却至次日，纯文本继续放行
- [ ] x-errorcode ∈ {ErrorUserBanned, ErrorUserThrottled, InsufficientTokens} → 立即下线账号池中该账号
- [ ] graph copilotuploads provisioningNotAllowed → 标记 no-upload，避免反复撞 403 加深风控画像
- [ ] 403 不对同账号原地重试（浏览器 RetryCounter=0 实证）；区分 ErrorDisallowedAADUser——存储禁用 ≠ 聊天不可用
- [ ] 401 先刷新 token 再判死刑（对齐浏览器媒体类指数退避后静默续约）
- [ ] 上游 422 纳入重试白名单（浏览器实证其为可重试成员）
- [ ] rateLimited 启发式追加特征串："图像生成功能没有成功"、meterError 关键词
- [ ] throttling.metering.remainingAllowance 只做趋势预警、不做实时闸门（快照滞后悖论见 §1.2）
- [ ] conversationExpiryTime 骤缩监控（+6h 异常值可能预示会话级干预）
