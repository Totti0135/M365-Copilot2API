# HAR 挖掘任务基线与范围

记录时间：2026-08-31。

## 处理边界

- 只读取 `D:\M365-Copilot2API-dev` 与 `D:\Users\Downloads` 下的实际 HAR 文件。
- 不修改生产目录，不访问端口 4141，不提交、不推送、不打标签、不发布。
- 不处理或修改 `scripts/provision-accounts.ps1`。
- 保留工作树中全部已有及未知修改，不执行 reset、checkout、clean、删除或覆盖。
- HAR 仅在本机分析，不上传到任何外部服务。
- 文档不得记录 token、cookie、OID、TID、邮箱、会话 ID 或 URL 查询敏感值原文。

## 指令文件基线

- 已读取全局 `C:\Users\范千韶\.config\opencode\AGENTS.md`。
- `D:\M365-Copilot2API-dev` 内未找到项目级 `AGENTS.md`。

## 工作树基线

分支：`dev-pr-integration`，跟踪 `origin/dev-pr-integration`。

基线时已有修改：

```text
 M TODO.md
 M internal/web/codex_responses_compat_test.go
 M internal/web/conversation_cache.go
 M internal/web/protocol_compat_test.go
 M internal/web/server.go
 M internal/web/tool_state.go
?? internal/web/conversation_cache_test.go
?? scripts/provision-accounts.ps1
```

这些文件均视为既有工作。本任务只新增或更新 `docs/har-mining` 文档，不修改业务代码及上述既有文件。

## HAR 文件清单

共发现 16 个实际 `.har` 文件。按 SHA-256 去重后为 10 组独立内容，其中六组 M365 文件各存在一个同内容别名。所有文件均可解析为 HAR JSON。

| 文件 | 字节 | entries | WS entries | WS frames | SHA-256 |
|---|---:|---:|---:|---:|---|
| `har1.har` | 83173214 | 1105 | 7 | 391 | `37AF412B...BAAE293C` |
| `har2.har` | 15106138 | 172 | 0 | 0 | `D872C857...2D2118B1` |
| `har3.har` | 25429182 | 230 | 1 | 7 | `94BBA4F7...5DD7A268` |
| `har4.har` | 71864464 | 560 | 9 | 541 | `84EB2454...31187B6` |
| `har5.har` | 38998424 | 127 | 1 | 10 | `0D2D08AB...E99589` |
| `har6.har` | 1144372 | 36 | 0 | 0 | `EA132E92...BD0D0DF` |
| `m365.cloud.microsoft.har` | 83173214 | 1105 | 7 | 391 | `37AF412B...BAAE293C` |
| `m365.cloud.microsoft2.har` | 15106138 | 172 | 0 | 0 | `D872C857...2D2118B1` |
| `m365.cloud.microsoft3.har` | 25429182 | 230 | 1 | 7 | `94BBA4F7...5DD7A268` |
| `m365.cloud.microsoft调整设置.har` | 1144372 | 36 | 0 | 0 | `EA132E92...BD0D0DF` |
| `m365.cloud.microsoft临时会话.har` | 71864464 | 560 | 9 | 541 | `84EB2454...31187B6` |
| `m365.cloud.microsoft生图更多测试.har` | 38998424 | 127 | 1 | 10 | `0D2D08AB...E99589` |
| `tank.zard.loc.cc.har` | 4540130 | 68 | 9 | 4701 | `5F569A84...925F7DAB` |
| `work.weixin.qq.com.har` | 783002 | 24 | 0 | 0 | `C2F900A9...5115F327` |
| `work.weixin.qq.comflash.har` | 466155 | 15 | 0 | 0 | `9B08BC63...DEF20F6` |
| `www.scnet.cn.har` | 129196 | 14 | 0 | 0 | `878DBA82...6BD65112` |

## 既有文档基线

分析开始前，`docs/har-mining` 包含 `README.md` 及编号 01 至 08 的八份报告。既有文档中已观察到 U+FFFD 替换字符，因此最终质量扫描必须区分基线乱码与本任务新增乱码，并修正文档范围内可确定的损坏文本。

## 证据口径

- `confirmed`：HAR 中可直接定位并重复解析出的字段、帧、状态码或时序。
- `strong inference`：由多个独立样本或代码与 HAR 的一致关系支持，但 HAR 未直接声明语义。
- `unknown`：样本缺失、字段未出现、方向不完整或无法排除其他解释。
- 每项结论必须给出 HAR 文件名、entry 或 WebSocket frame 定位方式、方向、字段路径、脱敏示例、置信度及证据边界。
