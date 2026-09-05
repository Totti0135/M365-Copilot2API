# HAR 逆向报告 05：SignalR 流式响应帧结构与内容形态

> **本文档回答什么问题**：服务端的流式响应应该怎么读——903 帧 WebSocket 流量的 SignalR 类型分布与时序规律、writeAtCursor 与 snapshot 冗余双通道的正确消费方式、引用/建议问题/图片生成的挂载位置与出现时机，以及 client.go 流解析逻辑的遗漏帧类型清单（G1-G9）。
>
> 数据源：本地抓包文件 har1.har ~ har6.har，其中含 Chathub WebSocket `_webSocketMessages` 的会话共 **13 个**（har1×6、har4×7；har2/3/5/6 的 WS 为 Trouter/AugLoop/Designer，非 Chathub）。
> 原始 WS 帧 **903 帧**，按 `\x1e` 切分后得 **925 条 SignalR 记录**。解析脚本：Python（frames.jsonl 中间产物）。
> 本文所有样本均已脱敏（OID/TID/sessionId/messageId 截断或替换）。

---

## 1. 帧类型全量分布与时序

### 1.1 类型统计（925 条记录）

| SignalR type | 方向 | 数量 | 含义 | 备注 |
|---|---|---|---|---|
| 握手（无 type） | 双向 | 26 | `{"protocol":"json","version":1}` / `{}` ack | 13 会话 × 2 |
| 6 Ping | client→srv | 30 | 心跳 | 握手后立即发首个（+0.12s） |
| 6 Ping | srv→client | 16 | 心跳 | 仅在空闲 >15s 时出现 |
| 4 StreamInvocation | client→srv | 13 | 发起对话，target=`chat`, invocationId=`"0"` | 12 个完整回合 + 1 个重连 |
| 1 Invocation target=`update` | srv→client | **792** | 流式更新主通道 | 见 §2 |
| 1 Invocation target=`Metrics` | client→srv | 24 | 客户端遥测回传 | 见 §2.5 |
| 2 StreamItem | srv→client | **12** | 每回合恰好 1 个，最终态快照 | 见 §2.4 |
| 3 Completion | srv→client | **12** | 每回合恰好 1 个，`{"type":3,"invocationId":"0"}` | 无 error/result 字段 |

关键结论：
- **type=3 Completion 是空壳**：`{"type":3,"invocationId":"0"}`，不带 result/error。真正的最终文本在 type=2 的 `item.result.message` 里。以 type=3 作为流结束信号是正确的，但不能从 type=3 取内容。
- **每用户回合的帧序列固定为**：`handshake → ping → type4(chat) → [Progress → throttling → cursor+首快照 → (delta ∥ snapshot)×N → isLastUpdate → patch(spokenText) → suggestion挂载 → ReferencesListComplete] → type2(StreamItem) → type3`。
- 一个 WS 连接承载一个回合；多轮对话 = 多条 WS（与 03 号文档结论一致）。har4#246 与 #546 共享同一 chatsessionid（图像生成后的重连续传），此时 #546 无 type4。

### 1.2 时序间隔规律

| 会话 | 记录数 | 时长 | 服务端 ping 数 | ping 间隔 | 帧间 gap(max) |
|---|---|---|---|---|---|
| har1#364 | 23 | 6s | 0 | - | 2.8s |
| har1#650 | 30 | 58s | 3 | 15~16s | 15.9s |
| har1#976 | 212 | 31s | 2 | 15s 整 | 8.5s |
| har4#201 | 100 | 22s | 1 | - | 5.4s |
| har4#246 | 223 | **148s**(图像生成) | 6 | 15~16.7s | 16.0s |

- 客户端 ping：握手后 ~0.1s 发第一个，之后每 **15s** 一个（无论忙闲）。
- 服务端 ping：仅在流式间隙 >15s 时插入（如外部长时间搜索/生图），间隔同样 15~16s。
- **读超时建议 ≥45s**（3 个 ping 周期）；90s（现网实现值）安全。
- 首 token 延迟：Metrics 遥测显示 RequestSent→FirstTokenReceived ≈ 0.9s（简单问答）到数十秒（搜索+生图）。

---

## 2. type=1 update 帧解剖

### 2.1 arguments[0] 顶层字段全集（792 帧实测）

