# HAR 逆向报告 06：请求时序、延迟与连接管理

> **本文档回答什么问题**：一次聊天的延迟花在了哪里——13 次对话的分阶段时序分布（浏览器 vs Go 实测基线）、"用户打字窗口掩盖建连成本"的核心规律、WS 连接生命周期与 HTTP/2 连接复用画像，以及 connpool 死代码 bug 的发现过程与六项优化措施。
>
> 数据源：本地抓包文件 har1.har ~ har6.har（Chrome 导出，共 2225 entry，13 次 Chathub 聊天，
> 15 条 WebSocket：11 Chathub + trouter v4 ×2 + designerapp ×2）。
> 方法：Python 解析 `startedDateTime` / `time` / `timings` / `_webSocketMessages.time`（epoch 秒）/
> `_connectionId` / `_priority` / `_resourceType` / `_initiator`，以及 chat payload 合并帧内的
> **Metrics.Timestamps**（浏览器自报的五个时间戳，本篇最硬的证据）。
> 对照组：本项目 `dev_stderr2~7.log` 中 26 条 `chathub timing` 日志。

---

## 1. 一次完整聊天的关键路径时间线（浏览器，n=13）

### 1.1 浏览器侧各阶段分布（毫秒）

| 阶段 | n | min | P50 | P90 | max | 来源 |
|---|---|---|---|---|---|---|
| ConnectionStart → ConnectionEstablished（WS 拨号+TLS+101+SignalR 握手） | 12 | 672 | **2 235** | 11 365 | 19 025 | Metrics 帧 |
| 用户打字窗口 UserInputStart→Submit（可掩盖建连的时间预算） | 12 | ~500 | ~1 700 | — | 49 000 | Metrics 帧 |
| UserInputSubmit → RequestSent（提交处理延迟） | 12 | **1** | **146** | 2 618 | 13 348 | Metrics 帧 |
| chat 帧发出 → 首个 update 帧（首 token） | 12 | 168 | **450** | 2 303 | 3 930 | WS 帧时序 |
| chat 帧发出 → 流完成（type-2/3） | 11 | 2 949 | 8 567 | 22 563 | 30 416 | WS 帧时序 |
| WS 建立 → chat 帧发出（空闲等待，含预建连提前量） | 12 | 2 | 1 459 | 14 000 | 49 457 | 帧时序 |

单样本明细（payload 相对 WS start，ms）：

```
har1#364  hs=342   hs→pay=2832  pay→1stUpd=310  pay→done=2949
har1#590  hs=4693  hs→pay=2     pay→1stUpd=2489 pay→done=20262
har1#650  hs=314   hs→pay=49457 pay→1stUpd=168  pay→done=8107
har1#789  hs=404   hs→pay=1607  pay→1stUpd=182  pay→done=5019
har1#873  hs=408   hs→pay=13086 pay→1stUpd=189  pay→done=4695
har1#976  hs=408   hs→pay=8461  pay→1stUpd=176  pay→done=22563
har4#4    hs=1808  hs→pay=5     pay→1stUpd=449  pay→done=3993
har4#163  hs=1348  hs→pay=4     pay→1stUpd=451  pay→done=30416
har4#194  hs=4742  hs→pay=2     pay→1stUpd=625  pay→done=10309
har4#201  hs=12398 hs→pay=3     pay→1stUpd=3930 pay→done=16025
har4#211  hs=2535  hs→pay=11399 pay→1stUpd=537  pay→done=8567
har4#246  hs=2335  hs→pay=2     pay→1stUpd=527  pay→done=N/A(长对话截断)
```

### 1.2 Metrics.Timestamps 原始证据（浏览器自报，12 组全录）

```json
har1#364 {"ConnectionStart":"09:55:20.611","UserInputStart":"09:55:20.909","ConnectionEstablished":"09:55:21.376","UserInputSubmit":"09:55:24.052","RequestSent":"09:55:24.205"}
har1#590 {"ConnectionStart":"09:55:38.713","UserInputStart":"09:55:38.741","UserInputSubmit":"09:55:40.957","ConnectionEstablished":"09:55:43.763","RequestSent":"09:55:43.764"}
har1#650 {...,"ConnectionStart→Established":672ms,"Submit→RequestSent":232ms}
har4#201 {"ConnectionStart":"14:26:56.464","ConnectionEstablished":"14:27:15.489"(19025ms!),"UserInputSubmit":"14:27:04.155","RequestSent":"14:27:15.490"}
```

