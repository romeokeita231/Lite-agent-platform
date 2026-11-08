alter table runs add constraint runs_status_check
  check (status in ('queued','running','waiting_human','completed','failed','cancelled'));
alter table runs add constraint runs_terminal_reason_check
  check (terminal_reason is null or terminal_reason in
         ('step_timeout','policy_unreachable','max_retries',
          'tool_schema_violation','policy_invalid_action','cancelled_by_tenant'));
