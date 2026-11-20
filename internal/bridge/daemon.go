// Package bridge is the bmus daemon: it owns the domain store, the DBOS
// runtime that runs the crew-tour and mission workflows, the comms pumps
// (pumps.go) and the local RPC socket (rpc.go). See spec/phase-1-plan-2026-10-16.md §2.
package bridge

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	_ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite" // registers DBOS's SQLite system database

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/console"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
	"github.com/tscolari/bmus/internal/transporter"
)

const (
	// appName is the DBOS application name. DBOS only recovers workflows
	// recorded under the same name, so it must never change.
	appName = "bmus"

	// workflowVersion is the DBOS ApplicationVersion. DBOS only recovers
	// PENDING workflows whose recorded version equals the running one, and by
	// default it derives the version from a SHA-256 of the executable, so
	// every rebuild of bmus would orphan in-flight tours and missions. Pinning
	// it keeps them recoverable across upgrades. Bump it only when workflow
	// code changes incompatibly (a different sequence of steps, Recv/Send
	// calls or child workflows for the same recorded history), knowing that
	// workflows still PENDING under the old version are then left alone.
	//
	// The DBOS__APPVERSION environment variable overrides this; don't set it.
	workflowVersion = "1"

	// Workflow names, recorded in each workflow's status row and looked up
	// at recovery. Without them DBOS would use the Go function's fully
	// qualified name, so renaming the function or moving the module would
	// strand every PENDING workflow. Like appName, these never change.
	crewTourWorkflowName    = "crew-tour"
	missionWorkflowName     = "mission"
	transporterWorkflowName = "transporter"
	hailWorkflowName        = "hail"
	// findingsWorkflowName is in findings.go.

	// transporterQueueName is the DBOS queue transporter workflows run on
	// (spec/phase-2-plan-2026-10-19.md §5). Like the workflow names, it
	// never changes: enqueued workflows record it.
	transporterQueueName = "transporter"

	// shutdownTimeout bounds each stage of DBOS's graceful shutdown (about
	// six stages, so a full shutdown can take several times this).
	shutdownTimeout = 10 * time.Second

	// defaultDismissWait bounds DismissCrew's wait for the result of the
	// tour it signalled (#113). A tour past its terminal step ends within
	// milliseconds; one still running ends later and removes its beam
	// itself. Config.dismissWait overrides it in tests.
	defaultDismissWait = 3 * time.Second
)

