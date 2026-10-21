# Qoder 渠道接入设计（COSY 签名 + 私有 /algo 协议）

> 状态：已实现（`internal/qoder/` + `internal/relay/qoder.go`）
> 上游依据：9router `open-sse/shared/qoder/*`、`open-sse/services/qoderModels.js`、
> `open-sse/executors/qoder.js`、`src/lib/oauth/services/qoder.js`（2026-10 版）

## 1. 为什么改

早期实现只移植了 9router 的 **授权** 部分（device-token 轮询），却把渠道当成 OpenAI
兼容上游使用（`base_url=https://api3.qoder.sh` + 通用 `/v1/chat/completions` 转发）。
Qoder 上游早已没有 OpenAI 兼容面，实测：

| 请求 | 结果 |
| --- | --- |
| `GET https://api3.qoder.sh/v1/models` | `404`（端点不存在） |
| `GET https://api3.qoder.sh/algo/api/v2/model/list` | `403 {"code":"101","message":"Signature invalid"}`（真端点，要 COSY 签名） |
| `GET https://openapi.qoder.sh/api/v1/userinfo` | `401`（真端点，Bearer） |

所以「拉取模型 404」不是 token 失效，而是打了不存在的路径；聊天同理。**认证流程本身没变**，
变的是认证之后怎么调上游。

## 2. 协议全景

```
设备流授权（不变）
  本地生成 PKCE(S256) + nonce + machine_id
  → https://qoder.com/device/selectAccounts?challenge=…&nonce=…&machine_id=…
  → 轮询 GET {openapi}/api/v1/deviceToken/poll?nonce=…&verifier=…&challenge_method=S256
      202/404 = pending，200 + {token: dt-…} = 成功（约 30 天）
  → GET {openapi}/api/v1/userinfo（Bearer dt-…）→ userId ★ COSY 签名必需

推理（每次请求都要 COSY 签名）
  POST {chat}/algo/api/v2/service/pro/sse/agent_chat_generation
       ?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1
  body = WAF 混淆编码后的 Qoder payload（内部含该模型的 model_config）
  → SSE，每行 data: {statusCodeValue, body}，body 才是 OpenAI 风格 chunk

模型目录
  GET {chat}/algo/api/v2/model/list（COSY 签名，空 body）
  → {chat:[{key, display_name, enable, max_input_tokens, max_output_tokens,
            is_vl, is_reasoning, source, context_config:[…]}]}
  ⚠️ chat[] 条目本身就是转发必须回传的 model_config（传错会静默降级模型）
```

区域端点（形状完全一致，只有主机不同）：

| | 国际站 `qoder` | 国内站 `qoder-cn` |
| --- | --- | --- |
| 设备流 / userinfo / 配额 | `openapi.qoder.sh` | `openapi.qoder.com.cn` |
| token 刷新 | `center.qoder.sh` | `gateway.qoder.com.cn` |
| 推理 / 模型目录 / 图床上传 | `api3.qoder.sh`（`jt-…` 走 `api2.qoder.sh`） | `gateway.qoder.com.cn` |
| 授权页 | `qoder.com/device/selectAccounts` | `qoder.com.cn/device/selectAccounts` |

## 3. COSY 签名（`internal/qoder/cosy.go`）

每次请求构造：

1. `AES-128-CBC`（16 字符 key，取自新 UUID 前 16 字符；IV 复用 key）加密
   `{uid, security_oauth_token, name, aid, email}`；
