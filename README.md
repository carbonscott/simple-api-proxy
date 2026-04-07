# simple-api-proxy

A reverse proxy that sits between users and an OpenAI-compatible API. You give each user a proxy key; the proxy validates it and injects the real API key before forwarding upstream. Users never see the actual key.

**This is a vibe-coded prototype.** It was built quickly to solve a specific problem (sharing a Stanford AI Gateway key with a small team). It works, but it has not been audited, load-tested at scale, or hardened for hostile environments. Use with caution.

## Quick start

```bash
# Build
go build -o simple-api-proxy .

# Create a key for a user
./simple-api-proxy add -db keys.db alice

# Put your real API key in a file
echo -n "sk-your-real-key" > key.dat

# Start the proxy
./simple-api-proxy serve -db keys.db -apikey key.dat -upstream https://your-api.example.com

# Test it
curl http://localhost:4000/health
curl http://localhost:4000/v1/models -H "Authorization: Bearer pk-<alice's key>"
```

## CLI reference

```
simple-api-proxy <command> [flags]
```

### serve

Start the proxy server.

```
simple-api-proxy serve [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `-port` | `4000` | Listen port |
| `-db` | `keys.db` | Path to SQLite database |
| `-apikey` | `~/.config/simple-api-proxy/key.dat` | Path to real API key file |
| `-upstream` | `https://aiapi-prod.stanford.edu` | Upstream base URL |
| `-admin-token-file` | (none) | Path to admin token file |

The admin token can also be set via the `ADMIN_TOKEN` environment variable. If neither is set, the admin API is disabled.

### add

Generate a proxy key for a user.

```
simple-api-proxy add -db keys.db <username>
```

The key is printed once and never shown again.

### revoke

Revoke a user's proxy key.

```
simple-api-proxy revoke -db keys.db <username>
```

### list

List all users and their key prefixes.

```
simple-api-proxy list -db keys.db
```

### migrate

One-time migration from the old JSON format to SQLite.

```
simple-api-proxy migrate -from keys.json -db keys.db
```

**Note:** Flags must come before positional arguments (Go's `flag` package stops parsing at the first non-flag argument).

## Admin API

When an admin token is configured, the proxy exposes management endpoints at `/admin/keys`. All requests require `Authorization: Bearer <admin-token>`.

### List keys

```bash
curl http://localhost:4000/admin/keys \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

### Add a user

```bash
curl -X POST http://localhost:4000/admin/keys \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"username":"alice"}'
```

Returns the full key once. Save it — it won't be shown again.

### Revoke a user

```bash
curl -X DELETE http://localhost:4000/admin/keys/alice \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

Key changes take effect immediately — no restart needed.

## Docker

```bash
# Build
docker build -t simple-api-proxy .

# Run
docker run -d -p 4000:4000 \
  -v /path/to/data:/data \
  -v /path/to/key.dat:/etc/secrets/key.dat:ro \
  -e ADMIN_TOKEN=your-admin-secret \
  simple-api-proxy
```

The SQLite database lives at `/data/keys.db` inside the container. Mount a volume there so it persists across restarts.

## Kubernetes

Manifests are in `k8s/`. The basics:

```bash
# Set up secrets (edit the .example files first)
cp k8s/secret.yaml.example k8s/secret.yaml       # real API key
cp k8s/admin-secret.yaml.example k8s/admin-secret.yaml  # admin token

# Deploy
kubectl apply -f k8s/
```

See `k8s/deployment.yaml` for the full setup. Note: SQLite requires `replicas: 1`.

## Configuration

| Setting | Flag | Env var | Default |
|---------|------|---------|---------|
| Listen port | `-port` | — | `4000` |
| Database path | `-db` | — | `keys.db` |
| API key file | `-apikey` | — | `~/.config/simple-api-proxy/key.dat` |
| Upstream URL | `-upstream` | — | `https://aiapi-prod.stanford.edu` |
| Admin token | `-admin-token-file` | `ADMIN_TOKEN` | (disabled) |

## Known limitations

This is a prototype. Things it does **not** have:

- **Rate limiting** — one user can consume the entire upstream API quota
- **Usage tracking** — no logging of tokens used or costs incurred
- **TLS** — serves plain HTTP; put it behind a reverse proxy (Caddy, nginx) for HTTPS
- **Horizontal scaling** — SQLite is single-writer, so `replicas: 1` only
- **Constant-time token comparison** — admin token auth uses simple string equality
- **Automated tests** — tested manually, no test suite
- **Graceful key rotation** — revoke + re-add is the only way to rotate a key

If any of these matter for your use case, you'll need to add them.
