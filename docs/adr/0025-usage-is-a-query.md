# 用量是查询，不是表

Status: accepted

`GET /v1/tenants/{id}/usage` 直接聚合 `model_call_logs`，按 `(tenant, model)` 汇总，并可下钻到具体 Run。不建任何用量表、不写任何汇总行。

## Considered Options

- **用量汇总表**：需要一条写路径来维护它，而这条写路径与 `model_call_logs` 的写入不是同一个事务时，两边会对不上；是同一个事务时，它只是同一份事实的第二种表示。

## Consequences

- 报表口径与计费口径只有一处真源：`model_call_logs`。任何"账对不上"的问题都只需要查这一张表。
- 聚合成本随调用量线性增长。demo 规模下可以接受；如果以后真的成为瓶颈，优化对象是索引与查询，而不是先把第二份数据写出来。
- `model_call_logs` 的字段一旦漏记（例如没记 `effective_model`），用量查询就无法补出这个维度。写入时的完整性与这张表同等重要。
