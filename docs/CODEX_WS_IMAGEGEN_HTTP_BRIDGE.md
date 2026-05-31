# Codex WebSocket 图片生成 HTTP 桥接

## 背景问题

Codex 客户端通过 **WebSocket** 连接 sub2api，sub2api 再用 WebSocket 把请求转发到
`chatgpt.com`（OAuth 账号上游）。在这条链路上请求"生成一张海报"之类的图片生成任务时，
模型会反复声称没有内置的图片生成工具，转而回退到本地 SVG/HTML/脚本渲染，无法产出真实位图。

### 根因

`chatgpt.com` 的 **WebSocket 后端会剥离外部注入的 `image_generation` server tool**。
无论 sub2api 在 WS payload 里如何注入图片生成工具，到达上游后都会被丢弃，模型因此判定
"没有原生生图工具"并回退到本地渲染。

对照参考实现 CRS（claude-relay-service）一直可用，原因是它**全程使用 HTTP POST 转发**，
而 `chatgpt.com` 的 HTTP 端点 `/backend-api/codex/responses` **接受外部注入的工具**。
这就是 WS 与 HTTP 两条路径的本质差异。

## 解决方案：WS 入口 + 图片生成 turn 转走 HTTP（"WS→HTTP 桥接"）

客户端侧保持不变（Codex 仍用 WS 连接 sub2api，无需改动客户端配置）。在 sub2api 内部，
**只把"图片生成 turn"这一类请求改走 HTTP POST 打到 chatgpt.com**，其余请求照常走 WS。

对客户端而言它仍是一个纯 WS 网关；对上游而言生图请求被"降级"为 HTTP，从而获得
HTTP 才具备的"接受注入工具"能力。

### 关键点：turn 的定位

Codex 的 WS 会话是**多轮（multi-turn）**的：

| turn          | 内容                                              | `input` 字段          |
| ------------- | ------------------------------------------------- | --------------------- |
| 首条消息 / turn 1 | 会话初始化：注入 instructions、tools、环境/developer 上下文 | **没有真正的用户消息** |
| turn 2+       | 用户真正的提问（如"生成一张海报"）                  | **在这里**            |

早期实现只在**首条消息**上做桥接判断：首条消息 `bridge_applied=true`（工具注入成功），
但 `WSPayloadHasInput=false`（没有用户消息），桥接不触发；而真正携带用户消息的 turn 2+
走的是 `ProxyResponsesWebSocketFromClient` 内部循环，**完全绕过了桥接**，又被上游 WS 剥掉
工具而失败。

### 最终拦截点：`BeforeRequest` 钩子

把拦截点从"首条消息"挪到 `BeforeRequest` 钩子（它只在 `turn > 1` 触发，正好是用户消息
所在的轮次）：

```
首条消息：判定 bridge_applied → 置 imageBridgeActive = true（仅标记本连接启用桥接，不实际转发）
        ↓
turn 2+（BeforeRequest 钩子）：
    if imageBridgeActive && WSPayloadHasInput(payload):   // 这一轮带用户消息
        ProxyImageGenViaHTTP(...)  // HTTP POST 到 chatgpt.com，注入 image_generation 工具
        成功 → 把上游 SSE 逐条回放成 WS 消息，再以 StatusNormalClosure 正常关闭
        失败 → return nil，回落正常 WS（不断连，避免触发 Codex 重连风暴）
```

## `ProxyImageGenViaHTTP` 内部细节

1. **只用桥接提示词当 instructions，丢掉约 13KB 的 Codex 系统提示。**
   原始 instructions 是面向"编码助手"的庞大系统提示，若带上，模型会从"编码上下文"出发
   生图（曾出现生成代码场景图而非用户想要的海报的问题）。因此只保留聚焦的
   `codexImageGenerationBridgeText`，并将用户真实的 `input` 原样带上——与 CRS 用
   `CODEX_CLI_INSTRUCTIONS` 替换 instructions 的做法一致。

2. **SSE 中继的健壮处理：**
   - 使用 32MB 的 scanner 缓冲，应对 `partial_image` 的大段 base64 数据；
   - `chatgpt.com` 的流通常不发送 `[DONE]` 即直接断开，因此**收到 `response.completed`
     事件后，即使随后遇到 EOF 也判定为成功**。

## 涉及的代码

- `backend/internal/handler/openai_gateway_handler.go`
  - `ResponsesWebSocket`：新增 `imageBridgeActive` 标记；在 `BeforeRequest` 钩子中拦截
    携带用户消息的 turn 2+，转走 HTTP 桥接，成功后正常关闭、失败回落 WS。
- `backend/internal/service/openai_gateway_service.go`
  - `ProxyImageGenViaHTTP`：构造 HTTP 请求打到 `chatgpt.com/backend-api/codex/responses`，
    注入 `image_generation` 工具，并把上游 SSE 回放为 WS 消息。
  - `WSPayloadHasInput`：判定一个 WS payload 是否携带用户消息（区别于纯工具结果轮次）。
  - `ApplyCodexImageGenerationBridgeToWSPayload`：在满足条件（官方 Codex 客户端、分组允许
    生图、桥接开关开启）时注入工具与桥接指令，作为 `imageBridgeActive` 的判定依据。

## 一句话总结

客户端继续走 WS，sub2api 把携带用户消息的生图 turn 在 `BeforeRequest` 钩子里截下来、
改用 HTTP POST 打 `chatgpt.com` 的 Responses 端点（HTTP 才接受注入的 `image_generation`
工具），SSE 回放后正常收尾——本质是绕开 chatgpt.com WS 后端"剥工具"的限制。
