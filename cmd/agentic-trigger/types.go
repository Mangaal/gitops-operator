package main

import (
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var (
	applicationGVR = schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1alpha1",
		Resource: "applications",
	}
	agenticRunGVR = schema.GroupVersionResource{
		Group:    "agentic.openshift.io",
		Version:  "v1alpha1",
		Resource: "agenticruns",
	}
	agenticResultGVR = schema.GroupVersionResource{
		Group:    "agentic.openshift.io",
		Version:  "v1alpha1",
		Resource: "agenticresults",
	}
)

type TriggerService struct {
	kubeClient kubernetes.Interface
	dynClient  dynamic.Interface
	config     ServiceConfig
	dedup      *DedupCache
	logger     logr.Logger
}

type DiagnoseRequest struct {
	Application          string `json:"application"`
	ApplicationNamespace string `json:"applicationNamespace"`
	DestinationNamespace string `json:"destinationNamespace,omitempty"`
	SyncStatus           string `json:"syncStatus"`
	HealthStatus         string `json:"healthStatus"`
	Revision             string `json:"revision,omitempty"`
}

type DiagnoseResponse struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
	Run      *struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"run,omitempty"`
}

type DiagnosisResponse struct {
	Status      string                `json:"status"`
	Trigger     string                `json:"trigger,omitempty"`
	Run         *RunInfo              `json:"run,omitempty"`
	Application *ApplicationInfo      `json:"application,omitempty"`
	Stale       bool                  `json:"stale"`
	Summary     string                `json:"summary,omitempty"`
	Suggestions []DiagnosisSuggestion `json:"suggestions,omitempty"`
}

type RunInfo struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
}

type ApplicationInfo struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	UID          string `json:"uid"`
	SyncStatus   string `json:"syncStatus"`
	HealthStatus string `json:"healthStatus"`
	Revision     string `json:"revision"`
}

type DiagnosisSuggestion struct {
	ID          string `json:"id"`
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Remediation string `json:"remediation"`
}

type AppCondition struct {
	Type    string
	Message string
}

type UnhealthyResource struct {
	Group         string
	Kind          string
	Namespace     string
	Name          string
	HealthStatus  string
	SyncStatus    string
	HealthMessage string
}
