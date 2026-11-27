// Command switchboard is the out-of-tree control plane for the ModelSphere
// Higress gateway. Today it serves GET /v1/models filtered by the caller's
// key, derived from the gateway's own routes and key-auth config.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/modelsphere/switchboard/internal/api"
	"github.com/modelsphere/switchboard/internal/gateway"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		listen      = flag.String("listen", ":8080", "HTTP listen address")
		kubeconfig  = flag.String("kubeconfig", "", "kubeconfig path (empty: in-cluster)")
		opts        gateway.WatchOptions
		ownedBy     = flag.String("owned-by", "modelsphere", "owned_by value in model objects")
		debug       = flag.Bool("debug", false, "debug logging")
	)
	flag.StringVar(&opts.ModelHeader, "model-header", "x-higress-llm-model", "header model-router writes the request's model into (modelToHeader)")
	flag.StringVar(&opts.IngressClass, "ingress-class", "higress", "ingress class the gateway serves (empty: all Ingresses)")
	flag.StringVar(&opts.SystemNamespace, "system-namespace", "higress-system", "gateway namespace (Higress route names omit it)")
	flag.StringVar(&opts.KeyAuthPlugin, "key-auth-plugin", "", "namespace/name of the key-auth WasmPlugin (empty: auto-detect by url in --system-namespace)")
	flag.DurationVar(&opts.Resync, "resync", 0, "informer resync period (0: none; the catalog is rebuilt on every change)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := gateway.ValidateKeyAuthPlugin(opts.KeyAuthPlugin); err != nil {
		log.Error("bad flag", "err", err)
		os.Exit(2)
	}
	if err := run(*listen, *kubeconfig, *ownedBy, opts, log); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(listen, kubeconfig, ownedBy string, opts gateway.WatchOptions, log *slog.Logger) error {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	w := gateway.NewWatcher(kube, dyn, opts, log)
	watchErr := make(chan error, 1)
	go func() { watchErr <- w.Run(ctx) }()

	srv := &http.Server{
		Addr:              listen,
		Handler:           (&api.Server{Source: w, OwnedBy: ownedBy}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	srvErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", listen, "version", version)
		srvErr <- srv.ListenAndServe()
	}()

	select {
	case err = <-watchErr:
	case err = <-srvErr:
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
