create table model_routes (
    tenant_id text not null,
    label text not null,
    chain jsonb not null default '[]',
    created_at timestamptz not null default now(),
    primary key (tenant_id, label)
);
