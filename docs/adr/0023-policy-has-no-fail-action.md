# Policy 没有 `fail` action

Status: accepted

Policy 的响应只有 `llm_call`、`tool_call`、`human_review`、`done` 四种 action，没有 `fail`。业务上的失败通过 `done` 的 `output` 交回，由业务侧解读。

## Consequences

- 平台不知道一个 Agent 想达成什么，就没有资格判断业务上成没成。让 Policy 宣布失败，等于让平台替业务定义成功，而平台恰恰没有这个信息。
- 一个 Run 只会因为平台原因失败：超时、重试耗尽、坏 action、Schema 不合法、被租户取消。因此 `terminal_reason` 的枚举才封闭得住，按原因分组统计才不会被业务语义污染。
- 业务侧要把失败表达成 output 里的字段（如 `{"status": "rejected", "reason": ...}`），这是业务契约的一部分，平台只负责原样交回。
