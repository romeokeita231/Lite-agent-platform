# 对外与内部是两条路由、两套凭证

Status: accepted（2026-11-07 仅措辞澄清，结论不变）

`/v1` 只认租户 API Key，`/internal` 只认平台对 Policy 的 HMAC（ADR 0015），两者互不接受对方的凭证。`/internal/policy/decide` 属于 **Policy 进程**的路由，不是平台进程的路由：平台只暴露 `/v1`、`/healthz`、`/readyz`、`/metrics`。对外清单：Run 的创建/查询/账本/人工输入/取消，模型网关与模型列表，工具列表/调用/注册，Agent 定义，Trace 查询，评测提交与报告，租户用量。

## Consequences

- "这个端点谁能调"永远有唯一答案。一个 mux 挂两套凭证，最典型的事故是内部端点被 API Key 调通——而且它不报错，只安静地发生。
- 新增 `GET /v1/runs/{id}/steps`，原设计没有对应物：客户端不再上报，除了读账本，调用方没有别的途径知道这次执行发生了什么。这个端点是"平台拥有执行"的直接后果。
- 删掉 `checkpoint` / `pause` / `resume` 之后，没有任何对外途径能把一个 Run 从中间推进。推进只发生在平台内部——这正是 ADR 0001 想要的形状。
