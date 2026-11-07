create table eval_reports (
    id text primary key,
    tenant_id text not null,
    status text not null,
    cases_snapshot jsonb not null,
    results jsonb,
    created_at timestamptz not null default now(),
    ended_at timestamptz
);

create index eval_reports_tenant_idx on eval_reports (tenant_id);

alter table runs add column if not exists eval_id text;
