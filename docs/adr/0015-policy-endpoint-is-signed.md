# 平台调 Policy 用 HMAC 签名

Status: accepted

Policy 是一个平台每一步都要打的 HTTP endpoint。用 `(agent_id, secret)` 对 `(method, path, body 哈希, 时间戳)` 做 HMAC，Policy 侧校验签名与 5 分钟时间窗。共享 bearer token 被否掉，因为泄漏即全开；mTLS 被否掉，因为在 demo 规模下证书的成本不成比例。

## Consequences

- 顺带解决一个真实风险：Policy 的 endpoint 一旦被外人知道，没有签名就等于谁都能让它替自己的输入做决策。
- 只校验签名与时间窗，不存 nonce；乱序由时间窗兜住。少一张表，代价是窗口内的重放没有被挡住——这个代价明写在这里，别当成疏漏。
- 与租户 API Key 是两套凭证：前者证明"这是平台"，后者证明"这是哪个租户"。**不要合并成一套**，合并之后两个问题都没有答案。
