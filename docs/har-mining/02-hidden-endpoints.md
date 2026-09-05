# HAR 逆向报告 02：substrate.office.com 与 graph.microsoft.com 全业务端点

> **本文档回答什么问题**：除聊天主链路外，M365 Copilot 前端还调用了哪些业务端点——substrate/graph/designer 三域的全量端点枚举、图片生成完整调用链与节流信号、鉴权体系（JWT scope/fileToken/x-anchormailbox），以及每项发现对 2API 项目的价值分级与落地建议。
>
> 数据源：本地抓包文件 har1.har ~ har6.har（6 个文件，共约 2225 个 entry）
> 分析方式：Python 全量解析 HAR（HTTP entries + `_webSocketMessages` WebSocket 帧逐帧解析）
> 账号样本：
> - 账号 A（har1/har2/har3）：`user-a@example.com`，OID `<USER_A_OID>`，租户 `<TENANT_ID>`，licenseType=**Starter**，ring=Dogfood
> - 账号 B（har4/har5/har6）：`user-b@example.com`，OID `<USER_B_OID>`，同租户
>
> 说明：任务清单中提到的 `dataservice.o365filtering` 在本批 6 个 HAR 中**零出现**；`m365Copilot/UploadFile` 独立端点亦未出现（上传走 `graph.microsoft.com/v1.0/me/drive/special/copilotuploads` + OneDrive 链路，见 §5.2）。

---

## 目录

