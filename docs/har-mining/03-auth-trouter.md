# HAR 逆向报告 03：OAuth/PKCE 认证、token 刷新与 Trouter 长连接

> **本文档回答什么问题**：登录态是如何维持的——浏览器 OAuth broker 包装细节、20 次 token 刷新实测出的 expires_in 规律、FOCI 家族 refresh_token 互换的决定性证据、Trouter 通知长连接的协议与认证方式，以及 internal/auth 与 pkce_auth_gateway.py 的十项改进清单。

数据源：本地抓包文件 har1.har ~ har6.har（2026-08-05 ~ 2026-08-23 录制，M365 Copilot web，Edge/Windows）。
证据标注格式 `harN[entry索引]`。所有 token 值已脱敏（仅保留前缀与长度）。

---

## 1. login.microsoftonline.com OAuth 调用全量枚举

6 个 HAR 共 **20 次 token 端点调用**，全部为
`POST https://login.microsoftonline.com/<TENANT_ID>/oauth2/v2.0/token`
——注意 authority 是**租户专属**（tid=<TENANT_ID>），不是 `/common`。

| # | har/idx | body client_id | grant_type | 请求 scope（资源部分） | 响应 scope 摘要 | expires_in |
|---|---------|----------------|------------|------------------------|------------------|-----------|
| 1 | har1[508] | c0ab8ce9 (brk) | refresh_token | dataservice.o365filtering.com/.default | ServiceHealth.Read | 4293 |
| 2 | har1[511] | c0ab8ce9 (brk) | refresh_token | loki.delve.office.com/.default | Chat.Read Files.ReadWrite LLM.Read Mail.Read Policy.Read User.* | 4275 |
| 3 | har1[527] | c0ab8ce9 (brk) | refresh_token | clients.config.office.net/.default | UserPolicies.Read | 4532 |
| 4 | har2[79] | **4765445b** | **authorization_code** | www.office.com/v2/OfficeHome.All | M365Copilot.Read.All MDCPP.ALL OfficeHome.All | 5347 |
| 5 | har2[85] | c0ab8ce9 (brk) | refresh_token | graph.microsoft.com/.default | 25 个 Graph delegated scope | 4955 |
| 6 | har2[86] | c0ab8ce9 (brk) | refresh_token | titles.prod.mos.microsoft.com/.default | AuthConfig.Read Title.ReadWrite | 4831 |
| 7 | har2[87] | c0ab8ce9 (brk) | refresh_token | m365.cloud.microsoft/v2/.default | M365Copilot.Read.All | 5300 |
| 8 | har2[88] | c0ab8ce9 (brk) | refresh_token | arc.msn.com/v4/.default | User.Read | 4341 |
| 9 | har2[162] | c0ab8ce9 (brk) | refresh_token | clients.config.office.net/.default | UserPolicies.Read | 4458 |
| 10 | har3[100] | c0ab8ce9 (brk) | refresh_token | titles.prod.mos.microsoft.com/.default | 同上 | 3687 |
| 11 | har3[217] | c0ab8ce9 (brk) | refresh_token | substrate.office.com/search/.default | SubstrateSearch-Internal.ReadWrite | 4157 |
| 12 | har3[218] | c0ab8ce9 (brk) | refresh_token | ic3.teams.office.com/.default | Calling/Endpoint/Media/Messaging.ReadWrite.All | 4152 |
| 13 | har4[127] | c0ab8ce9 (brk) | refresh_token | dataservice.o365filtering.com/.default | ServiceHealth.Read | 5290 |
| 14 | har4[136] | c0ab8ce9 (brk) | refresh_token | loki.delve.office.com/.default | 同 #2 | 4391 |
| 15 | har4[174] | c0ab8ce9 (brk) | refresh_token | designerappservice.officeapps.live.com/.default | designerappservice.all | 4361 |
| 16 | har4[267] | **4765445b** | refresh_token | loki.delve.office.com/.default | 同 #2 + Files.Read | 4566 |
| 17 | har4[456] | **4765445b** | refresh_token | loki.delve.office.com/.default | 同上 | 5114 |
| 18 | har5[109] | **4765445b** | refresh_token | designerappservice…//.default | designerappservice.all | 5330 |
| 19 | har5[117] | **4765445b** | refresh_token | augloop.office.com/v2/.default | AugLoop.All | 3875 |
| 20 | har6[13] | c0ab8ce9 (brk) | refresh_token | substrate.office.com/.default | ActivityFeed/Contacts/Context/SubstrateSearch 等 11 个 | 4705 |

