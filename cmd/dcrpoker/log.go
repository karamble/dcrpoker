package main

import dcrlog "github.com/vctt94/dcrpoker/internal/log"

// One logger per thing the program does, rather than one for the command.
// Everything here lives in a single package, so a tag per file would say only
// which file a line came from, which is the one thing the line already says.
var (
	brdgLog = dcrlog.BRDG
	chanLog = dcrlog.CHAN
	coinLog = dcrlog.COIN
	dsptLog = dcrlog.DSPT
	httpLog = dcrlog.HTTP
	playLog = dcrlog.PLAY
	pokrLog = dcrlog.POKR
	setlLog = dcrlog.SETL
	tablLog = dcrlog.TABL
)
