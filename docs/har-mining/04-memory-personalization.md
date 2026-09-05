# HAR 逆向报告 04：临时会话（disableMemory）与个性化记忆系统

> **本文档回答什么问题**：临时会话到底改变了什么——与正常会话在 WS URL/payload/REST/Cookie 四个层面的逐项对比、个性化的三层开关模型（租户/用户/会话）、CustomInstructions CRUD 全流程与 system prompt 注入机制，以及在 API 面暴露临时会话与记忆管理端点的完整方案。
>
> 数据源：har4.har (68MB, 2026-08-23, 临时会话场景, 7 次 Chathub 连接)、har6.har (1.1MB, 2026-08-23, 个性化设置面板全操作)；交叉对照 har1.har (2026-08-05, 正常会话)、har2/3/5。
> 租户：<TENANT_ID>，用户 OID <USER_B_OID>（user-b@example.com），licenseType=Starter。

---

## 1. 临时会话与正常会话逐条对比

### 1.1 WS URL 参数差异 —— 唯一开关就是 `disableMemory=1`

**证据 A（har4 entry[4]/[163]/[194]/[201]/[211]/[246]/[546]，全部 7 条 Chathub 连接）**：

```
wss://substrate.office.com/m365Copilot/Chathub/{OID}@{TID}?
  chatsessionid=<REQ_ID_1>            ← 每轮新连接换新 UUID
  &XRoutingParameterSessionKey=同上
  &clientrequestid=同上                  ← 三者恒相等
  &X-SessionId=<SESSION_ID_B>   ← 浏览器会话级，7 连接不变
  &ConversationId=<CONVERSATION_ID_B> ← 临时会话全程复用同一个
  &disableMemory=1                       ← ★ 正常会话完全没有此参数
  &access_token=<REDACTED_TOKEN>       ← Bearer JWT 直接放 URL query（aud=sydney）
  &variants=EnableMcpServerWidgets,...(68 项)
  &source="officeweb"                    ← 带字面双引号
  &product=Office&agentHost=Bizchat.FullScreen&licenseType=Starter&isEdu=false
  &agent=web&scenario=OfficeWebIncludedCopilot
```

**证据 B（har1 entry[364]/[590]/[650]/[789]/[873]/[976]，正常会话 6 条连接）**：query 中 `disableMemory` **ABSENT**；两个对话分别用 ConvId `<CONVERSATION_ID_A1>` 和 `<CONVERSATION_ID_A2>`。har1 的 chatsessionid 有两种形态：hex 无连字符（`<SESSION_ID_A_HEX>`）与标准 UUID——说明服务端对 chatsessionid 格式无强校验。

| 参数 | har4 临时会话 | har1 正常会话 |
|---|---|---|
| disableMemory | **1** | 不发送 |
| ConversationId | 7 连接复用同一值 | 一个对话一个值 |
| chatsessionid/clientrequestid/XRoutingParameterSessionKey | 每连接新 UUID，三者相等 | 同左（偶见 hex 形态） |
| X-SessionId | 跨连接固定 | 跨连接固定 |
| access_token | URL query 传递 | 同左 |
| 其余全部参数 | 一致（含 variants/source 引号） | 一致 |

### 1.2 WS chat payload 差异 —— **零差异**

对 har1 entry[364] 与 har4 entry[4] 的首条 `{"target":"chat","type":4}` 帧（SignalR 多帧粘连，用 raw_decode 取首帧）做结构 diff：

- optionsSets 集合差集双向为空，33 个完全一致。**两侧都包含 `update_memory_plugin` 和 `add_custom_instructions`** —— 即临时会话的 payload 照样声明记忆能力，由 URL 层的 disableMemory 让服务端跳过执行。
- 顶层 keys、message keys 双向差集均为空。
- 共同关键点（复现必需）：`streamingMode:"ConciseWithPadding"`、`tone:"Magic"`、`plugins:[{"Id":"BingWebSearch","Source":"BuiltIn"}]`、`allowedMessageTypes` 含 `MemoryUpdate`/`HintInvocation` 等 31 种、`isStartOfSession:false`（首轮也是 false）、`invocationId:"0"`、`disconnectBehavior:"continue"`、双层 clientInfo（外层 + message 内层）。

### 1.3 ConversationId 复用模式

**证据（har4 七连接时间线）**：

