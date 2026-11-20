package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/tscolari/bmus/internal/store"
)

// Conditions, in BridgeStatus.Condition.
const (
	ConditionGreen  = "green"
	ConditionYellow = "yellow"
	ConditionRed    = "red"
)

// infoRegsNotConfigured is BridgeStatus.Info's line for a bridge without
// regs.yaml.
const infoRegsNotConfigured = "regs.yaml not configured"

// BridgeStatus is the bridge's condition and counters (#70): bridge.status's
// result, and what the console's status event carries.
//
//   - Condition is red with anyone in sickbay or any open red order (pool,
//     directed, away or hailing with store.PriorityRed), else yellow with
//     anyone hailing, Command down (store.StatusSnapshot.CommandDown, #150),
//     crew beaming in whose provisioning waits for a tsh login (#106) or a
//     tsh login reason (#102), else green. Reasons name the crew members
//     and orders that set it, in id order, crew members first ("sorell in
//     sickbay", "ORD-0007 red alert", "sorell hailing"; an unclaimed red
//     order waiting on others, #142: "ORD-0007 red alert (waiting on
//     ORD-0006)"); a lower
//     condition's causes aren't listed, except Command's, the beam wait's
//     and the login's, which always come last, in that order ("Command not
//     on deck"; "2 crew waiting for a beam (tsh login)"; "tsh login
//     expires in 10m", "tsh login expired", "tsh login unknown: <err>"), so
//     a sickbay can't hide them. Green has no reasons.
//   - Crew counts crew members by status; dismissed ones aren't counted.
//     BeamingIn counts those whose tour is provisioning a beam or waiting
//     for its hello (#106).
//   - Orders.Open counts orders not yet claimed (pool or directed),
//     Orders.Away orders on a mission (away or hailing).
//   - HailsOpen counts open hails.
//   - BeamsLive counts crew members with a current beam who are neither in
//     sickbay nor dismissed. A crew member gets its beam once serve is up,
//     and keeps the alias in sickbay and dismissed.
//   - LastLogID is the ship's log's last id when these were read, 0 for
//     an empty log.
//   - Console is the console's address, "" when it is off.
//   - StandingOrders is whether standing orders are on (#78): the
//     setting, else the roster's default, which the first
//     Daemon.StandingOrders call seeds it with. Reading it writes nothing.
//   - Info lists facts worth knowing that never change the condition:
//     "regs.yaml not configured" (every hail goes to the captain, #149).
type BridgeStatus struct {
	Condition      string      `json:"condition"`
	Reasons        []string    `json:"reasons"`
	Info           []string    `json:"info"`
	Crew           CrewCounts  `json:"crew"`
	Orders         OrderCounts `json:"orders"`
	HailsOpen      int         `json:"hails_open"`
	BeamsLive      int         `json:"beams_live"`
	LastLogID      int64       `json:"last_log_id"`
	Console        string      `json:"console"`
	StandingOrders bool        `json:"standing_orders"`
}

type CrewCounts struct {
	BeamingIn int `json:"beaming_in"`
	OnDeck    int `json:"on_deck"`
	Away      int `json:"away"`
	Hailing   int `json:"hailing"`
	Sickbay   int `json:"sickbay"`
}

type OrderCounts struct {
	Open int `json:"open"`
	Away int `json:"away"`
}

// Status reads the store in one transaction and computes the bridge's
// status from it. bridge.status calls it, and so will the console's status
// event.
func (d *Daemon) Status(ctx context.Context) (BridgeStatus, error) {
	snap, err := d.store.ReadStatus(ctx)
	if err != nil {
		return BridgeStatus{}, err
	}
	snap.CommandDown = d.regsConfigured && commandDown(snap.Crew)
	s := statusOf(snap, d.loginReason(), d.beamLoginWaiting())
	s.Console = d.consoleAddr
	if !d.regsConfigured {
		s.Info = append(s.Info, infoRegsNotConfigured)
	}
	if s.StandingOrders, err = d.standingOrdersNow(ctx); err != nil {
		return BridgeStatus{}, err
	}
	return s, nil
}

// statusOf computes a BridgeStatus from a snapshot, the login's reason
// (loginState.reason; "" for none) and the crew members whose provisioning
// waits for a login (Daemon.beamLoginWaiting; nil for none). Only those
// beaming in count: the set is a hint kept outside the store, and the row
// is what the captain sees.
func statusOf(snap store.StatusSnapshot, login string, beamWaits map[string]bool) BridgeStatus {
	s := BridgeStatus{
		Orders: OrderCounts{
			Open: snap.OrderStates[store.OrderPool] + snap.OrderStates[store.OrderDirected],
			Away: snap.OrderStates[store.OrderAway] + snap.OrderStates[store.OrderHailing],
		},
		HailsOpen: snap.HailsOpen,
		LastLogID: snap.LastLogID,
		Info:      []string{},
	}
	var sick, hailing []string
	waiting := 0
	for _, c := range snap.Crew {
		switch c.Status {
		case store.CrewBeamingIn:
			s.Crew.BeamingIn++
			if beamWaits[c.ID] {
				waiting++
			}
		case store.CrewOnDeck:
			s.Crew.OnDeck++
		case store.CrewAway:
			s.Crew.Away++
		case store.CrewHailing:
			s.Crew.Hailing++
			hailing = append(hailing, c.ID+" hailing")
		case store.CrewSickbay:
			s.Crew.Sickbay++
			sick = append(sick, c.ID+" in sickbay")
		}
		if c.CurrentBeam != "" && c.Status != store.CrewSickbay && c.Status != store.CrewDismissed {
			s.BeamsLive++
		}
	}
	red := sick
	for _, id := range snap.RedOrders {
		reason := id + " red alert"
		if w := snap.RedWaiting[id]; len(w) > 0 {
			reason += " (waiting on " + strings.Join(w, ", ") + ")"
		}
		red = append(red, reason)
	}
	switch {
	case len(red) > 0:
		s.Condition, s.Reasons = ConditionRed, red
	case len(hailing) > 0 || snap.CommandDown || waiting > 0 || login != "":
		s.Condition, s.Reasons = ConditionYellow, hailing
	default:
		s.Condition, s.Reasons = ConditionGreen, []string{}
	}
	if snap.CommandDown {
		// Only a sickbay downs Command today, so this mostly shows under red.
		s.Reasons = append(s.Reasons, whyCommandNotOnDeck)
	}
	if waiting > 0 {
		s.Reasons = append(s.Reasons, fmt.Sprintf("%d crew waiting for a beam (tsh login)", waiting))
	}
	if login != "" {
		s.Reasons = append(s.Reasons, login)
	}
	return s
}
