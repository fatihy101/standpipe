package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fatihy101/standpipe/internal/adapter"
	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/httpapi"
	"github.com/fatihy101/standpipe/internal/metrics"
	"github.com/fatihy101/standpipe/internal/store"
)

// version is stamped into the detailed health body.
var version = "0.1.0-phase0"

// Compile-time proof that the concrete packages satisfy the HTTP surface's
// narrow views. A mismatch should surface as a compile error in the gateway
// composition, and this makes it an actual compile error rather than a hope.
var (
	_ httpapi.Store      = (*store.DB)(nil)
	_ httpapi.Dispatcher = (*forge.Client)(nil)
)

func runGateway(args []string) error {
	fs := flag.NewFlagSet("standpiped", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, daemonUsage) }
	cfgPath := fs.String("config", "standpipe.yaml", "path to standpipe.yaml")
	listen := fs.String("listen", "", "override gateway.listen")
	adminListen := fs.String("admin-listen", "", "override gateway.admin_listen")
	// `standpipe onboard` sends SIGHUP "from a pid file or with
	// --signal". Without this flag the pid file half of that sentence has no
	// producer, and an operator who wanted the documented behaviour had to
	// find the pid by hand every time.
	pidFile := fs.String("pid-file", "", "write this process's pid here for `standpipe onboard` to SIGHUP")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The operator verbs are not defined here on purpose. Answering
	// `standpiped inventory` with the config it never got would be worse than
	// saying which binary owns the verb.
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("%q is not a flag; operator commands live in the standpipe binary", fs.Arg(0))
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Gateway.Listen = *listen
	}
	if *adminListen != "" {
		cfg.Gateway.AdminListen = *adminListen
	}
	if cfg.Gateway.Listen == cfg.Gateway.AdminListen {
		return errors.New("gateway: --listen and --admin-listen must differ")
	}
	// At startup — unlike `config validate` — an unreadable App key is fatal.
	if err := cfg.CheckFiles(); err != nil {
		return err
	}

	log := newLogger(cfg.Observability)
	for _, w := range cfg.Warnings {
		log.Warn("configuration warning", "detail", w)
	}
	if len(cfg.Observability.StructuredLogFields) > 0 {
		// The key is accepted and validated, but Phase 0 emits a fixed field set
		// rather than filtering to this list. Saying so beats letting an
		// operator believe a filter is in force.
		log.Info("observability.structured_log_fields is recorded but not applied as a filter in Phase 0",
			"configured", cfg.Observability.StructuredLogFields)
	}

	reg := metrics.New()
	reloader := config.NewReloader(cfg)

	// ---- Startup step 1: SQLite opens first, with the migration check.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := store.Open(ctx, store.Config{
		Path:         cfg.Storage.SQLitePath,
		MinFreeBytes: cfg.Storage.MinFreeBytes,
	})
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer func() { _ = db.Close() }()

	// The adapter: the ONE place store types become forge types and back.
	fstore := &adapter.ForgeStore{DB: db}

	gh, err := forge.New(forge.Config{
		AppID:           cfg.GitHub.AppID,
		PrivateKeyPath:  cfg.GitHub.PrivateKeyPath,
		MaxInflight:     cfg.GitHub.MaxInflight,
		MaxQueueWait:    cfg.GitHub.MaxQueueWait.D(),
		UpstreamTimeout: cfg.Gateway.UpstreamTimeout.D(),
		// The RESOLVED configuration value is
		// passed, not a zero. forge normalises nil to its documented default
		// (true) and reads it through tier1Armed, and Client.Plan is what
		// fills forge.PlanOptions — nothing in this codebase constructs a
		// PlanOptions by hand, so a zero-valued one has no call site.
		ReturnRunDetails:         cfg.GitHub.ReturnRunDetails,
		InventoryHistoryDays:     cfg.Inventory.HistoryDays,
		InventoryRefreshInterval: cfg.Inventory.RefreshInterval.D(),
		// github.base_url. Defaulted by internal/config to api.github.com, so
		// this is a pass-through and not a second place the default lives.
		// Before this key existed the field was left zero and the compiled
		// daemon could only ever reach github.com — which is why the
		// acceptance test had to rebuild this composition instead of running
		// the binary, and why GitHub Enterprise Server had no configuration
		// to reach it.
		BaseURL:   cfg.GitHub.BaseURL,
		UserAgent: "standpipe/" + version,
	}, forge.WithStore(fstore), forge.WithMetrics(reg))
	if err != nil {
		return fmt.Errorf("github client: %w", err)
	}

	srv, err := httpapi.New(httpapi.Options{
		Reloader: reloader,
		Store:    db,
		Forge:    gh,
		Metrics:  reg,
		Log:      log,
		Version:  version,
	})
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()

	// ---- Startup step 2: the listeners open BEFORE any GitHub work.
	//
	// net.Listen, not ListenAndServe: binding here means a port already in use
	// is a startup error, and it means the socket is accepting before the
	// bootstrap goroutine starts. A gateway whose whole purpose is to work
	// while GitHub is broken must not wait on GitHub to come up.
	publicLn, err := net.Listen("tcp", cfg.Gateway.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Gateway.Listen, err)
	}
	adminLn, err := net.Listen("tcp", cfg.Gateway.AdminListen)
	if err != nil {
		_ = publicLn.Close()
		return fmt.Errorf("listen %s: %w", cfg.Gateway.AdminListen, err)
	}

	// The pid file is written only AFTER both listeners bind. A pid file that
	// exists while the port is still unbound would have `standpipe onboard`
	// signal a gateway that cannot yet reload, and on a bind failure it would
	// name a process that no longer exists.
	if *pidFile != "" {
		if err := writePIDFile(*pidFile); err != nil {
			_ = publicLn.Close()
			_ = adminLn.Close()
			return err
		}
		defer func() {
			// Removed on an orderly exit only. A pid file left behind by a
			// crash is stale, and `kill -HUP` against a recycled pid is why
			// onboard reports what it signalled rather than assuming.
			_ = os.Remove(*pidFile)
		}()
	}

	public := newHTTPServer(srv.PublicHandler(), cfg)
	admin := newHTTPServer(srv.AdminHandler(), cfg)

	serveErr := make(chan error, 2)
	go func() { serveErr <- serveIgnoringClose(public, publicLn) }()
	go func() { serveErr <- serveIgnoringClose(admin, adminLn) }()

	log.Info("gateway listening",
		"listen", cfg.Gateway.Listen, "admin_listen", cfg.Gateway.AdminListen,
		"sqlite", cfg.Storage.SQLitePath, "version", version,
		"tier1_armed", cfg.GitHub.Tier1Armed())

	// ---- Startup steps 4-5: background work. A bootstrap failure NEVER blocks
	// startup; it is logged and shows up on gateway_target_unavailable.
	var wg sync.WaitGroup
	bg := &background{
		log: log, db: db, gh: gh, srv: srv, reg: reg,
		reloader: reloader, cfg: cfg,
	}
	bg.start(ctx, &wg)

	// ---- signals
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	var runErr error
loop:
	for {
		select {
		case err := <-serveErr:
			if err != nil {
				runErr = err
				break loop
			}
		case sig := <-sigs:
			switch sig {
			case syscall.SIGHUP:
				// Config reload. A failed reload changes NOTHING: Reload validates
				// into a fresh value and publishes only on success, so the
				// running configuration survives a broken file. SIGHUP is
				// never a way to shoot the process in the foot.
				res, err := reloader.Reload()
				if err != nil {
					log.Error("configuration reload FAILED; the previous configuration is still in force", "err", err)
					continue
				}
				srv.ApplyConfig(res)
				log.Info("configuration reloaded", "path", cfg.Path)
			default:
				log.Info("shutdown requested", "signal", sig.String())
				break loop
			}
		}
	}

	// ---- Shutdown order.
	//
	// 1. Stop accepting new requests. http.Server.Shutdown closes the
	//    listeners and then WAITS for in-flight handlers; it does not cancel
	//    their contexts, and the trigger handler additionally detaches its
	//    dispatch from the request context, so a dispatch already on the wire
	//    finishes and records its outcome.
	// 2. Wait shutdown_grace, which the configuration sets above upstream_timeout.
	// 3. Anything still unfinished is left in_flight in the database, and
	//    the crash-recovery sweep — which runs on the NEXT start too — takes it to
	//    `unknown`. That is the truthful state: we do not know whether GitHub
	//    received it, and a dispatch is never retried on a guess because a
	//    retry can run the workflow twice.
	grace := cfg.Gateway.ShutdownGrace.D()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), grace)
	defer shutCancel()

	var shutWG sync.WaitGroup
	shutWG.Add(2)
	go func() { defer shutWG.Done(); _ = public.Shutdown(shutCtx) }()
	go func() { defer shutWG.Done(); _ = admin.Shutdown(shutCtx) }()
	shutWG.Wait()

	if err := shutCtx.Err(); errors.Is(err, context.DeadlineExceeded) {
		log.Warn("shutdown grace elapsed with requests still in flight; their idempotency rows will be swept to `unknown`",
			"shutdown_grace", grace)
	}

	// Background loops stop after the listeners, not before: the resolver and
	// the maintenance sweep are the things that clean up after an interrupted
	// dispatch.
	cancel()
	wg.Wait()

	// Flush the open 429 coalescing windows.
	_ = srv.Close()

	log.Info("gateway stopped")
	return runErr
}

// writePIDFile writes the current pid, atomically, next to the target path.
func writePIDFile(path string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".standpipe-pid-*")
	if err != nil {
		return fmt.Errorf("pid file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := fmt.Fprintf(tmp, "%d\n", os.Getpid()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pid file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pid file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("pid file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("pid file: %w", err)
	}
	return nil
}

// newHTTPServer applies the server's security settings (timeouts, header limit).
func newHTTPServer(h http.Handler, cfg *config.Config) *http.Server {
	return &http.Server{
		Handler: h,
		// WriteTimeout must exceed upstream_timeout, or
		// the server cuts the response off while a legal dispatch is still
		// running.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.Gateway.ShutdownGrace.D() + 15*time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
}

func serveIgnoringClose(s *http.Server, ln net.Listener) error {
	if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newLogger(o config.Observability) *slog.Logger {
	var level slog.Level
	switch o.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	// JSON by default. It also means a value that reached a log field
	// without passing X-Request-Id's charset filter is escaped rather than
	// able to forge a line.
	if o.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
