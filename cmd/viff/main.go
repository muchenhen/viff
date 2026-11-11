// Command viff is a desktop Git client in the style of IntelliJ IDEA.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/vivaui/viva/app"
	"github.com/vivaui/viva/ui"

	"viff/assets/branding"
	vapp "viff/internal/app"
	"viff/internal/domain"
	"viff/internal/gitcli"
	"viff/internal/observability"
	"viff/internal/platform"
	"viff/internal/sshpath"
	"viff/internal/storage"
	"viff/internal/svncli"
	viffui "viff/internal/ui"
	"viff/internal/updater"
	"viff/internal/version"
)

func main() {
	os.Exit(run())
}

// run is the whole process. It is separate from main so every exit path can
// flush diagnostics and show a system dialog when the window cannot start:
// a GUI process started from Finder or Explorer has no visible stderr.
func run() (code int) {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println(version.String())
		return 0
	}
	started := time.Now()
	settingsPath := storage.DefaultPath()
	settingsLoad := time.Now()
	settings := storage.Load(settingsPath)

	logDir := filepath.Join(filepath.Dir(settingsPath), "logs")
	crashDir := observability.CrashDir(logDir)
	level := observability.ParseLevel(settings.LogLevel)
	if envLevel := os.Getenv("VIFF_LOG"); envLevel != "" {
		level = observability.ParseLevel(envLevel)
	}

	var rec observability.Recorder = observability.Nop{}
	var incidents observability.IncidentController = observability.Nop{}
	var log *observability.Log
	var logErr error
	if l, err := observability.New(observability.Options{Dir: logDir, Level: level}); err == nil {
		log, rec, incidents = l, l, l
	} else {
		logErr = err
	}
	// phaseOf times one startup phase and records it under perf.slow, so a
	// slow start can be located from the JSONL alone.
	phaseOf := func(name string, from time.Time) {
		d := time.Since(from)
		rec.Event(observability.Event{
			Event: observability.EventPerfSlow,
			Level: observability.LevelInfo,
			Fields: map[string]any{
				"phase":  name,
				"dur_ms": d.Milliseconds(),
			},
		})
	}
	phaseOf("settings_load", settingsLoad)
	defer func() {
		if log != nil {
			rec.Event(observability.Event{
				Event: observability.EventAppStop,
				Level: observability.LevelInfo,
				Fields: map[string]any{
					"reason":    "exit",
					"uptime_ms": time.Since(started).Milliseconds(),
					"dropped":   log.Dropped(),
				},
			})
			log.Flush()
			log.Close()
		}
	}()

	// Panics at the framework boundary (AppKit/Win32 callbacks) are recovered
	// by Viva; without a handler they vanish without a trace.
	app.SetFaultHandler(func(f app.Fault) {
		path, _ := observability.WriteCrash(crashDir, observability.Crash{
			Where:   f.Where,
			Message: f.Message,
			Stack:   f.Stack,
			Context: rec.Snapshot(time.Time{}, 200),
		})
		rec.Event(observability.Event{
			Event:  observability.EventFault,
			Level:  observability.LevelError,
			Fields: map[string]any{"where": f.Where, "msg": f.Message, "crash": filepath.Base(path)},
		})
		rec.Flush()
	})

	// A panic in the window thread or a background goroutine is fatal for the
	// process; write the crash file and tell the user where it is.
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			path, _ := observability.WriteCrash(crashDir, observability.Crash{
				Where:   "main",
				Message: fmt.Sprint(r),
				Stack:   stack,
				Context: rec.Snapshot(time.Time{}, 200),
			})
			rec.Event(observability.Event{
				Event:  observability.EventPanic,
				Level:  observability.LevelError,
				Fields: map[string]any{"where": "main", "msg": fmt.Sprint(r), "crash": filepath.Base(path)},
			})
			rec.Flush()
			app.ShowFatalError("Viff crashed",
				fmt.Sprintf("Viff stopped because of an internal error.\n\nA crash report was saved to:\n%s\n\nPlease report it at https://github.com/1lck/viff/issues and attach the crash report.", path))
			code = 1
		}
	}()

	settingsLevel := observability.LevelInfo
	settingsFields := map[string]any{"schema": settings.Version, "theme": settings.Theme}
	if settings.Warning != "" {
		settingsLevel = observability.LevelWarn
		settingsFields["warning"] = settings.Warning
	}
	rec.Event(observability.Event{Event: observability.EventSettingsLoad, Level: settingsLevel, Fields: settingsFields})

	git := &gitcli.Runner{Path: settings.GitPath}
	svn := &svncli.Runner{Path: settings.SVNPath}
	gitSource := "path"
	if settings.GitPath != "" {
		gitSource = "settings"
	}
	svnSource := "path"
	if settings.SVNPath != "" {
		svnSource = "settings"
	}
	// The Git and SVN probes used to run synchronously before the window
	// existed; on Windows the first process spawn can take seconds
	// (antivirus, cold start — worse for a VM-backed WSL checkout), so they
	// now run in the background and the window shows immediately. The
	// diagnostics export probes each tool itself when needed.
	probeStart := time.Now()
	go func() {
		probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		gitVersion, gitVersionErr := git.Version(probeCtx)
		fields := map[string]any{
			"phase":      "git_version",
			"dur_ms":     time.Since(probeStart).Milliseconds(),
			"git_source": gitSource,
		}
		probeLevel := observability.LevelInfo
		if gitVersionErr != nil {
			probeLevel = observability.LevelWarn
			fields["git_version"] = ""
			fields["code"] = gitcli.CodeOf(gitVersionErr)
		} else {
			fields["git_version"] = gitVersion
		}
		rec.Event(observability.Event{Event: observability.EventPerfSlow, Level: probeLevel, Fields: fields})

		svnStart := time.Now()
		svnCtx, cancelSVN := context.WithTimeout(context.Background(), 2*time.Second)
		svnVersion, svnVersionErr := svn.Version(svnCtx)
		cancelSVN()
		svnFields := map[string]any{
			"phase":      "svn_version",
			"dur_ms":     time.Since(svnStart).Milliseconds(),
			"svn_source": svnSource,
		}
		svnLevel := observability.LevelInfo
		if svnVersionErr != nil {
			svnLevel = observability.LevelWarn
			svnFields["svn_version"] = ""
		} else {
			svnFields["svn_version"] = svnVersion
		}
		rec.Event(observability.Event{Event: observability.EventPerfSlow, Level: svnLevel, Fields: svnFields})
	}()
	rec.Event(observability.Event{
		Event: observability.EventAppStart,
		Level: observability.LevelInfo,
		Fields: map[string]any{
			"version":         version.Version,
			"commit":          version.Commit,
			"build_date":      version.BuildDate,
			"go":              runtime.Version(),
			"viva":            version.VivaVersion(),
			"os":              runtime.GOOS,
			"arch":            runtime.GOARCH,
			"settings_schema": storage.SchemaVersion,
			"theme":           settings.Theme,
			"scale":           settings.UIScale,
			"log_level":       level.String(),
			"log_dir":         filepath.Join("<cfg>", "Viff", "logs"),
			"git_source":      gitSource,
			"svn_source":      svnSource,
		},
	})

	var win *viffui.Window
	windowCreate := time.Now()
	ws := vapp.NewWorkspace(git, settings, func() {
		if win != nil {
			win.Invalidate()
		}
	})
	// Both adapters are registered; the app picks one per repository by a local
	// probe of .git/.svn, so one binary opens either kind of working copy.
	ws.SetBackends(git, svn)
	ws.SetRecorder(rec, incidents)
	ws.SetLogLevel(settings.LogLevel)
	// The updater owns downloads and self-replacement; the app only sees the
	// narrow UpdateService interface. Cleanup removes leftovers from an
	// interrupted install before the window opens.
	updates := updater.NewClient(version.Version)
	ws.SetUpdateService(updates)
	updates.Cleanup()
	ws.Watch = func(repo domain.Repo, changed func(domain.RepoChange)) (vapp.Watcher, error) {
		return platform.Watch(repo, changed, platform.WatchOptions{
			WorktreeRelevant: func(ctx context.Context, paths []string) (bool, error) {
				return git.WorktreeChangesRelevant(ctx, repo, paths)
			},
		})
	}
	win = viffui.New(ws)
	phaseOf("window_create", windowCreate)

	if logErr != nil {
		ws.Notify(vapp.NoticeWarning, "Logging disabled", "Viff cannot write its diagnostics log: "+logErr.Error())
	}

	// A path argument opens that repository; otherwise the last one, as
	// IntelliJ reopens the last project. A remote path (an ssh:// URL or a WSL
	// checkout) is passed on without a stat: probing it can block on the
	// network or on a file system that has to boot first, and Discover reports
	// failures anyway.
	dir := ""
	if len(os.Args) > 1 {
		if sshpath.Is(os.Args[1]) {
			dir = os.Args[1]
		} else if abs, err := filepath.Abs(os.Args[1]); err == nil {
			dir = abs
		}
	} else if len(settings.Recent) > 0 {
		recent := settings.Recent[0].Path
		remote := vapp.IsRemotePath(recent)
		if remote {
			dir = recent
		} else if _, err := os.Stat(recent); err == nil {
			dir = recent
		}
	}
	win.Start(dir)
	phaseOf("window_start", windowCreate)

	// The crash-directory scan runs after the window is up: nothing in the
	// first frame depends on it, and a notice queued here shows on the next
	// wake.
	go func() {
		if crash, ok := observability.PreviousCrash(crashDir, 7*24*time.Hour); ok {
			rec.Event(observability.Event{
				Event:  observability.EventAppRecover,
				Level:  observability.LevelWarn,
				Fields: map[string]any{"crash": filepath.Base(crash)},
			})
			win.SetCrashNotice(crash)
		}
	}()

	// RunApplication, not RunCanvas: the application owns the window, which is
	// what lets Viff keep an icon in the status area. RunCanvas has no handle to
	// add one to.
	if err := app.RunApplication(func(application *app.Application) error {
		native, err := application.OpenWindow(win.Config(), &ui.Canvas{})
		if err != nil {
			return err
		}
		win.SetTray(native, application.Quit)
		// macOS menu and Dock Quit bypass the window's close button; keep the
		// same unsaved-editor confirmation on that path.
		application.OnQuitRequest(func(allow func()) { win.ConfirmQuit(allow) })
		// A desktop with no status area (a Linux session without a
		// StatusNotifier host) still runs; only the icon is missing then.
		if _, err := application.AddStatusItem(app.StatusItemConfig{
			Tooltip: "Viff",
			Icon:    branding.Icon.StatusIcon(32),
			OnClick: func(app.StatusClick) { win.ShowFromTray() },
			Menu:    win.TrayMenu,
		}); err != nil {
			rec.Event(observability.Event{
				Event:  observability.EventAppStart,
				Level:  observability.LevelWarn,
				Fields: map[string]any{"status_item": err.Error()},
			})
		}
		return nil
	}); err != nil {
		rec.Event(observability.Event{
			Event:  observability.EventAppFatal,
			Level:  observability.LevelError,
			Fields: map[string]any{"phase": "window", "msg": err.Error()},
		})
		fmt.Fprintln(os.Stderr, "viff:", err)
		app.ShowFatalError("Viff cannot start", fmt.Sprintf("%s\n\nLogs: %s", err, logDir))
		return 1
	}
	return 0
}
