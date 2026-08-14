package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
)

// The credential for this game's own interface.
//
// It used to be the host's: the dashboard proxied the page and swapped a
// short-lived token of its own onto every request, so this process only ever
// saw the one token it also reached the host with. There is no proxy now - the
// page is served from here, to a browser on this machine - so this process has
// to issue something itself.
//
// It is not the bridge credential and must never be. That one is an identity a
// person carried here and it moves money; this one only says "you are the
// person sitting at this machine", which is a different and much smaller claim.

// newUIToken mints the token for this run.
//
// Fresh every start rather than stored. It is a session key for a local page,
// so there is nothing to remember: a restart is a new session, and a token that
// outlived the process would sit in a file being older than it needs to be.
func newUIToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate a token for the interface: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// uiURL is what to open, token and all.
//
// Printed on startup because that is the only place it exists. A token in a URL
// would be a real problem if it crossed a network - it lands in history and in
// every log between - but this one is loopback, and the alternative is asking
// somebody to paste a hex string into a page every time they restart a game.
func uiURL(listen, token string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = "127.0.0.1", "8790"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("http://%s:%s/ui/#token=%s", host, port, token)
}

// isTerminal reports whether a person is there to answer a question.
//
// Asked before the first-run wizard prompts. Something started by a service
// manager has no terminal, and a program that blocked on a read nobody was
// going to answer would look like a hang rather than like a question.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// loopbackOnly reports whether an address is reachable only from this machine.
//
// The interface is guarded by a token, but the token is the only thing between
// a caller and a table's money, and it is printed to a terminal. Binding it
// somewhere the whole network can reach turns a local session key into a
// network credential, which is not what it was minted to be.
func loopbackOnly(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
