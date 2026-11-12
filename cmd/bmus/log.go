package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tscolari/bmus/internal/bridge"
)

const (
	// followEvery is how often `log tail --follow` polls.
	followEvery = 500 * time.Millisecond
	// maxBusyBackoff caps the poll interval while the bridge is busy.
	maxBusyBackoff = 5 * time.Second
	// logPage is the log.tail page size asked for.
	logPage = 500
)

func newLogCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "log", Short: "Read the captain's log"}
	var (
		follow bool
		since  int64
		asJSON bool
	)
	tail := &cobra.Command{
		Use:   "tail",
		Short: "Print the captain's log, one entry per line: <ts> <kind> <crew> <order> <text>",
		Long: `Print the captain's log, one entry per line: <ts> <kind> <crew> <order> <text>.

With --follow it keeps polling for new entries every 500ms until interrupted.
Once it has reached the bridge it rides out a bridge restart, printing a note
on stderr while the bridge is down. A bridge too busy to answer is retried,
backing off up to 5s. --json prints one JSON object per entry.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if follow {
				var stop context.CancelFunc
				ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
				defer stop()
			}
			return tailLog(ctx, a, cmd.OutOrStdout(), cmd.ErrOrStderr(), since, follow, asJSON)
		},
	}
	tail.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new entries until interrupted")
	tail.Flags().Int64Var(&since, "since", 0, "print entries after this id")
	tail.Flags().BoolVar(&asJSON, "json", false, "print one JSON object per entry")
	cmd.AddCommand(tail)
	return cmd
}

func tailLog(ctx context.Context, a *app, out, errOut io.Writer, since int64, follow, asJSON bool) error {
	enc := json.NewEncoder(out)
	first, down, busy := true, false, false
	wait := followEvery
	for {
		var res bridge.LogTailResult
		err := a.call(ctx, "log.tail", bridge.LogTailArgs{SinceID: since, Limit: logPage}, &res)
		var re *bridge.RPCError
		switch {
		case err != nil && follow && ctx.Err() != nil:
			return nil // interrupted
		case err != nil && follow && errors.As(err, &re) && re.Code == bridge.CodeUnavailable:
			// Every RPC slot is taken: back off and retry.
			if !busy {
				fmt.Fprintln(errOut, "bmus: bridge busy; retrying")
				busy = true
			}
			down = false
			wait = min(wait*2, maxBusyBackoff)
		case err != nil && follow && !first && bridge.IsBridgeDown(err):
			// The bridge is stopping or gone (a restart, say): wait for
			// it to come back.
			if !down {
				fmt.Fprintln(errOut, "bmus: bridge not running; waiting for it")
				down = true
			}
			busy = false
			wait = followEvery
		case err != nil:
			return err
		default:
			first, down, busy = false, false, false
			wait = followEvery
			for _, e := range res.Entries {
				if asJSON {
					if err := enc.Encode(e); err != nil {
						return err
					}
				} else if _, err := fmt.Fprintf(out, "%s %s %s %s %s\n", e.TS.Local().Format(time.RFC3339),
					e.Kind, dash(e.CrewID), dash(e.OrderID), oneLine(e.Text)); err != nil {
					return err
				}
			}
			since = res.LastID
			if len(res.Entries) >= logPage {
				continue // a full page: there may be more right away
			}
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}
