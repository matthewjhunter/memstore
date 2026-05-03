// memstored is the memstore network daemon. It serves the memstore HTTP API
// and processes embeddings in the background.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/pgstore"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run executes the memstored daemon with the given arguments. It returns when
// ctx is cancelled or the server exits with an error. Extracted from main so
// tests can drive the lifecycle directly. onListening, if non-nil, is invoked
// once the listener is bound (used by tests to discover an ephemeral port).
func run(ctx context.Context, args []string, stderr io.Writer, onListening func(net.Addr)) error {
	cfg := memstore.LoadConfig()

	defaultAddr := cfg.Addr
	if defaultAddr == "" {
		defaultAddr = ":8230"
	}

	fs := flag.NewFlagSet("memstored", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultAddr, "listen address")
	pgDSN := fs.String("pg", cfg.PG, "PostgreSQL connection string (required)")
	vecDim := fs.Int("vec-dim", cfg.VecDim, "embedding vector dimension (e.g. 768)")
	namespace := fs.String("namespace", cfg.Namespace, "namespace")
	ollamaURL := fs.String("ollama", cfg.Ollama, "LLM API base URL (Ollama, LiteLLM, or OpenAI-compatible)")
	model := fs.String("model", cfg.Model, "embedding model name")
	apiKey := fs.String("api-key", cfg.APIKey, "API key for authentication (empty = disabled)")
	llmAPIKey := fs.String("llm-api-key", cfg.LLMAPIKey, "API key for the LLM provider (empty = no auth)")
	genModel := fs.String("gen-model", cfg.GenModel, "LLM model for generation (enables /v1/generate)")
	genURL := fs.String("gen-url", cfg.GenURL, "separate LLM URL for generation (defaults to --ollama)")
	persona := fs.String("persona", cfg.Persona, "subject for user/preference-scoped session summaries (default \"user\")")
	embedInterval := fs.Duration("embed-interval", 2*time.Second, "embed queue poll interval")
	embedBatch := fs.Int("embed-batch", 32, "embed queue batch size")
	tlsCertFile := fs.String("tls-cert-file", cfg.TLSCertFile, "TLS certificate file (PEM)")
	tlsKeyFile := fs.String("tls-key-file", cfg.TLSKeyFile, "TLS private key file (PEM)")
	tlsClientCA := fs.String("tls-client-ca-file", cfg.TLSClientCAFile,
		"PEM bundle of CAs trusted for client certs; presence enables mTLS")
	tlsDisabled := fs.Bool("tls-disabled", cfg.TLSDisabled,
		"disable TLS (only for proxy-fronted deployments)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *pgDSN == "" {
		return errors.New("PostgreSQL is required: pass --pg or set MEMSTORE_PG " +
			"(for single-user local development, use memstore-mcp directly with no daemon)")
	}

	embedder := memstore.NewOpenAIEmbedder(*ollamaURL, *llmAPIKey, *model)

	pgPool, err := pgxpool.New(ctx, *pgDSN)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pgPool.Close()

	pgStore, err := pgstore.New(ctx, pgPool, embedder, *namespace, *vecDim)
	if err != nil {
		return fmt.Errorf("init postgres store: %w", err)
	}
	var store memstore.Store = pgStore
	log.Printf("using PostgreSQL store (dim=%d)", *vecDim)

	sessCtx := httpapi.NewSessionContext()
	defer sessCtx.Stop()

	handlerOpts := []httpapi.HandlerOpt{
		httpapi.WithSessionContext(sessCtx),
	}
	var sessionStore *pgstore.SessionStore
	if ss, err := pgstore.NewSessionStore(ctx, pgPool); err == nil {
		sessionStore = ss
		handlerOpts = append(handlerOpts, httpapi.WithSessionStore(ss))
		log.Printf("session store enabled")
	} else {
		log.Printf("session store init failed: %v", err)
	}

	// Token-based auth. Bootstrap from MEMSTORE_API_KEY if set so existing
	// single-key deployments keep working without operator action.
	ts, err := pgstore.NewTokenStore(ctx, pgPool)
	if err != nil {
		return fmt.Errorf("init token store: %w", err)
	}
	if *apiKey != "" {
		if added, err := ts.EnsureLegacyToken(ctx, *apiKey); err != nil {
			log.Printf("legacy token bootstrap failed: %v", err)
		} else if added {
			log.Printf("legacy token bootstrap: imported MEMSTORE_API_KEY as name=legacy")
		}
	}
	handlerOpts = append(handlerOpts, httpapi.WithTokenVerifier(tokenVerifier{ts}))
	log.Printf("bearer-token auth enabled (api_tokens table)")
	var xq *httpapi.ExtractQueue
	if *genModel != "" {
		genBaseURL := *ollamaURL
		if *genURL != "" {
			genBaseURL = *genURL
		}
		gen := memstore.NewOpenAIGenerator(genBaseURL, *llmAPIKey, *genModel)
		handlerOpts = append(handlerOpts, httpapi.WithGenerator(gen))
		log.Printf("generation enabled (model=%s, url=%s)", *genModel, genBaseURL)
		if sessionStore != nil {
			xq = httpapi.NewExtractQueue(store, embedder, gen, sessionStore)
			xq.Persona = *persona
			xq.Start()
			handlerOpts = append(handlerOpts, httpapi.WithExtractQueue(xq))
			log.Printf("extract queue enabled with hint generation (gen-model=%s, persona=%q)", *genModel, xq.Persona)
		} else {
			log.Printf("extract queue disabled: requires PostgreSQL session store (--pg)")
		}
	}
	if xq != nil {
		defer xq.Stop()
		// Backfill feedback scores for historical sessions on startup.
		// Budget ~3s per fact × ~40 facts/session × ~60 sessions ≈ 2h.
		go func() {
			bfCtx, cancel := context.WithTimeout(ctx, 4*time.Hour)
			defer cancel()
			result, err := xq.BackfillFeedback(bfCtx, func(done, total int) {
				log.Printf("backfill-feedback: %d/%d sessions", done, total)
			})
			if err != nil {
				log.Printf("backfill-feedback: %v", err)
				return
			}
			if result.Sessions > 0 {
				log.Printf("backfill-feedback: done — %d sessions, %d ratings, %d errors",
					result.Sessions, result.Rated, result.Errors)
			}
		}()
	}
	// MEMSTORE_API_KEY (if set) was already imported into the api_tokens
	// table; the verifier owns auth from here on.
	handler := httpapi.New(store, embedder, "", handlerOpts...)

	eq := httpapi.NewEmbedQueue(store, embedder, *embedInterval, *embedBatch)
	eq.Start()
	defer eq.Stop()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
	}

	useTLS := !*tlsDisabled
	if useTLS {
		if *tlsCertFile == "" || *tlsKeyFile == "" {
			return errors.New("TLS required: pass --tls-cert-file and --tls-key-file (or --tls-disabled)")
		}
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13}
		if *tlsClientCA != "" {
			pool, err := loadClientCAs(*tlsClientCA)
			if err != nil {
				return fmt.Errorf("load client CA: %w", err)
			}
			tlsCfg.ClientCAs = pool
			tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
			log.Printf("mTLS enabled (client CA: %s)", *tlsClientCA)
		}
		srv.TLSConfig = tlsCfg
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *addr, err)
	}
	if onListening != nil {
		onListening(ln.Addr())
	}

	// Cancel-on-ctx: close the server when the parent context fires.
	go func() {
		<-ctx.Done()
		log.Println("shutting down...")
		srv.Close()
	}()

	if useTLS {
		log.Printf("memstored listening on %s (TLS, namespace=%s, model=%s)", ln.Addr(), *namespace, *model)
		err = srv.ServeTLS(ln, *tlsCertFile, *tlsKeyFile)
	} else {
		log.Printf("WARNING: memstored listening on %s WITHOUT TLS (--tls-disabled)", ln.Addr())
		err = srv.Serve(ln)
	}
	if err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

// tokenVerifier adapts pgstore.TokenStore to the httpapi.TokenVerifier
// interface, translating VerifyResult into httpapi.Identity. Lives in main
// so neither package depends on the other.
type tokenVerifier struct{ ts *pgstore.TokenStore }

func (t tokenVerifier) VerifyToken(ctx context.Context, token string) (httpapi.Identity, error) {
	r, err := t.ts.Verify(ctx, token)
	if err != nil {
		return httpapi.Identity{}, err
	}
	return httpapi.Identity{Name: r.Name, Scopes: r.Scopes, Source: "bearer"}, nil
}

// loadClientCAs reads a PEM bundle and returns a CertPool suitable for
// tls.Config.ClientCAs.
func loadClientCAs(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no PEM certificates found in %s", path)
	}
	return pool, nil
}
