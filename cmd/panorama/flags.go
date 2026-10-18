// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"flag"
	"fmt"
	"strings"
)

// parseArgs parses a subcommand's flags wherever they appear among its
// arguments, and returns the arguments that are not flags, in order.
//
// Go's flag package stops at the first argument that is not a flag, and every
// command here was written as though it did not: `providers check -model X
// bedrock` named the provider and `providers check bedrock -model X` was
// refused as naming none, and on a command taking no arguments at all a flag
// after a stray word was dropped without a sound. A person types the provider
// and the flags in whichever order they think of them, so both orders mean the
// same thing, and `--` ends the flags for anybody who needs a word that starts
// with a dash.
//
// The FlagSet decides which flags take a value, so a value is never mistaken
// for a positional the way a hand-written table of flag names eventually is.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		// The flag package consumes a `--` and stops, which shows as the
		// argument just before what it left behind.
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// parseFlags is parseArgs for a command that takes no arguments, refusing any it
// was given. Ignoring one would be the same silent failure in another shape:
// `token create -email a@example.com laptop` reads as naming the token and
// names nothing.
func parseFlags(fs *flag.FlagSet, args []string) error {
	extra, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		return fmt.Errorf("%s takes no arguments, only flags; it was given %q", fs.Name(), strings.Join(extra, " "))
	}
	return nil
}