### 1.1 Broker（brk）包装 —— 关键协议细节

凡 body client_id=`c0ab8ce9` 的调用都带三层身份（har1[508] 全文）：

```
URL query : brk_client_id=4765445b-32c6-49b0-83e6-1d93765276ca
            brk_redirect_uri=https%3A%2F%2Fm365.cloud.microsoft%2Fspalanding
body      : client_id=c0ab8ce9-e9a0-42e7-b064-33d422df41f1
            redirect_uri=brk-multihub://outlook.office.com   ← URL-encoded
            grant_type=refresh_token
```

这是 MSAL.js 的 multihub broker 模式：M365 门户 SPA（4765445b）作为 broker，
替内嵌 Copilot 客户端（c0ab8ce9）向 AAD 换取各资源 token。

### 1.2 固定遥测头（每次刷新必带）

```
x-client-SKU: msal.js.browser        x-client-VER: 5.9.0
x-ms-lib-capability: retry-after, h429
x-client-current-telemetry / x-client-last-telemetry: 5|61,0,…
client_info: 1
X-AnchorMailbox: Oid:<用户oid>       ← har1[508]: Oid:<USER_A_OID>
client-request-id: <guid>（也在 URL query）
```

### 1.3 expires_in 规律

- 实测区间 **3687 ~ 5347 秒（约 61~89 分钟），均值 ≈4680s**；不是固定 3600。
- `ext_expires_in == expires_in`（20/20）；**从不返回 `refresh_in` 字段**。
- 同一资源两次请求的 expires_in 也不同（如 loki: 4275 vs 4391 vs 4566 vs 5114）→ AAD 动态发放。
- 结论：代码不能假设 3600s，也不能假设固定值；应以响应 expires_in 或 JWT exp 为准。

### 1.4 refresh 流程与 FOCI 家族 token 共享（决定性证据）

- 所有 RT 前缀均为 `1.AT4A`，长度 1300~1620 波动；**每次刷新都返回新 RT（rotate-on-use）**。
- 家族共享证据链：har2[79] 中 client_id=4765445b 通过 authorization_code 获得 RT（len=1304）；
  随后 har2[85..88] 四次请求 body client_id=c0ab8ce9 直接复用同一 len=1304 的 RT 刷新成功。
  反向亦然：har4[267]/[456] 用 c0ab8ce9 流程拿到的 RT 以 client_id=4765445b 刷新成功。
- 即 **4765445b 与 c0ab8ce9 属于同一 first-party token family（FOCI），RT 可互换使用**。
  这解释了为什么项目里 FOCIClientID(d3590ed6) 与 DefaultClientID(c0ab8ce9) 的 RT 有时互通。

---

## 2. 浏览器实际 client_id/scope 与项目代码的差异

### 2.1 client_id 对比

| 来源 | client_id | 角色 |
|---|---|---|
| 项目 `internal/auth/config.go:8` DefaultClientID | c0ab8ce9-e9a0-42e7-b064-33d422df41f1 | 与浏览器 Chathub WS token 的 appid 一致 ✓ |
| 浏览器 broker 层 | 4765445b-32c6-49b0-83e6-1d93765276ca（M365 门户 SPA） | 项目未实现此层 |
| 浏览器 authorization_code 入口 | 4765445b（har2[79]，scope=OfficeHome.All） | 项目用 nativeclient 直连 c0ab8ce9，绕过 broker，可行 |

差异结论：项目直连 PKCE 用 c0ab8ce9 是有效的（appid 一致），但浏览器原生路径是
"4765445b 先拿 code → broker 匿名换取 c0ab8ce9 各资源 token"。两条路拿到的 sydney token 权限相同（见 §4）。

### 2.2 scope 差异（重点）

项目 `config.go:12` DefaultScope：
```
openid profile offline_access
https://substrate.office.com/sydney/M365Chat.Read
https://substrate.office.com/sydney/sydney.readwrite
```

浏览器 Chathub WS 实际使用的 sydney token（har1[364] query access_token，len=3471）scp 含 **18 项**：
```
CopilotPlatformContent.Process.All
CopilotPlatformDataLossPreventionPolicy.Evaluate
CopilotPlatformFiles.Read / ReadWrite / ReadWriteAll
CopilotPlatformFileStorageContainer.Selected
CopilotPlatformLicenseAssignment.Read.All
CopilotPlatformMail.Read / Mail.Read.Shared / Mail.ReadWrite.Shared
CopilotPlatformPresence.Read / Presence.Read.All
CopilotPlatformProtectionScopes.Compute.All
CopilotPlatformSites.Read.All
CopilotPlatformTeams.ReadWrite.All
CopilotPlatformUser.Read
M365Chat.Read
sydney.readwrite
```

