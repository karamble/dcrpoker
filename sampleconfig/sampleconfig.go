// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

// Package sampleconfig holds dcrpoker's documented example configuration,
// embedded so a binary can write one wherever it first runs.
package sampleconfig

import _ "embed"

//go:embed sample-dcrpoker.conf
var sampleDcrpokerConf string

// FileContents returns the sample configuration file.
func FileContents() string { return sampleDcrpokerConf }
