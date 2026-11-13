package bridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// Beam creation's retries (spec/phase-2-plan-amendment-2026-10-19.md §3): a
// failed Create is tried again after defaultProvisionBackoff, the wait
// doubling up to provisionBackoffCap times it, until the waits would pass
// provisionBudget times it (1, 2, 4, 5, 5, 5, 5 minutes: eight attempts
// over 27 minutes of waiting). Config.provisionBackoff overrides the
// first wait in tests, scaling the rest.
const (
	defaultProvisionBackoff = time.Minute
	provisionBackoffCap     = 5
	provisionBudget         = 30
)

// defaultQueuePollEvery is how often the transporter queue is polled for
// work, and a tour for its transporter workflow's result: DBOS's own
// default. Config.queuePollEvery overrides it in tests.
const defaultQueuePollEvery = time.Second

// beamWorkflowID is the workflow id of the transporter workflow giving a
// crew member's tour its beam (spec §5).
func beamWorkflowID(crewID string, tourNo int) string {
	return "beam/" + crewID + "/" + strconv.Itoa(tourNo)
}

// TransporterInput is the input of a transporter workflow, id
// beam/<CrewID>/<TourNo>.
type TransporterInput struct {
	CrewID string
	TourNo int
}

// TransporterResult is a transporter workflow's outcome. At most one of
// Dismissed and Failed is set; with neither, Beam is up, serve runs in it,
// and the crew member is beaming_in with it until its hello (#106).
type TransporterResult struct {
	Beam transporter.Beam
	// Dismissed: the crew member was dismissed. Beam is the one made for
	// it before the dismissal landed, if any, for the tour to remove.
	Dismissed bool `json:",omitempty"`
	// Failed is why the crew member has no beam.
	Failed string `json:",omitempty"`
}

// provisionAttempt is one provision step's recorded outcome.
type provisionAttempt struct {
	TransporterResult
	// CreateFailed is why the transporter couldn't create a beam: the
	// workflow tries again after a backoff.
	CreateFailed string `json:",omitempty"`
	// LoginExpired: the transporter refused a call because the tsh login
	// has expired (#102). The workflow waits for a login and tries again,
	// outside the retry budget.
	LoginExpired bool `json:",omitempty"`
}

// transporterWorkflow gives a crew member's tour its beam. Tours run it on
// the transporter queue (d.transporterQueue), which bounds how many beams
// are being provisioned at once and how many are started a minute
// (TransporterLimits), and wait for its result.
//
// Steps:
//  1. remove-previous: remove the beam of an earlier tour (see
//     removePrevious).
//  2. provision (see provision), repeated while the transporter fails to
//     create a beam: a durable dbos.Sleep between attempts, with backoff,
//     until the budget is spent; then the workflow reports Failed. While
//     the tsh login is expired (#102) provision is repeated every
//     loginWait, for as long as that lasts: those waits don't count
//     against the budget, and don't advance the backoff.
//
// The retries are a loop of steps in the workflow rather than step retries
// or a sleep loop inside one step: each attempt and each wait is recorded,
// so a bridge restart resumes the wait where it was and doesn't start the
// budget again, and the workflow branches only on recorded outcomes.
func transporterWorkflow(ctx dbos.Context, in TransporterInput) (TransporterResult, error) {
	d := daemonFrom(ctx)
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
		return struct{}{}, removePrevious(c, daemonFrom(c), in)
	}, append([]dbos.StepOption{dbos.WithStepName("remove-previous")}, storeRetries...)...); err != nil {
		if ctx.Err() != nil {
			return TransporterResult{}, err
		}
		// Its own failures are logged and left to the beam's TTL; only the
		// store can fail it, and provision reads the same store.
		d.logger.Error("transporter: remove-previous", "crew", in.CrewID, "tour", in.TourNo, "err", err)
	}

	wait := d.provisionBackoff
	var waited time.Duration
	waitingLogin := false // the last attempt found the login expired
	// The status's hint (#106), not workflow state: never branched on.
	defer d.setBeamLoginWait(in.CrewID, in.TourNo, false)
	for {
		at, err := dbos.RunAsStep(ctx, func(c context.Context) (provisionAttempt, error) {
			return provision(c, daemonFrom(c), in, waitingLogin)
		}, append([]dbos.StepOption{dbos.WithStepName("provision")}, provisionRetries...)...)
		if err != nil {
			if ctx.Err() != nil {
				return TransporterResult{}, err
			}
			return TransporterResult{Failed: err.Error()}, nil
		}
		if at.LoginExpired {
			if !waitingLogin {
				d.logger.Warn("transporter: provisioning waits for a tsh login", "crew", in.CrewID, "tour", in.TourNo)
			}
			waitingLogin = true
			d.setBeamLoginWait(in.CrewID, in.TourNo, true)
			if _, err := dbos.Sleep(ctx, d.loginWait); err != nil {
				return TransporterResult{}, err
			}
			continue
		}
		waitingLogin = false
		d.setBeamLoginWait(in.CrewID, in.TourNo, false)
		if at.CreateFailed == "" {
			return at.TransporterResult, nil
		}
		if waited+wait > provisionBudget*d.provisionBackoff {
			return TransporterResult{Failed: "create beam: " + at.CreateFailed}, nil
		}
		d.logger.Warn("transporter: beam creation failed; retrying", "crew", in.CrewID, "tour", in.TourNo,
			"in", wait, "err", at.CreateFailed)
		if _, err := dbos.Sleep(ctx, wait); err != nil {
			return TransporterResult{}, err
		}
		waited += wait
		wait = min(2*wait, provisionBackoffCap*d.provisionBackoff)
	}
}

