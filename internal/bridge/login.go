package bridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// The transporter's login (#102, spec/phase-2-plan-amendment-2026-10-19.md
// §1): every tsh call runs on the captain's tsh login, and a day-long run
// outlives it. The daemon reads the login's expiry every
// defaultLoginCheckEvery. Status turns yellow in its last loginWarnBefore,
// once it has expired, and when it can't be read. Tsh itself refuses calls
// while it is expired (transporter.ErrLoginExpired). Meanwhile tours claim
// no orders, and provisioning and the missions' beam steps wait for a new
// login (transporterWorkflow, loginWaiter).
const (
	defaultLoginCheckEvery = time.Minute
	// defaultLoginWait is how long a workflow waits for a login before
	// trying again. Config.loginWait overrides it in tests.
	defaultLoginWait = time.Minute
	loginWarnBefore  = 30 * time.Minute
	// loginUnknownAfter is how many failed reads in a row make the status
	// say the login is unknown.
	loginUnknownAfter = 2
	// maxLoginErr caps the error in "tsh login unknown: <err>".
	maxLoginErr = 120
)

// loginState is the login as last read.
type loginState struct {
	known  bool      // read at least once
	until  time.Time // when it expires; zero: never
	absent bool      // not logged in
	// failures counts reads in a row that failed for another reason than
	// a missing login, and lastErr is the latest one's error. The state
	// above is kept through them.
	failures int
	lastErr  string
}

// expired reports whether the login is absent or, at now, within
// transporter.LoginExpiryMargin of its expiry or past it.
func (l loginState) expired(now time.Time) bool {
	return l.known && (l.absent || (!l.until.IsZero() && !now.Before(l.until.Add(-transporter.LoginExpiryMargin))))
}

// reason is the status reason the login gives at now, or "". The minutes
// left count to the login's own expiry, though it counts as expired
// LoginExpiryMargin earlier.
func (l loginState) reason(now time.Time) string {
	switch {
	case l.expired(now):
		return "tsh login expired"
	case l.failures >= loginUnknownAfter:
		return "tsh login unknown: " + l.lastErr
	case l.known && !l.until.IsZero() && l.until.Sub(now) <= loginWarnBefore:
		return fmt.Sprintf("tsh login expires in %dm", int(math.Ceil(l.until.Sub(now).Minutes())))
	}
	return ""
}

