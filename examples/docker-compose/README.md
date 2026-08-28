# memstore in Docker Compose -- example only

The shortest path to a running `memstored` for trying memstore on one machine. Token auth, plaintext on loopback, one user. It is deliberately not a deployment: see the caveats at the end before pointing anything but your own laptop at it.

## Run it

```bash
cp .env.example .env
$EDITOR .env                 # change POSTGRES_PASSWORD and MEMSTORE_API_KEY
docker compose up -d --wait  # first run pulls three images and a 270MB model
```

Then, on the same machine, with the `memstore` CLI installed (`go install github.com/matthewjhunter/memstore/cmd/memstore@latest`):

```bash
memstore setup --remote http://localhost:8230/memstore --token "$MEMSTORE_API_KEY"
```

That writes `~/.config/memstore/config.toml`, installs the Claude Code hooks, and registers the MCP server over HTTP. Restart Claude Code and `memory_status` should answer.

`--token` only goes into a `config.toml` that `setup` creates. If you already have one, `setup` leaves it alone and says so; add `api_key = "..."` to it by hand.

## What is running

| Service | Image | Role |
|---|---|---|
| `postgres` | `pgvector/pgvector:pg17` | Facts, vectors, tokens, sessions. Data in the `postgres-data` volume. |
| `ollama` | `ollama/ollama` | Serves the embedding model only. Models in the `ollama-models` volume. |
| `ollama-pull` | `ollama/ollama` | One-shot: pulls `EMBEDDING_MODEL`, exits. `memstored` waits for it. |
| `memstored` | `ghcr.io/matthewjhunter/memstored:latest` | The daemon, on `127.0.0.1:8230`, API under `/memstore/`. |

On the first start `memstored` records `MEMSTORE_DEFAULT_USER` as the owner of the empty database and binds `MEMSTORE_API_KEY` to that user as an admin-scoped token named `legacy`. Every later start is a no-op for both.

## Using an Ollama you already run

If Ollama is already on the host (and has the embedding model), skip the bundled one: set `EMBEDDING_BASE_URL` in `.env` to where it listens and start only the two services you need:

```bash
docker compose up -d --wait postgres memstored
```

On Linux, `host.docker.internal` needs `extra_hosts: ["host.docker.internal:host-gateway"]` on the `memstored` service, or use the host's LAN address. Do not run two Ollamas against one GPU.

## Enabling generation and rerank

Both are off. Extraction, hint generation, and the curator need a chat model; rerank needs a cross-encoder server. Neither is quick to download or fast on CPU, which is why the example does not include them. To turn them on, point `memstored` at endpoints you already have:

```yaml
    environment:
      MEMSTORE_GEN_URL: http://host.docker.internal:11434/v1   # OpenAI-compatible
      MEMSTORE_GEN_MODEL: gemma4
      RERANK_BACKEND: jina
      RERANK_BASE_URL: http://host.docker.internal:8085
      RERANK_MODEL: jinaai/jina-reranker-v2-base-multilingual
```

See `docs/installation.md` for the full list of knobs.

## Caveats

- **Plaintext.** `MEMSTORE_TLS_DISABLED` and `MEMSTORE_INSECURE_PLAINTEXT` are both set. The port is bound to `127.0.0.1` so nothing leaves the host; change that binding and every token and recalled fact crosses the network in the clear.
- **One admin token.** `MEMSTORE_API_KEY` is admin-scoped. For per-device, read-only, or ingest tokens, use `memstore admin issue-token` against the running daemon.
- **One user.** Multi-user isolation exists in the daemon, but this example creates one user and one token. A shared deployment issues a token per person.
- **No OAuth.** The daemon can act as an OAuth resource server when pointed at an authorization server (`MEMSTORE_OAUTH_ISSUER`); with none configured it advertises nothing and challenges with nothing, which is correct here.
- **Model lock-in.** The embedding model is recorded on first use and the daemon refuses to start under a different one, since vectors from two models do not compare. Changing `EMBEDDING_MODEL` later is a deliberate step: stop the daemon, run `memstore admin reset-embeddings --yes` against the database, start again, and the embed queue rebuilds every vector.

## Taking it down

```bash
docker compose down        # keeps the volumes
docker compose down -v     # deletes facts and models
```
