create table tool_registry (
    tenant_id text not null,
    tool_name text not null,
    version text not null,
    description text not null default '',
    "schema" jsonb not null,
    endpoint text not null,
    is_default boolean not null default false,
    created_at timestamptz not null default now(),
    primary key (tenant_id, tool_name, version)
);

create unique index tool_registry_default_idx on tool_registry (tenant_id, tool_name) where is_default;

create table tool_invocations (
    id uuid primary key,
    tenant_id text not null,
    run_id uuid,
    tool_name text not null,
    version text not null,
    args jsonb not null,
    result jsonb,
    status text not null,
    created_at timestamptz not null default now()
);

create index tool_invocations_tenant_run_idx on tool_invocations (tenant_id, run_id);
