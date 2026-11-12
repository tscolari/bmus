package bridge

import (
	"context"

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
//     anyone hailing, Command down (store.StatusSnapshot.CommandDown, #150)
//     or a tsh login reason (#102), else green. Reasons name the crew
//     members and orders that set it, in id order, crew members first
//     ("sorell in sickbay", "ORD-0007 red alert", "sorell hailing"); a
//     lower condition's causes aren't listed, except Command's and the
//     login's, which always come last, in that order ("Command not on
//     deck"; "tsh login expires in 10m", "tsh login expired", "tsh login
//     unknown: <err>"), so a sickbay can't hide them. Green has no
//     reasons.
//   - Crew counts crew members by status; dismissed ones aren't counted.
//   - Orders.Open counts orders not yet claimed (pool or directed),
//     Orders.Away orders on a mission (away or hailing).
//   - HailsOpen counts open hails.
//   - BeamsLive counts crew members with a current beam who are neither in
//     sickbay nor dismissed. A crew member gets its beam once serve is up,
//     and keeps the alias in sickbay and dismissed.
//   - LastLogID is the captain's log's last id when these were read, 0 for
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
	OnDeck  int `json:"on_deck"`
	Away    int `json:"away"`
	Hailing int `json:"hailing"`
	Sickbay int `json:"sickbay"`
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
	s := statusOf(snap, d.loginReason())
	s.Console = d.consoleAddr
	if !d.regsConfigured {
		s.Info = append(s.Info, infoRegsNotConfigured)
	}
	if s.StandingOrders, err = d.standingOrdersNow(ctx); err != nil {
		return BridgeStatus{}, err
	}
	return s, nil
}

// statusOf computes a BridgeStatus from a snapshot and the login's reason
// (loginState.reason; "" for none).
func statusOf(snap store.StatusSnapshot, login string) BridgeStatus {
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
	for _, c := range snap.Crew {
		switch c.Status {
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
		red = append(red, id+" red alert")
	}
	switch {
	case len(red) > 0:
		s.Condition, s.Reasons = ConditionRed, red
	case len(hailing) > 0 || snap.CommandDown || login != "":
		s.Condition, s.Reasons = ConditionYellow, hailing
	default:
		s.Condition, s.Reasons = ConditionGreen, []string{}
	}
	if snap.CommandDown {
		// Only a sickbay downs Command today, so this mostly shows under red.
		s.Reasons = append(s.Reasons, whyCommandNotOnDeck)
	}
	if login != "" {
		s.Reasons = append(s.Reasons, login)
	}
	return s
}
