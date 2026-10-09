// Command database is booth-database's entrypoint: the ADR 0080 credential broker's
// `postgres`-kind provider, provisioning one database per workspace on a bundled or external
// PostgreSQL (ADR 0081), plus the reaper that ends expired leases.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/projectbooth/booth-database/internal/api"
	"github.com/projectbooth/booth-database/internal/auth"
	"github.com/projectbooth/booth-database/internal/config"
	"github.com/projectbooth/booth-database/internal/credentialbroker"
	"github.com/projectbooth/booth-database/internal/nodepin"
	"github.com/projectbooth/booth-database/internal/provision"
	"github.com/projectbooth/booth-database/internal/server"
)

const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	prov, err := provision.New(ctx, cfg.AdminDSN, provision.Options{LeaseConnectionLimit: cfg.LeaseConnectionLimit})
	if err != nil {
		return err
	}
	defer prov.Close()

	if cfg.Mode == config.ModeExternal && !cfg.RestrictMaintenanceAccess {
		// ADR 0054 §2's required warning, applied to this module's own external-cluster mode.
		log.Print("WARNING: external PostgreSQL with restrictMaintenanceAccess=false — PUBLIC keeps its default CONNECT on the postgres/template1 databases, so an issued workspace credential can open a session there and read server-wide catalogs (database and role names, never other workspaces' data). Set external.restrictMaintenanceAccess=true if nothing else on this cluster relies on that default.")
	}

	// The bundled server may come up after this pod; prepare in the background and report
	// unready until it succeeds rather than crash-looping.
	var ready atomic.Bool
	go func() {
		for {
			err := prov.Ping(ctx)
			if err == nil && cfg.RestrictMaintenanceAccess {
				err = prov.RestrictMaintenanceAccess(ctx)
			}
			if err == nil {
				ready.Store(true)
				log.Printf("connected to %s PostgreSQL; ready to issue credentials", cfg.Mode)
				return
			}
			log.Printf("waiting for PostgreSQL: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
	go prov.RunReaper(ctx, cfg.ReapInterval)

	if cfg.PinStatefulSet != "" {
		// ADR 0090: keep the bundled server on the node its local-path volume lives on. A failure
		// to build the client is fatal rather than silently skipped: the chart only sets this when
		// it has also granted the RBAC, so a failure here is a real misconfiguration.
		restCfg, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("node pinning is enabled but there is no in-cluster Kubernetes config: %w", err)
		}
		kube, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return fmt.Errorf("creating Kubernetes client for node pinning: %w", err)
		}
		go nodepin.Run(ctx, kube, cfg.Namespace, cfg.PinStatefulSet, nodepin.Options{})
	}

	// The admin API's verifier is built in the background, retrying OIDC discovery: the identity
	// provider being unreachable must never stop this module issuing credentials, and until it
	// works the API answers 503 rather than letting anything through (internal/api).
	var verifier atomic.Pointer[auth.Verifier]
	if cfg.OIDC.IssuerURL == "" {
		log.Print("WARNING: BOOTH_OIDC_ISSUER_URL is not set — the admin view's API (/api/*) will answer 503. Set oidc.issuerUrl/clientId in the chart to enable it.")
	} else {
		go func() {
			for {
				v, err := auth.NewVerifier(ctx, cfg.OIDC)
				if err == nil {
					verifier.Store(v)
					return
				}
				log.Printf("admin API: OIDC discovery failed (%v); retrying", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Second):
				}
			}
		}()
	}
	adminAPI := api.NewHandler(api.Deps{
		Verifier: func() auth.TokenVerifier {
			if v := verifier.Load(); v != nil {
				return v
			}
			return nil
		},
		Source: prov,
	})

	httpServer := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: server.NewRouter(server.Deps{
			DB:    prov,
			Ready: &ready,
			Provider: credentialbroker.NewHandler(credentialbroker.Deps{
				Credential: cfg.CredentialBrokerCredential,
				Issuer:     prov,
				Endpoint:   credentialbroker.Endpoint{Host: cfg.ClientHost, Port: cfg.ClientPort, SSLMode: cfg.ClientSSLMode},
				MinTTL:     cfg.MinTTL,
				MaxTTL:     cfg.MaxTTL,
			}),
			API: adminAPI,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("booth-database listening on %s (mode=%s)", cfg.HTTPAddr, cfg.Mode)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}
