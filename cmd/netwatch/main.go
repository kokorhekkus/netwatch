package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/kokorhekkus/netwatch/internal/app"
	"github.com/kokorhekkus/netwatch/internal/launchd"
	"github.com/kokorhekkus/netwatch/internal/server"
	"github.com/kokorhekkus/netwatch/internal/store"
)

var build = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "run":
		err = cmdRun(log, args)
	case "status":
		err = cmdStatus(args)
	case "probe":
		err = cmdProbe(args)
	case "speedtest":
		err = cmdSpeedtest(args)
	case "trust":
		err = cmdTrust(args)
	case "install":
		err = cmdInstall(args)
	case "uninstall":
		err = cmdUninstall(args)
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `netwatch — continuous internet quality monitor

  netwatch run                 start the daemon (serves the dashboard)
  netwatch status [-window 1h] summarise recent quality
  netwatch probe [-n 20]       one-shot probe, for checking against ping(8)
  netwatch trust [-off]        allow capacity tests on this network
  netwatch speedtest           run a capacity test now (~315 MB)
  netwatch install             install and start the launchd agent
  netwatch uninstall           stop and remove the launchd agent

`)
}

func cmdRun(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultPath(), "database path")
	addr := fs.String("http", server.DefaultAddr, "dashboard listen address (empty to disable)")
	fs.Parse(args)

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	log.Info("netwatch starting", "db", *dbPath, "build", build)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	if *addr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.New(db, log).ListenAndServe(ctx, *addr); err != nil {
				log.Error("dashboard", "err", err)
			}
		}()
	}

	if err := app.New(db, log, build).Run(ctx); err != nil {
		return err
	}
	wg.Wait()
	log.Info("netwatch stopped")
	return nil
}

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultPath(), "database path")
	addr := fs.String("http", server.DefaultAddr, "dashboard listen address")
	fs.Parse(args)

	plistPath, err := launchd.Install(*dbPath, *addr)
	if err != nil {
		return err
	}
	_, binPath, logDir := launchd.Paths()

	fmt.Printf("installed %s\n", plistPath)
	fmt.Printf("  binary    %s\n", binPath)
	fmt.Printf("  database  %s\n", *dbPath)
	fmt.Printf("  logs      %s/netwatch.log\n", logDir)
	fmt.Printf("  dashboard http://%s\n\n", *addr)

	if loaded, detail := launchd.Status(); loaded {
		fmt.Printf("agent loaded (%s)\n", detail)
	} else {
		fmt.Println("agent does not appear to be loaded; check the log")
	}
	fmt.Println("\nmacOS may show a \"Background Items Added\" notification — that is this agent.")
	return nil
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	fs.Parse(args)

	if err := launchd.Uninstall(); err != nil {
		return err
	}
	// The database is deliberately left in place: it is the history, and
	// months of it should not vanish because the agent was uninstalled.
	_, _, _ = launchd.Paths()
	fmt.Println("agent stopped and removed. The database was left untouched.")
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultPath(), "database path")
	window := fs.Duration("window", time.Hour, "how far back to summarise")
	fs.Parse(args)

	if _, err := os.Stat(*dbPath); os.IsNotExist(err) {
		return fmt.Errorf("no database at %s — has `netwatch run` been started?", *dbPath)
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	sts, err := app.Status(context.Background(), db, *window)
	if err != nil {
		return err
	}
	app.WriteStatus(os.Stdout, sts, *window)
	return nil
}