| 字段 | 出现帧数 | 类型与含义 |
|---|---|---|
| `nonce` | 779 | Base64 随机串（如 `<NONCE_SAMPLE>`），帧去重/防重放标记 |
| `messages` | 499 | 消息数组（快照通道 + Progress + Suggestion + 标记帧），见 §2.2 |
| `requestId` | 496 | = 用户消息 messageId/clientCorrelationId |
| `writeAtCursor` | **274** | string，游标增量文本，见 §3 |
| `references` | 274 | dict，与 writeAtCursor 同帧出现的引用增量，见 §4 |
| `cursor` | **12** | object，每回合首帧一次，见 §3.1 |
| `isLastUpdate` | 11 | bool=true，最终权威快照标记 |
| `patches` | 7 | JSON-Patch 数组，实测只清空 spokenText |
| `throttling` | 12 | object，回合级配额计数器，见 §5 |
| `messageIdsToDelete` | 2 | string[]，指示客户端删除临时消息（生图 Progress 卡） |
| `conversationTransferToken` / `meteringInformation` / `suggestedResponses` | **0** | ⚠️ 这三个字段在 update 参数里**从未出现** |

### 2.2 messages[].messageType 枚举（接收方向）

服务端在 903 帧中实际发出的 messageType 只有 **4 种**：

| messageType | 条数 | contentOrigin | 结构特征 |
|---|---|---|---|
| （缺省=Chat 文本流） | 468 | `DeepLeo` | 累计快照文本，见 §3.2 |
| `Progress` | 20 | `EarlyProgress`(6) / `ImageGeneration`(6) / 空(8) | 进度卡 |
| `ReferencesListComplete` | 11 | 空 | 引用列表完结标记，text=""，无 body |
| `Suggestion` | （独立消息 69 条 contentOrigin） | `SuggestionsProviderService` | 建议问题，见 §4.3 |

请求侧 `allowedMessageTypes`（type=4 声明的 30 种全量）：

```json
["Chat","Suggestion","InternalSearchQuery","Disengaged","InternalLoaderMessage",
 "Progress","GeneratedCode","RenderCardRequest","AdsQuery","SemanticSerp",
 "GenerateContentQuery","GenerateGraphicArt","SearchQuery","ConfirmationCard",
 "AuthError","DeveloperLogs","TriggerPlugin","HintInvocation","MemoryUpdate",
 "EndOfRequest","TriggerConfirmation","ResumeInvokeAction","ResumeUserInputRequest",
 "TriggerUserInputRequest","EscapeHatch","TriggerPluginAuth","ResumePluginAuth",
 "SideBySide","ReferencesListComplete","SwitchRespondingEndpoint"]
```

⚠️ **InternalSearchQuery / GeneratedCode / RenderCardRequest / SearchQuery / Disengaged 等 20+ 种声明类型在这 903 帧中一次都没出现过**——它们是协议保留位。M365 Copilot 把搜索查询塞进 `Progress.contentType="SearchResults"`（见 §2.3），把代码执行结果并入正文 Markdown，而不是像 Sydney 那样发独立 messageType。**不要照抄 Sydney 的 allowedMessageTypes 处理表**。

### 2.3 Progress 子形态（contentType 枚举）

| contentType | 条数 | text 样例 | 关键附加字段 |
|---|---|---|---|
| `EarlyProgress` | 18 | `"正在处理…"` `"正在深入分析…"` `"正在整理…"` | isExpanded=false, isPersisted, addToChainOfThought=false |
| `SearchResults` | 4 | `"好的，我将搜索 'today world news…'..."` / `"正在搜索..."` | **searchQueries: ["..."]** —— 搜索意图在此 |
| `image` | 8 | `"Loading image"` | contentGenerationProgressList[]，见 §4.2 |
| `GraphicArt` | 8 | （同上，完成态） | 同上 + ImageReferenceUrls |

Progress/EarlyProgress 样本（脱敏）：

```json
{"text":"正在处理…","isExpanded":false,"isPersisted":true,"addToChainOfThought":false,
 "contentType":"EarlyProgress","author":"bot","createdAt":"2026-08-05T09:55:24.9Z",
 "timestamp":"…","messageId":"<MESSAGE_ID>","offense":"Unknown","contentOrigin":"EarlyProgress"}
```

SearchResults 样本：