即浏览器的权限是超集（多出文件/邮件/团队/站点等 Copilot 平台检索能力）。录制窗口内浏览器从未显式请求 sydney scope
（token 来自会话前缓存），推断获取方式为 `https://substrate.office.com/sydney/.default` 静态展开。
**建议项目改用 `/sydney/.default`**，一次拿到全部 18 项。

### 2.3 特殊 scope 用途表

| scope 资源 | 返回权限 | 用途 | 证据 |
|---|---|---|---|
| dataservice.o365filtering.com/.default | ServiceHealth.Read | M365 服务健康横幅（服务中断提示） | har1[508], har4[127] |
| loki.delve.office.com/.default | Chat.Read Files.* LLM.Read Mail.Read Policy.Read User.* | **BizChat/Copilot 对话历史存储**（aud=GUID 394866fc-eedb-4f01-8536-3ff84b16be2a） | har1[511], har4[136][267][456] |
| ic3.teams.office.com/.default | Calling/Endpoint/Media/Messaging.ReadWrite.All | **Trouter 认证专用**（见 §3） | har3[218] |
| substrate.office.com/search/.default | SubstrateSearch-Internal.ReadWrite | 搜索联想 | har3[217] |
| clients.config.office.net/.default | UserPolicies.Read | 客户端配置策略 | har1[527] |
| titles.prod.mos.microsoft.com/.default | AuthConfig.Read Title.ReadWrite | 应用标题/入口配置 | har3[100] |
| m365.cloud.microsoft/v2/.default | M365Copilot.Read.All | 门户自身 API | har2[87] |
| www.office.com/v2/* | OfficeHome.All MDCPP.ALL | 门户首页 | har2[79] |
| arc.msn.com/v4/.default | User.Read | 内容卡个性化 | har2[88] |
| augloop.office.com/v2/.default | AugLoop.All | 实时协作/AI 辅助管道 | har5[117] |
| designerappservice…/.default | designerappservice.all | Designer 图像生成 | har4[174]（对应 Go 侧 RefreshWithScope 注释） |

---

## 3. Trouter WebSocket 分析

### 3.1 连接方式

```
wss://go.trouter.teams.microsoft.com/v4/c
    ?check=<epoch_ms>&cor_id=<guid>
    &epid=<chatsessionid>          ← 与 Chathub WS 的 chatsessionid 相同
    &tc={"cv":"2025.30.01.1","ua":"BizChat","hr":"","v":"3639/1.0.0"}
无 Sec-WebSocket-Protocol 子协议
```

`ua=BizChat` 表明这是 M365 Copilot（BizChat）的通知通道，不是 Teams 主通道。
出现于 har1[263]（18 帧）、har3[227]（7 帧）。

### 3.2 协议帧格式（socket.io v4 文本帧）

```
1::                                  ← 服务器 open
5:::{json}                           ← 客户端 event
5:N::{json}                          ← 服务器 event（N=序号）
5:N+:{"name":"ping"}                 ← 客户端 ack-ping
6:::N+["pong"]                       ← 服务器 pong
```

### 3.3 认证握手（har1[263] 第 2 帧 send）

```json
{"name":"user.authenticate","args":[{"headers":{
  "Authorization":"Bearer <REDACTED_TOKEN>" }}]}
```

该 JWT 解码（完整）：`aud=https://ic3.teams.office.com`，`scp=Calling.ReadWrite.All Endpoint.ReadWrite.All Media.ReadWrite.All Messaging.ReadWrite.All`，
`appid=c0ab8ce9`，lifetime 5021s，iss=`https://sts.windows.net/<tid>/`。
**Trouter 认证不接收 sydney token，必须先刷 ic3.teams.office.com token**（对应 har3[218] 的刷新时机恰在 WS 前后）。

### 3.4 服务器事件

- `trouter.connected`（830B）：下发 id/ccid、url/surl/curlb（pub-ent-ince-10-f 等区域节点）、
  healthUrl=`go.trouter.../v4/h`、reconnectUrl（wss 备援）、registrarUrl=
  `communications.svc.cloud.microsoft/registrar/apac/v3/registrations`、ttl=597348s 及 connectparams{sr,sp,se,st} 签名参数。
- `trouter.message_loss`：droppedIndicators[{tag,etag}]，tag ∈ {"", messaging, messagingsync, pinnedchannel, tps}
  ——离线期间错过的消息类别通知。
- 之后仅 ping/pong 心跳（约 25s 间隔）。录制窗口内未观察到业务推送帧（无新消息到达）。

### 3.5 可否用于接收限流/账号异常推送？

- **消息级通知可以**：messaging/messagingsync tag 的 message_loss 证明对话类事件经此通道投递；
  保持长连接即可实时收到 BizChat 新消息/对话事件，无需轮询。
- **限流信号不走 trouter**：Chathub 流式回复中的限流/错误是内联在 SignalR 响应帧里（见 §3.6），
  trouter 只做"离线补偿"摘要。账号异常（吊销会话）预期以 message_loss+重连失败形式表现，
  可作为健康探针：连接被拒/user.authenticate 失败 ≈ token 或账号异常。
- 工程价值：给代理池每个账号挂一条 trouter 连接成本极低（40s timeout 心跳），可替代主动轮询做账号活性检测。

### 3.6 对照组：Chathub WS（真正的对话通道）

`wss://substrate.office.com/m365Copilot/Chathub/<oid>@<tid>?chatsessionid=…&access_token=<JWT len=3471>`
——**access_token 明文放在 URL query**（非 header！）。SignalR JSON 握手：
`{"protocol":"json","version":1}` → `{}`，keepalive `{"type":6}`，invoke 帧
`arguments[0]={source:"officeweb", sessionId, optionsSets:[…cwc_flux_image, cwc_code_interpreter…]}`
（har1[364]）。每轮对话新建一条 WS（har4 中 7 条，最多 222 帧）。
同 URL 还有 `variants` 参数，内容为 feature flags 逗号串，非 JWT、非认证用途。

---

## 4. Access Token JWT 结构与 Copilot 必需 scope

对 53 个解码 JWT 的汇总：

| claim | 观测值 | 说明 |
|---|---|---|
| aud | `https://substrate.office.com/sydney`（字符串）/ GUID 394866fc…（loki）/ https://graph.microsoft.com 等 | v1.0 token 用资源 URI 做 aud |
| iss | `https://sts.windows.net/<TENANT_ID>/`（v1 格式） | ver=1.0 配 v1 issuer |
| appid/azp | c0ab8ce9（或 4765445b，loki 场景） | 调用方客户端 |
| scp | 见 §2.2 十八项 | **全部为 delegated，roles 字段 20/20 为空** |
| ver | 1.0（sydney/ic3/dataservice）、2.0（loki） | 同一用户不同资源混用 v1/v2 token |
| idtyp | user | |
| tid/oid | <TENANT_ID> / <USER_A_OID> 或 <USER_B_OID> | 两个账号（HAR 跨账号录制） |
| 其他 | rh, aio, uti, sid, signin_state[kmsi], xms_act_fct, xms_ftd, xms_pftexp, puid, acct, amr, acr, ipaddr, tenant_region_scope, secaud, xms_idrel, xms_sub_fct | |
| lifetime(iat→exp) | 4694s / 4805s / 5021s | 与 expires_in 一致量级 |

**Copilot 必需 scope 判定**（以 Chathub WS 接受为准）：最小集 = `M365Chat.Read + sydney.readwrite`
（项目现状即可对话）；完整功能集需追加 16 个 `CopilotPlatform*`（文件/邮件引用、DLP 评估、presence、teams/sites 检索）。
若只做纯对话，缺 CopilotPlatform* 不影响 chat 本身，但服务端可能按 license/权限裁剪引用类功能。

---

## 5. cache.go 与 pkce_auth_gateway.py 改进清单

对照 `internal/auth/cache.go`、`internal/auth/config.go`、`internal/auth/token.go`、根目录 `pkce_auth_gateway.py`：

**P1（正确性/保活）**
1. `config.go:12` DefaultScope 改为 `openid profile offline_access https://substrate.office.com/sydney/.default`
   ——一次静态同意展开全部 18 项（§2.2），避免未来服务端要求新增 CopilotPlatform* 时二次授权。
2. `token.go:76 Refresh()` 补发 `X-AnchorMailbox: Oid:<oid>` 头（浏览器每次必带，har1[508]），
   提升 AAD 路由亲和；Store 已有 OID 字段，改动小。
3. Authority 存租户后用 `<tid>/oauth2/v2.0/token` 刷新而非 `/common`（浏览器全走租户端点，§1）；
   AccountToken.TID 已落盘，具备条件。
4. RT rotate 兜底：AAD 每次 refresh 都换新 RT（§1.4），cache.go Upsert 已保存新值 ✓；
   但 Python 网关 `persist_tokens` 写明文 accounts.json 且外壳结构带多余字段（Go 可读但 refreshToken 未加密，
   绕过了 cache.go 的 AES-GCM `enc:v1` 方案）——Python 侧应复用同一加密或只产出中间文件交给 Go 导入。
5. 时钟校准：ExpiresAt = now+expires_in 受本机时钟漂移影响；建议同时解 AT 的 exp/iat，
   取 `iat+expires_in` 与本地计算交叉验证（JWT 内已有 iat/exp，token.go 已解 claims 但未用 exp）。

**P2（健壮性）**
6. `pkce_auth_gateway.py:279` state 不匹配仅打印警告——应硬拒绝（PKCE 防 CSRF 的另一半）。
7. `pkce_auth_gateway.py:136` 单一全局 SESSION：连续添加第二个账号会覆盖 verifier 导致上一个 code 作废；建议按 state 建会话表。
8. `EnsureValid` 提前量固定 30s（cache.go:460）；观测 expires_in 均值 78 分钟且波动大（61~89min，§1.3），
   建议改为剩余寿命 <10% 或 <120s 双阈值触发，并加单飞抖动避免多账号同步刷新。
9. 若实现 §3.5 的 trouter 健康探针，需要新增 `RefreshWithScope(refreshToken, clientID,
   "https://ic3.teams.office.com/.default openid offline_access")` 获取 trouter 凭据（函数已存在，直接可用）。

**P3（信息补强）**
10. accounts.json 可记录 foci 标志与 appid/aud（Python 网关已在写，Go 侧 TokenSet 未存），
    便于排查"哪个 family 的 RT 在哪个端点失效"。AADSTS 错误码解析（token.go aadstsCode）已有，保持。

---

## 6. 证据索引速查

| 发现 | 位置 |
|---|---|
| broker 三层参数全文 | har1[508] postData |
| FOCI RT 互换 | har2[79](4765445b,RT len1304) → har2[85..88](c0ab8ce9 同 RT) |
| expires_in 波动 | §1.3 表 20 行 |
| Chathub WS token in query | har1[364] wss URL access_token (len=3471) |
| sydney scp 18 项全文 | har1[364] 该 JWT payload |
| trouter 认证 JWT(ic3) | har1[263] ws send 帧 user.authenticate |
| trouter.connected 全字段 | har1[263] recv 帧（含 registrarUrl/ttl/connectparams） |
| message_loss tags | har1[263]/har3[227] recv ×9 |
| SignalR 握手序列 | har1[364] 前 4 帧 |
| id_token claims(kmsi) | har2[79] id_token |
| 无 Authorization 头的 REST | har1 EventListener/CustomInstructions/search suggestions（cookie/session 认证） |

---

## 落地清单

> 本报告对认证模块的改动建议汇总（P1 正确性/保活 → P3 信息补强），详细依据见上文 §5。

- [ ] **P1** DefaultScope 改用 `https://substrate.office.com/sydney/.default`，一次静态同意展开全部 18 项 scp（config.go:12）
- [ ] **P1** Refresh() 补发 `X-AnchorMailbox: Oid:<oid>` 头（token.go:76）
- [ ] **P1** 刷新走租户专属端点 `<tid>/oauth2/v2.0/token` 而非 `/common`
- [x] **P1** RT rotate-on-use 的新值已落盘（cache.go Upsert ✓，无需改动）
- [ ] **P1** Python 网关明文 accounts.json 整改：复用 cache.go 的 AES-GCM 加密，或只产出中间文件交 Go 导入
- [ ] **P1** ExpiresAt 用 JWT iat/exp 与本地 now+expires_in 交叉校准，抗时钟漂移
- [ ] **P2** pkce_auth_gateway.py:279 state 不匹配从警告改为硬拒绝
- [ ] **P2** pkce_auth_gateway.py:136 按 state 建会话表，避免多账号 verifier 相互覆盖
- [ ] **P2** EnsureValid 提前量改双阈值（剩余寿命 <10% 或 <120s）+ 单飞抖动
- [ ] **P2** trouter 健康探针所需的 RefreshWithScope(ic3) 函数已存在，直接可用
- [ ] **P3** accounts.json 记录 foci 标志与 appid/aud，便于排查 RT 在哪个 family/端点失效
