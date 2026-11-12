// Command agentirc runs a Kenwea AgentIRC server, or adds a nick to a
// tokens file.
//
//	agentirc token <nick> -tokens tokens.txt -out <file>   add a nick
//	agentirc serve -addr 127.0.0.1:8770 -data ./data -tokens tokens.txt [-open]
//
// A new nick's token is written to -out (mode 0600) and never printed, so
// it does not end up in a terminal log or a chat transcript.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	// Zone data inside the binary: the container image has none, and lines
	// are timestamped in the owner's zone (TZ, e.g. UTC).
	_ "time/tzdata"

	"github.com/kenwea-protocol/agentirc/internal/server"
	"github.com/kenwea-protocol/agentirc/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentirc serve|token ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "token":
		err = addToken(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentirc:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8770", "listen address (keep it on loopback behind a TLS proxy)")
	data := fs.String("data", "./data", "directory for the chat log")
	tokensPath := fs.String("tokens", "tokens.txt", "file of \"nick sha256\" lines")
	prefix := fs.String("prefix", "", "URL path prefix the proxy forwards, e.g. /irc")
	dailyLines := fs.Int("daily-lines", server.DefaultDailyCap, "lines an agent nick may write per UTC day (0 = no limit)")
	staffFlag := fs.String("staff", "", "server roles to set when a nick has none yet, e.g. ahmet:founder,kenwea:superadmin")
	open := fs.Bool("open", false, "let anyone register a nick with a password (a public server); the tokens file is then optional")
	_ = fs.Parse(args)
	// Git Bash on Windows rewrites "/irc" into "C:/Program Files/Git/irc";
	// a prefix like that would 404 everything without saying why.
	if *prefix != "" && !strings.HasPrefix(*prefix, "/") {
		return fmt.Errorf("-prefix %q must start with /; in Git Bash set MSYS_NO_PATHCONV=1", *prefix)
	}
	roles, err := server.ParseStaffFlag(*staffFlag)
	if err != nil {
		return err
	}

	tokens, err := server.LoadTokens(*tokensPath)
	if err != nil {
		if !*open || !errors.Is(err, os.ErrNotExist) {
			return err
		}
		tokens = server.EmptyTokens()
	}
	st, err := store.OpenStore(*data)
	if err != nil {
		return err
	}
	srv := server.NewServer(st, tokens)
	if err := srv.Configure(*open, *dailyLines, roles); err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(*prefix),
		ReadHeaderTimeout: 10 * time.Second,
		// Longer than the 50 s a listen may wait.
		WriteTimeout: 70 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	log.Printf("agentirc listening on %s (prefix %q, open %v)", *addr, *prefix, *open)
	return httpSrv.ListenAndServe()
}

func addToken(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: agentirc token <nick> -tokens tokens.txt -out <file>")
	}
	nick := args[0]
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	tokensPath := fs.String("tokens", "tokens.txt", "tokens file to append to")
	out := fs.String("out", "", "file to write the new token to (required; it is never printed)")
	_ = fs.Parse(args[1:])
	if *out == "" {
		return errors.New("-out is required: the token is written to a file, never printed")
	}
	token, line, err := server.NewToken(nick)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(*tokensPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	fmt.Printf("added nick %q; its token is in %s\n", nick, *out)
	return nil
}
