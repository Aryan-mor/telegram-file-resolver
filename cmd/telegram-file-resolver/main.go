// Command telegram-file-resolver runs the resolver on its own Telegram account.
//
//	telegram-file-resolver login
//	telegram-file-resolver submit "https://t.me/somebot?start=token"
//	telegram-file-resolver serve -addr 127.0.0.1:8787     (POST /submit {"link": "..."})
//
// API_ID and API_HASH come from https://my.telegram.org. The session and the list of joined
// channels live in -data (default ./data). The first run asks for the phone number, the login
// code and, if the account has one, the two-step password.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/term"

	fileresolver "github.com/Aryan-mor/telegram-file-resolver"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: telegram-file-resolver login | submit <link> | serve [-addr host:port] [-data dir]")
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	data := fs.String("data", "data", "folder for the session and the joined-channel list")
	addr := fs.String("addr", "127.0.0.1:8787", "serve: listen address")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	apiID, err := strconv.Atoi(os.Getenv("API_ID"))
	if err != nil || os.Getenv("API_HASH") == "" {
		return errors.New("set API_ID and API_HASH (https://my.telegram.org)")
	}
	if err := os.MkdirAll(*data, 0o700); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client := telegram.NewClient(apiID, os.Getenv("API_HASH"), telegram.Options{
		SessionStorage: &session.FileStorage{Path: filepath.Join(*data, "session.json")},
	})
	return client.Run(ctx, func(ctx context.Context) error {
		if err := client.Auth().IfNecessary(ctx, auth.NewFlow(terminalAuth{}, auth.SendCodeOptions{})); err != nil {
			return err
		}
		self, err := client.Self(ctx)
		if err != nil {
			return err
		}
		api := tg.NewClient(client)
		cfg := fileresolver.DefaultConfig()
		cfg.StateDir, cfg.Owner, cfg.Log = filepath.Join(*data, "state"), self.ID, log
		eng, err := fileresolver.New(fileresolver.ProviderFunc(func(ctx context.Context, fn func(context.Context, fileresolver.API) error) error {
			return fn(ctx, api)
		}), cfg)
		if err != nil {
			return err
		}
		switch cmd {
		case "login":
			fmt.Printf("signed in as %s (id %d)\n", self.Username, self.ID)
			return nil
		case "submit":
			if fs.NArg() != 1 {
				return errors.New("usage: telegram-file-resolver submit <link>")
			}
			res, err := submit(ctx, eng, fs.Arg(0), func(l string) { fmt.Println(" ", l) })
			if err != nil {
				return err
			}
			fmt.Printf("done: %d message(s) forwarded to Saved Messages from @%s\n", res.Delivered.Count, res.Bot)
			return nil
		case "serve":
			return serve(ctx, eng, *addr, log)
		}
		return fmt.Errorf("unknown command %q", cmd)
	})
}

func submit(ctx context.Context, eng *fileresolver.Engine, link string, progress func(string)) (*fileresolver.Result, error) {
	return eng.Submit(ctx, link, fileresolver.Options{Progress: progress, Sink: fileresolver.ForwardSink{}})
}

func serve(ctx context.Context, eng *fileresolver.Engine, addr string, log *slog.Logger) error {
	go func() { _ = eng.Run(ctx) }()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /submit", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Link string `json:"link"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			http.Error(w, "send {\"link\": \"https://t.me/bot?start=...\"}", http.StatusBadRequest)
			return
		}
		res, err := submit(r.Context(), eng, body.Link, nil)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "bot": res.Bot, "messages": len(res.Messages), "delivered": res.Delivered.Count, "joined": res.Joined, "handoffs": res.Handoffs, "failures": res.Failures})
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// terminalAuth asks for the login details on the terminal.
type terminalAuth struct{}

func prompt(label string) (string, error) {
	fmt.Print(label)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}

func (terminalAuth) Phone(context.Context) (string, error) {
	return prompt("Phone number with country code (+...): ")
}

func (terminalAuth) Code(context.Context, *tg.AuthSentCode) (string, error) {
	return prompt("Login code: ")
}

func (terminalAuth) Password(context.Context) (string, error) {
	fmt.Print("Two-step password: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	return string(b), err
}

func (terminalAuth) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error { return nil }

func (terminalAuth) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("this number has no Telegram account")
}