// removePrevious is the remove-previous step: if the crew member's latest
// beam was created by an earlier tour and still exists, it removes it, so a
// crew member holds at most one beam (spec §5). A failure to check or remove
// it is logged, here and in the captain's log, and the beam is left to its
// TTL. A beam removed is logged "beam <alias> removed".
func removePrevious(ctx context.Context, d *Daemon, in TransporterInput) error {
	beams, err := d.store.ListBeams(ctx, in.CrewID)
	if err != nil {
		return err
	}
	n := len(beams)
	if n == 0 || beams[n-1].TourNo >= in.TourNo {
		return nil
	}
	return d.removeBeam(ctx, in.CrewID, beams[n-1])
}

// removeBeam removes crewID's beam b if it still exists, logging "beam
// <alias> removed". A failure to check or remove it is logged, here and in
// the captain's log ("beam <alias> not removed, left to its TTL: <err>"),
// and the beam is left to its TTL; only ctx ending or the store fails it.
// A beam removed or found gone gets its row's removed_at
// (store.SetBeamRemoved); one left to its TTL keeps it NULL.
// remove-previous, a dismissed tour's remove-beam and DismissCrew use it.
func (d *Daemon) removeBeam(ctx context.Context, crewID string, b store.BeamRecord) error {
	beam := transporter.Beam{Alias: b.Alias, ExpiresAt: b.ExpiresAt}
	payload := map[string]any{"beam": beam.Alias, "tour_no": b.TourNo}
	exists, err := d.transporter.Exists(ctx, beam)
	if err == nil && !exists {
		return d.store.SetBeamRemoved(ctx, crewID, beam.Alias, time.Now())
	}
	if err == nil {
		err = d.transporter.Remove(ctx, beam)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		d.logger.Warn("transporter: beam not removed; leaving it to its TTL", "crew", crewID, "beam", beam.Alias, "err", err)
		payload["error"] = err.Error()
		return d.appendLog(ctx, logBeam, crewID, "", "beam "+beam.Alias+" not removed, left to its TTL: "+err.Error(), payload)
	}
	if err := d.store.SetBeamRemoved(ctx, crewID, beam.Alias, time.Now()); err != nil {
		return err
	}
	return d.appendLog(ctx, logBeam, crewID, "", "beam "+beam.Alias+" removed", payload)
}

