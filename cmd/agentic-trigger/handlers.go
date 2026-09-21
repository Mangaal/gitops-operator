package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (s *TriggerService) HandleDiagnose(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := s.logger.WithName("diagnose")

	// --- Auth ---
	token := extractBearerToken(r.Header.Get("Authorization"))
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, DiagnoseResponse{Accepted: false, Reason: "missing authorization"})
		return
	}
	caller, err := authenticateCaller(ctx, s.kubeClient, token, logger)
	if err != nil {
		logger.Error(err, "token review failed")
		writeJSON(w, http.StatusInternalServerError, DiagnoseResponse{Accepted: false, Reason: "authentication error"})
		return
	}
	if caller == nil {
		writeJSON(w, http.StatusForbidden, DiagnoseResponse{Accepted: false, Reason: "unauthenticated"})
		return
	}

	// --- Decode payload ---
	var req DiagnoseRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, DiagnoseResponse{Accepted: false, Reason: "invalid request body"})
			return
		}
	}

	// Use payload fields directly
	name := req.Application
	namespace := req.ApplicationNamespace
	if name == "" || namespace == "" {
		writeJSON(w, http.StatusBadRequest, DiagnoseResponse{Accepted: false, Reason: "application and applicationNamespace are required"})
		return
	}

	logger = logger.WithValues("app", namespace+"/"+name)
	logger.Info("diagnose request received",
		"healthStatus", req.HealthStatus,
		"syncStatus", req.SyncStatus,
		"destinationNamespace", req.DestinationNamespace,
		"revision", req.Revision,
		"trigger", caller.TriggerType,
	)

	// --- Policy check using payload ---
	if caller.TriggerType == triggerAutomatic && !qualifiesForAutomatic(req.SyncStatus, req.HealthStatus) {
		writeJSON(w, http.StatusOK, DiagnoseResponse{
			Accepted: false,
			Reason:   fmt.Sprintf("application %q does not qualify for automatic diagnosis", name),
		})
		return
	}

	// --- Dedup using payload ---
	syncStatus := req.SyncStatus
	healthStatus := req.HealthStatus
	revision := req.Revision

	// --- Optional enrichment: fetch live Application for UID, conditions, resources ---
	var appUID string
	var conditions []AppCondition
	var unhealthyResources []UnhealthyResource
	app, err := s.getApplication(ctx, namespace, name)
	if err != nil {
		logger.V(1).Info("could not fetch live Application for enrichment, proceeding with payload only", "error", err)
	} else {
		appUID = string(app.GetUID())
		// Use live status if available (source of truth)
		if live, _, _ := unstructured.NestedString(app.Object, "status", "sync", "status"); live != "" {
			syncStatus = live
		}
		if live, _, _ := unstructured.NestedString(app.Object, "status", "health", "status"); live != "" {
			healthStatus = live
		}
		if live, _, _ := unstructured.NestedString(app.Object, "status", "sync", "revision"); live != "" {
			revision = live
		}
		conditions = extractConditions(app)
		unhealthyResources = extractUnhealthyResources(app)
	}

	signature := buildSignature(appUID, syncStatus, healthStatus, revision)
	dedupKey := buildDedupKey(appUID, signature)
	if !s.dedup.TryRecord(dedupKey) {
		writeJSON(w, http.StatusOK, DiagnoseResponse{
			Accepted: false,
			Reason:   "cooldown active for this application state",
		})
		return
	}

	// --- Build and create AgenticRun ---
	runName := buildAgenticRunName(name)
	targetNS := req.DestinationNamespace
	if targetNS == "" {
		if app != nil {
			targetNS, _, _ = unstructured.NestedString(app.Object, "spec", "destination", "namespace")
		}
		if targetNS == "" {
			targetNS = namespace
		}
	}

	agenticRun := buildAgenticRunObject(runName, s.config.RunNamespace, AgenticRunParams{
		AppName:            name,
		AppNamespace:       namespace,
		AppUID:             appUID,
		SyncStatus:         syncStatus,
		HealthStatus:       healthStatus,
		Revision:           revision,
		TargetNS:           targetNS,
		TriggerType:        caller.TriggerType,
		Agent:              s.config.AnalysisAgent,
		SkillsImage:        s.config.SkillsImage,
		Conditions:         conditions,
		UnhealthyResources: unhealthyResources,
	})

	if err := s.createAgenticRun(ctx, agenticRun); err != nil {
		logger.Error(err, "failed to create AgenticRun")
		writeJSON(w, http.StatusInternalServerError, DiagnoseResponse{Accepted: false, Reason: "failed to create AgenticRun"})
		return
	}

	if err := s.annotateApplication(ctx, namespace, name, runName); err != nil {
		logger.Error(err, "failed to annotate application (non-fatal)")
	}

	logger.Info("created AgenticRun", "name", runName, "trigger", caller.TriggerType)

	writeJSON(w, http.StatusCreated, DiagnoseResponse{
		Accepted: true,
		Run: &struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		}{
			Name:      runName,
			Namespace: s.config.RunNamespace,
		},
	})
}