```
entry[  4] t=14:25:35 csid=<REQ_ID_1> conv=<CONVERSATION_ID_B> chats=1 isStartOfSession=[False]
entry[163] t=14:25:50 csid=<REQ_ID_2> conv=<CONVERSATION_ID_B> chats=1
entry[194] t=14:26:39 csid=<REQ_ID_3> conv=<CONVERSATION_ID_B> chats=1
entry[201] t=14:27:03 csid=<REQ_ID_4> conv=<CONVERSATION_ID_B> chats=1
entry[211] t=14:27:35 csid=<REQ_ID_5> conv=<CONVERSATION_ID_B> chats=1
entry[246] t=14:28:03 csid=<REQ_ID_RECONNECT> conv=<CONVERSATION_ID_B> chats=1 msgs=222
entry[546] t=14:30:35 csid=<REQ_ID_RECONNECT> conv=<CONVERSATION_ID_B> chats=0 msgs=7   ← 断线重连，复用 csid，未发消息
```

规律：
1. **每轮提问 = 新建一条 WS 连接**（不是长连接多轮），每连接只发 1 条 chat、invocationId 恒 `"0"`；
2. **ConversationId 由客户端生成并跨轮复用**（整个"临时会话"就是一个 ConvId），没有独立的"创建会话"REST 调用；
3. 断线重连时 chatsessionid 复用（entry[246]/[546] 同 csid）；
4. 服务端回答完毕后发 SignalR Close 帧 `{"type":3,"invocationId":"0"}` 结束本轮（entry[246] 尾部 S->C 实录）。

### 1.4 Cookie / Header 差异 —— 没有 cookie

har1 与 har4 的 Chathub 握手 header 逐项对比：origin=`https://m365.cloud.microsoft`、sec-websocket-version=13 等，**两者均无 Cookie 头**（认证 100% 依赖 query 中的 access_token）。差异仅 UA 版本号（Chrome 139 vs 151）和 accept-language 权重顺序，均为环境噪声。

### 1.5 REST 行为差异 —— 临时会话不触碰个性化 API

扫描 har4 全部 560 entries：`PersonalizationUserFlags` / `CustomInstructions` / `/puds/` 调用数为 **0**。即前端进入临时会话后既不读取也不写个性化设置，记忆屏蔽完全由服务端在 WS 侧执行。

### 1.6 服务端响应流验证

har4 全部 S->C 帧中 `MemoryUpdate`/`memory`/`CustomInstruction`/`personaliz` 关键词命中 **0 次**（对照组 har1 也为 0，说明记忆写入是低频事件、正常闲聊不触发；但 allowedMessageTypes 声明证明该通道存在）。结合 1.1 的 URL 开关可确认：disableMemory=1 时服务端既不注入已有记忆/自定义指令到 system context，也不下发 MemoryUpdate 写入帧。

---

## 2. PersonalizationUserFlags API 全解析

来源：har6 entry[8]（GET）、entry[10]/[11]/[21]/[22]（POST ×4）。

### 2.1 GET 读取

```
GET https://substrate.office.com/m365Copilot/PersonalizationUserFlags?variants=feature.EnablePersonalization
Headers:
  x-routingparameter-sessionkey: {OID}
  x-anchormailbox: Oid:{OID}@{TID}
  x-scenario: OfficeWebIncludedCopilot
  x-clientrequestid: {32位hex随机}
  content-type: application/json
  Referer: https://m365.cloud.microsoft/
  （无 Authorization/Cookie —— 浏览器侧由 Service Worker 注入 token，
   HAR 响应元数据 _fetchedViaServiceWorker:true 佐证；Go 复现需自行加 Bearer）
```

响应 200：

```json
{
  "isMemoryEnabled": false,
  "isCustomInstructionEnabled": true,
  "isPersonalizationEnabledByTenant": true,
  "isInsightsFromConversationHistoryEnabled": false,
  "isM365GraphContentEnabled": false,
  "result": {"value": "Success", "renewCert": false, "serviceVersion": "1.0.03520.49037"}
}
```

Flag 含义：
| Flag | 含义 | 证据 |
|---|---|---|
| isMemoryEnabled | 记忆功能总开关（写入+读取） | POST 切换前后遥测事件 `MemorySwitchOn`/`MemorySwitchOff`（har6 events） |
| isCustomInstructionEnabled | 自定义指令总开关 | 遥测 `CustomInstructionToggleOn/Off` 与 POST body 一一对应 |
| isPersonalizationEnabledByTenant | 租户管理员级个性化授权（只读，POST 不接受） | GET 返回 true 而 isMemoryEnabled=false，用户级开关在其约束下生效 |
| isInsightsFromConversationHistoryEnabled | 从历史会话提取洞察（"洞察"类个性化） | flag 名 + 默认 false |
| isM365GraphContentEnabled | 允许用 M365 Graph 内容（邮件/文档等）做个性化 | flag 名 + 默认 false |