**核心规律：`UserInputStart ≈ ConnectionStart`（12/12 样本，偏差 −250ms～+4.8s）。**
即用户光标进入输入框/敲第一个字的瞬间，前端就发起 Chathub WS 拨号；
打字窗口（P50 ≈ 1.7s）把 672ms～19s 的建连成本完全藏住。
连接热时 `Submit→RequestSent` 仅 1–260ms；冷时（har4#201 建连 19s 慢于打字）用户感知 13.3s——
**浏览器的首字延迟方差几乎 100% 由建连时机决定，而不是由推理决定。**

### 1.3 页面级时间线（har1，页面 t0=09:55:05.799）

| t(ms) | 事件 |
|---|---|
| 0 | GET /chat document（657ms，VeryHigh，h2），HTML 内含 preconnect×1 + dns-prefetch×2 + preload×23 |
| ~1300 | onContentLoad |
| 7057–8844 | 登录链 savedusers → Me.srf（SSO 静默续期） |
| 15486 | Chathub WS#364 发出 SignalR 握手帧（拨号起点≈15259） |
| 21298–22833 | 3 次 POST login.microsoftonline.com/oauth2/v2.0/token（wait 150–224ms） |
| 17859 | chat 帧（用户 24.052 提交） |
| 20807 | 首个 update 帧 |
| 23367 | 流完成 |

token 获取在首次聊天之前完成且与建连并行；token POST wait P50≈550ms（n=21，
范围 150–1470ms，`connect` 冷启动另加 40–2015ms，复用连接后 `-1`）。

## 2. WS 连接生命周期

### 2.1 每轮新建，绝不跨轮复用

13 次聊天 = 13 条独立 Chathub WS（har1 六连 + har4 七连）。同一 ConversationId
（har4 `<CONVERSATION_ID_B>` 七轮、har1 `<CONVERSATION_ID_A1>` 四轮）也各自新建 WS。旧连接在流结束后
不关闭，**空闲保活 2.5–50.2s**（idle_tail = WS 总时长 − 流完成时刻：
har1#650 达 50 187ms），期间双向 type-6 ping（约 15–30s 间隔）直到页面 GC 或跳转。
结论：新建是被页面生命周期驱动的选择，不是协议限制——**服务端允许长空闲连接**。

### 2.2 ConnectionId 的分配方式：没有协商步骤

Chathub 与 trouter v4 不同，**不存在 negotiate/ConnectionId 分配往返**：

```
f0 send {"protocol":"json","version":1}␞        ← t0
r1 recv {}␞                                     ← t0+RTT（"绑定"完成的唯一确认）
f2 send {"type":6}␞                             ← ACK 后 0–1ms
f3 send {chat payload / AttachToSession}␞       ← ACK 后 1–5ms（热）或数万 ms（预建连等输入）
```

身份全在 URL 里（`/{oid}@{tid}?chatsessionid&clientrequestid&ConversationId…`），
101 协议切换 + `{}` 握手 ACK 即完成绑定。trouter v4（go.trouter.teams.microsoft.com，
har1#263，存活 200.4s）才有独立 negotiate。

### 2.3 AttachToSession 重连（har4#546）

- 触发条件：页面刷新后续接同一会话。前一条 WS #246 于 rel=301 678ms 结束，
  #546 于 rel=304 331ms 开始（间隔 2.65s），ConversationId 相同 `<CONVERSATION_ID_B>`。
- 耗时分解：dial→握手 ACK **1 402ms**，ACK→AttachToSession 帧仅 **1ms**，
  之后 recv 仅 3 帧、无 update、4.57s 后关闭——恢复的是服务端会话状态（续写上下文），
  不重放对话历史。
- 对我们的价值：断流重连时用 AttachToSession 可免整轮重试（见 §5-O4）。

## 3. HTTP/2 多路复用与 HPACK

