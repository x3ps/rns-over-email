package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	rnspipe "github.com/x3ps/go-rns-pipe"

	"github.com/x3ps/rns-iface-email/internal/config"
	imapworker "github.com/x3ps/rns-iface-email/internal/imap"
	"github.com/x3ps/rns-iface-email/internal/inbox"
	"github.com/x3ps/rns-iface-email/internal/online"
	"github.com/x3ps/rns-iface-email/internal/pipe"
	"github.com/x3ps/rns-iface-email/internal/transport"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, rnspipe.ErrPipeClosed) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Logger setup.
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: cfg.SlogLevel()}
	if cfg.Logging.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	logger := slog.New(handler)

	config.WarnInsecureTLS(cfg, logger)

	// Context with signal handling.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Transport.
	sender := &transport.SMTPSender{
		Host:     cfg.SMTP.Host,
		Port:     cfg.SMTP.Port,
		Username: cfg.SMTP.Username,
		Password: cfg.SMTP.Password,
		From:     cfg.SMTP.From,
		TLSMode:  cfg.SMTP.TLS,
		Logger:   logger,
	}

	// Pipe.
	iface := rnspipe.New(rnspipe.Config{
		Name:      cfg.Pipe.Name,
		MTU:       cfg.Pipe.MTU,
		ExitOnEOF: true,
		Logger:    logger,
	})
	iface.OnStatus(func(online bool) {
		logger.Info("pipe status", "online", online)
	})

	// Online state aggregator: combines IMAP and SMTP health into a single
	// signal. Must be created before iface.Start() so the initial
	// callback(false) closes the startup window.
	agg := online.NewAggregator(iface.SetOnline, logger)

	pipeHandler := pipe.NewHandler(
		sender, logger, cfg.Peer.Email, cfg.SMTP.From,
		agg.SetSMTP,
		time.Duration(cfg.SMTP.RecoveryDelay)*time.Second,
		time.Duration(cfg.SMTP.MaxRecoveryDelay)*time.Second,
	)
	iface.OnSend(func(pkt []byte) error { return pipeHandler.HandlePacket(ctx, pkt) })

	// Inbound: IMAP worker.
	inboxRepo := inbox.NewJSONRepo(cfg.Checkpoint.Path, logger)
	inject := func(ctx context.Context, pkt []byte) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if err := iface.Receive(pkt); err == nil {
				return nil
			} else if !errors.Is(err, rnspipe.ErrOffline) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	imapW := imapworker.NewWorker(cfg.IMAP, cfg.Peer.Email, cfg.SMTP.From, inboxRepo, inject, logger)
	imapW.SetOnline(agg.SetIMAP)

	errCh := make(chan error, 2)

	// Start pipe.
	go func() {
		errCh <- iface.Start(ctx)
	}()

	// Start IMAP inbound worker.
	go func() {
		errCh <- imapW.Run(ctx)
	}()

	// Wait for first error or signal.
	err = <-errCh
	cancel()
	// Drain remaining goroutine.
	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		logger.Warn("shutdown timed out waiting for goroutines")
	}
	return err
}
