// Command monbooru serves the galleries, in the server, desktop or
// portable profile.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/jobs"
	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
	internalweb "github.com/monbooru/monbooru/internal/web"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	// Before anything prints: a Windows GUI build starts with no console.
	desktop.AttachConsole()

	if len(os.Args) >= 2 && os.Args[1] == "tagger-worker" {
		runWorker(os.Args[2:])
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "healthcheck" {
		runHealthcheck(os.Args[2:])
		return
	}

	configPath := flag.String("config", "./monbooru.toml", "path to monbooru.toml config file")
	hashPassword := flag.String("hash-password", "", "print bcrypt hash of the given password and exit")
	desktopMode := flag.Bool("desktop", defaultDesktop == "true", "desktop profile: OS-native paths, a log file, a browser at startup")
	noBrowser := flag.Bool("no-browser", false, "with -desktop, do not open a browser at startup")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(versionLine())
		return
	}
	if *hashPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(*hashPassword), bcrypt.DefaultCost)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error hashing password: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(hash))
		return
	}

	// Registered first so it runs last, once the other defers have
	// flushed the pools, stopped the watchers and closed the log.
	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()

	// Runs after the defers below, so the new process finds the port, the
	// database and the log file released.
	restartOnExit := false
	var layout desktop.Layout
	defer func() {
		if !restartOnExit {
			return
		}
		if err := desktop.Relaunch(); err != nil {
			if layout.LogDir != "" {
				if f, ferr := desktop.OpenLog(layout.LogDir, appName); ferr == nil {
					defer func() { _ = f.Close() }()
				}
			}
			reportFatal("restart: " + err.Error())
		}
	}()

	var seed func(*config.Config)
	var profile internalweb.Desktop
	if *desktopMode {
		dialogOnFatal = true
		resolved, err := desktop.Resolve(appName, explicitFlag("config", *configPath))
		if err != nil {
			fatalf("resolving the desktop paths: %v", err)
		}
		layout = resolved
		*configPath = layout.ConfigPath
		profile = internalweb.Desktop{Active: true, LogDir: layout.LogDir}
		// Before config load, so every fatal below reaches the file too.
		if f, err := desktop.OpenLog(layout.LogDir, appName); err != nil {
			logx.Warnf("could not open a log file under %s: %v", layout.LogDir, err)
		} else {
			defer func() { _ = f.Close() }()
		}
		seed = desktopSeed(layout)
	} else if !inContainer() {
		seed = hostSeed(*configPath)
	}

	_, statErr := os.Stat(*configPath)
	freshConfig := os.IsNotExist(statErr)

	load := config.LoadWithDefaults
	if layout.Portable {
		load = config.LoadPortable
	}
	cfg, err := load(*configPath, seed)
	if err != nil {
		fatalf("loading config: %v", err)
	}
	if layout.Portable {
		portableGallery(layout, cfg)
	}
	logx.Set(cfg.Log.Level)
	logx.Infof("config: bind=%s galleries=%d default=%q models=%s log=%s",
		cfg.Server.BindAddress, len(cfg.Galleries), cfg.DefaultGallery, cfg.Paths.ModelPath, cfg.Log.Level)

	var ln net.Listener
	if *desktopMode {
		ln, err = desktop.Listen(appName, cfg.Server.BindAddress, bindWait)
		if errors.Is(err, desktop.ErrRunning) {
			if err := desktop.HandOver(appName, cfg.Server.BindAddress, !*noBrowser); err != nil {
				logx.Warnf("%v", err)
			}
			return
		}
		if err != nil {
			fatalf("%v", err)
		}
	} else if ln, err = net.Listen("tcp", cfg.Server.BindAddress); err != nil {
		fatalf("HTTP server: %v", err)
	}

	jobManager := jobs.NewManager()

	srv, err := internalweb.NewServer(cfg, *configPath, jobManager, profile)
	if err != nil {
		if freshConfig && inContainer() {
			logx.Warnf("monbooru wrote %s with default settings meant for the docker image.", *configPath)
			logx.Warnf("edit gallery_path (your images), paths.data_path (db + thumbnails) and paths.model_path (optional auto-taggers), then run it again. docs: %s", internalweb.DocURL)
		}
		fatalf("creating web server: %v", err)
	}
	defer srv.Close()

	srv.StartWatchers()

	httpSrv := &http.Server{
		Addr:    cfg.Server.BindAddress,
		Handler: srv.Handler(),
		// Headers only: an upload or import body can take minutes on a slow link.
		ReadHeaderTimeout: 30 * time.Second,
		// No WriteTimeout: some responses run for minutes on a large library.
		IdleTimeout: 120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	srvErr := make(chan error, 1)
	go func() {
		logx.Infof("monbooru listening on %s", cfg.Server.BindAddress)
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	if *desktopMode {
		if !*noBrowser {
			go func() {
				if err := desktop.OpenWhenServing(appName, cfg.Server.BindAddress, 30*time.Second); err != nil {
					logx.Warnf("%v", err)
				}
			}()
		}
		if srv.TrayEnabled() {
			trayCtx, stopTray := context.WithCancel(context.Background())
			trayDone := make(chan struct{})
			// The icon stays on screen until the tray's own loop removes it.
			defer func() {
				stopTray()
				select {
				case <-trayDone:
				case <-time.After(time.Second):
				}
			}()
			go func() {
				defer close(trayDone)
				runTray(trayCtx, srv, cfg.Server.BindAddress)
			}()
		}
	}

	select {
	case <-quit:
		logx.Infof("shutting down...")
	case <-srv.QuitRequested():
		logx.Infof("shutting down on request...")
	case err := <-srvErr:
		reportFatal("HTTP server: " + err.Error())
		exitCode = 1
	}
	signal.Stop(quit)
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutCtx) //nolint:errcheck
	restartOnExit = srv.RestartRequested()
}