```json
{"text":"正在搜索...","isExpanded":false,"isPersisted":false,"addToChainOfThought":false,
 "contentType":"SearchResults","searchQueries":["AI news August 2026 OpenAI Anthropic Google"],
 "author":"bot","messageType":"Progress","messageId":"<MESSAGE_ID>","turnCount":2}
```

时序：EarlyProgress 在 type4 后 **~50ms** 到达，是最早的生命信号；SearchResults 在工具调用前发出。

### 2.4 Chat 缺省型消息（DeepLeo 快照）字段全集

468 条 DeepLeo 消息的字段出现率：

```
text/author/responseIdentifier/createdAt/timestamp/messageId/offense/
adaptiveCards/sourceAttributions/contentOrigin  100%
references        97% (dict, 可空)          spokenText   52%
scores            4.7% (仅 isLastUpdate/final 帧)   requestId/suggestedResponses  2.3%(收尾帧)
```

要点：
- `responseIdentifier:"Default"` 固定。
- `adaptiveCards[0].body[0].text` 与 `text` 内容一致（渲染副本）；cursor 路径正指向这里。
- `sourceAttributions` **恒为 []**（Sydney 的引用字段在 M365 已废弃，引用走 references dict，见 §4.1）。
- `offense`: user 消息常见 "None"/"Unknown"；异常值可作内容拦截信号（Go 已利用）。
- author=user 的消息（type2 回显）带 `entityAnnotationTypes:["People","File","Event","Email","TeamsMessage"]`、`inputMethod:"Keyboard"`、`turnCount:N`、`storageMessageId`。

### 2.5 target=Metrics（client→srv，遥测）

两种形态，均由浏览器发出（我们的实现模拟了它，服务端不校验也不依赖）：

```json
{"arguments":[{"Timestamps":{"ConnectionStart":"…","UserInputStart":"…",
  "ConnectionEstablished":"…","UserInputSubmit":"…","RequestSent":"…"}}],
 "target":"Metrics","type":1}
{"arguments":[{"Timestamps":{"FirstServiceResponseReceived":"…","FirstTokenReceived":"…",
  "LastTokenReceived":"…","SuggestionsReceived":"…"}}],"target":"Metrics","type":1}
```

har4 还发了量化版：`ReceivedTokenMetrics{TokenCount,CharCount,TimeBetweenTokens{Mean,Median,Sd,Variance,Max,Min},AverageChunkSize}` 和 `RenderedChunkMetrics{ChunkCount,ChunkRateStatistics,BurstRate,StallRate}`。纯遥测，可忽略。

---

## 3. ★ writeAtCursor vs snapshot 双通道（核心发现）

### 3.1 cursor 帧（每回合第一帧文本，建立游标锚点）

```json
{"type":1,"target":"update","arguments":[{
  "cursor":{"j":"$['<BOT_MESSAGE_ID>'].adaptiveCards[0].body[0].text","p":-1},
  "messages":[{"text":"你好","author":"bot","messageId":"<BOT_MESSAGE_ID>",
    "adaptiveCards":[{"type":"AdaptiveCard","version":"1.0",
      "body":[{"type":"TextBlock","text":"你好","wrap":true}]}],
    "contentOrigin":"DeepLeo"}],
  "nonce":"<NONCE_SAMPLE>","requestId":"<REQ_ID_A>"}]}
```

- `cursor.j` = JSONPath，**12 个会话全部**指向 `$['<botMessageId>'].adaptiveCards[0].body[0].text`。
- `cursor.p` = 写入偏移，**全部为 -1（末尾追加）**。未观察到 p>=0 的中间插入或改写。
- cursor 帧本身同时携带第一份快照（首 token）。

### 3.2 双通道语义：并行冗余流，不是互补增量

对 har4#246（222 帧）逐帧回放得到铁证（len 单调序列）：

```
[022] CURSOR(p=-1) + snapshot len=1   '#'
[023] DELTA " 全能力测试结"
[024] DELTA "简版）\n\n"
[025] snapshot len=18  '# 全能力测试结果（精简版）\n\n##'     ← 包含了上面两条 delta
[026] snapshot len=21 …                                        ← 继续单调增长
[044] DELTA "Google" + refs[1]
[048] snapshot len=251 …                                       ← 又包含
…
```

统计验证（13 个会话全量）：

| 会话 | snapshot 帧数 | delta 帧数 | 快照回退(shrink)次数 |
|---|---|---|---|
| har1#364 | 8 | 3 | 0 |
| har1#976 | 91 | 105 | 0 |
| har4#246 | 135 | 62 | 0 |
| 其余 10 会话 | 3~70 | 2~25 | 0 |
| **合计** | **468** | **274** | **0** |

