alter table tenants add column api_key_hash text;
alter table tenants add column api_key_prefix text;
create unique index tenants_api_key_hash_key on tenants (api_key_hash);

create table agents (
    id text primary key,
    tenant_id text not null,
    name text not null,
    policy_url text not null,
    secret_env text not null,
    created_at timestamptz not null default now(),
    unique (tenant_id, name)
);

create table runs (
    id uuid primary key,
    tenant_id text not null,
    agent_id text not null,
    status text not null default 'queued',
    input jsonb not null,
    output jsonb,
    attempt int not null default 0,
    next_attempt_at timestamptz,
    pending_step jsonb,
    human_prompt text not null default '',
    deadline timestamptz,
    terminal_reason text,
    eval_id text,
    cancel_requested boolean not null default false,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    ended_at timestamptz
);

create index runs_runnable_idx on runs (status, next_attempt_at);
create index runs_tenant_idx on runs (tenant_id);