### 2.2 POST 部分更新

请求体**只携带要修改的单个字段**（PATCH 语义但用 POST 方法）：

```
POST /m365Copilot/PersonalizationUserFlags?variants=feature.EnablePersonalization
{"isMemoryEnabled":true}    → 200 {"result":{"value":"Success","message":"Successfully updated personalization user flags.","renewCert":false,"serviceVersion":"1.0.03520.49037"}}
{"isMemoryEnabled":false}   → 200 同上
{"isCustomInstructionEnabled":false} → 200 同上
{"isCustomInstructionEnabled":true}  → 200 同上
```

### 2.3 与 disableMemory 的关系

三层模型：
1. **租户层** `isPersonalizationEnabledByTenant=true` 授权个性化可用；
2. **用户层** `isMemoryEnabled` 决定服务端是否为该用户维护/应用长期记忆（影响所有普通会话）；
3. **会话层** WS URL `disableMemory=1` 单次会话级屏蔽，优先级最高，且不影响任何持久化 flag（har4 全程零 REST 调用即为证）。

variants 里同时出现 `feature.EnablePersonalization` 与 `agt_researcheragent_enableMemoryRead`（har4 URL 也带），说明 variants 只是前端能力声明，真正的门是上述三层开关。

---

## 3. CustomInstructions API 全流程（CRUD）

来源：har6 entry[23]/[27]（GET 空列表）、entry[29]（POST 创建）、entry[30]/[32]（GET 读回）、entry[33]（DELETE）、entry[35]（GET 验证删除）。

### 3.1 数据结构

```json
{"instructions":[{
  "id": "<MAILBOX_ITEM_ID>",
  "instruction": "<指令正文>",
  "displayName": "",
  "useCase": "Copilot Custom Instruction"
}],"result":{"value":"Success","renewCert":false,"serviceVersion":"1.0.03520.49037"}}
```

`id` 是 base64 的 **Exchange 邮箱 ItemId**（`AAMkAG` 前缀，解码可见 mailbox GUID 与 folder/message 层级）→ 自定义指令持久化在用户邮箱里，随邮箱走而非独立 DB。

### 3.2 创建（POST）

```
POST /m365Copilot/CustomInstructions?variants=feature.EnablePersonalization
{"instruction":"You are an AI assistant accessed via an API... Today's Yap score is: {Yap-Score}. # Valid channels: analysis, commentary, final...",
 "userAssignedName":"Global Instruction",
 "useCase":"GenericChat"}
→ 200:
{"id":"<MAILBOX_ITEM_ID>",
 "conversationId":"<SERVER_CORRELATION_ID>",   ← 服务端内部关联 ID，非聊天会话
 "requestId":"<CLIENT_REQUEST_ID_HEX>",             ← 回显 x-clientrequestid
 "telemetry":{"startTime":"2026-08-23T14:24:44.7799456Z"},
 "result":{"value":"Success","serviceVersion":"1.0.03520.49037"}}
```

注意：创建时 `useCase` 提交 `"GenericChat"`、名称字段叫 `userAssignedName`；读回时 useCase 变成 `"Copilot Custom Instruction"`、名称字段变成 `displayName` 且值为空——**userAssignedName 未被服务端持久化**（至少 Starter license 下如此）。

### 3.3 注入机制

WS chat payload 的 optionsSets 固定含 `add_custom_instructions`（har1/har4 均有，见 §1.2）。当 `isCustomInstructionEnabled=true` 且 WS 未带 `disableMemory=1` 时，服务端自动把 instructions 数组内容拼入本轮 system context——客户端无需（也无法）在 payload 里传指令文本。这就是「把任意 system prompt 注入 M365 Copilot」的官方通道：**POST CustomInstructions 后所有后续普通会话生效，直到 DELETE**。

### 3.4 删除（DELETE）

```
DELETE /m365Copilot/CustomInstructions/<MAILBOX_ITEM_ID>?variants=feature.EnablePersonalization
→ 204 No Content
```

ItemId 作为路径段直接拼接（浏览器实际发送的是未转义原串；Go 侧建议 `url.PathEscape` 保留 `/`+ 兼容）。删除后 GET 返回 `{"instructions":[],...}`。

---

## 4. puds/v1/me/settings/copilot 设置解析

来源：har6 entry[14]/[17]。

