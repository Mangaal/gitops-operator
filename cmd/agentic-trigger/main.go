package main

import (
	"context"
	"crypto/tls"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
	var addr string
	var tlsCert string
	var tlsKey string
	var tlsEnabled bool

	flag.StringVar(&addr, "addr", ":8443", "Listen address")
	flag.StringVar(&tlsCert, "tls-cert", "/etc/agentic-trigger/tls/tls.crt", "TLS certificate path")
	flag.StringVar(&tlsKey, "tls-key", "/etc/agentic-trigger/tls/tls.key", "TLS key path")
	flag.BoolVar(&tlsEnabled, "tls-enabled", true, "Enable TLS (set false for plain HTTP)")
	flag.Parse()

	log.SetLogger(zap.New(zap.UseDevMode(true)))
	logger := log.Log.WithName("agentic-trigger")

	cfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Error(err, "failed to get in-cluster config")
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "failed to create kubernetes client")
		os.Exit(1)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "failed to create dynamic client")
		os.Exit(1)
	}

	config := loadServiceConfig()

	svc := &TriggerService{
		kubeClient: kubeClient,
		dynClient:  dynClient,
		config:     config,
		dedup:      NewDedupCache(config.Cooldown),
		logger:     logger,
	}

	// Periodically clean up expired dedup entries
	go func() {
		ticker := time.NewTicker(config.Cooldown)
		defer ticker.Stop()
		for range ticker.C {
			svc.dedup.Cleanup()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/diagnose", svc.HandleDiagnose)
	mux.HandleFunc("GET /api/v1/applications/{namespace}/{name}/diagnosis", svc.HandleDiagnosis)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	if tlsEnabled {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	go func() {
		if tlsEnabled {
			logger.Info("starting agentic-trigger service with TLS", "addr", addr)
			if err := server.ListenAndServeTLS(tlsCert, tlsKey); err != nil && err != http.ErrServerClosed {
				logger.Error(err, "server error")
				os.Exit(1)
			}
		} else {
			logger.Info("starting agentic-trigger service without TLS (plain HTTP)", "addr", addr)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error(err, "server error")
				os.Exit(1)
			}
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
}

func loadServiceConfig() ServiceConfig {
	cooldown := 10 * time.Minute
	if v := os.Getenv("AGENTIC_RUN_COOLDOWN"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cooldown = d
		}
	}

	namespace := os.Getenv("AGENTIC_RUN_NAMESPACE")
	if namespace == "" {
		namespace = "openshift-lightspeed"
	}

	agent := os.Getenv("AGENTIC_ANALYSIS_AGENT")
	if agent == "" {
		agent = "default"
	}

	skillsImage := os.Getenv("AGENTIC_SKILLS_IMAGE")

	return ServiceConfig{
		RunNamespace:  namespace,
		AnalysisAgent: agent,
		Cooldown:      cooldown,
		SkillsImage:   skillsImage,
	}
}

type ServiceConfig struct {
	RunNamespace  string
	AnalysisAgent string
	Cooldown      time.Duration
	SkillsImage   string
}
