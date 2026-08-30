// memstored is the memstore network daemon. It serves the memstore HTTP API
// and processes embeddings in the background.
package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/infodancer/oidclient"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/memstore"
	"github.com/matthewjhunter/memstore/httpapi"
	"github.com/matthewjhunter/memstore/internal/screening"
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

// checkTransport decides whether the daemon may serve on the transport it was
// configured for.
//
// TLS is the default and plaintext is the exception, but disabling TLS is not
// enough on its own: the operator has to affirm the listener is reachable only
// over a trusted path. memstored cannot determine that itself. Under Docker a
// proxy-fronted deployment binds 0.0.0.0 inside a private network, which is
// indistinguishable from 0.0.0.0 on a routable LAN, so a check that sniffed the
// interface would refuse the safe configuration and get switched off out of
// irritation -- leaving nothing at all.
//
// What rides on that listener is every bearer token and every fact recalled
// through it, in the clear. A trusted LAN is a legitimate answer; assuming one
// on an operator's behalf is not.
func checkTransport(tlsDisabled, insecurePlaintext bool, certFile, keyFile string) error {
	if !tlsDisabled {
		if certFile == "" || keyFile == "" {
			return errors.New("TLS required: pass --tls-cert-file and --tls-key-file, " +
				"or --tls-disabled --insecure-plaintext to serve without it")
		}
		return nil
	}
	if !insecurePlaintext {
		return errors.New("--tls-disabled serves every token and every recalled fact in the clear. " +
			"Pass --insecure-plaintext (or set MEMSTORE_INSECURE_PLAINTEXT=true) to affirm that this " +
			"listener is reachable only over a trusted path: loopback, a private container network, or " +
			"a LAN you control. Otherwise configure --tls-cert-file and --tls-key-file")
	}
	return nil
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
	// First-start identity. An empty database has no owner for anything, and
	// pgstore refuses to open one rather than guess. Naming the owner here is
	// the same act as `memstore admin tier3-init --default-user`, done by the
	// daemon so a container can come up cold; on a database that already has
	// its owner recorded it is a no-op.
	defaultUser := fs.String("default-user", os.Getenv("MEMSTORE_DEFAULT_USER"),
		"user to record as the owner of an empty database on first start "+
			"(default: MEMSTORE_DEFAULT_USER; empty = the database must already have one)")
	ollamaURL := fs.String("ollama", cfg.Ollama, "LLM API base URL for chat generation (defaults --gen-url)")
	// Secrets are not flag defaults: flag prints defaults in --help output, which
	// would echo the configured key to the terminal. Resolved from cfg after Parse.
	apiKey := fs.String("api-key", "", "API key for authentication (default: from config file or MEMSTORE_API_KEY; empty = disabled)")
	llmAPIKey := fs.String("llm-api-key", "", "API key for the chat LLM provider (default: from config file or MEMSTORE_LLM_API_KEY; empty = no auth)")
	genModel := fs.String("gen-model", cfg.GenModel, "LLM model for generation (enables /v1/generate)")
	genURL := fs.String("gen-url", cfg.GenURL, "separate LLM URL for generation (defaults to --ollama)")
	// OAuth protected-resource discovery. Both empty (the default) means the
	// metadata document is not served and no challenge is emitted, which is
	// current behaviour. The public URL cannot be derived from a request --
	// this daemon may sit behind a proxy, and the API module cannot see its own
	// mount prefix -- so the operator has to state it.
	publicURL := fs.String("public-url", os.Getenv("MEMSTORE_PUBLIC_URL"),
		"scheme and host this daemon is publicly reached at, e.g. https://memstore.example.net "+
			"(enables OAuth protected-resource discovery together with --oauth-issuer)")
	oauthIssuer := fs.String("oauth-issuer", os.Getenv("MEMSTORE_OAUTH_ISSUER"),
		"OAuth authorization server issuer URL advertised to clients, "+
			"e.g. https://webauth.example.net/t/memstore")
	// Separate from --oauth-issuer so discovery can be advertised before token
	// acceptance is switched on; see where it is consumed below.
	// The scope namespace this authorization server uses for memstore. Empty
	// suits a server serving only memstore; a shared one namespaces its scopes,
	// and the convention is that deployment's, not something either program can
	// know. It must match on both sides -- see WithProtectedResource.
	oauthScopePrefix := fs.String("oauth-scope-prefix", os.Getenv("MEMSTORE_OAUTH_SCOPE_PREFIX"),
		"namespace the authorization server prefixes memstore's scopes with, "+
			"e.g. \"memstore:\" (empty = bare read/write/admin)")
	oauthJWKS := fs.String("oauth-jwks", os.Getenv("MEMSTORE_OAUTH_JWKS"),
		"OAuth authorization server JWKS URL; setting it ENABLES accepting OAuth "+
			"bearer tokens, which autoprovisions a memstore user for any subject "+
			"the issuer will mint a token for")
	screenMode := fs.String("screen-mode", cfg.ScreenMode,
		"model screen participation: off | observe (readable, verdict recorded) | gate (unreadable until screened, blocks)")
	screenThreat := fs.Int("screen-threat", cfg.ScreenThreat,
		"model threat score (0-10) at which a write is blocked (gate mode)")
	screenDetectWrite := fs.String("screen-detect-write", cfg.ScreenDetectWrite,
		"what the regex screen does to a tripping write: allow | warn | block")
	screenDetectRead := fs.String("screen-detect-read", cfg.ScreenDetectRead,
		"what the regex screen does to a tripping read: allow | warn | block")
	screenDetectReadScore := fs.Int("screen-detect-read-score", cfg.ScreenDetectReadScore,
		"detect score at which a read is withheld; above the write score on purpose, "+
			"because a blocked read is silent")
	screenDetectScore := fs.Int("screen-detect-score", cfg.ScreenDetectScore,
		"detect score (0-100) at which the inline regex screen rejects a write")
	screenConcurrency := fs.Int("screen-concurrency", cfg.ScreenConcurrency,
		"simultaneous model screens")
	screenBatch := fs.Int("screen-batch", cfg.ScreenBatch, "pending facts claimed per worker tick")
	screenInterval := fs.Int("screen-interval-seconds", cfg.ScreenIntervalSec, "seconds between worker ticks")
	screenMaxAttempts := fs.Int("screen-max-attempts", cfg.ScreenMaxAttempts,
		"failed screens before a fact is abandoned")
	embedInterval := fs.Duration("embed-interval", 2*time.Second, "embed queue poll interval")
	embedBatch := fs.Int("embed-batch", 32, "embed queue batch size")
	tlsCertFile := fs.String("tls-cert-file", cfg.TLSCertFile, "TLS certificate file (PEM)")
	tlsKeyFile := fs.String("tls-key-file", cfg.TLSKeyFile, "TLS private key file (PEM)")
	tlsClientCA := fs.String("tls-client-ca-file", cfg.TLSClientCAFile,
		"PEM bundle of CAs trusted for client certs; presence enables mTLS")
	tlsDisabled := fs.Bool("tls-disabled", cfg.TLSDisabled,
		"disable TLS (only for proxy-fronted deployments); requires --insecure-plaintext")
	insecurePlaintext := fs.Bool("insecure-plaintext", cfg.InsecurePlaintext,
		"affirm that the plaintext listener is reachable only over a trusted path "+
			"(loopback, a private container network, or a LAN you control); required with --tls-disabled")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument(s): %v (memstored takes flags only, no subcommands)", fs.Args())
	}

	// Fall back to the configured secrets when the flags are unset.
	if *apiKey == "" {
		*apiKey = cfg.APIKey
	}
	if *llmAPIKey == "" {
		*llmAPIKey = cfg.LLMAPIKey
	}

	// Settle the transport before connecting to anything. These are argument
	// errors: surfacing them only after a successful database connection means a
	// misconfigured deployment fails late and for the wrong-looking reason.
	if err := checkTransport(*tlsDisabled, *insecurePlaintext, *tlsCertFile, *tlsKeyFile); err != nil {
		return err
	}

	if *pgDSN == "" {
		return errors.New("PostgreSQL is required: pass --pg or set MEMSTORE_PG_SECRET " +
			"(for single-user local development, use memstore-mcp directly with no daemon)")
	}

	embCfg, err := memstore.EmbedConfigFromEnv()
	if err != nil {
		return fmt.Errorf("embedder config: %w", err)
	}
	embedder, err := embedding.New(embCfg)
	if err != nil {
		return fmt.Errorf("create embedder: %w", err)
	}
	log.Printf("embedder configured (backend=%s, model=%s)", embCfg.Backend, embCfg.Model)
	memstore.LogEmbedModel(embCfg)

	pgPool, err := pgxpool.New(ctx, *pgDSN)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer pgPool.Close()

	cacheSize, err := queryCacheSize()
	if err != nil {
		return err
	}
	pgStore, err := pgstore.New(ctx, pgPool, embedder, *namespace, *vecDim, cacheSize)
	if errors.Is(err, pgstore.ErrNoDefaultUser) && *defaultUser != "" {
		// First start on an empty database: the schema now exists and has no
		// owner. Record the one we were given and open again.
		if err := pgstore.InitIdentity(ctx, pgPool, *namespace, *defaultUser); err != nil {
			return fmt.Errorf("init default user: %w", err)
		}
		log.Printf("first start: recorded %q as the default user", *defaultUser)
		pgStore, err = pgstore.New(ctx, pgPool, embedder, *namespace, *vecDim, cacheSize)
	}
	if err != nil {
		return fmt.Errorf("init postgres store: %w", err)
	}
	var store memstore.Store = pgStore
	log.Printf("using PostgreSQL store (dim=%d, query-cache=%d)", *vecDim, cacheSize)

	rr, rcfg, err := memstore.RerankerFromEnv("MEMSTORE_RERANK")
	if err != nil {
		return err
	}
	rerankPolicy, err := memstore.RerankPolicyFromEnv("MEMSTORE_RERANK")
	if err != nil {
		return err
	}
	if rr != nil {
		pgStore.SetReranker(rr)
		poolLabel := func(n int) string {
			if n > 0 {
				return strconv.Itoa(n)
			}
			return "default"
		}
		log.Printf("reranker configured (backend=%s, model=%s, normalize=%t, mode=%s, threshold=%.3f, search-candidates=%s, recall-candidates=%s, search-doc-bytes=%s, recall-doc-bytes=%s)",
			rcfg.Backend, rcfg.Model, rcfg.NormalizeScores, cmp.Or(string(rerankPolicy.Mode), "off"),
			rerankPolicy.Threshold, poolLabel(rerankPolicy.Candidates), poolLabel(rerankPolicy.RecallCandidates),
			poolLabel(rerankPolicy.DocBytes), poolLabel(rerankPolicy.RecallDocBytes))
		if !rcfg.NormalizeScores {
			log.Printf("WARNING: reranker NormalizeScores is off -- correct only if the backend " +
				"already returns [0,1] scores (Cohere/Jina/TEI). A raw-logit backend such as " +
				"llama.cpp --reranking needs MEMSTORE_RERANK_NORMALIZE_SCORES=true for fusion to work.")
		}
		if !rerankPolicy.Mode.Enabled() {
			log.Printf("note: reranker is configured but MEMSTORE_RERANK_MODE is off -- " +
				"search and recall stay first-stage until a mode is set (off|balanced|dominant|gate).")
		}
	} else {
		log.Printf("reranker disabled (set MEMSTORE_RERANK_BASE_URL and MEMSTORE_RERANK_MODEL to enable)")
	}

	sessCtx := httpapi.NewSessionContext()
	defer sessCtx.Stop()

	handlerOpts := []httpapi.HandlerOpt{
		httpapi.WithSessionContext(sessCtx),
	}
	if rr != nil {
		// Recall reranks under the daemon's configured policy; search callers may
		// override mode/threshold per request but inherit the candidate pool size.
		handlerOpts = append(handlerOpts, httpapi.WithReranker(rr, rerankPolicy))
	}
	// Which few pending tasks a session opens with (POST /v1/tasks/select).
	taskSelector, taskSelectorName, err := memstore.TaskSelectorFromEnv("MEMSTORE", rr, rerankPolicy.DocBytes)
	if err != nil {
		return err
	}
	log.Printf("task selector: %s", taskSelectorName)
	handlerOpts = append(handlerOpts, httpapi.WithTaskSelector(taskSelector, taskSelectorName))
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
	// Injection screening. The inline regex screen runs on every write regardless of
	// these settings -- nothing enters the store unscreened -- so what is configured
	// here is the model pass and the thresholds.
	pgStore.SetInlineRejectScore(*screenDetectScore)

	// The regex screen has two edges with different failure modes, so they are set
	// separately: a blocked write returns an error the writer can act on, while a
	// blocked read simply withholds the memory.
	detectWrite, err := memstore.ParseScreenDetectMode(*screenDetectWrite)
	if err != nil {
		return err
	}
	detectRead, err := memstore.ParseScreenDetectMode(*screenDetectRead)
	if err != nil {
		return err
	}
	pgStore.SetDetectModes(detectWrite, detectRead)
	pgStore.SetDetectReadScore(*screenDetectReadScore)

	// Both edges, always. The write edge announces itself through errors when it
	// fires; the read edge is silent, so the configuration in force has to be
	// visible at startup or a withheld memory is indistinguishable from one that
	// was never stored.
	log.Printf("injection screening: detect write=%s (score>=%d), read=%s (score>=%d)",
		detectWrite, *screenDetectScore, detectRead, *screenDetectReadScore)

	mode, err := memstore.ParseScreenMode(*screenMode)
	if err != nil {
		return err
	}
	var screenWorker *screening.Worker
	if mode != memstore.ScreenModeOff {
		if *genModel == "" {
			return fmt.Errorf("--screen-mode=%s requires a generation model (--gen-model): "+
				"the model screen has nothing to call, and enabling it without one would "+
				"queue every write for a pass that never runs", mode)
		}
		genBaseURL := *ollamaURL
		if *genURL != "" {
			genBaseURL = *genURL
		}
		screenGen := memstore.NewOpenAIGenerator(genBaseURL, *llmAPIKey, *genModel)

		// Only gate mode enforces. In observe mode the verdict is recorded and gates
		// nothing, which is the whole point: it measures the model on live traffic
		// without any user-visible change.
		pol := screening.Policy{BlockThreat: *screenThreat, Enforce: mode == memstore.ScreenModeGate}
		sc := screening.NewScreener(pol, screenGen, slog.Default())

		// The mode must be set on the service-scoped store too: the worker spans users,
		// and per-request scoped stores are copies derived from this one.
		pgStore.SetScreenMode(mode)
		svc := pgStore.ServiceScope()
		svc.SetScreenMode(mode)
		svc.SetInlineRejectScore(*screenDetectScore)
		svc.SetDetectModes(detectWrite, detectRead)
		svc.SetDetectReadScore(*screenDetectReadScore)

		screenWorker = screening.NewWorker(svc, sc, screening.WorkerConfig{
			Interval:    time.Duration(*screenInterval) * time.Second,
			Concurrency: *screenConcurrency,
			Batch:       *screenBatch,
			MaxAttempts: *screenMaxAttempts,
		}, slog.Default())
		screenWorker.Start()
		defer screenWorker.Stop()

		switch mode {
		case memstore.ScreenModeGate:
			log.Printf("injection screening: GATE -- model=%s blocks at threat>=%d, concurrency=%d; "+
				"new facts are unreadable until screened (about one tick plus one model call, "+
				"~%ds+ at this interval)",
				*genModel, *screenThreat, *screenConcurrency, *screenInterval)
		case memstore.ScreenModeObserve:
			log.Printf("injection screening: OBSERVE -- model=%s records verdicts (would block at "+
				"threat>=%d), concurrency=%d; facts stay readable and nothing is blocked by the model",
				*genModel, *screenThreat, *screenConcurrency)
		}
	} else {
		log.Printf("injection screening: regex only; model screen off")
	}

	// Similarity gates depend only on the embedding model, and both the
	// extract queue and the embed queue link against them, so resolve once
	// here rather than inside the extraction branch.
	simPolicy, err := memstore.SimilarityPolicyFromEnv("MEMSTORE", embCfg.Model)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("similarity gates (model=%s): link>=%.2f supersede>=%.2f calibrated=%t%s",
		embCfg.Model, simPolicy.LinkMinSim, simPolicy.SupersedeMinSim, simPolicy.Calibrated,
		map[bool]string{false: " -- historical constants; measure and set MEMSTORE_LINK_MIN_SIM / MEMSTORE_SUPERSEDE_MIN_SIM", true: ""}[simPolicy.Calibrated])

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
			xq.SetSimilarityPolicy(simPolicy)
			xq.Start()
			handlerOpts = append(handlerOpts, httpapi.WithExtractQueue(xq))
			log.Printf("extract queue enabled with hint generation (gen-model=%s)", *genModel)
		} else {
			log.Printf("extract queue disabled: requires PostgreSQL session store (--pg)")
		}
	}
	if xq != nil {
		defer xq.Stop()
		// Backfill feedback scores for historical sessions on startup.
		// Uses service scope so it reaches facts and sessions across all users.
		// Budget ~3s per fact × ~40 facts/session × ~60 sessions ≈ 2h.
		go func() {
			bfCtx, cancel := context.WithTimeout(ctx, 4*time.Hour)
			defer cancel()
			if sessionStore == nil {
				return
			}
			// Use service scope for backfill so it reaches facts and sessions
			// across all users. ServiceScope() is available only in main (holds
			// the concrete pgStore/sessionStore); it must never reach a handler.
			bfStore := pgStore.ServiceScope()
			bfSess := sessionStore.ServiceScope()
			result, err := xq.BackfillFeedbackService(bfCtx, bfStore, bfSess, func(done, total int) {
				log.Printf("backfill-feedback: %d/%d sessions", done, total)
			})
			if err != nil {
				log.Printf("backfill-feedback: %v", err)
				return
			}
			if result.Sessions > 0 {
				log.Printf("backfill-feedback: done -- %d sessions, %d ratings, %d errors",
					result.Sessions, result.Rated, result.Errors)
			}
		}()
	}
	// MEMSTORE_API_KEY (if set) was already imported into the api_tokens
	// table; the verifier owns auth from here on.
	// OAuth discovery, when configured. Validate before wiring: a half-built
	// metadata document advertises a resource identifier that tokens will be
	// bound to, and a wrong one fails at verification time with an audience
	// mismatch that looks like a client bug.
	var protectedResource httpapi.ProtectedResource
	if *publicURL != "" || *oauthIssuer != "" {
		protectedResource = httpapi.ProtectedResource{
			PublicBaseURL:        *publicURL,
			Prefix:               httpapi.DefaultPrefix,
			AuthorizationServers: []string{*oauthIssuer},
			ScopesSupported:      []string{httpapi.ScopeRead, httpapi.ScopeWrite, httpapi.ScopeAdmin},
			ScopePrefix:          *oauthScopePrefix,
		}
		if err := protectedResource.Validate(); err != nil {
			return fmt.Errorf("oauth discovery: %w (set both --public-url and --oauth-issuer, or neither)", err)
		}
		handlerOpts = append(handlerOpts, httpapi.WithProtectedResource(protectedResource))
		log.Printf("oauth discovery enabled (resource=%s, issuer=%s)",
			protectedResource.ResourceURL(), *oauthIssuer)

		// The verifier is separate from discovery on purpose. Serving the
		// metadata document is inert; accepting tokens is not, and it stays off
		// until --oauth-jwks is given. See docs/mcp-oauth-scope.md decision 5:
		// autoprovisioning delegates admission to the authorization server, and
		// that delegation is only real once webauth can express a scope grant
		// and an audience (sections B1-B3). Turning this on sooner would be open
		// enrolment wearing the costume of delegation.
		if *oauthJWKS != "" {
			rs, err := oidclient.NewResourceServer(ctx, oidclient.ResourceServerConfig{
				IssuerURL: *oauthIssuer,
				Resource:  protectedResource.ResourceURL(),
				JWKSURL:   *oauthJWKS,
			})
			if err != nil {
				return fmt.Errorf("oauth resource server: %w", err)
			}
			resolver, err := httpapi.NewProvisioningResolver(
				pgstore.NewOAuthUserStore(pgStore), *oauthIssuer, log.Printf)
			if err != nil {
				return fmt.Errorf("oauth user provisioning: %w", err)
			}
			verifier, err := httpapi.NewOAuthVerifier(rs, resolver, *oauthScopePrefix)
			if err != nil {
				return fmt.Errorf("oauth verifier: %w", err)
			}
			handlerOpts = append(handlerOpts, httpapi.WithTokenVerifier(verifier))
			log.Printf("oauth token verification enabled (jwks=%s)", *oauthJWKS)
		}
	}

	handler := httpapi.New(store, embedder, "", handlerOpts...)

	// Embedding is user-agnostic: a fact needs a vector regardless of owner.
	// Use service scope so NeedingEmbedding/SetEmbedding/MarkEmbedFailed span
	// all users. ServiceScope() is concrete (only reachable via pgStore here).
	eq := httpapi.NewEmbedQueue(pgStore.ServiceScope(), embedder, *embedInterval, *embedBatch)
	// The embed queue links a fact to its neighbours once it has a vector,
	// which is the earliest moment there is anything to compare. Without
	// this, only session-extracted facts are ever linked.
	eq.SetSimilarityPolicy(simPolicy)
	// The configured budget, not the model's registered one: sizing chunks
	// against the registry while requests are clipped to a lower configured
	// budget truncates every chunk's tail silently.
	eq.SetCeiling(embCfg.Limits().MaxBytes)
	eq.Start()
	defer eq.Stop()

	// Score the facts that predate the detect_score column, so the read filter has
	// something to act on. Runs regardless of screen_mode: the read filter is the
	// regex screen, which is independent of the model pass, so a deployment with the
	// model screen off still needs its corpus scored. Service scope for the same
	// reason as the embed queue -- the backlog spans users.
	//
	// It drains and exits rather than ticking forever; every fact written from here
	// on is scored at insert.
	// Zero interval and batch take the runner's own defaults: this is a one-shot
	// drain, not a steady-state loop, so it does not want to share the embed queue's
	// interval or the screening worker's batch size.
	detectBackfill := httpapi.NewDetectBackfill(pgStore.ServiceScope(), 0, 0)
	detectBackfill.Start()
	defer detectBackfill.Stop()

	// The API moves under a prefix so a second service on this host -- another
	// MCP surface, a web UI -- has somewhere of its own to mount rather than
	// finding memstore at the root. Existing clients keep working: Mount serves
	// the same handler at the root too, until their configs catch up.
	srv := &http.Server{
		Addr:              *addr,
		Handler:           httpapi.Mount(httpapi.DefaultPrefix, handler, httpapi.WithProtectedResourceMetadata(protectedResource)),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
	}

	useTLS := !*tlsDisabled
	if useTLS {
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
		log.Printf("memstored listening on %s (TLS, namespace=%s, embed=%s)", ln.Addr(), *namespace, embCfg.Model)
		err = srv.ServeTLS(ln, *tlsCertFile, *tlsKeyFile)
	} else {
		log.Printf("WARNING: memstored listening on %s WITHOUT TLS -- tokens and recalled facts "+
			"cross this listener in the clear (--tls-disabled --insecure-plaintext)", ln.Addr())
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
	return httpapi.Identity{Name: r.Name, Scopes: r.Scopes, Source: "bearer", UserID: r.UserID}, nil
}

// defaultQueryCacheSize bounds the in-process query-embedding LRU when
// MEMSTORE_QUERY_CACHE_SIZE is unset. A few hundred entries gives a high hit
// rate at single-user scale.
const defaultQueryCacheSize = 512

// queryCacheSize reads the query-embedding cache bound from
// MEMSTORE_QUERY_CACHE_SIZE, falling back to defaultQueryCacheSize when unset.
// A value of 0 disables the cache; negative or non-integer values are errors.
func queryCacheSize() (int, error) {
	v := os.Getenv("MEMSTORE_QUERY_CACHE_SIZE")
	if v == "" {
		return defaultQueryCacheSize, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid MEMSTORE_QUERY_CACHE_SIZE %q: must be a non-negative integer", v)
	}
	return n, nil
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