// Config configures the bridge daemon.
type Config struct {
	// Home is the bridge directory holding bridge.db and bridge.lock,
	// normally store.DefaultDir().
	Home string
	// Transporter provisions and reaches beams.
	Transporter transporter.Provider
	// Logger receives DBOS's and the daemon's logs. Nil means slog.Default().
	Logger *slog.Logger
	// CombadgeArgs are extra `combadge serve` flags passed with every
	// StartCombadge (transporter.CombadgeSpec.Args), e.g. holodeck tests'
	// --heartbeat-interval or --step-log.
	CombadgeArgs []string
	// Repos is the repo registry, name to URL: the repos orders may name
	// (#51). A URL is anything `git fetch` takes. The bridge keeps its own
	// clone of each in <Home>/repos/<name> (repos.go).
	Repos map[string]string
	// MaxBundleBytes caps a work or checkpoint bundle's size (#65): pull-work
	// and pull-checkpoint check every bundle against it before copying.
	// 0 means DefaultMaxBundleBytes.
	MaxBundleBytes int64
	// Harnesses are the harnesses this bridge can run, each one of
	// bridge.Harnesses; AddCrew refuses the others. Nil means
	// DefaultHarnesses.
	Harnesses []string
	// Divisions are the roster's divisions (RosterConfig.Divisions).
	// AddCrew and CreateOrder refuse a division not among them, unless
	// there are none: then divisions aren't configured and any passes.
	// Their harness and model are their crew members' defaults (#59).
	Divisions map[string]DivisionConfig
	// StandingOrders is the roster's default for standing orders; see
	// Daemon.StandingOrders.
	StandingOrders bool
	// TransporterLimits are the roster's transporter limits. A zero field
	// takes its default (CheckRoster).
	TransporterLimits TransporterLimits
	// Regs is regs.yaml (regs.LoadRegs), and RegsConfigured whether it was
	// found. Run checks Regs (regs.CheckRegs) when it was. The hail
	// workflow routes by them (hail.go); without regs.yaml every hail goes
	// to the captain.
	Regs           regs.Config
	RegsConfigured bool
	// Tracker is the bridge's issue tracker (tracker.yaml, #54); nil
	// means none. Order sources naming a tracker item are read from it,
	// and tracker requests from beams run on it; without one both are
	// refused.
	Tracker tracker.Tracker
	// ConsultTimeout is how long a hail waits for Command's consult_reply
	// before it goes to the captain (hail.go). 0 means
	// DefaultConsultTimeout.
	ConsultTimeout time.Duration
	// HailParkAfter is how long an sdk beam holds a hail before it parks
	// the session (combadge serve --hail-park-after, #154, #180): every
	// beam's CombadgeSpec.HailParkAfter. 0 passes no flag, leaving serve's
	// default (15m); otherwise CheckHailParkAfter must pass.
	HailParkAfter time.Duration
	// CheckpointEvery and CheckpointTurns are the sdk harness's checkpoint
	// cadence (combadge serve --checkpoint-every and --checkpoint-turns,
	// #145): every beam's CombadgeSpec fields. nil passes no flag, leaving
	// serve's defaults (15m, 10 turns); 0 turns that trigger off. Otherwise
	// CheckCheckpointEvery and CheckCheckpointTurns must pass.
	CheckpointEvery *time.Duration
	CheckpointTurns *int
	// ConsolePort is the port the console listens on, on 127.0.0.1;
	// 0 picks a free one. NoConsole runs the bridge without the console.
	ConsolePort int
	NoConsole   bool

	// onReady, if set, is called once the daemon is up (DBOS launched and
	// crew tours re-attached), before Run blocks. Tests use it to reach the
	// running Daemon.
	onReady func(*Daemon)
	// tunePump, if set, adjusts every pump's config before it starts. Tests
	// use it to shorten timings.
	tunePump func(*comms.PumpConfig)
	// registerExtra, if set, registers more workflows before DBOS launches.
	// Tests use it for a stand-in bridge workflow that raises hails
	// (raiseBridgeHail, #152).
	registerExtra func(dbos.Context)
	// missionCheckEvery, if set, replaces defaultMissionCheckEvery.
	missionCheckEvery time.Duration
	// wakeSend, if set, replaces wakeDivision's send to a tour. Tests use
	// it to lose wake-ups.
	wakeSend func(tourID string, sig CrewSignal) error
	// pumpBackoff, if set, replaces defaultPumpBackoff.
	pumpBackoff time.Duration
	// provisionBackoff, if set, replaces defaultProvisionBackoff (and so
	// scales the cap and budget of beam creation's retries).
	provisionBackoff time.Duration
	// queuePollEvery, if set, replaces defaultQueuePollEvery.
	queuePollEvery time.Duration
	// watchChangeLead, if set, replaces defaultWatchChangeLead.
	watchChangeLead time.Duration
	// standDownTimeout and settleTimeout, if set, replace standDownTimeout
	// and missionSettleTimeout.
	standDownTimeout, settleTimeout time.Duration
	// helloTimeout, if set, replaces defaultHelloTimeout.
	helloTimeout time.Duration
	// belayTimeout, if set, replaces belayTimeout.
	belayTimeout time.Duration
	// getResultFault, if set, is called before each of awaitBeam's
	// GetResult calls; an error it returns stands in for GetResult's.
	// Tests use it to inject an unrecorded failure (#110).
	getResultFault func(d *Daemon, tourID string) error
	// dismissWait, if set, replaces defaultDismissWait.
	dismissWait time.Duration
	// dismissHook, if set, is called by DismissCrew just before its
	// transaction, and nextTourHook by the next-tour step between its
	// dismissed read and NextTour. Tests use them to land a write in
	// those windows (#113).
	dismissHook, nextTourHook func(d *Daemon, crewID string)
	// pullWorkHook, if set, is called by the pull-work step before it
	// pulls order orderID's work. Tests use it to hold a mission between
	// its report and order-done (#146).
	pullWorkHook func(d *Daemon, orderID string)
	// sendOrderHook, if set, is called by the send-order step before its
	// first attempt. Tests use it to end the tour between ship-repos and
	// send-order (#195).
	sendOrderHook func(d *Daemon, orderID string)
	// loginCheckEvery and loginWait, if set, replace
	// defaultLoginCheckEvery and defaultLoginWait.
	loginCheckEvery, loginWait time.Duration
	// copyRetries, if set, replaces the package's copyRetries for the
	// steps that wait for a tsh login (ship-repos, pull-checkpoint,
	// pull-work) and for fetch-work. Tests use it to shorten those steps'
	// retries.
	copyRetries []dbos.StepOption
	// trackerRetries, if set, replaces the package's trackerRetries for
	// list-backlog and file-finding (#155), and a campaign's order steps
	// (#184).
	trackerRetries []dbos.StepOption
	// taskRecheck, if set, replaces defaultTaskRecheck (#184).
	taskRecheck time.Duration
	// landBackoff, if set, replaces defaultLandBackoff (#185).
	landBackoff time.Duration
}

