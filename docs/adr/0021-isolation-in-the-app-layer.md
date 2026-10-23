# 隔离靠应用层，密钥只存哈希

Status: accepted

所有表冗余带 `tenant_id`（包括能从 Run 推导出来的 `checkpoints` 与 `spans`），查询只从 repository 出口走，`tenant_id` 由 context 注入。不用 PostgreSQL 行级安全（RLS）。API Key 只存 SHA-256 哈希与前 8 位明文前缀。

## Considered Options

- **RLS**：死穴是连接池。`SET app.tenant_id` 是连接级状态，连接复用时忘了重置就是跨租户泄漏，而这类错误不会在测试里出现，只会在客户出现时出现。
- **每租户一个 schema**：对 demo 规模是过度设计。

## Consequences

- 冗余存 `tenant_id` 看起来违反范式。它只服务两件事：复合索引 `(tenant_id, ...)`，以及让"忘写过滤"成为一条能被测试抓住的错误。
- 必须有一条测试：repository 的每个方法都带租户过滤。没有这条，这个选择的代价就是真的。
- 密钥不存明文、不提供"找回"。找回的需求本身就是设计错误。
