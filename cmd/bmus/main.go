// Command bmus is the bmus CLI: `bmus bridge run` runs the bridge daemon,
// and every other command talks to it over its local RPC socket,
// <home>/bridge.sock (spec/phase-1-plan-2026-10-16.md §9).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/store"
)

// Exit codes.
const (
	exitError      = 1 // any failure
	exitNotRunning = 3 // the bridge isn't running
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "bmus:", err)
		if isNotRunning(err) {
			os.Exit(exitNotRunning)
		}
		os.Exit(exitError)
	}
}

// app holds the global flags.
type app struct {
	home string
}

func newRootCmd() *cobra.Command {
	a := &app{}
	root := &cobra.Command{
		Use:           "bmus",
		Short:         "Run and command a crew of agents on Teleport Beams",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&a.home, "home", defaultHome(),
		"bridge directory holding bridge.db and bridge.sock ($BMUS_HOME)")
	root.AddCommand(
		newBridgeCmd(a),
		newCrewCmd(a),
		newOrderCmd(a),
		newHailsCmd(a),
		newLogCmd(a),
		newStatusCmd(a),
		newStandingOrdersCmd(a),
		newConsoleCmd(a),
		newRegsCmd(a),
		newCommandCmd(a),
	)
	return root
}

// defaultHome is $BMUS_HOME, else ~/.bmus.
func defaultHome() string {
	if h := os.Getenv("BMUS_HOME"); h != "" {
		return h
	}
	if d, err := store.DefaultDir(); err == nil {
		return d
	}
	return ".bmus"
}

func (a *app) socket() (string, error) {
	home, err := filepath.Abs(a.home)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, bridge.SocketFile), nil
}

// call runs one RPC against the bridge.
func (a *app) call(ctx context.Context, cmd string, args, result any) error {
	sock, err := a.socket()
	if err != nil {
		return err
	}
	return bridge.Call(ctx, sock, cmd, args, result)
}

// printJSON writes v as indented JSON.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// dash shows an empty table cell as "-".
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func isNotRunning(err error) bool { return errors.Is(err, bridge.ErrBridgeNotRunning) }
