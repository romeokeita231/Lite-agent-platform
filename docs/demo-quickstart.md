# Demo quickstart (Windows amd64)

This zip is a demo distribution, not the production delivery form. Production remains one platform binary plus policy and Postgres containers ([ADR 0027](adr/0027-demo-zip-not-a-delivery-form.md)). Windows amd64 is the only supported demo platform.

## Run

1. Extract the zip anywhere outside the repository.
2. In the extracted directory:

   ```powershell
   .\platform.exe demo
   ```

The command does not need Go, Docker, or a test toolchain. It unpacks the bundled Postgres 16.4 under `%LOCALAPPDATA%\LiteAgentPlatform\demo`, starts Postgres, runs migrations and seed, starts `policy-example.exe` as a child process, and serves the HTTP API and console.

When it is ready it prints:

```
demo stack ready
console: http://127.0.0.1:8080/console
tenant demo-a api key: ...
tenant demo-b api key: ...
```

Open the console URL and paste one of the tenant keys. The console is read-only; it lists runs, shows Checkpoints and Spans, and answers a `waiting_human` run.

## Overrides

| Variable | Default | Meaning |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | HTTP and console listen address |
| `DATABASE_URL` | `postgres://postgres:postgres@127.0.0.1:5433/platform?sslmode=disable` | Postgres the demo starts |
| `POLICY_ADDR` | `:9090` | Example Policy listen address |
| `METRICS_ADDR` | `127.0.0.1:9091` | Prometheus metrics listener |
| `DEMO_DATA_DIR` | `%LOCALAPPDATA%\LiteAgentPlatform\demo` | Data directory and logs |

Ports 8080, 9090 and the loopback metrics port 9091 are the same ones the compose stack uses, so the demo and `docker compose` are mutually exclusive. The database port 5433 is not used by compose. Set the variables above to move the ports instead.

## Reset and stop

Tenant data survives restarts. `--reset` deletes the data directory before starting, which clears all tenants, agents, runs and Checkpoints:

```powershell
.\platform.exe demo --reset
```

Ctrl-C stops the stack and every child process (Postgres and the Policy).

## Troubleshooting

- `initdb` failing with a missing `VCRUNTIME`/`MSVCP` DLL means the Visual C++ runtime is absent. Install it from <https://learn.microsoft.com/en-us/cpp/windows/latest-supported-vc-redist>.
- A port already in use message means the compose stack or another process owns the port. Stop it or set the variable for that port.
