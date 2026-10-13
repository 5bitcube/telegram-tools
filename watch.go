package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// watchRecord extends record with the chat id for multiplexed output.
type watchRecord struct {
	ID         int    `json:"id"`
	ChatID     int64  `json:"chat_id"`
	Date       string `json:"date"`
	SenderID   int64  `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Text       string `json:"text"`
	Media      string `json:"media"`
	ReplyTo    int    `json:"reply_to"`
	FwdFrom    string `json:"fwd_from"`
}

func cmdWatch(ctx context.Context, args []string) error {
	fs := newFlagSet("watch", `telegram-tools watch [--chat "<title or id>"] ...

Connects to Telegram and streams new messages as JSON lines (one per message)
to stdout. Stays connected until interrupted (Ctrl+C).
Without --chat, watches all chats.`)

	var chatFlags multiString
	fs.Var(&chatFlags, "chat", "chat to watch (title substring or numeric id); repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if _, err := os.Stat(sessionPath()); err != nil {
		return errNotLoggedIn
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ents := newEntities()
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)

	var allowedPeers map[int64]bool

	mergeEntities := func(e tg.Entities) {
		for id, u := range e.Users {
			ents.users[id] = u
		}
		for id, c := range e.Chats {
			ents.chats[id] = c
		}
		for id, ch := range e.Channels {
			ents.channels[id] = ch
		}
	}

	emit := func(m tg.NotEmptyMessage, e tg.Entities) {
		mergeEntities(e)

		pid := peerID(m.GetPeerID())
		if allowedPeers != nil && !allowedPeers[pid] {
			return
		}

		r := toRecord(m, ents)
		enc.Encode(watchRecord{
			ID:         r.ID,
			ChatID:     pid,
			Date:       r.Date,
			SenderID:   r.SenderID,
			SenderName: r.SenderName,
			Text:       r.Text,
			Media:      r.Media,
			ReplyTo:    r.ReplyTo,
			FwdFrom:    r.FwdFrom,
		})
	}

	dispatcher := tg.NewUpdateDispatcher()

	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, update *tg.UpdateNewMessage) error {
		switch msg := update.Message.(type) {
		case *tg.Message:
			emit(msg, e)
		case *tg.MessageService:
			emit(msg, e)
		}
		return nil
	})

	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, update *tg.UpdateNewChannelMessage) error {
		switch msg := update.Message.(type) {
		case *tg.Message:
			emit(msg, e)
		case *tg.MessageService:
			emit(msg, e)
		}
		return nil
	})

	dispatcher.OnEditMessage(func(ctx context.Context, e tg.Entities, update *tg.UpdateEditMessage) error {
		switch msg := update.Message.(type) {
		case *tg.Message:
			emit(msg, e)
		}
		return nil
	})

	dispatcher.OnEditChannelMessage(func(ctx context.Context, e tg.Entities, update *tg.UpdateEditChannelMessage) error {
		switch msg := update.Message.(type) {
		case *tg.Message:
			emit(msg, e)
		}
		return nil
	})

	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionPath()},
		Middlewares:    []telegram.Middleware{floodWaitMiddleware()},
		UpdateHandler:  dispatcher,
		Device: telegram.DeviceConfig{
			AppVersion: appVersion,
		},
	})

	return client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check auth status: %w", err)
		}
		if !status.Authorized {
			return errNotLoggedIn
		}

		if len(chatFlags) > 0 {
			allowedPeers = make(map[int64]bool)
			api := client.API()
			for _, spec := range chatFlags {
				d, err := resolveChat(ctx, api, spec)
				if err != nil {
					return fmt.Errorf("--chat %q: %w", spec, err)
				}
				allowedPeers[d.ID] = true
				fmt.Fprintf(os.Stderr, "watching: %s (%d)\n", d.Title, d.ID)
			}
		}

		fmt.Fprintln(os.Stderr, "watching for new messages... (Ctrl+C to stop)")
		<-ctx.Done()
		return ctx.Err()
	})
}

type multiString []string

func (m *multiString) String() string { return strings.Join(*m, ", ") }
func (m *multiString) Set(s string) error {
	*m = append(*m, s)
	return nil
}
