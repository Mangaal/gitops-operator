# AgenticRun GitOps Integration

## Overview

The GitOps Operator integrates with OpenShift Lightspeed Agentic to provide automatic
and manual AI-powered diagnosis for Argo CD Applications that enter a degraded state.

## Architecture

Instead of watching Application resources directly (which is expensive), the integration
uses a lightweight **agentic-trigger** service:

1. **ArgoCD Notifications** detects health degradation and calls the trigger service (automatic).
2. **Console Plugin** proxies user requests to the trigger service via ConsolePlugin proxy (manual).
3. **Trigger service** validates the caller, checks the live Application state, enforces cooldown, and creates an `AgenticRun` CR.
4. **Lightspeed Agentic Operator** processes the AgenticRun and produces diagnosis results.
5. **Console Plugin** reads normalized diagnosis status from the trigger service GET endpoint.

The GitOps Operator deploys and manages the trigger service lifecycle.

## Enable the Feature

The integration is disabled by default.

Enable via GitopsService annotation:

```bash
oc annotate gitopsservice cluster \
  gitops.openshift.io/agentic-integration-enabled="true" --overwrite
```

Or via operator env vars:

```bash
ENABLE_AGENTIC_GITOPS_INTEGRATION=true
AGENTIC_RUN_NAMESPACE=openshift-lightspeed    # default
AGENTIC_RUN_COOLDOWN=10m                       # default
AGENTIC_ANALYSIS_AGENT=default                 # default
AGENTIC_TRIGGER_MODE=automatic                 # automatic or manual
```

## Automatic Trigger Flow

1. Argo CD Application health transitions to `Degraded`.
2. ArgoCD Notifications fires webhook to agentic-trigger service.
3. Service authenticates the SA token (→ automatic trigger).
4. Service GETs the live Application to confirm it is still Degraded.
5. Service checks cooldown (no duplicate runs within window).
6. Service creates AgenticRun in `openshift-lightspeed`.
7. Lightspeed Agentic Operator runs diagnosis.

Only `Degraded` (and optionally `Unknown`) health states qualify.
Transient states like `Progressing`, `Missing`, or `OutOfSync`-alone are skipped.

## Manual Trigger Flow

1. User clicks "Diagnose with Lightspeed" in the Console Plugin.
2. Console Plugin calls POST via the ConsolePlugin proxy (user token forwarded).
3. Service authenticates the user token (→ manual trigger).
4. Service GETs the live Application for current state.
5. Service creates AgenticRun (manual triggers bypass health gating).
6. Plugin polls GET endpoint for results.

## Reading Diagnosis Results

Console Plugin calls:

```
GET /api/v1/applications/{namespace}/{name}/diagnosis
```

Returns normalized status independent of raw CRD schemas. See the
[contract doc](agentic_diagnosis_backend_plugin_contract.md) for the full response model.

## Validation Commands

Check if the trigger service is deployed:

```bash
oc get deployment agentic-trigger -n openshift-gitops
oc get service agentic-trigger -n openshift-gitops
```

List AgenticRuns:

```bash
oc get agenticruns.agentic.openshift.io -n openshift-lightspeed
```

Test the trigger endpoint manually (requires a valid token):

```bash
TOKEN=$(oc whoami -t)
curl -k -H "Authorization: Bearer $TOKEN" \
  -X POST https://agentic-trigger.openshift-gitops.svc:8443/api/v1/applications/openshift-gitops/my-app/diagnose
```

Read diagnosis:

```bash
curl -k -H "Authorization: Bearer $TOKEN" \
  https://agentic-trigger.openshift-gitops.svc:8443/api/v1/applications/openshift-gitops/my-app/diagnosis
```
