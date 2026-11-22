# nexus-go
A simple and lightweight way to create and manage remote configurations for your application. Nexus Go allows you to centralize application settings and make them available remotely, making configuration management easier and more flexible.

## Documentation

- [`SPEC.md`](SPEC.md) — storage format and write protocol (the contract any writer must follow).
- [`docs/DESIGN.md`](docs/DESIGN.md) — design decisions and their rationale.

## Best practices for creating remote configs

These are recommendations for whoever registers remote configs. The library does not enforce them.

- **Put the unit in the name of numeric values.** There is no `duration` type: a timeout is an
  `integer` or `float`, and nothing in the value says whether it means milliseconds or seconds.
  Prefer `timeout_ms`, `cache_ttl_s`, `max_upload_mb` over `timeout`, `cache_ttl`, `max_upload`.
- **Choose fallbacks that are safe.** Every `Get*` call takes a fallback, which is used when the
  config is missing, has the wrong type, or has not loaded yet. The fallback should be the value
  that does the least harm if the remote value is unavailable (for example, a new feature
  defaults to off).
- **Never store secrets.** Any application with access to the Redis instance can read every
  scope. Tokens, passwords, and API keys belong in a secret manager.
- **Group values that must change together in one `object`.** Separate configs are reloaded
  together within a scope, but an `object` makes it explicit that the values belong together and
  are edited as one.
- **Follow the naming rules.** Scope and config names must match `^[a-z0-9_-]+$`
  (see [`SPEC.md`](SPEC.md) §2).
- **Remove configs that are no longer read.** Once no application reads a config, delete it, so
  that the panel only shows what is still in use.