// HandleDiagnosis is the handler for the diagnosis endpoint.
// It returns the latest diagnosis for an application.
func (s *TriggerService) HandleDiagnosis(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	namespace := r.PathValue("namespace")
	name := r.PathValue("name")
	logger := s.logger.WithValues("app", namespace+"/"+name)

	token := extractBearerToken(r.Header.Get("Authorization"))
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, DiagnosisResponse{Status: "error"})
		return
	}

	caller, err := authenticateCaller(ctx, s.kubeClient, token, logger)
	if err != nil || caller == nil {
		writeJSON(w, http.StatusForbidden, DiagnosisResponse{Status: "error"})
		return
	}

	app, err := s.getApplication(ctx, namespace, name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, DiagnosisResponse{Status: "error"})
		return
	}

	appUID := string(app.GetUID())
	syncStatus, _, _ := unstructured.NestedString(app.Object, "status", "sync", "status")
	healthStatus, _, _ := unstructured.NestedString(app.Object, "status", "health", "status")
	revision, _, _ := unstructured.NestedString(app.Object, "status", "sync", "revision")

	// Build the application information
	appInfo := &ApplicationInfo{
		Name:         name,
		Namespace:    namespace,
		UID:          appUID,
		SyncStatus:   syncStatus,
		HealthStatus: healthStatus,
		Revision:     revision,
	}

	// Prefer direct lookup via annotation (fast, O(1))
	var latestRun *unstructured.Unstructured
	annotations := app.GetAnnotations()
	if runName := annotations["gitops.openshift.io/agentic-run"]; runName != "" {
		run, err := s.getAgenticRun(ctx, s.config.RunNamespace, runName)
		if err == nil {
			latestRun = run
		} else {
			logger.V(1).Info("annotated AgenticRun not found, falling back to label list", "runName", runName)
		}
	}

	// Fallback: label selector list (handles annotation missing or stale)
	if latestRun == nil {
		runs, err := s.listAgenticRuns(ctx, name, namespace, appUID)
		if err != nil {
			logger.Error(err, "failed to list AgenticRuns")
			writeJSON(w, http.StatusInternalServerError, DiagnosisResponse{Status: "error"})
			return
		}
		if len(runs) > 0 {
			latestRun = runs[0]
		}
	}

	if latestRun == nil {
		writeJSON(w, http.StatusOK, DiagnosisResponse{
			Status:      "idle",
			Application: appInfo,
		})
		return
	}

	resp := s.buildDiagnosisFromRun(ctx, latestRun, name, namespace, appUID, syncStatus, healthStatus, revision)
	writeJSON(w, http.StatusOK, resp)
}