// MaxHailParkAfter is the longest Config.HailParkAfter: combadge serve's
// cap on --hail-park-after, setTimeout's limit of 2^31-1 ms (about 596h).
const MaxHailParkAfter = (1<<31 - 1) * time.Millisecond

// CheckHailParkAfter fails unless d is a --hail-park-after combadge serve
// takes: at least 1ms (Go formats shorter durations in µs or ns, which
// serve doesn't parse) and at most MaxHailParkAfter.
func CheckHailParkAfter(d time.Duration) error {
	switch {
	case d <= 0:
		return fmt.Errorf("%s is not positive", d)
	case d < time.Millisecond:
		return fmt.Errorf("%s is shorter than 1ms", d)
	case d > MaxHailParkAfter:
		return fmt.Errorf("%s is over the cap of %s (2^31-1 ms, combadge serve's limit)", d, MaxHailParkAfter)
	}
	return nil
}

// MinCheckpointEvery is the shortest Config.CheckpointEvery but 0 (#145),
// as combadge serve has it: a checkpoint costs a snapshot, bundles and a
// pull.
const MinCheckpointEvery = time.Minute

// CheckCheckpointEvery fails unless d is a --checkpoint-every combadge
// serve takes (#145): 0 (off), or from MinCheckpointEvery to
// MaxHailParkAfter.
func CheckCheckpointEvery(d time.Duration) error {
	switch {
	case d == 0:
		return nil
	case d < 0:
		return fmt.Errorf("%s is negative", d)
	case d < MinCheckpointEvery:
		return fmt.Errorf("%s is shorter than %s (0 turns it off)", d, MinCheckpointEvery)
	}
	return CheckHailParkAfter(d)
}

// CheckCheckpointTurns fails unless n is a --checkpoint-turns combadge
// serve takes (#145): 0 (off) or more.
func CheckCheckpointTurns(n int) error {
	if n < 0 {
		return fmt.Errorf("%d is negative", n)
	}
	return nil
}

