alter table tool_invocations add column if not exists step_index int not null default 0;
alter table tool_invocations add column if not exists idempotency_key text not null default '';
alter table tool_invocations add column if not exists latency_ms bigint not null default 0;