// watchLogin reads the login at once and then every loginCheckEvery until
// ctx ends.
func (d *Daemon) watchLogin(ctx context.Context) {
	t := time.NewTicker(d.loginCheckEvery)
	defer t.Stop()
	for {
		d.checkLogin(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkLogin reads the transporter's login and records it. A read that
// fails for another reason than a missing login keeps the last state and
// counts the failure. When the login turns valid again, it wakes every
// crew member on deck: their tours claimed nothing while it was expired.
func (d *Daemon) checkLogin(ctx context.Context) {
	until, err := d.transporter.LoginValidUntil(ctx)
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	d.loginMu.Lock()
	was := d.login.expired(now)
	switch {
	case errors.Is(err, transporter.ErrLoginExpired):
		d.login = loginState{known: true, absent: true}
	case err != nil:
		d.login.failures++
		d.login.lastErr = shortErr(err)
	default:
		d.login = loginState{known: true, until: until}
	}
	is := d.login.expired(now)
	if is {
		d.loginExpiredAt = now
	}
	d.loginMu.Unlock()
	switch {
	case err != nil && !errors.Is(err, transporter.ErrLoginExpired):
		d.logger.Warn("tsh login: can't read its expiry", "err", err)
	case is && !was:
		d.logger.Warn("tsh login expired: beam work waits until you run tsh login", "valid_until", until)
	case was && !is:
		d.logger.Info("tsh login valid again: beam work resumes", "valid_until", until)
		d.wakeOnDeck(ctx, "after the tsh login")
	}
}

// shortErr is err's first line, cut to maxLoginErr bytes, for a status
// reason.
func shortErr(err error) string {
	s := firstLine(err.Error())
	if len(s) > maxLoginErr {
		s = s[:maxLoginErr] + "..."
	}
	return s
}

// loginExpired reports whether the login, as last read, is expired now.
func (d *Daemon) loginExpired() bool {
	return d.loginExpiredSince(time.Now())
}

// loginExpiredSince reports whether the login, as last read, is expired
// now or was seen expired at or after since.
func (d *Daemon) loginExpiredSince(since time.Time) bool {
	d.loginMu.Lock()
	defer d.loginMu.Unlock()
	now := time.Now()
	if d.login.expired(now) {
		d.loginExpiredAt = now
		return true
	}
	return !d.loginExpiredAt.IsZero() && !d.loginExpiredAt.Before(since)
}

// loginReason is the login's status reason now (loginState.reason).
func (d *Daemon) loginReason() string {
	d.loginMu.Lock()
	defer d.loginMu.Unlock()
	return d.login.reason(time.Now())
}

// setBeamLoginWait records whether crewID's transporter workflow for tour
// tourNo waits for a tsh login (#106). Clearing it leaves a later tour's
// wait alone. Only the status reads it, so a replay setting it for a
// moment, before its next attempt runs, is harmless.
func (d *Daemon) setBeamLoginWait(crewID string, tourNo int, waiting bool) {
	d.loginMu.Lock()
	defer d.loginMu.Unlock()
	switch {
	case waiting:
		if d.beamLoginWaits == nil {
			d.beamLoginWaits = map[string]int{}
		}
		d.beamLoginWaits[crewID] = tourNo
	case d.beamLoginWaits[crewID] == tourNo:
		delete(d.beamLoginWaits, crewID)
	}
}

// beamLoginWaiting returns the crew members whose provisioning waits for a
// tsh login (setBeamLoginWait).
func (d *Daemon) beamLoginWaiting() map[string]bool {
	d.loginMu.Lock()
	defer d.loginMu.Unlock()
	out := make(map[string]bool, len(d.beamLoginWaits))
	for id := range d.beamLoginWaits {
		out[id] = true
	}
	return out
}

// wakeOnDeck signals the tour of every crew member on deck to claim, best
// effort, as wakeDivision does for one division. why ends its warnings:
// "after the tsh login".
func (d *Daemon) wakeOnDeck(ctx context.Context, why string) {
	crew, err := d.store.ListCrew(ctx)
	if err != nil {
		d.logger.Warn("crew not woken "+why+": list crew", "err", err)
		return
	}
	for _, c := range crew {
		if c.Status != store.CrewOnDeck {
			continue
		}
		tour := tourWorkflowID(c.ID, c.TourNo)
		if err := d.wakeSend(tour, CrewSignal{Kind: SignalOrderAssigned}); err != nil {
			d.logger.Warn("crew tour not woken "+why, "tour", tour, "err", err)
		}
	}
}

// loginWaiter makes a mission's beam step (ship-repos, pull-checkpoint,
// pull-work) wait for a tsh login instead of failing (#102). waiting is
// whether its last login-expired step found the login expired, so the
// wait is logged once; since is when the current attempt started (mark).
type loginWaiter struct {
	waiting bool
	since   time.Time
}

// mark records that an attempt of the step starts now. It only feeds the
// login-expired step, whose result is recorded, so a replay that marks a
// different time takes the same path.
func (w *loginWaiter) mark() { w.since = time.Now() }

// loginCheck is a login-expired step's recorded result.
type loginCheck struct {
	// Expired: the login is expired, or was seen expired since the
	// attempt started.
	Expired bool `json:",omitempty"`
	// Retry: run the step again after loginWait.
	Retry bool `json:",omitempty"`
}

// retry runs after step failed (and ctx is live). Its login-expired step
// reads the login afresh (checkLogin), so a login lost before its
// valid_until (tsh logout) isn't missed for a minute.
//
//   - If the login is expired, or was seen expired since the attempt
//     started (a login renewed while the step was failing on the old
//     one), it logs "<step> waits for a tsh login" (kind order) unless
//     this wait already did.
//   - If it is valid but the previous round was waiting, the attempt may
//     have failed on the old login with nobody looking: the step gets one
//     more round, and fails for good only if that one fails too.
//
// To run the step again it sleeps loginWait and returns true. Otherwise it
// returns false, and the step's failure stands. The workflow branches only
// on the recorded result, never on the step's error.
func (w *loginWaiter) retry(ctx dbos.Context, step, crewID, orderID string) (bool, error) {
	waiting, since := w.waiting, w.since
	check, err := dbos.RunAsStep(ctx, func(c context.Context) (loginCheck, error) {
		d := daemonFrom(c)
		d.checkLogin(c)
		if !d.loginExpiredSince(since) {
			return loginCheck{Retry: waiting}, nil
		}
		if !waiting {
			if err := d.appendLog(c, logOrder, crewID, orderID, step+" waits for a tsh login",
				map[string]any{"step": step}); err != nil {
				return loginCheck{}, err
			}
		}
		return loginCheck{Expired: true, Retry: true}, nil
	}, append([]dbos.StepOption{dbos.WithStepName("login-expired")}, storeRetries...)...)
	if err != nil {
		return false, err
	}
	w.waiting = check.Expired
	if !check.Retry {
		return false, nil
	}
	if _, err := dbos.Sleep(ctx, daemonFrom(ctx).loginWait); err != nil {
		return false, err
	}
	return true, nil
}
