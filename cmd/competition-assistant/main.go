package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/authn"
	"competition-assistant/internal/config"
	"competition-assistant/internal/fetcher"
	"competition-assistant/internal/notifier"
	"competition-assistant/internal/service"
	"competition-assistant/internal/store"
	"competition-assistant/internal/webapp"
	"github.com/robfig/cron/v3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command != "serve" && command != "run-once" && command != "reset-competition-data" {
		return fmt.Errorf("unsupported command %q; use serve, run-once or reset-competition-data", command)
	}
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "sources.yaml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	database, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	if command == "reset-competition-data" {
		if os.Getenv("CONFIRM_RESET_COMPETITION_DATA") != "YES" {
			return errors.New("refusing reset: set CONFIRM_RESET_COMPETITION_DATA=YES")
		}
		report, err := database.ResetCompetitionData(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("competition data reset: competitions=%d observations=%d source_documents=%d\n", report.Competitions, report.Observations, report.Documents)
		return nil
	}
	sender := notifier.NewApprise(cfg.AppriseURL, cfg.Web.AppriseSenderURL)
	app := service.New(cfg, database, fetcher.NewHTTPCollector(cfg), analyzer.New(cfg), sender, logger)
	if _, err := app.PurgeIneligibleCompetitions(context.Background()); err != nil {
		return fmt.Errorf("purge ineligible competitions: %w", err)
	}
	serveContext := context.Background()
	stop := func() {}
	if command == "serve" {
		serveContext, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	var webServer *http.Server
	var webErrors chan error
	if cfg.Web.Enabled {
		manager, err := authn.New(cfg.Web.AppSecret)
		if err != nil {
			return err
		}
		app.EnableMultiUser(cfg.Web.PublicBaseURL, manager)
		if command == "serve" {
			web, err := webapp.New(database, sender, manager, cfg.Web, cfg.Location, logger)
			if err != nil {
				return err
			}
			web.SetPushTrigger(func(userID int64) bool { return app.StartUserDelivery(serveContext, userID) })
			web.SetBackfillTrigger(app.BackfillUser)
			web.SetCompetitionChoiceTrigger(app.SetUserCompetitionDecision)
			webServer = &http.Server{
				Addr:              cfg.Web.ListenAddr,
				Handler:           web.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       15 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       60 * time.Second,
			}
			webErrors = make(chan error, 1)
			go func() {
				logger.Info("web interface started", "listen", cfg.Web.ListenAddr, "public_url", cfg.Web.PublicBaseURL)
				webErrors <- webServer.ListenAndServe()
			}()
		}
	}

	if command == "run-once" {
		return app.Run(context.Background())
	}
	ctx := serveContext
	scheduler := cron.New(
		cron.WithLocation(cfg.Location),
		cron.WithChain(cron.SkipIfStillRunning(cron.DefaultLogger)),
	)
	if _, err := scheduler.AddFunc(cfg.Schedule, func() {
		if err := app.Run(ctx); err != nil {
			logger.Error("scheduled scan failed", "error", err)
		}
	}); err != nil {
		return fmt.Errorf("invalid schedule: %w", err)
	}
	if _, err := scheduler.AddFunc("* * * * *", func() {
		if err := app.DeliverDue(ctx); err != nil {
			logger.Warn("notification delivery pass failed", "error", err)
		}
	}); err != nil {
		return fmt.Errorf("configure delivery schedule: %w", err)
	}
	scheduler.Start()
	logger.Info("scheduler started", "schedule", cfg.Schedule, "timezone", cfg.Timezone)
	if webErrors == nil {
		<-ctx.Done()
	} else {
		select {
		case <-ctx.Done():
		case err := <-webErrors:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				stop()
				return fmt.Errorf("web server failed: %w", err)
			}
		}
	}
	shutdown := scheduler.Stop()
	<-shutdown.Done()
	if webServer != nil {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := webServer.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down web server: %w", err)
		}
	}
	return nil
}