// Daemon is a running bridge. Workflow code reaches it with daemonFrom.
type Daemon struct {
	home           string // absolute Config.Home; the cargo bay is <home>/cargo
	combadgeArgs   []string
	store          *store.Store
	dbos           dbos.Context
	transporter    transporter.Provider
	logger         *slog.Logger
	repos          map[string]string         // Config.Repos
	harnesses      []string                  // Config.Harnesses, or DefaultHarnesses
	maxBundleBytes int64                     // cmp.Or(Config.MaxBundleBytes, DefaultMaxBundleBytes)
	divisions      map[string]DivisionConfig // Config.Divisions
	limits         TransporterLimits         // Config.TransporterLimits, defaults filled in; for the transporter queue (#97)
	consoleAddr    string                    // the console's resolved address, "" with Config.NoConsole
	regs           regs.Config               // Config.Regs, checked
	regsConfigured bool                      // Config.RegsConfigured
	consultTimeout time.Duration             // cmp.Or(Config.ConsultTimeout, DefaultConsultTimeout)
	tracker        tracker.Tracker           // Config.Tracker; nil: none
	hailParkAfter  time.Duration             // Config.HailParkAfter
	// checkpointEvery and checkpointTurns are Config's (#145).
	checkpointEvery *time.Duration
	checkpointTurns *int

	// standingOrdersDefault is Config.StandingOrders. settingsMu serialises
	// StandingOrders' seeding with SetStandingOrders, so a seed can't
	// overwrite the captain's toggle.
	standingOrdersDefault bool
	settingsMu            sync.Mutex

	// wakeSend sends wakeDivision's signal to a tour: Config.wakeSend, or
	// dbos.Send on the daemon's context.
	wakeSend func(tourID string, sig CrewSignal) error

	// tourEnds holds a channel per tour id, closed once the tour has ended
	// (markTourEnded, tourEnded).
	tourEndsMu sync.Mutex
	tourEnds   map[string]chan struct{}

	// repoLocks serialise git work on each repo's clone (lockRepo): a
	// one-slot channel per repo name.
	repoLocksMu sync.Mutex
	repoLocks   map[string]chan struct{}

	// The comms pump registry (pumps.go): one pump per live crew member,
	// started by EnsurePump and looked up by workflow steps through
	// SendCommand. Pumps run on pumpCtx, which closePumps cancels.
	pumpsMu     sync.Mutex
	pumps       map[string]pumpEntry
	pumpsClosed bool
	pumpCtx     context.Context
	cancelPumps context.CancelFunc
	tunePump    func(*comms.PumpConfig)

	// missionCheckEvery: how long a tour waits for a signal before it
	// checks its mission is alive or, idle, claims again.
	missionCheckEvery time.Duration
	// pumpBackoff: the first wait between the start-pump step's attempts.
	pumpBackoff time.Duration
	// provisionBackoff: the first wait between attempts to create a beam
	// (transporterWorkflow).
	provisionBackoff time.Duration
	// transporterQueue paces transporter workflows (d.limits); a tour
	// enqueues its own and polls for its result every queuePollEvery.
	transporterQueue dbos.Queue
	queuePollEvery   time.Duration
	// watchChangeLead: how long before its beam's ExpiresAt a tour changes
	// the watch.
	watchChangeLead time.Duration
	// standDownTimeout and settleTimeout bound a watch change's wait for
	// its mission to park, and an ending tour's wait for it to settle.
	standDownTimeout, settleTimeout time.Duration
	// helloTimeout bounds a new tour's wait for its beam's hello (#173).
	helloTimeout time.Duration
	// startedAt is when this daemon was made. awaitHello grants a tour
	// resumed by a daemon younger than helloTimeout a fresh wait.
	startedAt time.Time
	// belayTimeout bounds a belayed mission's wait for its run to end on
	// the beam.
	belayTimeout time.Duration
	// getResultFault is Config.getResultFault.
	getResultFault func(d *Daemon, tourID string) error
	// dismissWait bounds DismissCrew's wait for a signalled tour's result.
	dismissWait time.Duration
	// dismissHook and nextTourHook are Config's.
	dismissHook, nextTourHook func(d *Daemon, crewID string)
	// pullWorkHook and sendOrderHook are Config's.
	pullWorkHook, sendOrderHook func(d *Daemon, orderID string)

	// campaignMu serializes campaign.run (#186): it reads the next
	// campaign id, checks the branch that id names, and inserts under it.
	campaignMu sync.Mutex

	// login is the transporter's login as watchLogin last read it (#102),
	// and loginExpiredAt the last time it was seen expired.
	loginMu        sync.Mutex
	login          loginState
	loginExpiredAt time.Time
	// beamLoginWaits holds the crew members whose transporter workflow
	// last found the login expired and waits for one (#106), with the tour
	// it provisions for: a display hint for the status, kept by
	// transporterWorkflow outside its steps. Under loginMu.
	beamLoginWaits map[string]int
	// loginCheckEvery: how often watchLogin reads the login; loginWait:
	// how long provisioning waits for a login before it tries again.
	loginCheckEvery, loginWait time.Duration
	// copyRetries are the retries of ship-repos, pull-checkpoint,
	// pull-work and fetch-work: Config.copyRetries, or the package's
	// copyRetries.
	copyRetries []dbos.StepOption
	// trackerRetries are the retries of list-backlog and file-finding:
	// Config.trackerRetries, or the package's trackerRetries.
	trackerRetries []dbos.StepOption
	// taskRecheck is how often a campaign task rereads what it waits on
	// without a message (#184).
	taskRecheck time.Duration
	// landBackoff is land-push's first wait before a retry, doubled on
	// each (#185).
	landBackoff time.Duration
}

// daemonKey is the context key under which the Daemon travels to workflows.
type daemonKey struct{}

// daemonFrom returns the Daemon running the workflow or step whose context is
// ctx.
//
// DBOS workflows are package-level functions registered once per DBOS
// context, so they can't close over the Daemon. Instead Run puts the Daemon
// in the context.Context it hands to dbos.NewContext: DBOS derives every
// workflow context (fresh runs, child workflows and workflows resumed by
// recovery alike) and every step context from that one, so the value is
// visible everywhere workflow code runs. Unlike a package-level variable this
// keeps two daemons in one process (the tests) apart. The Daemon is never part
// of a workflow's recorded input, so nothing about it is serialized.
func daemonFrom(ctx context.Context) *Daemon {
	d, ok := ctx.Value(daemonKey{}).(*Daemon)
	if !ok {
		panic("bridge: no Daemon in context; workflow not run by bridge.Run")
	}
	return d
}