// provision gives the crew member a beam running combadge serve and sets it
// beaming_in with that beam: the tour moves it on_deck at the beam's hello
// (#106). It is the transporter workflow's provision step
// and is safe to re-run (DBOS re-runs a step that crashed before its outcome
// was recorded, and retries one that failed):
//
//   - A dismissed crew member gets no beam: the outcome is Dismissed.
//   - A new beam is recorded only in the beams table, with the tour's
//     number, right after Create. crew.current_beam (with beaming_in) is set
//     only once serve is up, so a bridge restarting mid-provisioning (Run's
//     ensurePumps) never attaches a pump to a beam with no serve, whose
//     heartbeat_missed would send the new tour to sickbay.
//   - A re-run starts from the crew member's latest beams row, if this tour
//     created it: a new tour never picks up its predecessor's beam. If that
//     beam still exists (Provider.Exists: listed, whatever runs in it) it is
//     reused and StartCombadge, which is idempotent, is called on it again.
//     A StartCombadge failure fails the attempt, and the next retry tries
//     the same beam again. Only a beam that is gone is replaced. So
//     retries don't create a VM each, and a transient failure never
//     destroys a good beam; a beam serve can't start in exhausts the
//     retries and the crew member goes to sickbay.
//   - A failed Create isn't an error: it is the outcome's CreateFailed, and
//     the workflow tries again later (whatever tsh said: a tenant cap or a
//     rate limit looks like any other failure).
//   - A transporter call refused because the tsh login has expired
//     (transporter.ErrLoginExpired) isn't an error either: the outcome is
//     LoginExpired, and the workflow waits for a login. Unless the
//     previous attempt was waiting too (waitingLogin), it logs
//     "provisioning waits for a tsh login" (kind beam), so the captain
//     sees it once per wait. The workflow also marks the crew member as
//     waiting (setBeamLoginWait) for the status's reason, outside the step.
//   - The status, beams-table and StartCombadge calls are idempotent. The
//     beams row and the status each commit with their log entry, and a
//     re-run that changes neither logs nothing.
//   - The crew member gets the beam only if it isn't dismissed by then
//     (DismissCrew may land while the beam is made): the outcome is then
//     Dismissed with the beam, which the tour removes.
//   - A resolved harness this bridge can't run (checkHarness) is the
//     outcome's Failed, before any beam is made: retrying can't help, and
//     the tour goes to sickbay saying why.
//
// serve runs the crew member's resolved harness (resolveHarness: crew.harness,
// else the division's default; CombadgeSpec.Harness), fixed for the tour,
// with --role command for a crew member of the command division, and
// Config.HailParkAfter, which the transporter passes only if serve runs sdk
// (#180). A transporter's own harness flags and the daemon's combadge args
// come after these, so a --harness among them overrides the crew member's
// (see CombadgeSpec).
func provision(ctx context.Context, d *Daemon, in TransporterInput, waitingLogin bool) (provisionAttempt, error) {
	at, err := provisionBeam(ctx, d, in)
	if err == nil || !errors.Is(err, transporter.ErrLoginExpired) || ctx.Err() != nil {
		return at, err
	}
	if !waitingLogin {
		if lerr := d.appendLog(ctx, logBeam, in.CrewID, "", "provisioning waits for a tsh login",
			map[string]any{"tour_no": in.TourNo, "error": err.Error()}); lerr != nil {
			return provisionAttempt{}, lerr
		}
	}
	return provisionAttempt{LoginExpired: true}, nil
}