**结论（直接决定 emitSnapshot/finalizeText 正确性）：**

1. `messages[].text`(DeepLeo) 是**累计全量快照**，严格前缀单调递增（0 次回退），每个 snapshot 都已包含此前所有 delta 的内容；
2. `writeAtCursor` 是**追加式 delta**，写入位置恒为文末（p=-1），与后续 snapshot 内容重叠；
3. 两通道是同一文本流的**冗余双写**（服务端对渲染层和文本层各推一份），**绝不能把两者线性拼接**——会把每段文字算两遍；
4. delta 可能被 snapshot 部分覆盖或完全跳过（snapshot 直接跳到更靠前的位置），**任何时刻以最新 snapshot 为准**；
5. `isLastUpdate:true` 帧 = 最终权威快照（附 scores 安全分）；type2 `item.result.message` = 最终兜底（二者内容一致）；
6. 尾部完整性：最后一个 delta 的内容必然出现在其后的某个 snapshot 中（replay 验证 ✓）。因此**只用 snapshot 通道不会丢字，只用 delta 通道才会丢**。

### 3.3 对 Go 实现（client.go emitSnapshot, :581）的验证意见

现实现的「prefix 匹配发尾差、非前缀且更长则丢弃并计数 skippedSnapshots、最终 finalizeText 用 type2 result.message 对账」策略与本协议实测行为**完全匹配**：
- delta "！" 在 cur="你好" 时因非前缀被 skip（len("！")<=len("你好") 分支），随后 snapshot "你好！很" 补齐——skip 无损；
- finalizeText 三分支（streamed≥final / final 为 streamed 前缀 / 分歧取 final）覆盖了 §3.2 的全部情形；
- **唯一理论缺口**：若连接在 isLastUpdate/type2 之前中断，已发送的 delta 尾部可能与真实文本有微小出入（delta 先于 snapshot 到达的窗口内）。当前 90s 读超时 + type2 兜底下概率极低。

---

## 4. 引用 / 图片 / 建议的出现时机与字段

### 4.1 references（引用）——注意挂载位置！

**主载体是 messages[].references，不是 update 参数级 references**（与直觉相反）：

| 挂载点 | 形态 | 实测规模 |
|---|---|---|
| update 参数级 `arg.references` | dict | 274 帧携带，其中 **265 帧为空 {}**，仅 10 个真实条目 |
| **DeepLeo 消息级 `msg.references`** | dict | **933 个条目**（随快照累积重复下发），单次 web 搜索回合去重后 6~7 条 |
| `msg.sourceAttributions` | array | **恒为 []，死字段** |

键格式：`"{label}-{8位hex}"`，如 `1-d07884`、`5-5b3f35`；另有 `uncite-{hex}` 键（提及但未正式引用的来源）。

值结构（脱敏）：

```json
{"targetLink":"https://example.com/article",
 "displayData":{"type":"text/json","renderType":"CITATION",
   "content":"{\"metadata\":{\"type\":\"Web\",\"referenceType\":0,\"referenceId\":\"turn2search2\","
            + "\"citationRefId\":\"turn2search2\",\"suppressCitation\":false,\"isExternal\":false},"
            + "\"label\":\"1\",\"providerDisplayName\":\"站点名…\",\"Title\":\"标题…\","
            + "\"snippet\":\"摘要…\",\"lastUpdatedDate\":\"2026-8-1 00:00:00\"}"}}
```

- `displayData.content` 是**转义 JSON 字符串**，需二次 parse 才能拿到 Title/snippet/label。
- metadata.type 实测只有 `"Web"`（本批 HAR 无内部文件引用回合；File 类引用预期也走此结构）。
- **出现时机**：首份 DeepLeo 快照即可能带空 dict；真实条目从第一次搜索完成后开始累积，大量条目搭写在 **writeAtCursor delta 帧**的参数级 references 上（对应 variants 中 `feature.enableDeltaStreamingForReferences`）；最终全集在 isLastUpdate 快照与 type2 里各有一份。
- `ReferencesListComplete` 标记帧（无 body）紧随最后一条引用到达，可作为 citations 收口信号。

### 4.2 图片生成（GraphicArt）生命周期

全部发生在 update 流内（无独立 channel），状态机：