// Run runs the bridge daemon until ctx is done, then shuts it down and returns
// nil (or the shutdown error). It takes <Home>/bridge.lock, failing with an
// error wrapping store.ErrLocked if another daemon holds it; opens the domain
// store and DBOS on <Home>/bridge.db; launches DBOS, which resumes every
// PENDING workflow on its own; starts a comms pump for each crew member with
// a live beam (ensurePumps); and makes sure each crew member that isn't
// dismissed has its tour workflow running. Then it serves the RPC socket,
// <Home>/bridge.sock (rpc.go), and the console (bound before the launch,
// unless Config.NoConsole) until ctx is done.
//
// Run doesn't handle signals. The CLI (#17) wraps it:
//
//	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
//	defer stop()
//	err := bridge.Run(ctx, cfg)
func Run(ctx context.Context, cfg Config) (err error) {
	if cfg.Home == "" {
		return errors.New("bridge: Config.Home is required")
	}
	if cfg.Transporter == nil {
		return errors.New("bridge: Config.Transporter is required")
	}
	if err := CheckRepos(cfg.Repos); err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	roster := RosterConfig{Divisions: cfg.Divisions, Transporter: cfg.TransporterLimits}
	if err := CheckRoster(&roster); err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	if cfg.RegsConfigured {
		if err := regs.CheckRegs(&cfg.Regs); err != nil {
			return fmt.Errorf("bridge: regs: %w", err)
		}
	}
	if cfg.ConsultTimeout < 0 {
		return fmt.Errorf("bridge: consult timeout %s is negative", cfg.ConsultTimeout)
	}
	if cfg.HailParkAfter != 0 {
		if err := CheckHailParkAfter(cfg.HailParkAfter); err != nil {
			return fmt.Errorf("bridge: hail park after: %w", err)
		}
	}
	if cfg.CheckpointEvery != nil {
		if err := CheckCheckpointEvery(*cfg.CheckpointEvery); err != nil {
			return fmt.Errorf("bridge: checkpoint every: %w", err)
		}
	}
	if cfg.CheckpointTurns != nil {
		if err := CheckCheckpointTurns(*cfg.CheckpointTurns); err != nil {
			return fmt.Errorf("bridge: checkpoint turns: %w", err)
		}
	}
	if !cfg.NoConsole {
		if err := console.CheckPort(cfg.ConsolePort); err != nil {
			return fmt.Errorf("bridge: %w", err)
		}
	}
	home, err := filepath.Abs(cfg.Home)
	if err != nil {
		return err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if err := CheckSocketPath(filepath.Join(home, SocketFile)); err != nil {
		return err
	}
	if err := securePrivateDir(home); err != nil {
		return err
	}

	lock, err := store.AcquireLock(filepath.Join(home, store.LockFile))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()

	// A token left by a run that didn't close its console is stale.
	if err := console.RemoveTokenFile(home); err != nil {
		return err
	}

	// The console binds now, so a taken port fails the start before any
	// workflow resumes, and serves once the tours re-attach.
	cons, err := listenConsole(cfg, home, logger)
	if err != nil {
		return err
	}
	if cons != nil {
		defer func() {
			if cerr := cons.Close(); cerr != nil {
				err = errors.Join(err, fmt.Errorf("close console server: %w", cerr))
			}
		}()
	}

	dbPath := filepath.Join(home, store.DBFile)
	dbURI, err := store.FileURI(dbPath)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { err = errors.Join(err, st.Close()) }()

	// The console's change feed (#72): every commit's log entries, from
	// here on.
	var feed *store.Feed
	if cons != nil {
		feed = store.NewFeed(0)
		// A failed read leaves the entries to the next commit; open
		// streams also catch up from the log at their heartbeat.
		onErr := func(err error) { logger.Warn("console feed", "err", err) }
		if err := st.SetLogHook(ctx, feed.Publish, onErr); err != nil {
			return fmt.Errorf("open store: %w", err)
		}
	}

	d := &Daemon{home: home, startedAt: time.Now(), combadgeArgs: append([]string(nil), cfg.CombadgeArgs...),
		store: st, transporter: cfg.Transporter, logger: logger,
		repos: maps.Clone(cfg.Repos), repoLocks: map[string]chan struct{}{},
		pumps: map[string]pumpEntry{}, tourEnds: map[string]chan struct{}{}, tunePump: cfg.tunePump,
		missionCheckEvery: cmp.Or(cfg.missionCheckEvery, defaultMissionCheckEvery),
		pumpBackoff:       cmp.Or(cfg.pumpBackoff, defaultPumpBackoff),
		provisionBackoff:  cmp.Or(cfg.provisionBackoff, defaultProvisionBackoff),
		queuePollEvery:    cmp.Or(cfg.queuePollEvery, defaultQueuePollEvery),
		watchChangeLead:   cmp.Or(cfg.watchChangeLead, defaultWatchChangeLead),
		standDownTimeout:  cmp.Or(cfg.standDownTimeout, standDownTimeout),
		settleTimeout:     cmp.Or(cfg.settleTimeout, missionSettleTimeout),
		helloTimeout:      cmp.Or(cfg.helloTimeout, defaultHelloTimeout),
		belayTimeout:      cmp.Or(cfg.belayTimeout, belayTimeout),
		getResultFault:    cfg.getResultFault,
		dismissWait:       cmp.Or(cfg.dismissWait, defaultDismissWait),
		dismissHook:       cfg.dismissHook,
		nextTourHook:      cfg.nextTourHook,
		pullWorkHook:      cfg.pullWorkHook,
		sendOrderHook:     cfg.sendOrderHook,
		loginCheckEvery:   cmp.Or(cfg.loginCheckEvery, defaultLoginCheckEvery),
		loginWait:         cmp.Or(cfg.loginWait, defaultLoginWait),
		copyRetries:       cfg.copyRetries,
		trackerRetries:    cfg.trackerRetries,
		taskRecheck:       cmp.Or(cfg.taskRecheck, defaultTaskRecheck),
		landBackoff:       cmp.Or(cfg.landBackoff, defaultLandBackoff),
		maxBundleBytes:    cmp.Or(cfg.MaxBundleBytes, DefaultMaxBundleBytes),
		divisions:         maps.Clone(cfg.Divisions), limits: roster.Transporter,
		standingOrdersDefault: cfg.StandingOrders, wakeSend: cfg.wakeSend,
		regs: cfg.Regs, regsConfigured: cfg.RegsConfigured,
		consultTimeout: cmp.Or(cfg.ConsultTimeout, DefaultConsultTimeout), hailParkAfter: cfg.HailParkAfter,
		checkpointEvery: cfg.CheckpointEvery, checkpointTurns: cfg.CheckpointTurns,
		tracker: cfg.Tracker}
	// Pumps outlive the steps that start them and stop with the daemon,
	// through closePumps (deferred below), not through ctx: they must stop
	// before DBOS and the store, which they use.
	if d.copyRetries == nil {
		d.copyRetries = copyRetries
	}
	if d.trackerRetries == nil {
		d.trackerRetries = trackerRetries
	}
	d.harnesses = append([]string(nil), cfg.Harnesses...)
	if cfg.Harnesses == nil {
		d.harnesses = DefaultHarnesses
	}
	if d.wakeSend == nil {
		d.wakeSend = func(tourID string, sig CrewSignal) error {
			return dbos.Send(d.dbos, tourID, sig, crewEventsTopic)
		}
	}
	d.pumpCtx, d.cancelPumps = context.WithCancel(context.WithoutCancel(ctx))
	defer d.cancelPumps()

	loginCtx, stopLogin := context.WithCancel(ctx)
	loginDone := make(chan struct{})
	go func() { defer close(loginDone); d.watchLogin(loginCtx) }()
	defer func() { stopLogin(); <-loginDone }()

	// The DBOS root context must not be cancelled by ctx. When it is
	// cancelled for any reason other than Shutdown, DBOS durably marks every
	// running workflow CANCELLED, and recovery never resumes those. Shutdown
	// cancels it with its own cause instead, which leaves them PENDING.
	dbosParent := context.WithValue(context.WithoutCancel(ctx), daemonKey{}, d)
	d.dbos, err = dbos.NewContext(dbosParent, dbos.Config{
		AppName: appName,
		// DBOS hands everything after "sqlite:" to the SQLite driver. A
		// percent-encoded file: URI keeps "?", "#" and "%" in Home from
		// being misread; "sqlite:" + a plain absolute path works only
		// without them. ("sqlite://..." is rejected as having a host.)
		DatabaseURL:        "sqlite:" + dbURI,
		ApplicationVersion: workflowVersion,
		Logger:             logger,
	})
	if err != nil {
		return fmt.Errorf("create DBOS context: %w", err)
	}
	// Shutdown is a no-op on a context that failed to launch (Launch already
	// tore it down), so it is safe to defer unconditionally.
	defer func() {
		if serr := dbos.Shutdown(d.dbos, shutdownTimeout); serr != nil {
			err = errors.Join(err, fmt.Errorf("shut down DBOS: %w", serr))
		}
	}()

	if err := d.register(); err != nil {
		return err
	}
	// Every order of a campaign task that ends wakes its task workflow
	// (#184), from here on.
	st.SetTaskHook(d.notifyTask)
	defer st.SetTaskHook(nil)
	if cfg.registerExtra != nil {
		cfg.registerExtra(d.dbos)
	}

	// Launch resumes every PENDING workflow of this app and version; there is
	// no separate recovery step.
	if err := dbos.Launch(d.dbos); err != nil {
		return fmt.Errorf("launch DBOS: %w", err)
	}
	// Deferred after the DBOS shutdown, so it runs before it.
	defer d.closePumps()

	// Recreate the pumps of crew members with a live beam before the
	// re-attach below: resumed workflows send commands through them.
	if err := d.ensurePumps(ctx); err != nil {
		return err
	}
	if err := d.attachCrewTours(ctx); err != nil {
		return err
	}
	// A running campaign inserted but never started starts now (#184).
	if err := d.attachCampaigns(ctx); err != nil {
		return err
	}

	// The console serves before the RPC socket, so bridge.status reports
	// its address only once the token file is current. consoleAddr is set
	// before Serve: console handlers read it, through Status.
	if cons != nil {
		d.consoleAddr = cons.Addr()
		// Actions get their own context, so the startRPC failure below
		// can end them too.
		actCtx, stopActs := context.WithCancel(ctx)
		actions := &actionGate{}
		if err := cons.Serve(consoleSource{d: d, feed: feed, ctx: actCtx, actions: actions}); err != nil {
			stopActs()
			return err
		}
		// Deferred before the console's and the RPC socket's closes, so it
		// runs after them: Close doesn't wait for a handler past its 5s,
		// and an action still running mustn't outlive the pumps or DBOS.
		defer func() {
			stopActs()
			actions.close()
		}()
	}

	// The RPC server starts last and, deferred last, stops first: no
	// request reaches a daemon that is shutting down its pumps or DBOS.
	sock := filepath.Join(home, SocketFile)
	rpc, err := d.startRPC(ctx, sock)
	if err != nil {
		if cons != nil {
			err = errors.Join(err, cons.Close())
		}
		return err
	}
	defer func() {
		if cerr := rpc.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close RPC socket: %w", cerr))
		}
	}()

	// Deferred last, the console stops first; Close is idempotent, so the
	// close deferred at bind time then does nothing.
	if cons != nil {
		defer func() {
			if cerr := cons.Close(); cerr != nil {
				err = errors.Join(err, fmt.Errorf("close console server: %w", cerr))
			}
		}()
	}

	logger.Info("bridge up", "home", home, "socket", sock, "console", cmp.Or(d.consoleAddr, "off"))
	if cfg.onReady != nil {
		cfg.onReady(d)
	}
	<-ctx.Done()
	logger.Info("bridge shutting down")
	return nil
}