1. [substrate.office.com/m365Copilot/* 端点全量枚举](#1)
2. [substrate.office.com/search/api/v1/*](#2)
3. [substrate.office.com/puds/v1/*](#3)
4. [graph.microsoft.com/v1.0/*](#4)
5. [designerapp.officeapps.live.com/designerapp/*（图片生成链路核心）](#5)
6. [图片生成完整调用链与 throttling 信号（重点深挖）](#6)
7. [m365.cloud.microsoft 服务端函数（_serverFn / POST /chat action）](#7)
8. [鉴权体系还原（JWT scope / fileToken / x-anchormailbox）](#8)
9. [价值分级总表 P0/P1/P2](#9)
10. [对 M365-Copilot2API 的落地建议](#10)

---

<a id="1"></a>
## 1. substrate.office.com/m365Copilot/* 端点全量枚举

### 1.1 `wss://substrate.office.com/m365Copilot/Chathub/{oid}@{tid}` — 聊天主链路（P0）

- **方法**: GET (WebSocket 升级, HTTP 101)
- **出现位置**: har1#364/#590/#650/#789/#873/#976, har4#4/#163/#194/#201/#211/#246/#546
- **完整 URL 参数**:
  ```
  wss://substrate.office.com/m365Copilot/Chathub/{oid}@{tenantId}
    ?chatsessionid={hex32}
    &XRoutingParameterSessionKey={同chatsessionid}
    &clientrequestid={uuid}
    &X-SessionId={uuid}
    &ConversationId={uuid}            // 二轮起携带
    &access_token=<JWT>               // aud=substrate.office.com/sydney
    &variants=<逗号分隔 flight 列表>   // 见下方金矿
    &source=%22officeweb%22
    &product=Office
    &agentHost=Bizchat.FullScreen
    &licenseType=Starter              // ★ 账号许可等级明文
    &isEdu=false
    &agent=web
    &scenario=OfficeWebIncludedCopilot
  ```
- **协议**: Microsoft SignalR JSON（handshake `{"protocol":"json","version":1}` → `{}`, ping/pong 为 `{"type":6}`）
- **请求体结构**（invoke 帧, type:1）:
  ```json
  {"arguments":[{
    "source":"officeweb",
    "clientCorrelationId":"<hex32>",
    "sessionId":"<hex32>",
    "optionsSets":[ /* 见下 */ ],
    "streamingMode":"ConciseWithPadding",
    "allowedMessageTypes":["Chat","Suggestion","InternalSearchQuery","Disengaged",
      "InternalLoaderMessage","Progress","GeneratedCode","RenderCardRequest","AdsQuery",
      "SemanticSerp","GenerateContentQuery","GenerateGraphicArt","SearchQuery",
      "ConfirmationCard","AuthError","DeveloperLogs","TriggerPlugin","HintInvocation",
      "MemoryUpdate","EndOfRequest","TriggerConfirmation","ResumeInvokeAction",
      "ResumeUserInputRequest","TriggerUserInputRequest","EscapeHatch","TriggerPluginAuth",
      "ResumePluginAuth","SideBySide","ReferencesListComplete","SwitchRespondingEndpoint"],
    "threadLevelGptId":{},
    "traceId":"<hex32>",
    "isStartOfSession":false,
    "clientInfo":{"clientPlatform":"mcmcopilot-web","clientAppName":"Office",
      "clientEntrypoint":"mcmcopilot-officeweb","ProductCategory":"Chat",
      "clientAppType":"Web","productEntryPoint":"ChatPanel","deviceOS":"Windows"},
    "message":{
      "author":"user","inputMethod":"Keyboard","text":"你好",
      "entityAnnotationTypes":["People","File","Event","Email","TeamsMessage"],
      "requestId":"<hex32>",
      "locationInfo":{"timeZoneOffset":8,"timeZone":"Asia/Shanghai"},
      "locale":"zh-cn","messageType":"Chat","experienceType":"Default",
      "connectedFederatedConnections":["dummyId"]},
    "extraExtensionParameters":{},"options":{}
  }],"target":"chat","type":1}
  ```
- **响应帧结构**（type:1 target:"update" 流式 + type:2 完成帧）:
  - Progress 帧：`messages[].messageType="Progress"`、`contentType:"EarlyProgress"`
  - **throttling 字段（每轮必发）**:
    ```json
    "throttling":{"maxNumUserMessagesInConversation":600,
                  "numUserMessagesInConversation":3,
                  "numLongDocSummaryUserMessagesInConversation":0}
    ```
    （har1#364 msg、har4#194 msg6 等，单会话上限 **600 条**）
  - 增量文本：`writeAtCursor` 游标写入 + `patches[]`(JSON-Patch op:replace path:`/{messageId}/spokenText`)
  - 完成：`conversationTransferToken`(base64 → `{"type":"FullConversation","conversationId":"..."}`)、
    `suggestedResponses[]`、`scores[{component:"BotOffense"|"dea_violation",score}]`
  - 客户端回执：`{"arguments":[{"Timestamps":{"RequestSent":..,"FirstTokenReceived":..}}],"target":"Metrics","type":1}`
- **optionsSets 关键 flight**（har1#364 SEND）:
  `cwc_flux_image, cwc_code_interpreter, cwcfluxgptv, flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch,
  gptvnorm2048, cwc_fileupload_odb, update_memory_plugin, add_custom_instructions, cwc_flux_v3,
  flux_v3_progress_messages, enable_batch_token_processing, enable_gg_gpt, flux_v3_references(_entities/_ci),
  add_filestore_filetype, flux_v3_image_gen_enable_dimensions/non_watermarked_storage/icon_dimensions/
  system_text_with_params/designer_dimensions_meta_prompting_in_system_prompts/story,
  code_interpreter_interactive_charts, rich_responses`
- **URL variants 金矿**（har1#364，服务端能力开关全集）:
  ```
  EnableMcpServerWidgets, feature.EnableImageGenInsufficientTokensThrottled,     // ★ 图片生成节流①
  feature.EnableImageGenSystemCapacityThrottled,                                 // ★ 图片生成节流②
  feature.EnableLuForChatCIQ, feature.enableChatCIQPlugin, EnableRequestPlugins,
  feature.EnableSensitivityLabels, EnableUnsupportedUrlDetector,
  feature.IsCustomEngineCopilotEnabled, feature.bizchatfluxv3, feature.enablechatpages,
  feature.enableCodeCanvas, feature.turnOnDARecommendation, IncludeSourceAttributionsConcise,
  feature.EnableDeduplicatingSourceAttributions, feature.IsCitationsReferencesOutputEnabled,
  feature.enableDeltaStreamingForReferences, feature.enablereferencesforagents,
  feature.EnableCodeInterpreterConversion, Enable3PActionProgressMessages,
  feature.enableClientWebRtc, feature.EnableMeetingRecapOfSeriesMeetingWithCiq,
  feature.StorageMessageSplitDisabled, SingletonEnvOn, cdxenablefccinmainline,
  EnableComposeWidget, -agt_researcheragent_enableMemoryRead,   // 负号 = 显式关闭
  feature.cwcallowedos, feature.EnableMergingPureDeltas, feature.disabledisallowedmsgs,
  feature.enableGenerateGraphicArtOptionsSet, cdximagen,
  feature.EnableClientFileURLSupportForOfficeWebPaidCopilot,     // ★ Paid Copilot 文件 URL 能力探测
  feature.EnableDesignEditorImageGrounding, feature.EnableDesignerEditor,
  feature.EnablePersonalization, feature.EnableConversationShareApis,
  feature.EnableContentApiandDocTypeHtmlInRichAnswers,
  feature.OfficeWebToHelix, ...Agt_bizchat_enableGpt5ForHelix    // ★ GPT-5 for Helix 开关
  ```

### 1.2 `/m365Copilot/CustomInstructions?variants=feature.EnablePersonalization` — 自定义指令 CRUD（P0）

| 操作 | 方法 | har 引用 | 状态码 |
|---|---|---|---|
| 列表 | GET | har6#23/#27/#30/#32/#35 | 200 |
| 创建 | POST | har6#29 | 200 |
| 删除 | DELETE `/{base64ItemId}` | har6#33 | 204 |

- **POST 请求体**:
  ```json
  {"instruction":"You are an AI assistant accessed via an API...Today's Yap score is: {Yap-Score}.",
   "userAssignedName":"Global Instruction",
   "useCase":"GenericChat"}
  ```
- **POST 响应**:
  ```json
  {"id":"<MAILBOX_ITEM_ID>",  // mailbox GUID 主键
   "conversationId":"<SERVER_CORRELATION_ID>",
   "requestId":"<CLIENT_REQUEST_ID_HEX>",
   "result":{"value":"Success","serviceVersion":"1.0.03520.49037"}}
  ```
- **GET 响应**:
  ```json
  {"instructions":[{"id":"<MAILBOX_ITEM_ID>","instruction":"...","displayName":"",
    "useCase":"Copilot Custom Instruction"}],
   "result":{"value":"Success","renewCert":false,"serviceVersion":"1.0.03520.49037"}}
  ```
- **DELETE**: URL = `CustomInstructions/{id}?variants=feature.EnablePersonalization`，204 空 body。
- 头部必须带 `x-anchormailbox: Oid:{oid}@{tid}` + Bearer token。
- 注：POST 的 instruction 内容即注入到系统提示的原文（样例中含 Yap score / channels 等 prompt 模板片段，说明该字段为自由文本直通 LLM system prompt）。

### 1.3 `/m365Copilot/PersonalizationUserFlags?variants=feature.EnablePersonalization` — 记忆开关（P0）

- **GET** (har6#8) 响应：
  ```json
  {"isMemoryEnabled":false,"isCustomInstructionEnabled":true,
   "isPersonalizationEnabledByTenant":true,
   "isInsightsFromConversationHistoryEnabled":false,
   "isM365GraphContentEnabled":false,
   "result":{"value":"Success","renewCert":false}}
  ```
  ★ 五个布尔位 = 租户级 + 用户级个性化能力的完整探测面。
- **POST**（部分更新语义，只传要改的字段）:
  - har6#10: `{"isMemoryEnabled":true}` → 200 `{"result":{"value":"Success","message":"Successfully updated personalization user flags."}}`
  - har6#21: `{"isCustomInstructionEnabled":false}` → 200 同上
- 等价前端封装：`POST https://m365.cloud.microsoft/chat` body `{"action":"GetPersonalizationUserFlags"}`（har1#122，返回相同结构，包在 `store.personalizationFlags` 下）。

