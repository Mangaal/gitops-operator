# Agentic Diagnosis Trigger Service Contract

This document defines the API contract for the **agentic-trigger** service that manages
AgenticRun lifecycle for OpenShift GitOps Argo CD Applications.

## Architecture Overview

The agentic-trigger service is a standalone Deployment managed by the GitOps Operator.
It replaces the former in-operator Application informer with an HTTP-driven approach:

- **Automatic triggers**: ArgoCD Notifications fires a webhook on health degradation.
- **Manual triggers**: Console Plugin proxies user requests via the OpenShift ConsolePlugin proxy.
- The service determines trigger type from **caller identity**, not the request body.

## Scope Ownership

- **GitOps Operator (this repo):**
  - Deploys and manages the agentic-trigger service (SA, RBAC, Deployment, Service)
  - Injects ArgoCD Notifications trigger/template configuration
  - Feature gate via `ENABLE_AGENTIC_GITOPS_INTEGRATION`
- **Agentic Trigger Service (`cmd/agentic-trigger/`):**
  - Caller authentication via TokenReview
  - Trigger type detection (SA = automatic, user = manual)
  - Live Application state retrieval
  - Automatic trigger gating (Degraded only)
  - Cooldown and deduplication
  - AgenticRun creation
  - Normalized diagnosis read endpoint
- **Console Plugin frontend (separate repo):**
  - Manual diagnosis button
  - Diagnosis card and state transitions
  - Calls service via ConsolePlugin proxy

## Authentication

### Caller Identity Determines Trigger Type

The service does NOT trust the request body for trigger type. Instead:

1. Extract bearer token from `Authorization` header.
2. TokenReview the token against the kube API.
3. If caller is a ServiceAccount matching the ArgoCD notifications SA → `automatic`.
4. If caller is a real user (forwarded via ConsolePlugin proxy) → `manual`.
5. Reject unauthenticated or unrecognized callers with 403.

### Console Plugin Proxy

The OpenShift ConsolePlugin CRD declares a proxy entry with `authorization: UserToken`.
The browser never receives a ServiceAccount token; the console backend injects the
user's bearer token into the proxied request.

```yaml
spec:
  proxy:
    - alias: agentic-trigger
      endpoint:
        type: Service
        service:
          name: agentic-trigger
          namespace: openshift-gitops
          port: 8443
      authorization: UserToken
```

Browser path:
`/api/proxy/plugin/gitops-plugin/agentic-trigger/api/v1/applications/{ns}/{name}/...`

## Endpoints

### POST `/api/v1/applications/{namespace}/{name}/diagnose`

Creates an AgenticRun for the target Application.

Request body (optional context from ArgoCD Notifications):

```json
{
  "healthStatus": "Degraded",
  "syncStatus": "Synced",
  "message": "optional context"
}
```

Processing steps:

1. Authenticate caller, determine trigger type.
2. GET the Application (`argoproj.io/v1alpha1`) live from the cluster.
3. Extract: `.metadata.uid`, `.status.sync.status`, `.status.health.status`, `.status.sync.revision`.
4. **Automatic-only gate**: if trigger is automatic and health is NOT `Degraded` or `Unknown`, return 200 with `"accepted": false` (skip transient states).
5. Build dedup signature: `uid + healthStatus + syncStatus + revision`.
6. Cooldown check: list existing AgenticRuns by labels, reject if recent run with same signature exists.
7. Create AgenticRun in configured namespace.
8. Return response.

Success response:

```json
{
  "accepted": true,
  "run": {
    "name": "gitops-app-diag-myapp-20260825120000",
    "namespace": "openshift-lightspeed"
  }
}
```

Skipped response (transient state or cooldown):

```json
{
  "accepted": false,
  "reason": "health status Progressing does not qualify for automatic diagnosis"
}
```

### GET `/api/v1/applications/{namespace}/{name}/diagnosis`

Returns the normalized diagnosis state for the Console Plugin UI.

Processing steps:

1. Authenticate caller (user token via proxy).
2. GET the Application to obtain current UID.
3. List AgenticRuns by labels `(app-name, app-namespace, app-uid)`, pick most recent.
4. Build normalized response.
5. Stale detection: compare trigger-snapshot annotations against live Application state.

Response:

```json
{
  "status": "idle|running|completed|failed",
  "trigger": "manual|automatic",
  "run": {
    "name": "",
    "namespace": "",
    "startedAt": "",
    "completedAt": ""
  },
  "application": {
    "name": "",
    "namespace": "",
    "uid": "",
    "syncStatus": "",
    "healthStatus": "",
    "revision": ""
  },
  "stale": false,
  "summary": "",
  "suggestions": [
    {
      "id": "",
      "severity": "info|warning|critical",
      "title": "",
      "description": "",
      "remediation": ""
    }
  ]
}
```