// listenConsole opens the console's listener, or returns nil with
// cfg.NoConsole.
func listenConsole(cfg Config, home string, logger *slog.Logger) (*console.Server, error) {
	if cfg.NoConsole {
		return nil, nil
	}
	addr := net.JoinHostPort(console.Host, strconv.Itoa(cfg.ConsolePort))
	cons, err := console.Listen(console.Config{Addr: addr, Home: home, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("bridge: %w (pass --no-console, or another --console-port)", err)
	}
	return cons, nil
}

// securePrivateDir makes the bridge home, which holds the database and the
// RPC socket, a directory only we can reach: it creates it 0700, refuses one
// owned by another user or a shared (sticky) directory such as /tmp, and
// refuses an existing one others can reach rather than chmod it: with
// --home ~ that would change the user's home directory.
func securePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("bridge home %s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("bridge home %s is owned by uid %d, not %d; use a home of your own", dir, st.Uid, os.Geteuid())
	}
	if fi.Mode()&os.ModeSticky != 0 {
		return fmt.Errorf("bridge home %s is a shared directory (sticky bit); use a dedicated one", dir)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("bridge home %s has mode %#o, accessible to other users; run `chmod 700 %s` or choose another --home",
			dir, perm, dir)
	}
	return nil
}

// register registers every workflow and queue with DBOS. It must run
// before Launch, so recovery can find the functions of resumed workflows
// and the queue runner the queues.
func (d *Daemon) register() error {
	ctx := d.dbos
	dbos.RegisterWorkflow(ctx, crewTourWorkflow,
		dbos.WithWorkflowName(crewTourWorkflowName),
		// A tour lives until dismissal or sickbay, and every bridge
		// restart counts as a recovery attempt, so the default limit of
		// 100 would eventually dead-letter a healthy tour. <= 0 disables it.
		dbos.WithMaxRecoveryAttempts(-1))
	dbos.RegisterWorkflow(ctx, missionWorkflow,
		dbos.WithWorkflowName(missionWorkflowName),
		// A parked mission may live through many restarts, as a tour does.
		dbos.WithMaxRecoveryAttempts(-1))
	dbos.RegisterWorkflow(ctx, transporterWorkflow,
		dbos.WithWorkflowName(transporterWorkflowName))
	dbos.RegisterWorkflow(ctx, hailWorkflow,
		dbos.WithWorkflowName(hailWorkflowName),
		// A hail may wait on the captain through many restarts.
		dbos.WithMaxRecoveryAttempts(-1))
	dbos.RegisterWorkflow(ctx, findingsWorkflow,
		dbos.WithWorkflowName(findingsWorkflowName),
		// Triage may wait on Command through many restarts.
		dbos.WithMaxRecoveryAttempts(-1))
	dbos.RegisterWorkflow(ctx, campaignWorkflow,
		dbos.WithWorkflowName(campaignWorkflowName),
		// A campaign waits on its task through many restarts (#184).
		dbos.WithMaxRecoveryAttempts(-1))
	dbos.RegisterWorkflow(ctx, campaignTaskWorkflow,
		dbos.WithWorkflowName(campaignTaskWorkflowName),
		// A task waits on orders and hails through many restarts.
		dbos.WithMaxRecoveryAttempts(-1))

	// The queue's limits are persisted in the system database. The roster
	// is their source, so its values always replace the stored ones.
	q, err := dbos.RegisterQueue(ctx, transporterQueueName,
		dbos.WithGlobalConcurrency(d.limits.MaxProvisioning),
		dbos.WithRateLimiter(&dbos.RateLimiter{Limit: d.limits.RatePerMinute, Period: time.Minute}),
		dbos.WithQueueBasePollingInterval(d.queuePollEvery),
		dbos.WithQueueOnConflict(dbos.QueueConflictAlwaysUpdate))
	if err != nil {
		return fmt.Errorf("register queue %s: %w", transporterQueueName, err)
	}
	d.transporterQueue = q
	return nil
}

