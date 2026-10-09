// telegram-tools: log into Telegram as a user account over MTProto (gotd/td)
// and dump chat history, something the Bot API cannot do.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
)

const (
	configFile  = "config.json"
	sessionFile = "session.json"
	appVersion  = "telegram-tools 1.0"
)

type config struct {
	APIID   int    `json:"api_id"`
	APIHash string `json:"api_hash"`
	Phone   string `json:"phone"`
}

// errNotLoggedIn is returned when no session exists or the session is not authorized.
var errNotLoggedIn = errors.New("not logged in, run: telegram-tools login")

func usage() {
	fmt.Fprintf(os.Stderr, `telegram-tools: dump Telegram chat history as a user account (MTProto)

Usage:
  telegram-tools login                       log in (sends a code to the phone in config.json)
  telegram-tools whoami                      print the logged in user
  telegram-tools chats                       list dialogs: kind, id, title (tab separated)
  telegram-tools history --chat "<title or id>" [--since T] [--until T] [--from <name or id>] [--oldest] [--limit N] [--out file] [--json]
  telegram-tools search  --chat "<title or id>" --query "text" [--from <name or id>] [--oldest] [--limit N] [--out file] [--json]

Run "telegram-tools <command> --help" for the flags of a command.
Config is read from config.json and the session is kept in session.json, both next to the exe.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch cmd {
	case "login":
		err = cmdLogin(ctx, args)
	case "whoami":
		err = cmdWhoami(ctx, args)
	case "chats":
		err = cmdChats(ctx, args)
	case "history":
		err = cmdHistory(ctx, args)
	case "search":
		err = cmdSearch(ctx, args)
	case "help", "-h", "--help", "-help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, errNotLoggedIn) {
			fmt.Fprintln(os.Stderr, errNotLoggedIn.Error())
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

// exeDir returns the directory the executable lives in (config and session live there).
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Dir(exe)
}

func loadConfig() (config, error) {
	path := filepath.Join(exeDir(), configFile)
	var cfg config
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, fmt.Errorf("%s not found; create it with api_id, api_hash and phone (see README.md)", path)
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.APIID == 0 || strings.TrimSpace(cfg.APIHash) == "" {
		return cfg, fmt.Errorf("%s: api_id and api_hash are empty; get them at https://my.telegram.org (see README.md)", path)
	}
	cfg.APIHash = strings.TrimSpace(cfg.APIHash)
	cfg.Phone = strings.TrimSpace(cfg.Phone)
	return cfg, nil
}

func sessionPath() string {
	return filepath.Join(exeDir(), sessionFile)
}

func newClient(cfg config) *telegram.Client {
	return telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionPath()},
		Middlewares:    []telegram.Middleware{floodWaitMiddleware()},
		NoUpdates:      true,
		Device: telegram.DeviceConfig{
			AppVersion: appVersion,
		},
	})
}

// withClient connects and runs f with an authorized client. It fails fast with
// errNotLoggedIn when there is no session file, before touching the network.
func withClient(ctx context.Context, f func(ctx context.Context, client *telegram.Client, api *tg.Client) error) error {
	if _, err := os.Stat(sessionPath()); err != nil {
		return errNotLoggedIn
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	client := newClient(cfg)
	return client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check auth status: %w", err)
		}
		if !status.Authorized {
			return errNotLoggedIn
		}
		return f(ctx, client, client.API())
	})
}

func userName(u *tg.User) string {
	if u == nil {
		return ""
	}
	if u.Deleted {
		return "Deleted Account"
	}
	name := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
	if name == "" && u.Username != "" {
		name = "@" + u.Username
	}
	if name == "" {
		name = fmt.Sprintf("user %d", u.ID)
	}
	return name
}

func describeUser(u *tg.User) string {
	s := fmt.Sprintf("%s (id %d", userName(u), u.ID)
	if u.Username != "" {
		s += ", @" + u.Username
	}
	if u.Phone != "" {
		s += ", +" + u.Phone
	}
	return s + ")"
}

// ---- login ----

// termAuth asks for the code and the 2FA password on stdin.
type termAuth struct {
	phone string
	in    *bufio.Reader
}

func (a termAuth) prompt(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	line, err := a.in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read %s: %w", strings.TrimSpace(strings.TrimSuffix(label, ":")), err)
	}
	return strings.TrimSpace(line), nil
}

func (a termAuth) Phone(_ context.Context) (string, error) {
	return a.phone, nil
}

func (a termAuth) Password(_ context.Context) (string, error) {
	return a.prompt("Two-factor (cloud) password: ")
}

func (a termAuth) Code(_ context.Context, sent *tg.AuthSentCode) (string, error) {
	via := "Telegram"
	switch sent.Type.(type) {
	case *tg.AuthSentCodeTypeSMS:
		via = "SMS"
	case *tg.AuthSentCodeTypeCall:
		via = "phone call"
	case *tg.AuthSentCodeTypeFlashCall:
		via = "flash call"
	case *tg.AuthSentCodeTypeApp:
		via = "the Telegram app"
	}
	fmt.Fprintf(os.Stderr, "A login code was sent via %s.\n", via)
	return a.prompt("Code: ")
}

func (a termAuth) AcceptTermsOfService(_ context.Context, _ tg.HelpTermsOfService) error {
	return nil
}

func (a termAuth) SignUp(_ context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("this phone number has no Telegram account; sign up is not supported")
}

func cmdLogin(ctx context.Context, args []string) error {
	fs := newFlagSet("login", "telegram-tools login\n\nSends a login code to the phone in config.json, asks for it on stdin (and for the 2FA password if set) and saves session.json.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.Phone == "" {
		return errors.New("config.json: phone is empty (international format, e.g. +34600000000)")
	}
	client := newClient(cfg)
	return client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check auth status: %w", err)
		}
		if status.Authorized {
			fmt.Printf("Already logged in as %s\n", describeUser(status.User))
			return nil
		}
		fmt.Fprintf(os.Stderr, "Logging in as %s ...\n", cfg.Phone)
		flow := auth.NewFlow(termAuth{phone: cfg.Phone, in: bufio.NewReader(os.Stdin)}, auth.SendCodeOptions{})
		if err := flow.Run(ctx, client.Auth()); err != nil {
			return fmt.Errorf("login: %w", err)
		}
		self, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("get self: %w", err)
		}
		fmt.Printf("Logged in as %s\nSession saved to %s\n", describeUser(self), sessionPath())
		return nil
	})
}

// ---- whoami ----

func cmdWhoami(ctx context.Context, args []string) error {
	fs := newFlagSet("whoami", "telegram-tools whoami\n\nPrints the logged in user.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withClient(ctx, func(ctx context.Context, client *telegram.Client, _ *tg.Client) error {
		self, err := client.Self(ctx)
		if err != nil {
			return err
		}
		fmt.Println(describeUser(self))
		return nil
	})
}
