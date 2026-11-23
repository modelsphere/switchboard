# switchboard

Control plane for the ModelSphere LLM gateway (Higress): backends and routing
decisions, API keys and model access, model listing. One Go service; modules
are added one at a time.

## Modules

| Module | What it does | Docs |
|---|---|---|
| models | `GET /v1/models` and `GET /v1/models/{id}` (OpenAI shape), listing only the models the calling key may use. Derived from the gateway's own per-model Ingresses and key-auth `allow` lists, so the list cannot disagree with what the gateway enforces | [design](docs/models-api-design.md), [deploy](docs/deploy.md) |

Enforcement stays in Higress. switchboard only reads gateway config, with
read-only RBAC.

## Develop

```bash
make check                 # gofmt, go vet, unit tests (what CI runs)
make build                 # bin/switchboard
go run ./cmd/switchboard --kubeconfig ~/.kube/config
make docker-build IMG=switchboard:dev
```

Releases: push a `X.Y.Z` tag; `release.yml` publishes
`4pdosc/switchboard:X.Y.Z` (and `latest` for non-prereleases).