// tourWorkflowID is the workflow id of a crew member's tour (spec §5).
func tourWorkflowID(crewID string, tourNo int) string {
	return "crew/" + crewID + "/tour/" + strconv.Itoa(tourNo)
}

// attachCrewTours starts the tour workflow of every crew member that isn't
// dismissed. The workflow id is the idempotency key: for a tour that already
// exists (PENDING and resumed by Launch, or already finished) RunWorkflow
// starts nothing and returns a handle to it, so this only starts tours that
// were never started.
//
// The tour number is crew.tour_no as stored. It defaults to 0, so a crew
// member's first tour is tour 0; whatever starts the next tour (watch
// change, re-energize) increments tour_no first (store.NextTour), so this
// also starts a next tour whose start a crash cut off.
func (d *Daemon) attachCrewTours(ctx context.Context) error {
	crew, err := d.store.ListCrew(ctx)
	if err != nil {
		return fmt.Errorf("list crew: %w", err)
	}
	for _, c := range crew {
		if c.Status == store.CrewDismissed {
			continue
		}
		id := tourWorkflowID(c.ID, c.TourNo)
		h, err := dbos.RunWorkflow(d.dbos, crewTourWorkflow,
			CrewTourInput{CrewID: c.ID, TourNo: c.TourNo},
			dbos.WithWorkflowID(id))
		if err != nil {
			return fmt.Errorf("attach tour %s: %w", id, err)
		}
		st, err := h.GetStatus()
		if err != nil {
			return fmt.Errorf("status of tour %s: %w", id, err)
		}
		switch st.Status {
		case dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued:
			d.logger.Info("crew tour attached", "workflow_id", id, "status", st.Status)
		case dbos.WorkflowStatusSuccess:
			d.logger.Debug("crew tour already ended", "workflow_id", id)
		default:
			// ERROR, CANCELLED, MAX_RECOVERY_ATTEMPTS_EXCEEDED: the crew
			// member isn't dismissed but has no live tour.
			d.logger.Warn("crew tour is not running", "workflow_id", id, "status", st.Status, "crew_status", c.Status)
		}
	}
	return nil
}