func (s *TriggerService) getApplication(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	return s.dynClient.Resource(applicationGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (s *TriggerService) createAgenticRun(ctx context.Context, run *unstructured.Unstructured) error {
	ns := run.GetNamespace()
	_, err := s.dynClient.Resource(agenticRunGVR).Namespace(ns).Create(ctx, run, metav1.CreateOptions{})
	return err
}

func (s *TriggerService) getAgenticRun(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	return s.dynClient.Resource(agenticRunGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (s *TriggerService) annotateApplication(ctx context.Context, namespace, name, runName string) error {
	patch := fmt.Appendf(nil,
		`{"metadata":{"annotations":{"gitops.openshift.io/agentic-run":%q,"gitops.openshift.io/agentic-runner":"agentic-trigger"}}}`,
		runName,
	)
	_, err := s.dynClient.Resource(applicationGVR).Namespace(namespace).Patch(
		ctx, name, types.MergePatchType, patch, metav1.PatchOptions{},
	)
	return err
}

func (s *TriggerService) listAgenticRuns(ctx context.Context, appName, appNamespace, appUID string) ([]*unstructured.Unstructured, error) {
	labelSelector := fmt.Sprintf(
		"gitops.openshift.io/application-name=%s,gitops.openshift.io/application-namespace=%s,gitops.openshift.io/application-uid=%s",
		appName, appNamespace, appUID,
	)

	result, err := s.dynClient.Resource(agenticRunGVR).Namespace(s.config.RunNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}

	runs := make([]*unstructured.Unstructured, 0, len(result.Items))
	for i := range result.Items {
		runs = append(runs, &result.Items[i])
	}

	sort.Slice(runs, func(i, j int) bool {
		ti := runs[i].GetAnnotations()["gitops.openshift.io/triggered-at"]
		tj := runs[j].GetAnnotations()["gitops.openshift.io/triggered-at"]
		return ti > tj
	})

	return runs, nil
}

func (s *TriggerService) getAgenticResult(ctx context.Context, runName string) (*unstructured.Unstructured, error) {
	return s.dynClient.Resource(agenticResultGVR).Namespace(s.config.RunNamespace).Get(ctx, runName, metav1.GetOptions{})
}

func qualifiesForAutomatic(syncStatus, healthStatus string) bool {
	if syncStatus == "OutOfSync" {
		return true
	}
	switch strings.ToLower(healthStatus) {
	case "Degraded", "Unknown", "Missing":
		return true
	}
	return false
}

type AgenticRunParams struct {
	AppName            string
	AppNamespace       string
	AppUID             string
	SyncStatus         string
	HealthStatus       string
	Revision           string
	TargetNS           string
	TriggerType        string
	Agent              string
	SkillsImage        string
	Conditions         []AppCondition
	UnhealthyResources []UnhealthyResource
}

func buildAgenticRunObject(name, namespace string, p AgenticRunParams) *unstructured.Unstructured {
	now := time.Now().UTC().Format(time.RFC3339)
	signature := buildSignature(p.AppUID, p.SyncStatus, p.HealthStatus, p.Revision)

	requestText := fmt.Sprintf(
		"Diagnose the following Argo CD Application running on an OpenShift cluster.\n"+
			"The Argo CD instance is installed and managed by the Red Hat OpenShift GitOps Operator.\n"+
			"Treat this environment as Red Hat OpenShift GitOps, not as a standalone upstream Argo CD installation.\n\n"+
			"Application:\n"+
			"  Name: %s\n"+
			"  Namespace: %s\n\n"+
			"Current status:\n"+
			"  Sync Status: %s\n"+
			"  Health Status: %s\n"+
			"%s\n"+
			"Destination:\n"+
			"  Namespace: %s\n\n"+
			"Investigate the reason for the application's current sync and health status.\n"+
			"Inspect the Argo CD Application, its managed Kubernetes/OpenShift resources,\n"+
			"and relevant OpenShift GitOps resources to determine:\n"+
			"- the root cause\n"+
			"- affected resources\n"+
			"- relevant errors, events, or conditions\n"+
			"- recommended remediation\n\n"+
			"When recommending remediation:\n"+
			"- follow Red Hat OpenShift GitOps conventions\n"+
			"- account for the fact that the Argo CD control plane is managed by the OpenShift GitOps Operator\n"+
			"- do not recommend directly modifying Operator-managed Argo CD Deployments,\n"+
			"  StatefulSets, Services, or other generated control-plane resources\n"+
			"- when the problem is Argo CD instance configuration, prefer the Operator-supported\n"+
			"  configuration through the ArgoCD custom resource or other OpenShift GitOps APIs\n"+
			"- when the problem is in an Application-managed workload, identify the appropriate\n"+
			"  Git/declarative configuration change rather than treating it as an Argo CD\n"+
			"  control-plane configuration problem\n"+
			"- do not assume an upstream Argo CD remediation is appropriate if OpenShift GitOps\n"+
			"  manages that configuration differently\n\n"+
			"Do not make changes to the cluster.\n"+
			"Only diagnose the issue and recommend the supported remediation.\n",
		p.AppName,
		p.AppNamespace,
		p.SyncStatus,
		p.HealthStatus,
		buildConditionsContext(&p),
		p.TargetNS,
	)

	run := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "agentic.openshift.io/v1alpha1",
			"kind":       "AgenticRun",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
				"labels": map[string]interface{}{
					"gitops.openshift.io/application-name":      p.AppName,
					"gitops.openshift.io/application-namespace": p.AppNamespace,
					"gitops.openshift.io/application-uid":       p.AppUID,
					"gitops.openshift.io/managed-by":            "agentic-trigger",
				},
				"annotations": map[string]interface{}{
					"gitops.openshift.io/trigger-type":          p.TriggerType,
					"gitops.openshift.io/trigger-sync-status":   p.SyncStatus,
					"gitops.openshift.io/trigger-health-status": p.HealthStatus,
					"gitops.openshift.io/trigger-revision":      p.Revision,
					"gitops.openshift.io/trigger-signature":     signature,
					"gitops.openshift.io/triggered-at":          now,
				},
			},
			"spec": buildAgenticRunSpec(requestText, p),
		},
	}

	return run
}

func buildAgenticRunSpec(requestText string, p AgenticRunParams) map[string]interface{} {
	spec := map[string]interface{}{
		"request": requestText,
		"analysis": map[string]interface{}{
			"agent": p.Agent,
		},
		"targetNamespaces": []string{p.TargetNS, p.AppNamespace},
	}
	if tools := buildToolsSpec(p.SkillsImage); len(tools) > 0 {
		spec["tools"] = tools
	}
	return spec
}

func buildToolsSpec(skillsImage string) map[string]interface{} {
	tools := map[string]interface{}{}
	if skillsImage != "" {
		tools["skills"] = []interface{}{
			map[string]interface{}{
				"image": skillsImage,
				"paths": []interface{}{
					"/skills/argocd-diagnostics",
					"/skills/argocd-remediation",
				},
			},
		}
	}
	return tools
}

func extractConditions(app *unstructured.Unstructured) []AppCondition {
	raw, found, _ := unstructured.NestedSlice(app.Object, "status", "conditions")
	if !found {
		return nil
	}
	var conditions []AppCondition
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(m, "type")
		msg, _, _ := unstructured.NestedString(m, "message")
		if t != "" {
			conditions = append(conditions, AppCondition{Type: t, Message: msg})
		}
	}
	return conditions
}

