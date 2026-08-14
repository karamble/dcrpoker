package main

import dcrlog "github.com/vctt94/dcrpoker/internal/log"

// One logger per thing the program does, rather than one for the command.
// Everything here lives in a single package, so a tag per file would say only
// which file a line came from, which is the one thing the line already says.
//
// HTTP has no alias here: nothing in this package logs through it directly, and
// its one user is the interface server's own error log, wired in run().
var (
	brdgLog = dcrlog.BRDG
	chanLog = dcrlog.CHAN
	coinLog = dcrlog.COIN
	dsptLog = dcrlog.DSPT
	playLog = dcrlog.PLAY
	pokrLog = dcrlog.POKR
	setlLog = dcrlog.SETL
	tablLog = dcrlog.TABL
)
