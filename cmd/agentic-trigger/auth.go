package main

import (
	"context"
	"strings"

	"github.com/go-logr/logr"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	triggerAutomatic = "automatic"
	triggerManual    = "manual"
)

type CallerIdentity struct {
	TriggerType string
	Username    string
	UID         string
}

func authenticateCaller(ctx context.Context, client kubernetes.Interface, token string, logger logr.Logger) (*CallerIdentity, error) {
	review := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token: token,
		},
	}

	result, err := client.AuthenticationV1().TokenReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	if !result.Status.Authenticated {
		return nil, nil
	}

	username := result.Status.User.Username
	uid := result.Status.User.UID

	triggerType := triggerManual
	if isServiceAccount(username) {
		triggerType = triggerAutomatic
		logger.V(1).Info("caller identified as service account", "username", username)
	} else {
		logger.V(1).Info("caller identified as user", "username", username)
	}

	return &CallerIdentity{
		TriggerType: triggerType,
		Username:    username,
		UID:         uid,
	}, nil
}

func isServiceAccount(username string) bool {
	return strings.HasPrefix(username, "system:serviceaccount:")
}

func extractBearerToken(authHeader string) string {
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	return ""
}