func extractUnhealthyResources(app *unstructured.Unstructured) []UnhealthyResource {
	raw, found, _ := unstructured.NestedSlice(app.Object, "status", "resources")
	if !found {
		return nil
	}
	var resources []UnhealthyResource
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		health, _, _ := unstructured.NestedString(m, "health", "status")
		sync, _, _ := unstructured.NestedString(m, "status")
		if health == "Degraded" || health == "Missing" || health == "Unknown" || sync == "OutOfSync" {
			msg, _, _ := unstructured.NestedString(m, "health", "message")
			resources = append(resources, UnhealthyResource{
				Group:         stringFromMap(m, "group"),
				Kind:          stringFromMap(m, "kind"),
				Namespace:     stringFromMap(m, "namespace"),
				Name:          stringFromMap(m, "name"),
				HealthStatus:  health,
				SyncStatus:    sync,
				HealthMessage: msg,
			})
		}
	}
	return resources
}

func stringFromMap(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

// buildConditionsContext builds the context for the AgenticRun
func buildConditionsContext(p *AgenticRunParams) string {
	var sb strings.Builder

	// ArgoCD conditions (ComparisonError, SyncError, etc.)
	if len(p.Conditions) > 0 {
		sb.WriteString("\nReported conditions:\n")
		for _, c := range p.Conditions {
			sb.WriteString(fmt.Sprintf("  - [%s] %s\n", c.Type, c.Message))
		}
	}

	// Unhealthy/OutOfSync resources from status.resources[]
	if len(p.UnhealthyResources) > 0 {
		sb.WriteString("\nUnhealthy or OutOfSync resources reported by ArgoCD:\n")
		for _, r := range p.UnhealthyResources {
			sb.WriteString(fmt.Sprintf("  - %s/%s (%s) in %s — health: %s, sync: %s\n",
				r.Kind, r.Name, r.Group, r.Namespace, r.HealthStatus, r.SyncStatus))
			if r.HealthMessage != "" {
				sb.WriteString(fmt.Sprintf("    message: %s\n", r.HealthMessage))
			}
		}
	}

	return sb.String()
}

func buildAgenticRunName(appName string) string {
	sanitized := sanitizeToDNSLabel(appName)
	timestamp := time.Now().UTC().Format("20060102150405")
	prefix := "gitops-app-diag-"
	name := prefix + sanitized + "-" + timestamp
	if len(name) <= validation.DNS1035LabelMaxLength {
		return strings.Trim(name, "-")
	}
	maxAppLen := validation.DNS1035LabelMaxLength - len(prefix) - len(timestamp) - 1
	if maxAppLen < 8 {
		maxAppLen = 8
	}
	if len(sanitized) > maxAppLen {
		sanitized = sanitized[:maxAppLen]
	}
	return strings.Trim(prefix+strings.Trim(sanitized, "-")+"-"+timestamp, "-")
}

func sanitizeToDNSLabel(input string) string {
	value := strings.ToLower(strings.TrimSpace(input))
	if value == "" {
		return "application"
	}

	var out strings.Builder
	lastWasDash := false
	for _, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			out.WriteRune(c)
			lastWasDash = false
			continue
		}
		if !lastWasDash {
			out.WriteRune('-')
			lastWasDash = true
		}
	}

	result := strings.Trim(out.String(), "-")
	if result == "" {
		return "application"
	}
	return result
}