### 1.4 `/m365Copilot/EventListener/Client?EventId=ExecuteAction` — 连接器/MCP 枚举（P0）

- **方法**: POST（另有一次 OPTIONS 204 预检，har1#245）
- **请求体** (har1#240):
  ```json
  {"ConversationId":"",
   "clientData":{"Verb":"list_connectors","Data":{"skipFunctions":true},
                 "MessageId":"","VisualElementId":""}}
  ```
- **响应**（截取）:
  ```json
  {"data":{"messages":[{"status":"Success","executeActionResult":[
    {"id":"AiwynTaxMCP01","name":"Aiwyn Tax","description":"Aiwyn Tax","icon":"...",
     "pluginType":"FederatedConnector","connectionState":"None",
     "auth":{"type":"OAuthPluginVault","referenceId":"<VAULT_REFERENCE>"},   // base64 vault 引用
     "additionalMetadata":{"dataSourceName":"Aiwyn Tax",
       "connectionIds":["AiwynTaxMCP01"],
       "providerIds":["<CONNECTOR_PROVIDER_ID>"],
       "isInstalled":true,
       "federatedConnectionIds":["AiwynTaxMCP01"],
       "federatedProviderIds":["<CONNECTOR_PROVIDER_ID>"]}},
    {"id":"ArticleGalaxyMCP01",...},{"id":"AutodeskMCP01",...},
    {"id":"BitlyMCP01",...},{"id":"BlockscoutMCP01",...},{"id":"BoardwiseMCP01",...},
    {"id":"CanvaMCP01",...} /* 共数十个 MCP/Federated Connector */]}]}}
  ```
- ★ 该 Verb 可无对话上下文直接调用（`ConversationId:""`），返回租户全部已安装 Federated Connector（MCP server）清单及其 OAuth vault 引用 —— 即"该账号能挂哪些外部工具"的完整探测。

### 1.5 未出现但确认存在的 m365Copilot 子路径

从 Chathub flights 推断存在但本批 HAR 未抓到的端点（供后续抓包目标）：
- `UploadFile`（对应 flight `cwc_fileupload_odb`/`add_filestore_filetype`）
- Conversation Share API（flight `feature.EnableConversationShareApis`）
- Memory Update 插件回调（allowedMessageTypes 含 `MemoryUpdate`）

---

<a id="2"></a>
## 2. substrate.office.com/search/api/v1/*

### 2.1 `POST /search/api/v1/userconfig` — 搜索内容源配置（P2）

- 出现：har1#236, har3#222(+OPTIONS#223), har6#3
- 请求体：
  ```json
  {"RequestedConfigTypes":["ContentSources"],
   "Scenario":{"Name":"officeweb"},
   "TextDecorations":"Off","UICulture":"zh-cn"}
  ```
- 响应：
  ```json
  {"ContentSourcesConfigResponse":{"Sources":[
    {"ContentSource":["SharePoint","OneDriveBusiness"],
     "SourceDisplayName":"SharePoint - OneDrive",
     "IconUrl":"https://res.cdn.office.net/.../sposite.svg",
     "EntityType":"File"}]},
   "Instrumentation":{"TraceId":"<SEARCH_TRACE_ID>"}}
  ```
- 价值：探测账号可搜索的 Graph 内容源（企业账号返回 SharePoint/OneDrive；纯 MSA 大概率空）→ 弱账号画像信号。

### 2.2 `POST /search/api/v1/suggestions?setflight=disablePromptRerankPerClientLayout,EnableMtgSeriesSugg` — 输入联想（P1）

- 出现：har1 共 25 次（#361/#363/#365/#372/#373/#872/#887...），部分 status=0（客户端取消）；har4#1/#3 status=0
- 请求体（har1#363）：
  ```json
  {"Scenario":{"Name":"Harmony.Web.Copilot_QF",
    "Dimensions":[{"DimensionName":"Canvas","DimensionValue":"Default"},
                  {"DimensionName":"UserLicenseType","DimensionValue":"Starter"},   // ★ 许可维度
                  {"DimensionName":"UserExperience","DimensionValue":"Web"}]},
   "EntityRequests":[
     {"EntityType":"Prompt","Query":{"QueryString":"nh"},"Size":10,
      "PropertySet":"ProvenanceOptimized","Provenances":["Exchange"]},          // ★ 历史提示词联想
     {"EntityType":"People","Query":{"QueryString":"nh"},"Size":10,
      "Filter":{"And":[{"Or":[{"Term":{"PeopleType":"Person"}},
                              {"Term":{"PeopleType":"Other"}}]},
                       {"Or":[{"Term":{"PeopleSubtype":"OrganizationUser"}},
                             {"Term":{"PeopleSubtype":"Guest"}},
                             {"Term":{"PeopleSubtype":"OrganizationContact"}}]}]},
      "Provenances":["Mailbox","Directory"],"ServeNoEmailContacts":true,"From":0},
     {"EntityType":"File","Query":{"QueryString":"nh"},"Size":10},
     {"EntityType":"Event","Query":{"QueryString":"nh"},"Size":10}],
   "SearchContext":{"InputContext":[{"Name":"CopilotConversationId","Value":""},
                                    {"Name":"TurnCount","Value":"0"}]},
   "TextDecorations":"Forward","TimeZone":"UTC",
   "LogicalId":"<LOGICAL_ID>","Cvid":"<CVID>"}
  ```
- 响应（空结果时）：`{"Groups":[],"LayoutHints":{"BestMatchSuggestions":[]},"Instrumentation":{"TraceId":"..."}}`
- 价值：
  - `EntityType=Prompt` + `Provenances=["Exchange"]` = 从邮箱历史提取用户高频提示词 → 可做"记忆挖掘"
  - People/File/Event 三类实体搜索 = Graph 人员/文件/日历模糊查询接口（无需构造复杂 Graph query）
  - 请求头需 `x-anchormailbox`

---

<a id="3"></a>
## 3. substrate.office.com/puds/v1/*

### 3.1 `PATCH /puds/v1/me/settings/copilot` — Copilot Web 设置（P1）

- 出现：har6#14/#17，均 204 No Content
- 请求体：
  ```json
  {"isWebSearchInWebEnabled":false}   // har6#14 关闭 web 搜索
  {"isWebSearchInWebEnabled":true}    // har6#17 重新开启
  ```
- 无响应体。头部同样要求 `x-anchormailbox` + Bearer。
- PUDS = Personalization User Data Store。此键控制聊天是否联网搜索；对 2API 项目可做"每会话粒度联网开关"，也可作为设置项读写的通用模板（推测同路径下存在其他 settings 键，如 conversation history 等）。
- 注意：GET 方向未抓到，需要后续验证 `GET /puds/v1/me/settings/copilot` 是否可直接读。

---

<a id="4"></a>
## 4. graph.microsoft.com/v1.0/*

### 4.1 `GET /me/informationProtection/sensitivityLabels` — 敏感度标签（P1）

- 出现：har1#235, har2#90, har3#49 均 200
- 响应：`{"@odata.context":"...users('<USER_A_OID>')/informationProtection/sensitivityLabels","value":[]}`
- 价值：账号合规能力探测；有标签租户返回非空数组。聊天上传文件前前端用它决定打标签 UI。

### 4.2 `GET /me/drive/special/copilotuploads` — Copilot 上传专用目录（P0★）

- 出现：har1#238 (GET 403), har3#194 (GET 403) + OPTIONS#195
- 403 响应体（错误指纹）：
  ```json
  {"error":{"code":"notAllowed",
    "innerError":{"code":"provisioningNotAllowed"},
    "message":"You do not have access to create this personal site or you do not have a valid license"}}
  ```
- 价值极高：
  - 这是 M365 Copilot **文件上传链路的第一跳**（flights `cwc_fileupload_odb`）。正常付费/开通账号应 200 并返回 driveItem；
  - `provisioningNotAllowed` = OneDrive 个人站未 provision → **精确区分"无 OneDrive 个人站"与"无 Copilot 许可"两种不可用原因**；
  - 对 2API 项目：可用作零成本账号能力预检（比发起一次真实聊天便宜得多）。

### 4.3 `GET /me/licenseDetails` — 许证明细（P0★）

- 出现：har3#216 (+OPTIONS#219)，200
- 响应关键字段（截取）：
  ```json
  {"value":[{"skuPartNumber":"STANDARDPACK",
    "servicePlans":[
      {"servicePlanName":"MDOLITE_ENTERPRISE","provisioningStatus":"Success"},
      {"servicePlanName":"MICROSOFT_TEAMS_EVENTS","provisioningStatus":"Success"},
      {"servicePlanName":"Bing_Chat_Enterprise","provisioningStatus":"Disabled"},
      ...
      {"servicePlanName":"GRAPH_CONNECTORS_SEARCH_INDEX","provisioningStatus":"Disabled"}]}]}
  ```
- 价值：`servicePlans` 中检索 `MICOPT`/`SPZA_IW`/`Bing_Chat_Enterprise`/`Microsoft_365_Copilot` 相关 plan 的 provisioningStatus，即可判定 Copilot 授权状态与功能子集。样本账号无任何 Copilot SKU 却能用 Starter 版聊天（licenseType=Starter 由 Chathub URL 上报），说明 **Starter 免费额度不依赖 licenseDetails 中的显式 SKU**——判定逻辑要以 Chathub throttling 实测为准。

### 4.4 `GET /me/photos/96x96/$value` — 头像（P2）

- har1#233(404), har2#89/#91(404), har3#47/#48(404)。无头像时 404。仅 UI 用途。

---

<a id="5"></a>
## 5. designerapp.officeapps.live.com/designerapp/*（Designer/图片生成域）

宿主页：`GET https://designer.svc.cloud.microsoft/editor?designerhost=&clientName=CWC&hostAppName=officeweb&lng=zh-cn&correlationId={uuid}&hostChannel=WorldWide&iframeid={uuid}`（har4#273，200 HTML，内嵌 CSP nonce）

### 5.1 `POST /designerapp/designai.ashx` — DesignAI 多动作网关（P0★）

multipart/mixed 协议（boundary=`DesignAI-Boundary-Outer`），一个请求可携带多个 JSON part。
- 出现：har5#13 (200)
- 请求 part 示例（Action=GetOCR）：
  ```json
  {"Action":"GetOCR","Scenario":"Posters",
   "Inputs":[{"Image":{"Url":"https://designerapp.officeapps.live.com/designerapp/document.ashx?path=%2F{oid}%2FSAM%2F0251614804959094137600.jpg&dcHint=WestUS2&fileToken=<REDACTED_TOKEN>"}}],
   "Hints":{},
   "Expectations":{"IncludeDesignAnalysisResult":false,"BatchSize":1,"OCRMinWordConfidence":0.75}}
  ```
- 响应（multipart 回包）：
  ```json
  {"OCRResults":[{"Polygon":["27, 35","603, 35","603, 65","27, 65"],"Text":"1. 今日AI行业5大國要新闻..."}, ...]}
  ```
- 价值：免费 OCR 引擎（带坐标多边形输出）。已知 Action：`GetOCR`；从 JS chunk 推断还有 design generation 类 Action（场景 `Posters`）。

### 5.2 `POST /designerapp/sam.ashx?Action=SAM` — 图像 embedding/分割（P1）

- 出现：har5#15 (200)
- 请求体：
  ```json
  {"Content":"https://designerapp.../document.ashx?path=...&fileToken=<REDACTED_TOKEN>",
   "ContentType":"Url","EnableFileCacheUrl":"false",
   "EnableObjectDetection":"true","EnableGZipCompression":"true",
   "EnablePreprocessing":true,"Version":"v2",
   "ODCoef":0.5,"ODNMSThreshold":0.6,"ODTopK":100,"EmbeddingPrecision":"fp32"}
  ```
- 响应：`{"Embedding":"k05VTVBZAQB2AHsnZGVzY3InOiAnPGY0JywgJ2ZvcnRyYW5fb3JkZXInOiBGYWxzZSwgJ3NoYXBlJzogKDEsIDI1NiwgNjQsIDY0KSwgfSA..."}`（base64 → numpy magic `\x93NUMPY`，descr `<f4`，shape `(1,256,64,64)` —— SAM 图像编码器特征图）
- 价值：图像理解向量化管线；对 2API 项目可复用为图像 embedding 服务（需 Designer 权限）。

### 5.3 `POST /designerapp/mediaprocessor.ashx` — 媒体处理动作网关（P0★）

- 出现：har5#3 (200)，OPTIONS#4
- 请求体（ImageSuperResolution）：
  ```json
  {"ActionType":"ImageSuperResolution",
   "ActionPayload":{"ImageURL":"https://designerapp.../document.ashx?path=%2F{oid}%2FDocumentCache%2Fdesign-{uuid}%2Fmedia%2Fasset-{uuid}.png&dcHint=WestUS2&fileToken=<REDACTED_TOKEN>",
                    "MetaData":{"ScalingFactor":2}}}
  ```
- 响应：
  ```json
  {"ActionType":"ImageSuperResolution",
   "ActionResponse":{"MOBDOV":null,
     "ImageSuperResolution":{"ImageUrl":"https://designerapp.../document.ashx?path=%2F{oid}%2FSAM%2F0251614804959094137600.jpg&dcHint=WestUS2&fileToken=<REDACTED_TOKEN>"},
     "ImageAutoAdjust":null,"ObjectSuggestions":null,"NormalMap":null}}
  ```
- 已知 ActionType：`ImageSuperResolution`；响应槽位暗示还有 `ImageAutoAdjust`、`ObjectSuggestions`、`MOBDOV`(背景去除类?)、`NormalMap` 四个未观测动作。
- ★ 免费超分辨率 ×2（ScalingFactor 参数可调），输出落到 `/SAM/` 目录并签发新 fileToken。

### 5.4 `POST /designerapp/Export.ashx?forceUseDRS=true` — 设计导出（P0★）

- 出现：har5#52/#53/#56/#59 (200)，OPTIONS#54/#55/#57
- 请求：`Content-Type: application/octet-stream`，body 为 protobuf 片段（可见字符串 `+design-{uuid}\timage/png(\x0108@H`），70 字节定长小包
- 响应：
  ```json
  {"inferredArtifactType":null,"WebUrl":null,"crop":null,
   "url":"https://designerapp.officeapps.live.com/designerapp/Media.ashx/?id=<MEDIA_ID>.png&fileToken=<REDACTED_TOKEN>&dcHint=WestUS2",
   "manifestStoreJson":null,"progress":0,"exportId":null}
  ```
- fileToken base64 解码：`{"TokenPrefix":"<TOKEN_PREFIX>","UserObjectId":"<USER_B_OID>","ClientName":"CWC"}`
- DRS = Document Rendering Service。forceUseDRS=true 走云端渲染而非本地 canvas。

### 5.5 `GET /designerapp/Media.ashx/?id={uuid}.{ext}&dcHint=WestUS2[&fileToken=...]` — 成品媒体下载（P1）

- 出现：har5#61/#64/#66/#68 (200, image/png|jpeg)；Export 响应中的 url 直接指向此处
- dcHint 为数据中心提示路由（WestUS2/JapanEast 两处均见）

### 5.6 `GET /designerapp/document.ashx?path=%2F{oid}%2F{Dir}%2F{file}[&dcHint=&speCId=&speType=&speIdx=&fileToken=]` — 文档/缓存读取（P0★）

- 目录类型实测：
  - `%2F{folderGuid}%2FDallEGeneratedImages%2Fdalle-{uuid}...png`（DALL·E 产物，har4#175~#216 多次 200；dcHint=JapanEast）
  - `%2F{folderGuid}%2FDallEGeneratedImages%2Fdalle-<GENERATED_IMAGE>.png`（har4#489~#550）
  - `%2F{oid}%2FSAM%2F0251614804959094137600.jpg`（mediaprocessor 输出，har5#8）
  - `%2F{oid}%2FDocumentCache%2Fdesign-{uuid}%2Fmedia%2Fasset-{uuid}.png`（编辑器文档缓存，mediaprocessor 输入）
- **鉴权双轨**（har5 实证）：
  - #8：无 fileToken，仅 cookie → **200**
  - #12/#14/#16/#18/#23：带过期 fileToken（TokenPrefix=<TOKEN_PREFIX>）→ **401**
  - 结论：cookie 会话优先；fileToken 为短时效签名（TokenPrefix+UserObjectId+ClientName 三元组），跨会话失效
- speCId/speType/speIdx 参数 = SharePoint 内容寻址（SkipRehydrationForSpeCIdImages flight 呼应）

### 5.7 `GET /designerapp/Account.ashx?action=ProfileInfo` / `?action=GetStorageInfo` — 账号信息（P0★）

- ProfileInfo（har4#354，200）：
  ```json
  {"DisplayName":"UserB","UPN":"user-b@example.com",
   "AgeGroup":"Undefined","CompanyName":"","JobTitle":""}
  ```
- GetStorageInfo（har4#452/#491，har5#19/#114/#122/#124，全部 **403**）
- ★ har5#22 RemoteUls 日志泄露 403 错误码原文：
  ```
  Account.ashx info. Status code 403, X-ErrorCode: ErrorDisallowedAADUser,
  Axios error code: ERR_BAD_REQUEST
  ```
- 价值：**ErrorDisallowedAADUser = Designer 拒绝 AAD 组织账号**的错误指纹。个人 MSA 才有完整 Designer 存储。这就是 har5"限制场景"的真实根因——不是配额限流，是身份类别被拒。2API 项目可用它做 Designer 能力预检。

### 5.8 其他 designerapp 端点

| 端点 | 方法 | har | 说明 | 等级 |
|---|---|---|---|---|
| `/designerapp/CreateKit.ashx?includesharedkits=true` | POST | har4#446 (200 `[]`) | 请求 `{"IncludeKitDefinition":false,"includesharedkits":true,"AppContext":{"Name":"Designer","SupportedSchemaVersions":"1.0.0"}}`，返回 Kit 模板列表（该账号为空） | P2 |
| `/designerapp/RemoteUls.ashx?usid={sid}&HostApp=CWC&Platform=Web&ReleaseChannel=` | POST | har4×15, har5×7 (200) | 批量 ULS 日志上报，body `{"G":..,"T":..,"M":"msg","C":383,"I":n,"D":50}` 数组；**日志中包含各上游请求的状态码与错误码**（旁路情报源） | P2 |
| `tracerequest.designerapp.../TraceRequest.ashx` | POST | har4#435, har5#49/#70/#106 | 场景遥测：`sessionInfo{userId,tenantId,buildVersion,docId,endpoint:"MiniApp-UnifiedEditor"...}` + `requests[{scenarioName:"ActiveUsage"/"CoreUsage",stopReason:"DownloadArtifact",metadata:{aiActions:2,...}}]` | P2 |
| `rtc.designerapp.../RealTimeChannel.ashx?sessionId=&cid=` | WS(101) | har4#477/#530 | 编辑器实时协作通道 | P2 |

---

<a id="6"></a>
## 6. 图片生成完整调用链与 throttling 信号（重点深挖）

### 6.1 聊天内生图链路（har4 实录，Chathub WebSocket 内完成）

```
用户消息("生成一张图片")
  └─① Chathub invoke 帧: messageType=Chat, allowedMessageTypes 含 GenerateGraphicArt,
      optionsSets 含 cwc_flux_image/cwc_flux_v3/flux_v3_*  (har4#246 msg3 SEND, len=4030)
  └─② RECV Progress: contentGenerationProgressList=[{
        "contentType":"image","size":"Xlimage","orientation":"Portrait",
        "pollUrl":"<base64>",           // 轮询凭据，非 HTTP URL！
        "fileToken":"<IMAGE_FILE_TOKEN>",
        "ImageReferenceUrls":[]}]       // msg7, har4#246
  └─③ RECV 轮询更新帧(msg18/msg19): ImageReferenceUrls 填充 →
        https://designerapp.officeapps.live.com/designerapp/document.ashx
          ?path=%2F{genFolderGuid}%2FDallEGeneratedImages%2Fdalle-<GENERATED_IMAGE>.png
          &dcHint=JapanEast&speCId={uuid}&speType=Image&speIdx=0&fileToken=AAD-{uuid}
  └─④ 浏览器 GET document.ashx 拉图（har4#489~#550, 200 image/png）
  └─⑤ type:2 完成帧: adaptiveCards 内含 GenerateGraphicArt 卡片 + invocation 函数名
```

**pollUrl 解码**（har4#163 msg6，base64 直接解）：
```json
{"PollId":"<POLL_ID>",
 "Intent":0,
 "FileToken":"<REDACTED_TOKEN>",
 "SubIntent":null,"Handled":true,"InteractionId":null}
```
内层 FileToken 再解：`{"TokenPrefix":"AAD-<TOKEN_PREFIX>","UserObjectId":"<USER_B_OID>","ClientName":"CopilotSydney"}`
→ 三层嵌套凭据（pollUrl ⊃ FileToken ⊃ TokenPrefix），ClientName 区分签发方（CopilotSydney / CWC）。

生成参数由模型侧决定并通过 `invocation` 字段透传（msg7 中 `"invocation":"[\"{\\\"function\\\":{\\\"arguments\\\":\\\"{\\\\\\\"or..."`，即 orientation/prompt 的 function-call JSON）。

### 6.2 Throttling 信号汇总

| 信号 | 位置 | 内容 | 触发含义 |
|---|---|---|---|
| 会话轮次计数 | Chathub 每轮 RECV | `throttling:{maxNumUserMessagesInConversation:600,numUserMessagesInConversation:N}` | 单会话 600 条硬上限；N 递增可做配额仪表盘 |
| 图片生成节流 flight① | Chathub URL `variants` | `feature.EnableImageGenInsufficientTokensThrottled` | 按消费 token 的图片配额不足型节流（服务端将返回专属错误类型） |
| 图片生成节流 flight② | Chathub URL `variants` | `feature.EnableImageGenSystemCapacityThrottled` | 系统容量型临时节流（高峰期降级） |
| 生成进度帧 | contentGenerationProgressList | `pollUrl` + 空 `ImageReferenceUrls` → 后续帧填充 | 轮询期间持续推帧；失败形态未见样本（待补抓） |
| Designer 身份拒绝 | har5#22 RemoteUls | `X-ErrorCode: ErrorDisallowedAADUser` (Account.ashx 403) | AAD 账号禁用 Designer 存储/生成入口——**har5"限制"实为此因**，非 429 |
| document.ashx 凭据失效 | har5#12~#23 | 带 fileToken 反复 401，cookie 通道 200 | TokenPrefix 短时效；下载成品图必须用响应中**新签发**的 fileToken |

> 结论：har5 场景中不存在 HTTP 429/Retry-After 样本。真正的"限制"分两类：**(a) 聊天生图**受 throttling 字段+两个 ImageGen*Throttled flight 控制（软限流，错误经 WS Progress/Error 帧下发）；**(b) Designer 独立编辑器**对 AAD 身份整体 403（硬拒绝）。2API 项目若要走 Designer 生图，MSA 个人号是唯一通路。

### 6.3 图片理解/后期链路（同一文件域）

```
上传图 → document.ashx(DocumentCache) 存入
  ├─ designai.ashx GetOCR        （文字提取，Polygon 坐标）
  ├─ sam.ashx Action=SAM         （256×64×64 fp32 特征图 + 目标检测框 ODTopK=100）
  └─ mediaprocessor.ashx ImageSuperResolution ScalingFactor=2 → 新图落 /SAM/ 目录
       ↓
Export.ashx?forceUseDRS=true （protobuf octet-stream，渲染 PNG）
       ↓
Media.ashx?id={uuid}.png&dcHint=WestUS2 （最终交付 CDN）
```

---

<a id="7"></a>
## 7. m365.cloud.microsoft 服务端函数

### 7.1 `POST /chat` with `{"action":"GetPersonalizationUserFlags"}`（P1）

- har1#122，200。响应包裹在 `store.personalizationFlags` 下，结构同 §1.3 GET。
- 属于 React Server Action 风格：同一 URL 不同 action 名分发不同后端逻辑。

### 7.2 `POST /_serverFn/{sha256}` — RSC Server Function 通道（P1）

- har2#70，200。body 为压缩序列化对象（t/i/p/k/v 编码），解码后关键载荷：
  - action=`RefreshNavPane`, accountInfo={accountType:"AAD", upn, objectId, tenantId, physicalRing:"Dogfood", audience:"WorldWide"}
  - 响应含：
    - `copilotPluginList: []`（插件清单）
    - `bizchatAsAgentGpt`: `{gptId:"bizchat-as-gpt-scenario", type:"DeclarativeCopilot", requiredClientFeatures:["FluxV3"], executionControls:{connectors/work/web/personalOneDrive/builtInPlugins/localDevice/dataverse}}` —— **内置 BizChat Agent 的完整能力声明**
    - `modelSelectorMetadata`: `{defaultModelSelectionId:"Magic", availableModelSelectionOptions:[{id:"Magic",title:"自动"},{id:"Chat",title:"快速答复"},{id:"Reasoning",title:"深度思考"},...]}` —— **模型档位选择器**（对应 GPT-5 reasoning 开关 Agt_bizchat_enableGpt5ForHelix）
- 价值：无需 WebSocket 即可拉取账号 agent/模型/连接器配置快照；hash 路径是函数指纹（版本敏感）。

### 7.3 页面路由（P2）

`/chat`、`/chat/all`、`/chat/agentstore`、`/chat/blocked`（har3#132，响应 `{"store":{"pageName":"ChatBlocked"}}` —— 封禁页状态机）、`/agents/new`、`/library`、`/library/visuals/all`、`/search`。其中 `/chat/blocked` 的存在证明前端有独立封禁落地页，可作为账号健康探测信号之一。

---

<a id="8"></a>
## 8. 鉴权体系还原

### 8.1 JWT access_token（Chathub query 携带，har1#364）

```json
{"aud":"https://substrate.office.com/sydney",
 "iss":"https://sts.windows.net/<TENANT_ID>/",
 "appid":"c0ab8ce9-e9a0-42e7-b064-33d422df41f1",       // M365 Copilot Web client id
 "idtyp":"user","scp":"CopilotPlatformContent.Process.All CopilotPlatformDataLossPreventionPolicy.Evaluate
   CopilotPlatformFiles.Read CopilotPlatformFiles.ReadWrite CopilotPlatformFiles.ReadWriteAll
   CopilotPlatformFileStorageContainer.Selected CopilotPlatformLicenseAssignment.Read.All
   CopilotPlatformMail.Read CopilotPlatformMail.Read.Shared CopilotPlatformMail.ReadWrite.Shared
   CopilotPlatformPresence.Read CopilotPlatformPresence.Read.All CopilotPlatformProtectionScopes.Compute.All
   CopilotPlatformSites.Read.All CopilotPlatformTeams.ReadWrite.All CopilotPlatformUser.Read
   M365Chat.Read sydney.readwrite",
 "secaud":{"aud":"00000003-0000-c000-0000-000000000000","scp":"Channel.Create Chat.ReadWrite ... Mail.Read ..."},
 "signin_state":["kmsi"],"xms_act_fct":"5 3","xms_sub_fct":"3 10"}
```
要点：
- 双 aud：外层 sydney（Copilot 后端），secaud 为 Graph 00000003-0000-c000...
- `sydney.readwrite` scope 是聊天主链路的通行证
- 有效期 exp-iat ≈ 4694s (~78min)，tokenExpirationTime 另在 AugLoop 握手帧出现（1787499783，3875s TTL）

### 8.2 fileToken（Designer 域自研签名）

`base64({"TokenPrefix":<uuid|AAD-uuid>, "UserObjectId":<oid>, "ClientName":"CopilotSydney"|"CWC"})`
- ClientName=CopilotSydney：聊天生图链路签发
- ClientName=CWC：Designer 编辑器签发
- 短时效，跨会话 401（§5.6 实证）

### 8.3 x-anchormailbox 路由头

`Oid:{oid}@{tid}` —— substrate 全系 API（search/puds/m365Copilot）必带，用于后端 mailbox 路由；缺失可能导致 401/路由漂移。

---

<a id="9"></a>
## 9. 价值分级总表

### P0（直接决定 2API 核心功能/账号能力）

| # | 端点/信号 | 价值点 | HAR 引用 |
|---|---|---|---|
| 1 | Chathub WebSocket 全协议 | 聊天主链路：SignalR 握手/invoke/update/type2/throttling/writeAtCursor/patches | har1#364, har4#246 |
| 2 | throttling 字段 | maxNum=600 会话上限实时计数器 → 配额查询 | har1#364 msg |
| 3 | CustomInstructions CRUD | 记忆/人设管理三件套（GET/POST/DELETE），直通 system prompt | har6#23~#35 |
| 4 | PersonalizationUserFlags GET/POST | isMemoryEnabled/isCustomInstructionEnabled/isM365GraphContentEnabled 五开关读写 → 记忆管理 API | har6#8~#22 |
| 5 | EventListener list_connectors | 租户 MCP/Federated Connector 全量枚举+OAuth vault 引用 | har1#240 |
| 6 | copilotuploads 403 指纹 | provisioningNotAllowed/notAllowed 双错误码 → 零成本账号预检 | har1#238, har3#194 |
| 7 | licenseDetails | SKU/servicePlans 全量 → Copilot 授权判定 | har3#216 |
| 8 | Chathub URL variants | 100+ flight 开关（GPT5-Helix/ImageGen 节流×2/Paid 文件支持/DesignerEditor） | har1#364 |
| 9 | licenseType=Starter query 参数 | 许可等级明文上报位 | har1#364 |
| 10 | ErrorDisallowedAADUser | Designer 身份硬拒指纹 → MSA/AAD 分流预检 | har5#22 |
| 11 | 图片生成 pollUrl/FileToken 三层凭据 | 生图异步轮询凭据结构与签发方标识 | har4#163/#246 |
| 12 | document.ashx 双轨鉴权 | 成品图下载通道（DallEGeneratedImages/SAM/DocumentCache 三目录） | har5#8/#12, har4#489 |

### P1（增强功能/数据面扩展）

| # | 端点/信号 | 价值点 | HAR 引用 |
|---|---|---|---|
| 13 | suggestions EntityRequests | Prompt 历史/People/File/Event 四合一联想搜索（Exchange Provenance） | har1#363 |
| 14 | puds settings/copilot PATCH | isWebSearchInWebEnabled 联网开关（会话粒度控制） | har6#14/#17 |
| 15 | sensitivityLabels | 合规标签能力探测 | har1#235 |
| 16 | _serverFn 快照 | bizchatAsAgentGpt 能力声明 + Magic/Chat/Reasoning 模型档位 | har2#70 |
| 17 | userconfig ContentSources | 可搜索内容源枚举 | har1#236 |
| 18 | mediaprocessor ImageSuperResolution | 免费×2 超分（含 4 个未观测 Action 槽位） | har5#3 |
| 19 | Export.ashx forceUseDRS | 云端渲染导出 PNG | har5#52 |
| 20 | sam.ashx SAM embedding | 图像特征提取（numpy fp32 shape(1,256,64,64)） | har5#15 |
| 21 | designai.ashx GetOCR | 带坐标 OCR | har5#13 |
| 22 | Media.ashx 下载 | 导出成品 CDN 通道（dcHint 路由） | har5#61 |
| 23 | POST /chat action 分发 | GetPersonalizationUserFlags 等 server action | har1#122 |

### P2（辅助/遥测/低频）

| # | 端点 | 说明 | HAR |
|---|---|---|---|
| 24 | CreateKit.ashx | Kit 模板列表（样本为空） | har4#446 |
| 25 | RemoteUls.ashx | 日志旁路情报源（上游错误码泄露点） | har4#289, har5#22 |
| 26 | TraceRequest.ashx | 场景遥测（aiActions 计数） | har5#49 |
| 27 | RealTimeChannel.ashx WS | Designer 协作通道 | har4#477 |
| 28 | me/photos 96x96 | 头像 404/200 | har1#233 |
| 29 | /chat/blocked | 封禁页状态 | har3#132 |
| 30 | dogfood.augloop WS | AugLoop 会话协议 token 下发（3875s TTL） | har5#113 |

---

<a id="10"></a>
## 10. 对 M365-Copilot2API 的落地建议

1. **聊天核心**：按 §1.1 复刻 SignalR over WSS；握手后先收 `{"type":6}` ping 保持心跳；把 `throttling.numUserMessagesInConversation/maxNumUserMessagesInConversation` 映射为 OpenAI 风格 usage/quota 字段返回给下游。
2. **记忆管理 API 化**：CustomInstructions + PersonalizationUserFlags + puds copilot 三件套可直接包装成 `/v1/memory`、`/v1/preferences` REST 面；instruction 直通 system prompt，可用于给每个 API-Key 注入不同人设。
3. **账号池分级**：登录后依次探 `copilotuploads`(403 code) → `licenseDetails`(SKU) → `ProfileInfo` → Chathub URL `licenseType` → 首条消息 `throttling.maxNum`，五步即可给账号打标（Starter/Paid/AAD受限/OneDrive未开通）。
4. **图片生成**：短期走 Chathub GenerateGraphicArt（pollUrl 轮询→document.ashx 下载，注意 fileToken 时效）；Designer 独立链路（designai/sam/mediaprocessor/Export）仅 MSA 可行（AAD 被 ErrorDisallowedAADUser 拒绝）。
5. **待补抓**：`m365Copilot/UploadFile`、Conversation Share API、MemoryUpdate 插件回调、ImageGen 节流触发时的 WS 错误帧样本、puds GET 读方向、dataservice.o365filtering（本批未出现）。

---

## 落地清单

> 本报告的落地建议汇总，详细依据见上文 §10 与各节的 P0/P1/P2 标注。

- [ ] 按 §1.1 复刻 SignalR over WSS 聊天主链路；把 throttling 计数器映射为 OpenAI 风格 usage/quota 字段返回下游
- [ ] 将 CustomInstructions + PersonalizationUserFlags + puds 三件套包装为 `/v1/memory`、`/v1/preferences` REST 面（instruction 直通 system prompt，可按 API-Key 注入人设）
- [ ] 账号池五步分级探测：copilotuploads(403 code) → licenseDetails(SKU) → ProfileInfo → Chathub licenseType → 首轮 throttling.maxNum
- [ ] 图片生成短期走 Chathub GenerateGraphicArt（pollUrl 轮询 → document.ashx 下载，注意 fileToken 短时效）
- [ ] Designer 独立生图链路仅 MSA 个人号可行（AAD 被 ErrorDisallowedAADUser 硬拒）
- [ ] 待补抓目标：m365Copilot/UploadFile、Conversation Share API、MemoryUpdate 回调、ImageGen 节流 WS 错误帧、puds GET 读方向、dataservice.o365filtering
