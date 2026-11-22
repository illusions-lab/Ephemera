package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/illusions-lab/Ephemera/internal/host"
	"github.com/illusions-lab/Ephemera/internal/status"
)

func runPS(args []string) error {
	flags := flag.NewFlagSet("ps", flag.ContinueOnError)
	var all, quiet bool
	flags.BoolVar(&all, "all", false, "include reclaiming and pending deregistration")
	flags.BoolVar(&all, "a", false, "alias for --all")
	flags.BoolVar(&quiet, "quiet", false, "show only instance IDs")
	flags.BoolVar(&quiet, "q", false, "alias for --quiet")
	socket := flags.String("socket", host.StatusSocket(), "daemon status socket")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *socket == "" {
		return errors.New("usage: ephe ps [-a|--all] [-q|--quiet] [--socket PATH]")
	}
	rows, err := status.ReadInstances(context.Background(), *socket)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("daemon is offline: %w", err)
	}
	if err != nil {
		return fmt.Errorf("list local runner VMs: %w", err)
	}
	return printInstances(os.Stdout, rows, all, quiet, time.Now())
}

func printInstances(output io.Writer, rows []status.Instance, all, quiet bool, now time.Time) error {
	slices.SortFunc(rows, func(a, b status.Instance) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if !quiet {
		fmt.Fprintln(writer, "INSTANCE ID\tRUNNER\tSTATE\tAGE")
	}
	for _, row := range rows {
		if !all && (row.State == "reclaiming" || row.State == "pending-deregistration") {
			continue
		}
		if quiet {
			fmt.Fprintln(writer, row.ID)
			continue
		}
		age := "unknown"
		if !row.CreatedAt.IsZero() {
			age = max(0, now.Sub(row.CreatedAt)).Truncate(time.Second).String()
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", row.ID, row.Runner, row.State, age)
	}
	return writer.Flush()
}