```
t+4.9s   Progress(contentType=image, status=null, ImageReferenceUrls=[], pollUrl=P, fileToken=F)
t+26.4s  同一 message 更新: status=2, ImageReferenceUrls=[designer document.ashx URL]
t+27.6s  messageIdsToDelete=["<该Progress消息id>"]   ← 清理临时卡
t+30.8s  type2 最终 item（GraphicArt 消息入 messages）
```

`contentGenerationProgressList[0]` 字段：

```json
{"contentType":"image","size":"Xlimage","orientation":"Landscape",
 "pollUrl":"<base64>",           // 解码: {"PollId":"<POLL_ID>","Intent":0,"FileToken":"<REDACTED_TOKEN>","SubIntent":null,"Handled":true,"InteractionId":null}
 "fileToken":"<IMAGE_FILE_TOKEN>",
 "ImageReferenceUrls":["https://designerapp.officeapps.live.com/designerapp/document.ashx?path=%2F…%2FDallEGeneratedImages%2Fdalle-….png&dcHint=JapanEast&speCId=…&fileToken=…"],
 "status":2}                      // null→2(Done)；中间态(1=Generating)本批未见
```

注意：**没有 `imageUrl` / `imageLink` 字面字段**（那是 Sydney 形态）。Go images.go 的泛型 URL walk 能抓到 ImageReferenceUrls，兼容。

### 4.3 suggestedResponses（建议问题）——挂载位置同样反直觉

实测 **11 帧全部挂在 bot Chat 消息的字段上**，参数级/type2 item 级均为 0 次；另有独立 `messageType=Suggestion` 消息（69 条 contentOrigin=SuggestionsProviderService）。

- 时机：**isLastUpdate 之后 ~1.15s、completion 之前 ~0.6s**，单独一个 update 帧重新下发完整 bot 快照 + sugg[3]；
- 字段：`commandText == text`（点击即发送的原文）、`hiddenText:"DynamicTurnN"`、`suggestionCategory:"DynamicTurnN"`、`author:"user"`(!)、`messageType:"Suggestion"`、`contentOrigin:"SuggestionsProviderService"`。

```json
{"commandText":"你能讲个笑话吗？","text":"你能讲个笑话吗？","hiddenText":"DynamicTurnN",
 "suggestionCategory":"DynamicTurnN","author":"user","messageType":"Suggestion",
 "offense":"Unknown","contentOrigin":"SuggestionsProviderService","messageId":"<MSG_ID_SUGGESTION>"}
```

---

## 5. throttling 帧完整结构与限流预测

### 5.1 两个位置、两种粒度

**① update 参数级（回合早期，type4 后 ~300ms，12/12 回合都有）——仅计数器：**

```json
{"throttling":{"maxNumUserMessagesInConversation":600,
               "numUserMessagesInConversation":1,
               "numLongDocSummaryUserMessagesInConversation":0}}
```

**② type2 item.throttling（回合结束态，12/12）——计数器 + metering 配额表：**

```json
{"maxNumUserMessagesInConversation":600,
 "numUserMessagesInConversation":1,
 "numLongDocSummaryUserMessagesInConversation":0,
 "metering":{
   "LLMOnly":{"remainingAllowance":100},
   "ImageGeneration":{"remainingAllowance":100},
   "GraphicArt":{"remainingAllowance":0},
   "CodeInterpreter":{"remainingAllowance":0},
   "FileReference":{"remainingAllowance":3},
   "WXPAgentMode":{"remainingAllowance":7},
   "...":{"remainingAllowance":0}}}
```

### 5.2 metering 能力清单（两账号实测并集，26 项）

| 能力 | 账号A(har1, Starter/Dogfood) | 账号B(har4, Starter) | 推测含义 |
|---|---|---|---|
| LLMOnly | 100 | 100 | 纯 LLM 问答配额（核心额度） |
| ImageGeneration | 100 | 100 | 生图次数 |
| VisualCreator / GraphicArt | 0 | 0 | 视觉创作（AAD 账号被禁，与 02 号文档 ErrorDisallowedAADUser 互证） |
| FileReference | 3 | 3 | 文件引用 |
| WXPAgentMode | 7 | 7 | Agent 模式动作数 |
| ArtifactGeneration | —(无此键) | 3 | 产物生成 |
| ReasoningModelTurnUsage | — | 10 | 推理模型回合 |
| ClaudeOpusQuery | — | **100** | Claude Opus 直查总额 |
| ClaudeOpusQuery75 | — | 75 | 子池 |
| ClaudeOpusQueryDaily | — | 40 | 日配额 |
| ClaudeOpusQueryDev / HourlyDev | — | 5 / 2 | 开发灰度池 |
| ClaudeOpusQueryC1/C2/DailyC1/C2 | — | 0 | 备用池（未启用） |
| CodeInterpreter/DeepResearch/CopilotTuning/DeepWork/NotebookCowork/ImageAnalysis/TenantDataAccess/PersonalDataAccess/CostQuota | 0 | 0 | 未授权能力 |

