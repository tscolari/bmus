package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/console"
)

func newConsoleCmd(a *app) *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "console",
		Short: "Print the console's URL (with its session token) and open it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var status bridge.BridgeStatus
			if err := a.call(cmd.Context(), "bridge.status", nil, &status); err != nil {
				return err
			}
			if status.Console == "" {
				return errors.New("the console is off: the bridge runs with --no-console")
			}
			home, err := filepath.Abs(a.home)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(filepath.Join(home, console.TokenFile))
			if err != nil {
				return fmt.Errorf("console token: %w", err)
			}
			token := strings.TrimSpace(string(data))
			if token == "" {
				return errors.New("console token: empty file")
			}
			url := fmt.Sprintf("http://%s/#t=%s", status.Console, token)
			fmt.Fprintln(cmd.OutOrStdout(), url)
			if !noOpen {
				if err := openBrowser(url); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "bmus: could not open the browser:", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the URL but don't open it in a browser")
	return cmd
}

// openBrowser opens url with the platform's default handler.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