func (s *TriggerService) buildDiagnosisFromRun(ctx context.Context, run *unstructured.Unstructured, appName, appNS, appUID, syncStatus, healthStatus, revision string) DiagnosisResponse {
	annotations := run.GetAnnotations()
	triggerType := annotations["gitops.openshift.io/trigger-type"]
	triggeredAt := annotations["gitops.openshift.io/triggered-at"]
	triggerRevision := annotations["gitops.openshift.io/trigger-revision"]
	triggerHealth := annotations["gitops.openshift.io/trigger-health-status"]

	status := mapRunStatus(run)

	stale := false
	if triggerRevision != revision || triggerHealth != healthStatus {
		stale = true
	}

	resp := DiagnosisResponse{
		Status:  status,
		Trigger: triggerType,
		Run: &RunInfo{
			Name:      run.GetName(),
			Namespace: run.GetNamespace(),
			StartedAt: triggeredAt,
		},
		Application: &ApplicationInfo{
			Name:         appName,
			Namespace:    appNS,
			UID:          appUID,
			SyncStatus:   syncStatus,
			HealthStatus: healthStatus,
			Revision:     revision,
		},
		Stale: stale,
	}

	if status == "completed" {
		result, err := s.getAgenticResult(ctx, run.GetName())
		if err != nil {
			s.logger.V(1).Info("AgenticResult not found for run", "run", run.GetName(), "error", err)
		} else {
			resp.Summary, _, _ = unstructured.NestedString(result.Object, "status", "summary")
			resp.Run.CompletedAt, _, _ = unstructured.NestedString(result.Object, "status", "completedAt")
			suggestions, found, _ := unstructured.NestedSlice(result.Object, "status", "suggestions")
			if found {
				for _, s := range suggestions {
					item, ok := s.(map[string]interface{})
					if !ok {
						continue
					}
					id, _, _ := unstructured.NestedString(item, "id")
					severity, _, _ := unstructured.NestedString(item, "severity")
					title, _, _ := unstructured.NestedString(item, "title")
					desc, _, _ := unstructured.NestedString(item, "description")
					remed, _, _ := unstructured.NestedString(item, "remediation")
					resp.Suggestions = append(resp.Suggestions, DiagnosisSuggestion{
						ID:          id,
						Severity:    severity,
						Title:       title,
						Description: desc,
						Remediation: remed,
					})
				}
			}
		}
	}

	return resp
}

func mapRunStatus(run *unstructured.Unstructured) string {
	conditions, found, _ := unstructured.NestedSlice(run.Object, "status", "conditions")
	if !found || len(conditions) == 0 {
		return "running"
	}

	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _, _ := unstructured.NestedString(cond, "type")
		condStatus, _, _ := unstructured.NestedString(cond, "status")
		reason, _, _ := unstructured.NestedString(cond, "reason")

		if condType == "Analyzed" {
			switch {
			case condStatus == "True":
				return "completed"
			case condStatus == "False" && reason == "Failed":
				return "failed"
			default:
				return "running"
			}
		}
	}
	return "running"
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