⚠️ 任务书提到的 `conversations/precision/userSlots` 字段**在本协议中不存在**——那是 Bing Chat/Sydney 时代的 throttling 形态。M365 chathub 的真实形态如上。

### 5.3 如何提前预测限流

1. **metering.remainingAllowance==0 的能力项 = 该能力已被硬禁**（非临时限流）：GraphicArt/VisualCreator=0 直接预测"无法生成图像"类拒绝，不必发请求试错；这与 02 号文档的 Designer 403 指纹形成双保险预检。
2. **剩余额度是回合级下发**：每次对话结束的 type2 都刷新全表 → 2API 可缓存 per-account 配额水位，在 ImageGeneration/LLMOnly 接近 0 时主动降级/切换账号，而不是等文本通道的人话报错（rateLimited 启发式的根因即在此：上游不用 HTTP 429，而是把限流话术当正文发）。
3. numUserMessagesInConversation/maxNum(600) 只反映**当前会话**轮次，跨会话不限；600 上限对 2API 无实际约束（每轮新建 conversation）。
4. ClaudeOpusQuery 系列（账号B）证明**同一租户下不同账号的能力矩阵不同**，metering 表可作为账号能力指纹用于路由（选有 ClaudeOpusQuery 额度的账号跑 opus 类模型请求）。

---

## 6. 对照 internal/chathub/client.go：遗漏帧类型与字段清单

已正确处理（HAR 数据验证通过）：normalize 的 kind 分类（ping/update/result/error/complete）、emitSnapshot 前缀对账、writeAtCursor 追加、patches→spokenText、参数级 references、offense/scores、type2 的 storageMessageId/throttling/result.message 兜底、finalizeText 三分支、images.go 泛型 URL 抓取（覆盖 ImageReferenceUrls）、Metrics 回传。

**遗漏/偏差（按危害排序）：**

| # | 问题 | 证据 | 影响 |
|---|---|---|---|
| G1 | **conversationTransferToken 读错层级**：client.go:766 从 update 参数读，但 12/12 回合它只在 **type2 item** 上 | §2.1 表：参数级出现 0 次 | Result.ConversationTransferToken 恒为空；该 token（base64 `{"type":"FullConversation","conversationId":…}`）疑用于会话迁移/续接场景 |
| G2 | **suggestedResponses 读错层级**：client.go:772/:888 分别找参数级与 item 级，但 11/11 帧它挂在 **bot 消息字段** `msg.suggestedResponses` | §4.3 | 建议问题永远拿不到（除非走 Suggestion 独立消息路径——classifyUpdateMessages 目前把它们当普通 text 事件丢给 onEvent 前就被过滤，实际也不会进 suggestions） |
| G3 | **消息级 references 未解析**：client.go:812 只解析参数级（全数据集仅 10 条真实引用），**933 条引用都在 msg.references** | §4.1 | web 搜索回合的 citations 基本全丢；需在 DeepLeo 消息处理分支补 `m["references"]` 合并（按 key 去重累积） |
| G4 | ReferencesListComplete 标记帧未消费 | §4.1 | 可用作 citations 收口/提前 finalize 信号，丢失影响小 |
| G5 | type2 item 的 `defaultChatName`（会话标题=首问）、`firstNewMessageIndex`、`telemetry.startTime`、`result.serviceVersion` 未采集 | §2.4 样例 | defaultChatName 可直接做 OpenAI API 的会话标题/命名 |
| G6 | `messageIdsToDelete` 未处理 | §4.2 | 仅 UI 层清理逻辑，API 场景无损 |
| G7 | `cursor` 字段未读取（直接假设 append） | §3.1 | 当前 12/12 均 p=-1，假设成立；若未来出现 p>=0（中间改写）会静默出错，建议加防御日志 |
| G8 | `isLastUpdate` 未作为 finalize 前置信号（靠 type2 兜底） | §3.2 | 行为正确但多等 ~0.6s（suggestion 帧后才来 type2）；可用 isLastUpdate 提前收流 |
| G9 | searchQueries 已在 SemanticEvents 提取（events.go:22）✓ 但 contentType=="Code"/"ToolCall" 分支在本协议中**从未出现**，属死代码（无害） | §2.3 | — |

