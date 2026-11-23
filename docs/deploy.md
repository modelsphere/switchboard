# Deploying switchboard

switchboard runs as one Deployment in the cluster that runs the Higress
gateway. The Helm chart will live in
[helm-charts](https://github.com/modelsphere/helm-charts) as
`charts/switchboard`. This page lists what that chart has to create, and the
one thing on the gateway side it cannot set up for you.

Image: `4pdosc/switchboard:<version>`, published by `release.yml` when a
`X.Y.Z` tag is pushed. `switchboard --version` prints the version.

## What the deployment needs

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--listen` | `:8080` | HTTP address (`/v1/models`, `/healthz`, `/readyz`) |
| `--model-header` | `x-higress-llm-model` | header model-router writes the request's `model` into (`modelToHeader`) |
| `--ingress-class` | `higress` | ingress class the gateway serves (its `--ingressClass`); empty reads every Ingress, `nginx` also takes Ingresses without a class, as Higress does |
| `--system-namespace` | `higress-system` | gateway namespace; Higress names routes there without the namespace |
| `--key-auth-plugin` | *(auto)* | `namespace/name` of the key-auth WasmPlugin; empty picks the single WasmPlugin in `--system-namespace` whose `url` ends in `key-auth`/`key_auth` |
| `--owned-by` | `modelsphere` | `owned_by` in model objects |
| `--resync` | `0` | informer resync; not needed, the catalog is rebuilt on every change |
| `--debug` | `false` | debug logs |

Probes: liveness `GET /healthz`, readiness `GET /readyz` (ready once both
informers have synced). Logs are JSON on stderr. Misconfiguration
(model routes key-auth does not authenticate, prefix/regex model routes,
service-scoped key-auth rules, several key-auth plugins) is logged as `WARN`
each time the set of warnings changes.

### RBAC (read-only)

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: switchboard
rules:
  - apiGroups: ["networking.k8s.io"]
    resources: ["ingresses"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: switchboard
  namespace: higress-system      # the namespace of the key-auth WasmPlugin
rules:
  - apiGroups: ["extensions.higress.io"]
    resources: ["wasmplugins"]
    verbs: ["get", "list", "watch"]
```

Bind both to the switchboard ServiceAccount. Ingresses are read
cluster-wide because Higress serves Ingresses from every namespace.

### Service and NetworkPolicy

A `ClusterIP` Service on port 8080. switchboard trusts `X-Mse-Consumer`, so
nothing but the gateway may reach it:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: switchboard
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: switchboard
  policyTypes: ["Ingress"]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: higress-system
          podSelector:
            matchLabels:
              app: higress-gateway   # Higress chart default (gateway.labels)
      ports:
        - port: 8080
```

Kubelet probes are not affected by NetworkPolicy on most CNIs; if yours
filters them, add the node CIDR.

### Gateway route for /v1/models

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: switchboard-models
  namespace: switchboard
spec:
  ingressClassName: higress
  rules:
    - host: api.example.com          # omit to serve every host
      http:
        paths:
          - path: /v1/models
            pathType: Prefix         # also matches /v1/models/{id}
            backend:
              service:
                name: switchboard
                port:
                  number: 8080
```

switchboard reads the client's `Host` to apply host-scoped Ingresses and
`domain` key-auth rules the same way the gateway does. Envoy forwards `Host`
unchanged by default. Do not set `higress.io/upstream-vhost` (or any other
host rewrite) on this Ingress: switchboard would then evaluate the rewritten
host. (Still to be confirmed on a real gateway; see the verification plan in
the design doc.)

## Gateway-side requirement: key-auth must authenticate the listing route

**This is not optional.** key-auth sets `X-Mse-Consumer` and strips any copy
the client sent, but only on routes where it authenticates. On a route it
does not cover, a client can send `X-Mse-Consumer: someone-else` and read
that consumer's list. The chart cannot set this up: it does not own the
key-auth WasmPlugin.

Either of these works:

- `global_auth: true` in the key-auth `defaultConfig`, with **no** `matchRules`
  entry that matches the listing route. Every consumer can then list.
- A `matchRules` entry for the listing route whose `allow` names **every**
  consumer that may list. A rule without `allow` (or with `allow: []`) admits
  nobody. The route name is `namespace/name` of the Ingress (bare `name` if it
  is in `higress-system`):

  ```yaml
  matchRules:
    - ingress: [switchboard/switchboard-models]
      config:
        allow: [team-a, team-b]
  ```

`matchRules` are evaluated in order and the first match wins, including
`domain` rules. A `domain` rule placed earlier that covers the listing host
decides for the listing route too.

Check it from outside:

```bash
# no key: 401 from key-auth (not from switchboard)
curl -si https://api.example.com/v1/models | head -1
# forged header, no key: still 401
curl -si -H 'X-Mse-Consumer: team-a' https://api.example.com/v1/models | head -1
# a valid key: 200 and that consumer's list
curl -s -H 'Authorization: Bearer <key>' https://api.example.com/v1/models
```

The 401 body tells the two apart: key-auth answers
`Request denied by Key Auth check…`, while switchboard answers an OpenAI-style
error with `"code":"invalid_api_key"`. A switchboard 401 means the request
reached it without an identity: key-auth is not authenticating that route.

## Per-model routes

switchboard lists a model only for routes written the way model-router
routing expects: an Ingress of the gateway's class carrying
`higress.io/exact-match-header-<model-header>: <model id>`. See
[models-api-design.md](models-api-design.md) for the exact derivation rules.
