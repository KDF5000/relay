package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/KDF5000/relay/internal/nodeconfig"
)

func mergeRuntimeConfig(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("relayctl config merge-runtimes", flag.ContinueOnError)
	flags.SetOutput(stderr)
	config := flags.String("config", "", "existing Node configuration")
	discovered := flags.String("discovered", "", "JSON array of discovered Runtime configurations")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *config == "" || *discovered == "" || len(flags.Args()) != 0 {
		return fmt.Errorf("usage: relayctl config merge-runtimes --config FILE --discovered FILE")
	}
	raw, err := os.ReadFile(*discovered)
	if err != nil {
		return err
	}
	result, err := nodeconfig.MergeRuntimes(*config, raw)
	if err != nil {
		return err
	}
	if len(result.Added) == 0 {
		fmt.Fprintln(stdout, "Existing Node configuration retained; no new Runtimes discovered.")
		return nil
	}
	fmt.Fprintf(stdout, "Backed up existing config to %s\nAdded Runtimes: %s\n", result.BackupPath, strings.Join(result.Added, ", "))
	return nil
}