**修复优先级：G1/G2/G3 为 P1**（功能性数据丢失），G5 P2，其余 P3。

---

## 7. 附录：完整回合逐帧回放（har1#364，脱敏）

```
+   0.12s send ping
+   2.95s send type4 chat(invocationId=0, optionsSets×33, allowedMessageTypes×30)
+   3.26s recv update MSG Progress/EarlyProgress len=5 '正在处理…'
+   3.31s recv update THROTTLE msgs=1/600                    ← 计数器帧
+   3.86s recv update CURSOR(p=-1) + MSG DeepLeo len=2 '你好' cards[1]
+   3.89s recv update DELTA '！'
+   3.90s recv update MSG DeepLeo len=4 '你好！很' spoken[3]
+   3.95s recv update DELTA '兴见到你。'
+   3.95s recv update MSG DeepLeo len=13 '…有什么'
+   3.96s recv update DELTA '可以帮你的吗？'
+   3.97s recv update MSG DeepLeo len=22 '…🙂' spoken[11]
+   4.11s recv update isLAST MSG len=22 + scores(BotOffense 4.7e-13, dea_violation 5.6e-09)
+   4.11s recv update PATCH replace /<mid>/spokenText ← ""
+   5.26s recv update MSG DeepLeo len=22 + suggestedResponses[3]   ← 建议挂载
+   5.27s recv update MSG ReferencesListComplete
+   5.90s recv TYPE2 item{conversationId, conversationExpiryTime(+6h), conversationTransferToken,
                          defaultChatName='你好', firstNewMessageIndex=1, messages[user+bot],
                          requestId, result{Success,message=<全文>,serviceVersion}, telemetry,
                          throttling{600計数 + metering15项}, turnState:'Completed'}
+   5.90s recv TYPE3 {"invocationId":"0"}                    ← 空壳，流终止符
```

## 8. 附录：方法说明

- 解析：`_webSocketMessages[].data` 按 `\x1e` 切分为 SignalR record，逐条 json 解析；Chathub 会话以 URL 含 `/m365Copilot/Chathub/` 识别。
- 单调性验证：对每个 (session, messageId) 追踪 DeepLeo text 长度序列，13 会话 0 次回退。
- 双通道重叠验证：任一 delta 子串必出现在其后继 snapshot 中（replay 抽验 3 会话 ✓）。
- 中间产物 frames.jsonl 位于临时目录，未入库；本文数字均可由 har1-har6 复现。

---

## 落地清单

> 本报告对 client.go 流解析的修复项汇总（G1-G9），详细依据见上文 §6。

- [ ] **P1 G1** conversationTransferToken 从 type:2 item 层读取（update 参数级 12/12 回合为空）
- [ ] **P1 G2** suggestedResponses 从 bot 消息字段 `msg.suggestedResponses` 读取（11/11 帧挂在消息上）
- [ ] **P1 G3** 解析 DeepLeo 消息级 msg.references 并按 key 去重累积（933 条真实引用都在这里，参数级仅 10 条）
- [ ] **P3 G4** ReferencesListComplete 标记帧用作 citations 收口/提前 finalize 信号
- [ ] **P2 G5** 采集 type2 item 的 defaultChatName（可作会话标题）/firstNewMessageIndex/telemetry.startTime/result.serviceVersion
- [ ] **P3 G6** messageIdsToDelete 处理（仅 UI 清理语义，API 场景无损）
- [ ] **P3 G7** cursor.p 非 -1 时加防御日志（当前"恒为文末追加"假设 12/12 成立）
- [ ] **P3 G8** isLastUpdate:true 作为 finalize 前置信号提前收流（省 ~0.6s）
- [x] 已验证无需改动：emitSnapshot 前缀对账 / finalizeText 三分支 / writeAtCursor 追加 / patches→spokenText / Metrics 回传 / images.go 泛型 URL 抓取
