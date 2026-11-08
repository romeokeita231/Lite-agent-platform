create table model_call_logs (
    tenant_id text not null,
    run_id uuid,
    route_label text not null default '',
    requested_model text not null,
    chain jsonb not null default '[]',
    effective_model text not null default '',
    fallback_from text not null default '',
    input_tokens int not null default 0,
    output_tokens int not null default 0,
    latency_ms bigint not null default 0,
    status text not null,
    created_at timestamptz not null default now()
);

create index model_call_logs_tenant_run_idx on model_call_logs (tenant_id, run_id);
