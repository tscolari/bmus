package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tscolari/bmus/internal/regs"
)

func newRegsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "regs", Short: "Manage the Regulations, regs.yaml"}
	cmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Write the default regs.yaml to <home>/regs.yaml",
		Long: `Write the default regs.yaml to <home>/regs.yaml.

It is a starting point: edit it, then restart the bridge. Without a
regs.yaml every hail goes to the captain. An existing file is never
overwritten.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := filepath.Abs(a.home)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(home, 0o700); err != nil {
				return err
			}
			path := filepath.Join(home, regs.File)
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists; edit it, or remove it to start over", path)
			}
			if err != nil {
				return err
			}
			_, err = f.Write(regs.Default)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				os.Remove(path) // don't leave a partial file for the next init to refuse
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "wrote", path)
			return nil
		},
	})
	return cmd
}