| host | 请求数 | 连接数 | 单连接峰值 | 协议分布 | 峰值并发 |
|---|---|---|---|---|---|
| res.public.onecdn.static.microsoft | 426(har1) | 6 | **233 req/conn** | h3 400 / h2 26 | 高（静态并行） |
| m365.cloud.microsoft | 53(har4) | 2 | **49 req/conn** | h2 | 2–4 |
| substrate.office.com | 38(har1) | **2**（18+8） | 18 req/conn | **h3 26** / HTTP1.1 6 | 3 |
| browser.events.data.microsoft.com | 30–33 | 1–3 | 31 req/conn | h2 | — |
| login.microsoftonline.com | 4–7 | — | — | HTTP/1.1 | — |

- substrate REST 高度复用：har1 全部 API 共享 2 条连接，且 **68% 走 QUIC/h3**；
  WS 本身仍走 HTTP/1.1 over TCP（status=101，未用 RFC 8441 h2-WS）。
- header 压缩：substrate REST 平均每请求 **25 个头 / 约 1 157B**，同连接上
  origin/referer/user-agent/accept 等逐字节重复。HPACK 动态表使这些只传索引
  （HAR 无法直接观测动态表状态，此处为推断），等效头开销估计降 60%+；
  authorization bearer 因 token 各异仍需字面量。对 Go 的启示：对同 host 的
  UploadFile/search 类 REST 保持单一 Transport 复用即可获得同等收益。

## 4. 资源优先级与预热策略

### 4.1 优先级画像（Chrome _priority）

- **VeryHigh**：document、全部 CSS、登录字体 segoeui woff2、copilot-thinking 动画 JSON（思考动画被当作关键资源预取，har4 五次命中）。
- **High**：所有 API fetch/XHR——substrate search/api（打字联想，165–396ms/次）、ecs 配置、clients.config、甚至遥测 events.data 也是 High。
- **Low**：onecdn 的 JS chunk（har1 393 个 Low）、content.lifecycle.office.net（23 个 Low）。
- 规律：**API 一律 High，JS chunk 一律 Low**——首屏让位给数据通道。

### 4.2 预连接指令（可直接借鉴的预热目标清单)

`m365.cloud.microsoft/chat` 文档 head：

```
preconnect×1   → https://substrate.office.com          ← 核心 API 域
dns-prefetch×2 → https://login.microsoftonline.com      ← token 域
                 https://res.public.onecdn.static.microsoft ← CDN 域
preload×23     → 关键 CSS/fonts
```

login.live.com 登录页另有 preconnect×5 + dns-prefetch×6（acctcdn.msauth.net、
logincdn.msauth.net、ms-sso.copilot.microsoft.com 等 auth CDN）。

效果实证：substrate 首个 REST（search/api #361，rel=15 029ms）`connect=-1`
（连接已由 preconnect 建好），token 第二次起 `dns=-1, connect=-1`。

## 5. 对照我们的 connpool/client.go：差距与可落地优化清单

### 5.1 我们的实测基线（dev_stderr*.log，n=26）

| 阶段（Go 日志口径） | min | P50 | P90 | P95 | max |
|---|---|---|---|---|---|
| ws_dial_ms（TCP+TLS+101） | 525 | **638** | 1 020 | 1 526 | 1 930 |
| handshake_ms（dial 起，含 SignalR 握手） | 636 | **773** | 1 433 | 1 818 | 2 107 |
| first_delta_ms（payload→首 delta） | 860 | **1 362** | 3 075 | 3 538 | 3 815 |
| completion_frame_ms | 3 359 | 6 775 | 11 288 | 12 874 | 19 300 |

26/26 条日志均为 `reused=false`，无一条 "warmed connection"——**池从未命中过**。

### 5.2 发现 P0 级 bug：连接池是死代码

`internal/chathub/client.go:454` 的 `returnConn := false` 之后全文 **26 处赋值全部是
`= false`，没有任何一处置 true**（client.go:454–1005）。因此 defer 里的
`c.Pool.Return(...)` 分支（client.go:456）永不可达，每轮都 `conn.Close()`；
`Take` 的池永远为空 → 必然冷拨号；而 `Take` 命中后才触发的 Warm（client.go:399–409）
也随之失效。这与日志 26/26 冷启动完全吻合。浏览器对照：其空闲连接可活 50s（§2.1），
服务端明确支持长闲置，我们却把每个连接用一次就扔。