```
PATCH https://substrate.office.com/puds/v1/me/settings/copilot
{"isWebSearchInWebEnabled":false}  → 204 No Content
                                   Resp ETag: W/"AAAc9aBx3k+wn0daN3l/NgAAEoR4/A=="
{"isWebSearchInWebEnabled":true}   → 204 No Content
```

- PUDS = Personal User Data & Settings（平台统一用户设置存储），`copilot` 是其中一个 settings bag；弱 schema，PATCH 只发改动键，返回 204+新 ETag（并发控制可用 If-Match，本抓包未见客户端发 If-Match）。
- `isWebSearchInWebEnabled`：控制 Copilot 在网页场景下的联网搜索插件。关闭后触发遥测 `WebPluginToggleOff`、UI 渲染 `WebSearchDisabledBannerRendered`；它作用于 WS payload 的 BingWebSearch plugin 执行与否。
- 已知设置项汇总（本次抓包实证 1 个；bag 支持任意扩展键，其余如语音/视觉权限等未在本流量中出现，不作猜测）。

### 附：遥测事件名清单（eventScope=Personalization）

har6 `m365.cloud.microsoft/events` POST 体中枚举出完整前端事件面，可直接用于行为对齐/监控：

```
MemorySwitchOn / MemorySwitchOff            → POST PersonalizationUserFlags {isMemoryEnabled}
CustomInstructionToggleOn/Off               → POST PersonalizationUserFlags {isCustomInstructionEnabled}
CustomInstructionsRendered / ...SelectionButtonClicked / CustomInstructionSave
CopilotPersonalization(x10) / SettingsTabRendered / PersonalizationTabRendered
CopilotPudsSettings                         → PATCH puds {isWebSearchInWebEnabled}
WebPluginToggleOn/Off / WebSearchDisabledBannerRendered / WebSearchDisclaimerRendered
```

另：`search/api/v1/userconfig`（har6 entry[3]，har1/har3 亦有）body `{"RequestedConfigTypes":["ContentSources"],"Scenario":{"Name":"officeweb"}}` 返回搜索 ContentSources 配置，属搜索插件配置而非记忆系统。

---

## 5. M365-Copilot2API 功能方案：临时会话 + 记忆管理

现状：`internal/chathub/client.go:1045 BuildWSURLWithOptions(..., disableMemory bool)` 已支持拼 `disableMemory=1`（client.go:1069-1071），但未暴露到 API 面；无任何个性化代理端点。

### 5.1 /v1/chat/completions 扩展

```jsonc
POST /v1/chat/completions
{
  "model": "gpt-5.6-reasoning",
  "messages": [...],
  "metadata": {
    "copilot_temp_session": true      // ★ 新增：置 true 则 BuildWSURLWithOptions 传 disableMemory=true
  }
}
// 兼容通道（三选一实现，推荐 metadata 为主）：
// 1) model 别名:  "gpt-5.6-reasoning-temp"  → resolver 剥离 -temp 后缀并置 disableMemory
// 2) header:      X-Copilot-Temp-Session: 1
```

语义对齐实测：temp 会话应使用一次性 ConversationId（客户端生成 UUID），请求结束不入库（或入库打 `ephemeral` 标记供清理任务回收），绝不复用既有会话 ConvId——避免把临时上下文污染进持久会话树。

### 5.2 新增记忆管理端点（反向代理 substrate）

```
GET    /v1/memory/flags                 → GET  PersonalizationUserFlags（原样透传 JSON）
PATCH  /v1/memory/flags                 → POST PersonalizationUserFlags（body 原样转发，天然部分更新）
GET    /v1/memory/instructions          → GET  CustomInstructions
PUT    /v1/memory/instructions          → POST CustomInstructions
         body: {"instruction": "...", "useCase": "GenericChat"}
DELETE /v1/memory/instructions/{id}     → DELETE CustomInstructions/{PathEscape(id)}
PATCH  /v1/memory/settings              → PATCH puds/v1/me/settings/copilot
         body: {"isWebSearchInWebEnabled": false}
```

鉴权沿用现有 API key 体系；管理类操作建议仅 admin key 可用（对齐 internal/web/admin_security.go 既有模式）。

### 5.3 Go 实现要点