2. AES key 用 RSA(PKCS#1 v1.5) 公钥包裹；
3. `payload = {version:"v1", requestId, info:<AES密文>, cosyVersion:"1.0.0", ideVersion:""}`，
   base64 后与 `cosyKey`、时间戳、**请求体原文**、`sigPath` 以 `\n` 连接取 MD5 → `sig`；
4. 头：`Authorization: Bearer COSY.<payloadB64>.<sig>` + 17 个 `Cosy-*` 头
   （`Cosy-Key/User/Date/Machineid/Machinehash…`）+ `Login-Version: v2` + `X-Request-Id`。

要点：

- **签名对象是最终发出的字节**：对话请求要先做 WAF 编码，再用编码后的 body 签名与算 `Cosy-Bodyhash`。
- `Cosy-Sigpath` 是 URL path **去掉前导 `/algo`** 后的值。
- `Accept-Encoding` 必须是 `identity`（gzip 会触发 CDN 的签名校验）。
- 每次请求 AES key 与 requestId 都是新的；同一签名不可重放（上游会回 `403/103 Duplicate request`）。

## 4. 正文编码（`internal/qoder/encoding.go`）

标准 base64 → 切成三等份重排为 `[尾][中][头]` → 按自定义字母表替换（`=` 替成 `$`），
配合 URL 上的 `Encode=1` 让上游逆序解码。目的是绕开阿里云 WAF 对明文请求体的特征匹配。
编码输出全是 ASCII，长度与标准 base64 一致。

## 5. 请求体映射（`internal/qoder/request.go`）

OpenAI chat/completions（KeyGrid 的 pivot）→ Qoder payload：

- `messages[role=system]` 全部抽出，用 `\n\n` 拼进 `payload.system`（上游拒绝 messages 里的 system）；
- 纯文本 content 扁平化成字符串；含图片时保留 block 数组（`image_url` 形状）；
- `tools` 原样透传；
- `parameters.max_tokens`：模型 `max_output_tokens` 与客户端 `max_tokens`/`max_completion_tokens` 取小；
- `model_config`：目录里的原始条目原样回传（**缺了直接报错**，不猜）；
- `chat_record_id` / `session_id`：由内容与 `userId+modelKey` 派生的稳定短哈希；
- `stream` 固定 `true`（上游只支持 SSE，非流式由 KeyGrid 折叠）；
- `business` 块提供客户端指纹（product=cli、version=1.0.0、type=agent）与会话标题。

## 6. 响应回写（`internal/qoder/sse.go` + `internal/relay/qoder.go`）

上游 SSE 逐行解信封 `{statusCodeValue, body}`：

- **首帧错误**（余额/配额/参数/登录过期）在写 200 之前转成真实 HTTP 状态；
  其中计费/配额拦截（`code` 110/112/10605 或含 `pricingUrl`）统一映射成 **429**，
  让 failover 继续尝试其它渠道。
- 流中错误：发结构化错误块（billing → `error.code=qoder_billing_block`）后终止。
- `finish_reason` 常挂在 `delta` 上；usage 在 finish 之后单独的 `choices: []` 帧里。
  拆封器把两帧合并成一条 `{choices:[{delta:{},finish_reason}], usage}` 终端块，
  否则下游客户端读不到 usage。
- 上游发完终止帧后往往仍保持 socket（agent keepalive）→ 见到终止帧立刻关闭上游连接。
- 非流式客户端：读完整个 SSE 后用 `workbuddy.CollapseSSE` 折叠成一条 `chat.completion`。
- 入口协议桥接复用 `bridgeChatChunksToEntry`（anthropic / responses 入口无需特殊处理）。

## 7. 附件与上下文档位

- **图片**：官方 qodercli 不把字节塞进对话请求，而是 `PUT {chat}/algo/api/v2/image/upload`
  （COSY 签名 multipart，字段 `file`）拿 OSS URL。`internal/qoder/attachments.go` 按此上传，
  同请求内按 sha256 去重；>10MB 或上传失败的大图替换成占位文本（<512KB 的小图保留内联）。
- **非图片文件/PDF**：一律替换成 `[file omitted: …]` 占位（上游通过文件 API 读取）。
- **上下文档位**（`internal/qoder/tier.go`）：`model_config.context_config` 里有 200K/400K/1M
  三档，而 `max_input_tokens` 只反映 IDE 当前选中档。按 prompt 体量（CJK 1 字≈1 token、
  其余 4 字符≈1 token，含 15% 余量）选「装得下的最小档」，并同步写入
  `parameters.context_length`、`chat_context.extra.ideModelConfigOverride.max_input_tokens`、
  `model_config.max_input_tokens`。策略由 `QODER_CONTEXT_TIER` 控制（`auto`|`max`|`default`|档位名）。

## 8. PAT（`pt-…`）

PAT 无法直接签名，必须先兑换短时 job token：

```
POST {openapi}/api/v1/jobToken/exchange   {"personal_token":"pt-…"}   普通 JSON，不签名
→ {token:"jt-…", expires_in|expires_at}
→ GET {openapi}/api/v1/userinfo（Bearer jt-…）→ userId
```

- 兑换结果按 `region+pt` 进程内缓存，剩余有效期 <5min 时重换（默认 TTL 24h）。
- **国际站的 `jt-…` 必须走 `api2.qoder.sh`**：`api3` 对 jt- 回 `403 Login expired`。
  国内站所有 token 都走单一网关。
- 渠道形态：`kind=api_key` + `base_url` 指向 Qoder 主机（预设 `qoder-pat`），relay 靠 host 识别。

## 9. 渠道识别与预设

`qoder.IsQoder(p)` 的判定（`internal/qoder/provider.go`）：

- `oauth_provider ∈ {qoder, qoder-cn}`，或
- `base_url` host 命中 `api3/api2/openapi/center.qoder.sh`、`gateway/openapi.qoder.com.cn` 等。

预设（`internal/oauth/presets.go`）：

| key | kind | base_url | 说明 |
| --- | --- | --- | --- |
| `qoder` | oauth | `https://api3.qoder.sh` | 国际站 device 流 |
| `qoder-cn` | oauth | `https://gateway.qoder.com.cn` | 国内站 device 流 |
| `qoder-pat` | api_key | `https://api3.qoder.sh` | PAT 直填 |

`base_url` 只承载主机名语义：具体路径（`/algo/...`）由 `internal/qoder` 拼，
relay 不再对 Qoder 渠道拼 `/v1/chat/completions`。

## 10. 与模型目录/测连的衔接

- `POST /api/providers/{id}/models` 与 `POST /api/providers/probe_models`：
  Qoder 分支走 COSY 签名的 `/algo/api/v2/model/list`（`forceRefresh`），返回全部 key
  （含 `enable:false` 的隐藏模型 —— 上游仍可路由）。
- `POST /api/providers/{id}/test`：改为打 `userinfo`（PAT 先兑换），与 9router 同策略；
  不发对话请求，不消耗额度。

## 11. 已知限制

- **device token 不能刷新**：上游 refresh 端点对 `dt-…` 回 403 → `Refresh()` 返回
  `refresh_not_supported`，凭据按不可恢复处理（标 revoked），用户需重新授权（约 30 天一次）。
- 存量凭据可能只有 `machineId` 没有 `userId`（旧版本授权）：relay 首次使用时调一次
  `userinfo` 补齐并进程内缓存（重启后重取一次）。
- 上游对话只支持流式；非流式客户端由我们折叠，首个 token 延迟等于上游开始输出的时间。
- 附件仅支持图片上传；PDF/其它文件以占位文本代替（不解析内容）。