## AgenticRun Metadata

Created AgenticRun resources include:

Labels:
- `gitops.openshift.io/application-name`
- `gitops.openshift.io/application-namespace`
- `gitops.openshift.io/application-uid`
- `gitops.openshift.io/managed-by: agentic-trigger`

Annotations:
- `gitops.openshift.io/trigger-type: automatic|manual`
- `gitops.openshift.io/trigger-sync-status`
- `gitops.openshift.io/trigger-health-status`
- `gitops.openshift.io/trigger-revision`
- `gitops.openshift.io/trigger-signature`
- `gitops.openshift.io/triggered-at`

## Correlation Rules

Runs are correlated by the triple `(application-name, application-namespace, application-uid)`.

App recreation with the same name but a new UID invalidates all previous runs (they become stale).

Selection order for GET endpoint:
1. List AgenticRuns by all three labels.
2. Sort by `triggered-at` annotation, pick most recent.
3. If no runs found, return `status: idle`.

## Stale Detection

Mark diagnosis stale when any of these differ between the AgenticRun trigger-snapshot
annotations and the current live Application state:

- `application-uid` mismatch (app was recreated)
- trigger revision != current revision
- trigger health status != current health status

If stale:
- `stale: true`
- Keep previous suggestions visible
- Include summary noting this is from a previous diagnosis

## Automatic Trigger Gating

Only these health states qualify for automatic diagnosis:
- `Degraded` — confirmed failure
- `Unknown` — potential issue (optional, configurable)

Explicitly skipped:
- `Progressing` — normal during rollout
- `Missing` — app just created
- `Suspended` — intentionally paused
- `Healthy` — no issue
- `OutOfSync` alone without Degraded health — transient during sync

Manual triggers bypass this gate entirely.

## Service RBAC (minimal)

| Resource | Verbs | Reason |
|----------|-------|--------|
| `argoproj.io/applications` | `get` | Read live app state |
| `agentic.openshift.io/agenticruns` | `create`, `get`, `list` | Create and read runs |
| `authentication.k8s.io/tokenreviews` | `create` | Verify caller identity |

No `watch` or `list` on Applications. No wildcard verbs.

## ArgoCD Notifications Configuration

Injected into `argocd-notifications-cm` by the GitOps Operator:

```yaml
trigger.on-health-degraded: |
  - when: app.status.health.status == 'Degraded'
    send: [agentic-diagnose]

template.agentic-diagnose: |
  webhook:
    agentic-trigger:
      method: POST
      path: /api/v1/applications/{{.app.metadata.namespace}}/{{.app.metadata.name}}/diagnose
      body: |
        {"healthStatus":"{{.app.status.health.status}}","syncStatus":"{{.app.status.sync.status}}"}

service.webhook.agentic-trigger: |
  url: https://agentic-trigger.openshift-gitops.svc:8443
  headers:
    - name: Authorization
      value: "Bearer $agentic-trigger-sa-token"
```

## Plugin UX Contract

### Manual mode

- Show action: **Diagnose with Lightspeed**
- State sequence: `idle` → `running` → `completed|failed`
- Disable repeated clicks while `running`
- Allow new request after completion/failure

### Automatic mode

- Do not show start button
- Show passive states: `Diagnosis in progress`, `Latest Lightspeed suggestion`
- Reuse the same diagnosis result card component

### Both modes

- Poll GET endpoint every 5-10s while `running`
- Display `stale` badge when diagnosis is outdated
- Show severity-colored suggestion cards

## Configuration

Feature gate and tuning via GitopsService annotations or operator env vars:

| Setting | Env Var | Annotation | Default |
|---------|---------|------------|---------|
| Enable | `ENABLE_AGENTIC_GITOPS_INTEGRATION` | `gitops.openshift.io/agentic-integration-enabled` | `false` |
| Namespace | `AGENTIC_RUN_NAMESPACE` | `gitops.openshift.io/agentic-namespace` | `openshift-lightspeed` |
| Cooldown | `AGENTIC_RUN_COOLDOWN` | `gitops.openshift.io/agentic-cooldown` | `10m` |
| Agent | `AGENTIC_ANALYSIS_AGENT` | `gitops.openshift.io/agentic-analysis-agent` | `default` |
| Trigger mode | `AGENTIC_TRIGGER_MODE` | `gitops.openshift.io/agentic-trigger-mode` | `automatic` |
