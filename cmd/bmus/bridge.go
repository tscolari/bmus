package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	// Tracker adapters register themselves for tracker.yaml (#54).
	_ "github.com/tscolari/bmus/internal/adapters/github"
	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/console"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/tracker"
	"github.com/tscolari/bmus/internal/transporter"
)

// maxHolodeckRoot keeps <root>/<alias>/serve.sock under the holodeck's
// 100-byte socket limit for aliases up to holodeck-<7-digit pid>-<4 digits>.
const maxHolodeckRoot = 60

type bridgeRunFlags struct {
	transporter    string
	combadge       string
	combadgeArgs   []string
	holodeckRoot   string
	holodeckScript string
	logLevel       string
	repos          []string
	roster         string
	regs           string
	tracker        string
	consultTimeout time.Duration
	hailParkAfter  time.Duration
	maxBundleBytes int64
	consolePort    int
	noConsole      bool
}

func newBridgeCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "bridge", Short: "Run the bridge daemon"}
	var f bridgeRunFlags
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the bridge daemon in the foreground until SIGINT or SIGTERM",
		Long: `Run the bridge daemon in the foreground until SIGINT or SIGTERM.

A graceful shutdown can take about a minute; a second signal kills it at once.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runBridge(cmd, a, f) },
	}
	fl := run.Flags()
	fl.StringVar(&f.transporter, "transporter", "tsh", "beam provider: tsh or holodeck")
	fl.StringVar(&f.combadge, "combadge", "",
		"combadge bundle (default: combadge.mjs next to the bmus executable, else ./combadge/dist/combadge.mjs)")
	fl.StringArrayVar(&f.combadgeArgs, "combadge-arg", nil, "extra `combadge serve` flag, repeatable (e.g. --combadge-arg=--heartbeat-interval --combadge-arg=1000)")
	fl.StringVar(&f.holodeckRoot, "holodeck-root", "", "holodeck beam directory (default <home>/holodeck); keep it short")
	fl.StringVar(&f.holodeckScript, "holodeck-script", "",
		"holodeck script every beam runs with the holodeck harness, overriding each crew member's harness (--transporter holodeck only; without it each beam runs its crew member's harness)")
	fl.StringVar(&f.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	fl.StringArrayVar(&f.repos, "repo", nil,
		"a repo orders may work on, as `<name>=<url>` (any URL git can fetch; repeatable). The bridge clones it into <home>/repos/<name> with your own git setup")
	fl.StringVar(&f.roster, "roster", "",
		"roster file: divisions, the standing orders' default and transporter limits (default <home>/"+bridge.RosterFile+"; none there means an empty roster)")
	fl.StringVar(&f.regs, "regs", "",
		"Regulations file: which hails are auto-allowed, go to Command or only to the captain (default <home>/"+regs.File+"; none there sends every hail to the captain; `bmus regs init` writes a starting point)")
	fl.StringVar(&f.tracker, "tracker", "",
		"tracker file: the issue tracker orders may come from (source: tracker:<ref>) and beams may ask to comment on or close (default <home>/"+tracker.File+"; none there means no tracker)")
	fl.DurationVar(&f.consultTimeout, "consult-timeout", bridge.DefaultConsultTimeout,
		"how long a hail waits for Command's consult reply before it goes to the captain")
	fl.DurationVar(&f.hailParkAfter, "hail-park-after", 0,
		"how long an sdk beam holds a hail before it parks the session, at most "+bridge.MaxHailParkAfter.String()+" (default: unset, serve's 15m)")
	fl.Int64Var(&f.maxBundleBytes, "max-bundle-bytes", bridge.DefaultMaxBundleBytes,
		"maximum size, in bytes, of a work or checkpoint bundle pulled from a beam")
	fl.IntVar(&f.consolePort, "console-port", 0, "console port, on 127.0.0.1 (0 picks a free one)")
	fl.BoolVar(&f.noConsole, "no-console", false, "run without the console")
	cmd.AddCommand(run)
	return cmd
}

func runBridge(cmd *cobra.Command, a *app, f bridgeRunFlags) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(f.logLevel)); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if cmd.Flags().Changed("hail-park-after") {
		if err := bridge.CheckHailParkAfter(f.hailParkAfter); err != nil {
			return fmt.Errorf("--hail-park-after: %w", err)
		}
	}
	if !f.noConsole {
		if err := console.CheckPort(f.consolePort); err != nil {
			return fmt.Errorf("--console-port: %w", err)
		}
	}

	home, err := filepath.Abs(a.home)
	if err != nil {
		return err
	}
	repos, err := parseRepoFlags(f.repos)
	if err != nil {
		return err
	}
	roster, err := loadRosterFlag(home, f.roster)
	if err != nil {
		return err
	}
	rc, regsFound, err := loadRegsFlag(home, f.regs)
	if err != nil {
		return err
	}
	tr, err := loadTrackerFlag(home, f.tracker)
	if err != nil {
		return err
	}
	bundle, err := resolveCombadge(f.combadge)
	if err != nil {
		return err
	}

	var tp transporter.Provider
	var harness, harnesses []string // the beams' harness flags; what AddCrew accepts (nil: the default)
	switch f.transporter {
	case "tsh":
		if f.holodeckScript != "" || f.holodeckRoot != "" {
			return errors.New("--holodeck-root and --holodeck-script need --transporter holodeck")
		}
		t := transporter.NewTsh("")
		t.Combadge = bundle
		tp = t
	case "holodeck":
		// A script makes every beam run the holodeck harness, whatever the
		// crew member's (the transporter's harness flags override it; see
		// transporter.CombadgeSpec).
		if f.holodeckScript != "" {
			script, err := filepath.Abs(f.holodeckScript)
			if err != nil {
				return err
			}
			if _, err := os.Stat(script); err != nil {
				return fmt.Errorf("--holodeck-script: %w", err)
			}
			harness = []string{"--harness", "holodeck", "--script", script}
			harnesses = bridge.Harnesses
		}
		if _, err := exec.LookPath("node"); err != nil {
			return fmt.Errorf("holodeck beams run combadge with node: %w", err)
		}
		root := f.holodeckRoot
		if root == "" {
			root = filepath.Join(home, "holodeck")
		}
		if root, err = filepath.Abs(root); err != nil {
			return err
		}
		if len(root) > maxHolodeckRoot {
			logger.Warn("holodeck root is long: beam socket paths may exceed the Unix socket limit; pass a shorter --holodeck-root",
				"root", root, "bytes", len(root), "max", maxHolodeckRoot)
		}
		h, err := transporter.NewHolodeck(transporter.HolodeckConfig{
			Root:     root,
			Combadge: bundle,
			Harness:  harness,
		})
		if err != nil {
			return err
		}
		tp = h
	default:
		return fmt.Errorf("--transporter: want tsh or holodeck, got %q", f.transporter)
	}

	// The first SIGINT or SIGTERM cancels ctx, a graceful shutdown; then
	// default handling is restored, so a second one kills at once.
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case sig := <-sigs:
			signal.Stop(sigs)
			logger.Info("signal received; shutting down (this can take about a minute, signal again to kill)", "signal", sig)
			cancel()
		case <-ctx.Done():
		}
	}()
	logger.Info("starting bridge", "transporter", f.transporter, "combadge", bundle)
	return bridge.Run(ctx, bridge.Config{
		Home:           home,
		Transporter:    tp,
		Logger:         logger,
		CombadgeArgs:   f.combadgeArgs,
		Repos:          repos,
		Harnesses:      harnesses,
		MaxBundleBytes: f.maxBundleBytes,
		ConsolePort:    f.consolePort,
		NoConsole:      f.noConsole,

		Divisions:         roster.Divisions,
		StandingOrders:    roster.StandingOrders,
		TransporterLimits: roster.Transporter,

		Regs:           rc,
		RegsConfigured: regsFound,
		ConsultTimeout: f.consultTimeout,
		HailParkAfter:  f.hailParkAfter,
		Tracker:        tr,
	})
}

// parseRepoFlags turns --repo <name>=<url> flags into the bridge's repo
// registry. A URL naming an existing local directory is made absolute, as
// the bridge runs git in its own clone's directory.
func parseRepoFlags(flags []string) (map[string]string, error) {
	repos := map[string]string{}
	for _, f := range flags {
		name, url, ok := strings.Cut(f, "=")
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("--repo %q: want <name>=<url>", f)
		}
		if _, dup := repos[name]; dup {
			return nil, fmt.Errorf("--repo: %s given twice", name)
		}
		if fi, err := os.Stat(url); err == nil && fi.IsDir() && !filepath.IsAbs(url) {
			abs, err := filepath.Abs(url)
			if err != nil {
				return nil, err
			}
			url = abs
		}
		repos[name] = url
	}
	if err := bridge.CheckRepos(repos); err != nil {
		return nil, fmt.Errorf("--repo: %w", err)
	}
	return repos, nil
}

// loadRosterFlag loads the --roster file, <home>/roster.yaml by default.
// A missing default file is an empty roster; a missing file named by the
// flag is an error.
func loadRosterFlag(home, flag string) (bridge.RosterConfig, error) {
	path := flag
	if path == "" {
		path = filepath.Join(home, bridge.RosterFile)
	} else if _, err := os.Stat(path); err != nil {
		return bridge.RosterConfig{}, fmt.Errorf("--roster: %w", err)
	}
	r, err := bridge.LoadRoster(path)
	if err != nil {
		return bridge.RosterConfig{}, fmt.Errorf("roster: %w", err)
	}
	return r, nil
}

// loadRegsFlag loads the --regs file, <home>/regs.yaml by default. A
// missing default file is not found; a missing file named by the flag is an
// error.
func loadRegsFlag(home, flag string) (regs.Config, bool, error) {
	path := flag
	if path == "" {
		path = filepath.Join(home, regs.File)
	} else if _, err := os.Stat(path); err != nil {
		return regs.Config{}, false, fmt.Errorf("--regs: %w", err)
	}
	c, found, err := regs.LoadRegs(path)
	if err != nil {
		return regs.Config{}, false, fmt.Errorf("regs: %w", err)
	}
	return c, found, nil
}

// loadTrackerFlag opens the --tracker file's adapter, <home>/tracker.yaml
// by default. A missing default file is no tracker (nil); a missing file
// named by the flag is an error.
func loadTrackerFlag(home, flag string) (tracker.Tracker, error) {
	path := flag
	if path == "" {
		path = filepath.Join(home, tracker.File)
	} else if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("--tracker: %w", err)
	}
	t, found, err := tracker.Load(path)
	if err != nil {
		return nil, fmt.Errorf("tracker: %w", err)
	}
	if !found {
		return nil, nil
	}
	return t, nil
}

// resolveCombadge returns the absolute path of the combadge bundle: flag if
// set, else combadge.mjs next to the executable, else
// ./combadge/dist/combadge.mjs. It fails if the file doesn't exist.
func resolveCombadge(flag string) (string, error) {
	hint := "build it with `npm run build` in combadge/ or pass --combadge"
	candidates := []string{flag}
	if flag == "" {
		candidates = nil
		if exe, err := os.Executable(); err == nil {
			if exe, err := filepath.EvalSymlinks(exe); err == nil {
				candidates = append(candidates, filepath.Join(filepath.Dir(exe), "combadge.mjs"))
			}
		}
		candidates = append(candidates, filepath.Join("combadge", "dist", "combadge.mjs"))
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			return "", err
		}
		fi, err := os.Stat(abs)
		if err == nil && fi.Mode().IsRegular() {
			return abs, nil
		}
		if flag != "" {
			if err == nil {
				err = errors.New("not a regular file")
			}
			return "", fmt.Errorf("combadge bundle %s: %v (%s)", abs, err, hint)
		}
	}
	return "", fmt.Errorf("combadge bundle not found (looked for %v): %s", candidates, hint)
}