1. **认证头**：浏览器经 Service Worker 注入 token（HAR 无 Authorization 头的直接原因，`_fetchedViaServiceWorker:true` 佐证）；Go 直连必须显式加 `Authorization: Bearer <acc.AccessToken>`，scope 需含 `sydney.readwrite`/M365Chat.Read（JWT scp 实测含 `M365Chat.Read sydney.readwrite`）。
2. **必带头**：`x-anchormailbox: Oid:{OID}@{TID}`、`x-routingparameter-sessionkey: {OID}`、`x-scenario: OfficeWebIncludedCopilot`、`x-clientrequestid: {32hex}`、`content-type: application/json`。缺 anchor mailbox 可能被路由拒。
3. **DELETE ItemId**：base64 含 `/ + =`，用 `url.PathEscape` 并保留原始大小写；不要 QueryEscape。
4. **flags 缓存**：GET flags 结果按账号缓存（TTL ~60s），chat 路径无需每次查询——因为临时性由 WS URL 控制，与 flags 解耦。
5. **system prompt 注入路径**：OpenAI 兼容层的 system message 有两种落地方式：(a) 仅本次有效 → 拼进首条 user text 或 per-request optionsSets（未观测到专用字段，风险高）；(b) 持久生效（实测可靠）→ PUT /v1/memory/instructions 写入后由服务端自动注入。推荐 (b) 并在文档注明全局副作用。
6. **会话收尾**：服务端以 `{"type":3}` Close 帧结束每轮，客户端读到 type:3 即可关连接换新；勿在同一连接发 invocationId>0 的第二条 chat（抓包显示前端从不这么做）。
7. **puds ETag**：204 响应带弱 ETag，若做设置回写冲突检测可在 PATCH 加 `If-Match`（可选增强，非必需）。

### 5.4 风险提示

- CustomInstructions 写入的是用户真实 M365 账号的邮箱存储，多租户共享账号池时会互相污染——instructions CRUD 必须绑定 account_id 维度并在 UI 上警示。
- `isPersonalizationEnabledByTenant=false` 的租户下 flags POST 可能被拒（本次抓包未覆盖该分支），实现时对非 Success result 做透传报错。

---

## 附录：证据索引

| 结论 | 位置 |
|---|---|
| disableMemory=1 全部 7 连接 | har4 entry[4,163,194,201,211,246,546] request.url |
| 正常会话无 disableMemory | har1 entry[364,590,650,789,873,976] |
| payload 零差异（optionsSets 33 公共） | har1 e364 vs har4 e4 首条 target:chat 帧 raw_decode diff |
| ConvId 复用 / 每轮一连接 / type:3 收尾 | har4 连接时间线 + entry[246] 尾部帧 |
| flags GET 五字段响应 | har6 entry[8] response.content.text |
| flags POST 部分更新 ×4 | har6 entry[10,11,21,22] postData.text |
| instructions 空表→建→读→删→验空 | har6 entry[23,29,30,33,35] |
| POST body 三字段 / userAssignedName 不回显 | har6 entry[29] vs entry[30] |
| puds PATCH + 弱 ETag | har6 entry[14,17] response.headers.etag |
| SW 注入 token（无 Authorization 头） | har6 entry[8,14] headers + _fetchedViaServiceWorker |
| 遥测事件名全集 | har6 m365.cloud.microsoft/events entry[24,28] 等 |
| 临时会话零个性化 REST | har4 全量 URL 扫描 0 hits |

---

## 落地清单

> 本报告的功能方案与实现要点汇总，详细依据见上文 §5。

- [ ] `/v1/chat/completions` 支持 `metadata.copilot_temp_session=true` → BuildWSURLWithOptions 传 disableMemory=true（兼容通道三选一：metadata / model 别名 `-temp` 后缀 / X-Copilot-Temp-Session 头，推荐 metadata）
- [ ] temp 会话使用一次性客户端生成 ConversationId，请求结束不入库（或打 ephemeral 标记供清理任务回收）
- [ ] 新增记忆管理端点反向代理 substrate：GET/PATCH `/v1/memory/flags`、GET/PUT/DELETE `/v1/memory/instructions`、PATCH `/v1/memory/settings`
- [ ] Go 直连必带头：Authorization Bearer + x-anchormailbox + x-routingparameter-sessionkey + x-scenario + x-clientrequestid
- [ ] DELETE CustomInstructions 的 ItemId 用 url.PathEscape（保留 `/` `+`），勿 QueryEscape
- [ ] GET flags 结果按账号缓存 ~60s；临时性由 WS URL 控制，与 flags 解耦
- [ ] system prompt 注入推荐持久 instructions 通道，并在文档注明全局副作用
- [ ] 读到 SignalR type:3 Close 帧即收尾换新连接；勿在同一连接发 invocationId>0 的第二条 chat
- [ ] instructions CRUD 绑定 account_id 维度，防共享账号池交叉污染并在 UI 警示
- [ ] flags POST 对非 Success result 透传报错（isPersonalizationEnabledByTenant=false 分支未实测）
