// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
)

// TestFlagsMayComeOnEitherSideOfAnArgument covers `providers check -model X
// bedrock`, which was refused as naming no provider because the flag package
// stopped reading at the first word that was not a flag.
func TestFlagsMayComeOnEitherSideOfAnArgument(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		positional []string
		model      string
		write      bool
	}{
		{name: "argument first", args: []string{"bedrock", "-model", "x"}, positional: []string{"bedrock"}, model: "x"},
		{name: "flags first", args: []string{"-model", "x", "bedrock"}, positional: []string{"bedrock"}, model: "x"},
		{name: "flags on both sides", args: []string{"-write", "bedrock", "-model=x"},
			positional: []string{"bedrock"}, model: "x", write: true},
		{name: "a value is not an argument", args: []string{"-model", "bedrock"}, model: "bedrock"},
		{name: "a boolean takes no value", args: []string{"-write", "bedrock"}, positional: []string{"bedrock"}, write: true},
		{name: "two arguments keep their order", args: []string{"a", "-write", "b"},
			positional: []string{"a", "b"}, write: true},
		{name: "-- ends the flags", args: []string{"-model", "x", "--", "-write", "b"},
			positional: []string{"-write", "b"}, model: "x"},
		{name: "-- after an argument", args: []string{"a", "--", "-model"}, positional: []string{"a", "-model"}},
		{name: "nothing at all", args: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("providers check", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			model := fs.String("model", "", "")
			write := fs.Bool("write", false, "")

			got, err := parseArgs(fs, tc.args)
			if err != nil {
				t.Fatalf("parseArgs(%v): %v", tc.args, err)
			}
			if !slices.Equal(got, tc.positional) {
				t.Errorf("positional = %q, want %q", got, tc.positional)
			}
			if *model != tc.model {
				t.Errorf("-model = %q, want %q", *model, tc.model)
			}
			if *write != tc.write {
				t.Errorf("-write = %v, want %v", *write, tc.write)
			}
		})
	}
}

func TestAnUnknownFlagIsRefusedWhereverItIs(t *testing.T) {
	for _, args := range [][]string{{"-nope", "bedrock"}, {"bedrock", "-nope"}} {
		fs := flag.NewFlagSet("providers check", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("model", "", "")
		if _, err := parseArgs(fs, args); err == nil {
			t.Errorf("parseArgs(%v) accepted an undefined flag", args)
		}
	}
}

// TestACommandWithNoArgumentsRefusesOne covers the quieter half: a word after
// the flags used to end parsing, so every flag after it was dropped.
func TestACommandWithNoArgumentsRefusesOne(t *testing.T) {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	email := fs.String("email", "", "")
	role := fs.String("role", "developer", "")

	err := parseFlags(fs, []string{"-email", "a@example.com", "laptop", "-role", "admin"})
	if err == nil || !strings.Contains(err.Error(), `"laptop"`) {
		t.Fatalf("err = %v, want a refusal naming the stray argument", err)
	}
	// Read in full before refusing, so the message is about the argument and
	// not about a flag that went missing after it.
	if *email != "a@example.com" || *role != "admin" {
		t.Errorf("flags = %q %q, want both read", *email, *role)
	}

	fs = flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("email", "", "")
	if err := parseFlags(fs, []string{"-email", "a@example.com"}); err != nil {
		t.Errorf("parseFlags with flags only: %v", err)
	}
}