### 5.3 优化清单（按预期收益排序，均附数据依据）

| # | 措施 | 预期收益（依据） |
|---|---|---|
| O1 | **修 returnConn bug**：成功返回前把连接交还池并保留读泵（ping 应答）。热请求直接跳过 dial+handshake | 省 **773ms（P50）～1 818ms（P95）**，即现总延迟的 ~20%；浏览器证明空闲 50s 的连接可用（§2.1），我们 GC 2min TTL 更保守 |
| O2 | **账号级主动预热**：token 刷新成功/健康检查通过时立即后台 Warm（现在只有完成后才触发，首次请求必然冷）。对应浏览器 UserInputStart 即拨号（§1.2，12/12） | 首次请求省 **~773ms**；把 P95 长尾（慢建连 1.9–19s，§1.2 har4#201）移出用户可见路径 |
| O3 | **放宽 Warm 节流**：connpool.go:81–86 只要池内有 <30s 的连接就整体跳过预热；改为"按 maxPoolPerKey 补足缺口"，并在 Warm 成功日志中带上 key 便于观测命中率（现为 0%） | 保证 O1/O2 在多账号并发下实际生效 |
| O4 | **断流重连改用 AttachToSession**：上游中断时新建 WS 后先发 AttachToSession 续接会话再决定是否整轮重试。参考 har4#546：握手 1 402ms + attach 帧 1ms，无需重放上下文 | 整轮重试成本 P50 **6 775ms** → 约 1.4s，且避免重复计费/限速暴露 |
| O5 | **REST 连接复用核查**：substrate REST 平均 25 头/1 157B，浏览器靠 h2+h3 两连接承载全部 API（§3）。确认 outbound.HTTPClient 未禁 keep-alive、UploadFile 与 WS 共享 DNS/TLS 热 path | UploadFile（图片场景串行阻塞 chat，client.go:466–471）省 connect 往返 40–2 015ms |
| O6 | **首 delta 差距归因复测**：浏览器 payload→首 update P50 **450ms** vs 我们 **1 362ms**（3 倍）。payload 同量级（3.6KB vs 3.5KB），streamingMode 相同；差异主要来自网络路径（代理/无 TLS session 复用）。O1/O2 落地后在热连接上复测 first_delta_ms，剩余差距才是真实推理排队 | 明确优化天花板，防止误判服务端慢 |

不采纳的浏览器做法：每轮新建 WS（受页面生命周期约束，方向与我们相反）；
h3 REST（Go net/http 无原生 h3 客户端，收益限于非关键 REST）。

## 6. 数据文件

- 解析产物（临时目录）：`chats2.json`（WS 清单+13 次聊天时序）、`frame_samples.json`（每连接前 4 帧）、`http_report.json`（host/token 统计）
- 关键证据定位：Metrics 帧=各 har chat send f3 尾部 `"Timestamps"` 字段；
  AttachToSession=har4#546 f3；preconnect=m365.cloud.microsoft/chat document HTML head

---

## 落地清单

> 本报告的性能优化措施汇总（O1-O6，按预期收益排序），详细依据见上文 §5.2。

- [ ] **O1（P0）** 修 returnConn 死代码 bug：成功返回前把连接交还池并保留读泵（预期省 P50 773ms / P95 1818ms）
- [ ] **O2** 账号级主动预热：token 刷新成功/健康检查通过即后台 Warm，把首次请求与慢建连长尾移出用户可见路径
- [ ] **O3** 放宽 Warm 节流：按 maxPoolPerKey 补足缺口而非整体跳过，Warm 日志带 key 观测命中率（现为 0%）
- [ ] **O4** 断流重连改用 AttachToSession 续接会话再决定是否整轮重试（成本 P50 6775ms → 约 1.4s）
- [ ] **O5** REST keep-alive 复用核查：确认 outbound.HTTPClient 未禁 keep-alive，UploadFile 与 WS 共享 DNS/TLS 热路径
- [ ] **O6** O1/O2 落地后在热连接上复测 first_delta_ms（浏览器 P50 450ms vs 我们 1362ms），剩余差距才是真实推理排队
