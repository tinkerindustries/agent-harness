package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// runConfig reads and writes the settings table: list, get, set, unset.
// Values are masked by default so a secret never lands in full in terminal
// scrollback or a CI log; only `get -reveal` prints one in full, and only
// for the single key it names.
func runConfig(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: harness config list | get <key> | set <key> <value> | unset <key>")
	}
	switch args[0] {
	case "list":
		return runConfigList(ctx, args[1:])
	case "get":
		return runConfigGet(ctx, args[1:])
	case "set":
		return runConfigSet(ctx, args[1:])
	case "unset":
		return runConfigUnset(ctx, args[1:])
	default:
		return fmt.Errorf("unknown config subcommand %q; expected list, get, set, or unset", args[0])
	}
}

// openConfigResolver loads config and returns a settings resolver over the
// harness database, which the caller must Close.
func openConfigResolver() (*settings.Resolver, *store.Store, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	st, err := openStore(cfg)
	if err != nil {
		return nil, nil, err
	}
	return settings.NewResolver(st), st, nil
}

// maskSecret masks value so at most its last 4 characters are visible. A
// value of 4 characters or fewer reveals none of itself — the guarantee is
// that no stored secret ever appears in full outside `get -reveal`.
func maskSecret(value string) string {
	if len(value) <= 4 {
		return strings.Repeat("*", len(value))
	}
	return strings.Repeat("*", len(value)-4) + value[len(value)-4:]
}

func runConfigList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("config list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, st, err := openConfigResolver()
	if err != nil {
		return err
	}
	defer st.Close()

	for _, key := range settings.ValidKeys {
		value, ok, err := res.Get(ctx, key)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Printf("%s = (not set)\n", key)
			continue
		}
		fmt.Printf("%s = %s\n", key, maskSecret(value))
	}
	return nil
}

func runConfigGet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("config get", flag.ContinueOnError)
	reveal := fs.Bool("reveal", false, "print the value in full instead of masking it")
	// Go's flag package stops at the first non-flag argument, which would
	// silently ignore `config get deepseek.api_key -reveal`. Split flags
	// from positionals first so -reveal works on either side of the key.
	flags, positional := splitFlags(args)
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: harness config get [-reveal] <key>")
	}
	key := positional[0]

	res, st, err := openConfigResolver()
	if err != nil {
		return err
	}
	defer st.Close()

	value, ok, err := res.Get(ctx, key)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("%s = (not set)\n", key)
		return nil
	}
	if *reveal {
		fmt.Printf("%s = %s\n", key, value)
	} else {
		fmt.Printf("%s = %s\n", key, maskSecret(value))
	}
	return nil
}

// splitFlags separates args into flag-looking (leading "-") and positional
// entries, preserving order within each group. Only runConfigGet needs it —
// it is the one subcommand with a flag, and it must work whether the flag
// precedes or follows the key.
func splitFlags(args []string) (flags, positional []string) {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
		} else {
			positional = append(positional, a)
		}
	}
	return flags, positional
}

func runConfigSet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: harness config set <key> <value>")
	}
	key, value := fs.Arg(0), fs.Arg(1)

	res, st, err := openConfigResolver()
	if err != nil {
		return err
	}
	defer st.Close()

	if err := res.Set(ctx, key, value); err != nil {
		return err
	}
	fmt.Printf("set %s\n", key)
	return nil
}

func runConfigUnset(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("config unset", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: harness config unset <key>")
	}
	key := fs.Arg(0)

	res, st, err := openConfigResolver()
	if err != nil {
		return err
	}
	defer st.Close()

	if err := res.Unset(ctx, key); err != nil {
		return err
	}
	fmt.Printf("unset %s\n", key)
	return nil
}