// provisionBeam is provision's work, returning a transporter error wrapping
// ErrLoginExpired as is.
func provisionBeam(ctx context.Context, d *Daemon, in TransporterInput) (provisionAttempt, error) {
	crewID := in.CrewID
	crew, err := d.store.GetCrew(ctx, crewID)
	if err != nil {
		return provisionAttempt{}, fmt.Errorf("read crew %s: %w", crewID, err)
	}
	if crew.Status == store.CrewDismissed {
		return provisionAttempt{TransporterResult: TransporterResult{Dismissed: true}}, nil
	}
	if err := d.checkHarness(crew); err != nil {
		return provisionAttempt{TransporterResult: TransporterResult{Failed: err.Error()}}, nil
	}
	harness, _ := d.resolveHarness(crew)
	beams, err := d.store.ListBeams(ctx, crewID)
	if err != nil {
		return provisionAttempt{}, err
	}
	var beam transporter.Beam
	if n := len(beams); n > 0 && beams[n-1].TourNo == in.TourNo {
		last := transporter.Beam{Alias: beams[n-1].Alias, ExpiresAt: beams[n-1].ExpiresAt}
		exists, err := d.transporter.Exists(ctx, last)
		if err != nil {
			return provisionAttempt{}, fmt.Errorf("check beam %s: %w", last.Alias, err)
		}
		if exists {
			beam = last
		} else {
			d.logger.Info("provision: recorded beam is gone; creating another", "crew", crewID, "beam", last.Alias)
		}
	}
	if beam.Alias == "" {
		if beam, err = d.transporter.Create(ctx); err != nil {
			if ctx.Err() != nil {
				return provisionAttempt{}, ctx.Err()
			}
			if errors.Is(err, transporter.ErrLoginExpired) {
				return provisionAttempt{}, err
			}
			if lerr := d.appendLog(ctx, logBeam, crewID, "", "beam creation failed: "+err.Error(),
				map[string]string{"error": err.Error()}); lerr != nil {
				return provisionAttempt{}, lerr
			}
			return provisionAttempt{CreateFailed: err.Error()}, nil
		}
		created, err := logEntry(logBeam, crewID, "", "beam "+beam.Alias+" created",
			map[string]any{"beam": beam.Alias, "expires_at": beam.ExpiresAt, "tour_no": in.TourNo})
		if err != nil {
			return provisionAttempt{}, err
		}
		if err := d.store.InsertBeam(ctx, store.BeamRecord{UUID: beam.Alias, Alias: beam.Alias, CrewID: crewID,
			TourNo: in.TourNo, ExpiresAt: beam.ExpiresAt}, created); err != nil {
			return provisionAttempt{}, err
		}
		// A beam whose watch change is due as it is made would have its
		// tours rotate straight away, one beam after another: say so once,
		// for the beam it happens to.
		if !beam.ExpiresAt.IsZero() && beam.ExpiresAt.Add(-d.watchChangeLead).Before(time.Now()) {
			text := fmt.Sprintf("beam %s expires at %s, within the watch-change lead %s: its tour changes the watch at once",
				beam.Alias, beam.ExpiresAt.UTC().Format(time.RFC3339), d.watchChangeLead)
			d.logger.Warn("provision: "+text, "crew", crewID)
			if err := d.appendLog(ctx, logBeam, crewID, "", text,
				map[string]any{"beam": beam.Alias, "expires_at": beam.ExpiresAt, "lead": d.watchChangeLead.String()}); err != nil {
				return provisionAttempt{}, err
			}
		}
	}
	spec := transporter.CombadgeSpec{CrewID: crewID, Harness: harness, HailParkAfter: d.hailParkAfter, Args: d.combadgeArgs}
	if crew.Division == DivisionCommand {
		// Command's sdk runs get split_order (#155).
		spec.Role = RoleCommand
	}
	if err := d.transporter.StartCombadge(ctx, beam, spec); err != nil {
		return provisionAttempt{}, fmt.Errorf("start combadge in %s: %w", beam.Alias, err)
	}
	up, err := logEntry(logBeam, crewID, "", "beam "+beam.Alias+" up, combadge started",
		map[string]any{"beam": beam.Alias, "expires_at": beam.ExpiresAt})
	if err != nil {
		return provisionAttempt{}, err
	}
	dismissed, err := d.store.SetCrewStatusUnlessDismissed(ctx, crewID, store.CrewBeamingIn, beam.Alias, up)
	if err != nil {
		return provisionAttempt{}, err
	}
	return provisionAttempt{TransporterResult: TransporterResult{Beam: beam, Dismissed: dismissed}}, nil
}
