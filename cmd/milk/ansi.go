package main

import (
	"os"

	"golang.org/x/term"
)

var isTTY = term.IsTerminal(int(os.Stdout.Fd()))

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[34m"
)

func colorize(s, code string) string {
	if !isTTY {
		return s
	}
	return code + s + ansiReset
}

func green(s string) string  { return colorize(s, ansiGreen) }
func blue(s string) string   { return colorize(s, ansiBlue) }
func yellow(s string) string { return colorize(s, ansiYellow) }
func red(s string) string    { return colorize(s, ansiRed) }
func dim(s string) string    { return colorize(s, ansiDim) }
func bold(s string) string   { return colorize(s, ansiBold) }

// milkTag returns the dimmed [milk] system prefix.
func milkTag() string { return dim("[milk]") }
