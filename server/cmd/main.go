package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/41tair/slopless/pkg/config"
	"github.com/41tair/slopless/pkg/controller"
	"github.com/41tair/slopless/pkg/data"
	"github.com/41tair/slopless/pkg/service"
	"github.com/41tair/slopless/pkg/tasks"
)

func main() {
	conf := flag.String("config", "./configs/config.yaml", "config path")
	bootstrap := flag.Int("bootstrap-invites", 0, "issue 1–1000 seed invitations and exit (local administration)")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: slopless [-config path] [-bootstrap-invites 1..1000]")
		fmt.Fprintln(flag.CommandLine.Output(), "       slopless [-config path] create-user -username name")
		flag.PrintDefaults()
	}

	flag.Parse()
	if *bootstrap < 0 || *bootstrap > 1000 {
		flag.Usage()
		os.Exit(2)
	}
	var username string
	if flag.NArg() != 0 {
		if flag.Arg(0) != "create-user" || *bootstrap != 0 {
			flag.Usage()
			os.Exit(2)
		}
		command := flag.NewFlagSet("create-user", flag.ExitOnError)
		command.StringVar(conf, "config", *conf, "config path")
		command.StringVar(&username, "username", "", "username for the new user")
		command.Usage = func() {
			fmt.Fprintln(command.Output(), "usage: slopless create-user [-config path] -username name")
			fmt.Fprintln(command.Output(), "Reads one password line from stdin (8–72 bytes).")
			command.PrintDefaults()
		}
		command.Parse(flag.Args()[1:])
		if command.NArg() != 0 || strings.TrimSpace(username) == "" {
			command.Usage()
			os.Exit(2)
		}
	}

	settings, err := config.ParseConfig(*conf)
	if err != nil {
		slog.Error("load configuration", "error", err, "path", *conf)
		os.Exit(1)
	}
	failureMessage := "run server"
	if username != "" {
		failureMessage = "create user"
		err = createUser(settings, username)
	} else if *bootstrap != 0 {
		failureMessage = "bootstrap invitations"
		err = bootstrapInvitations(settings, *bootstrap)
	} else {
		err = runServer(settings)
	}
	if err != nil {
		slog.Error(failureMessage, "error", err)
		os.Exit(1)
	}
}

func closeStore(store *data.Store, operation string) {
	if err := store.Close(); err != nil {
		slog.Error("close data store", "error", err, "operation", operation)
	}
}

func bootstrapInvitations(conf *config.Config, count int) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	store, err := data.Open(ctx, conf.Data.DSN)
	if err != nil {
		return err
	}
	defer closeStore(store, "bootstrap invitations")
	svc, err := service.New(store, conf.Credits)
	if err != nil {
		return err
	}
	invites, err := svc.BootstrapInvitations(ctx, count)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(invites)
}

func createUser(conf *config.Config, username string) error {
	// Include room for CRLF and one extra byte so oversized passwords are rejected.
	password, err := bufio.NewReader(io.LimitReader(os.Stdin, 75)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read password from stdin: %w", err)
	}
	password = strings.TrimSuffix(strings.TrimSuffix(password, "\n"), "\r")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	store, err := data.Open(ctx, conf.Data.DSN)
	if err != nil {
		return err
	}
	defer closeStore(store, "create user")
	svc, err := service.New(store, conf.Credits)
	if err != nil {
		return err
	}
	user, err := svc.CreateUser(ctx, username, password)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(user)
}

func runServer(conf *config.Config) error {
	if err := conf.Validate(); err != nil {
		return err
	}
	trustedProxies, err := conf.Server.TrustedProxyPrefixes()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	store, err := data.Open(ctx, conf.Data.DSN)
	if err != nil {
		return err
	}
	defer closeStore(store, "run server")
	svc, err := service.New(store, conf.Credits)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              conf.Server.Addr,
		Handler:           controller.NewRouter(svc, conf.Server.SecureCookies, trustedProxies...),
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		tasks.Run(ctx, svc)
	}()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("shut down HTTP server", "error", err)
			if closeErr := server.Close(); closeErr != nil {
				slog.Error("force close HTTP server", "error", closeErr)
			}
		}
	}()
	err = server.ListenAndServe()
	stop()
	<-shutdownDone
	<-cleanupDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
