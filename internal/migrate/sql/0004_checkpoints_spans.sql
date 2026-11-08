create table checkpoints (
    run_id uuid not null,
    tenant_id text not null,
    step_index int not null,
    step_type text not null,
    result jsonb not null,
    created_at timestamptz not null default now(),
    primary key (run_id, step_index)
);

create index checkpoints_tenant_run_step_idx on checkpoints (tenant_id, run_id, step_index);

create table spans (
    id uuid primary key,
    tenant_id text not null,
    run_id uuid,
    step_index int not null,
    attempt int not null,
    name text not null,
    started_at timestamptz not null,
    ended_at timestamptz not null,
    attributes jsonb,
    error text not null default ''
);

create index spans_tenant_run_step_idx on spans (tenant_id, run_id, step_index);
