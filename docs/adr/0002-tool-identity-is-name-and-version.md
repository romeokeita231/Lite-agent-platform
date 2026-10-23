# 工具的身份是 (name, version)，不是 name

Status: accepted

`tool_registry` 原设计以 `tool_name` 为主键，同时带一列 `version`：同一个工具的两个版本会在主键上直接冲突，这张表在 SQL 里建不出来。工具的身份改为 `(tool_name, version)`，另加一列 `is_default` 指定默认版本。

## Consequences

- Invoke 的版本解析规则写死：请求带 `version` 就用它，没带就用 `is_default`。
- 历史版本永久保留、仍可调用。已发生的 Run 必须钉住它当时解析到的版本，否则重放会换实现。
- 权限仍挂在 `tool_name` 上。若将来出现"某个版本参数更敏感、需要单独授权"，才需要拆出 `tool_versions` 表；现在不值得为它多两张大表。
