package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// maxMessageChars is Telegram's limit for one text message.
const maxMessageChars = 4096

// sent is the output of the send command.
type sent struct {
	ID   int    `json:"id"`
	Date string `json:"date"`
	Chat int64  `json:"chat"`
}

func cmdSend(ctx context.Context, args []string) error {
	fs := newFlagSet("send", "telegram-tools send --chat \"<title or id>\" --text \"...\" [--reply-to <msg id>] [--json]\n\n"+
		"Sends a text message to a chat as the logged in account.\n"+
		"With an empty --text the message is read from stdin (so multi line text works).\n"+
		"Prints the id and date of the sent message.")
	var (
		chat    string
		text    string
		replyTo int
		asJSON  bool
	)
	fs.StringVar(&chat, "chat", "", "chat title (case insensitive substring) or id (see: telegram-tools chats)")
	fs.StringVar(&text, "text", "", "message text; empty: read it from stdin")
	fs.IntVar(&replyTo, "reply-to", 0, "id of the message to reply to (0 = none)")
	fs.BoolVar(&asJSON, "json", false, "print the result as one JSON object")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if text == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read text from stdin: %w", err)
		}
		text = string(data)
	}
	text = strings.TrimPrefix(text, string(rune(0xFEFF))) // BOM, which PowerShell may put on a pipe
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("the message is empty (use --text or pipe it on stdin)")
	}
	if n := utf8.RuneCountInString(text); n > maxMessageChars {
		return fmt.Errorf("the message is %d characters long, the limit is %d", n, maxMessageChars)
	}
	if replyTo < 0 {
		return fmt.Errorf("--reply-to must be a message id")
	}

	return withClient(ctx, func(ctx context.Context, _ *telegram.Client, api *tg.Client) error {
		d, err := resolveChat(ctx, api, chat)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "chat: %s\n", d.String())

		req := &tg.MessagesSendMessageRequest{
			Peer:     d.Peer,
			Message:  text,
			RandomID: randomID(),
		}
		if replyTo > 0 {
			req.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: replyTo}
		}
		upd, err := api.MessagesSendMessage(ctx, req)
		if err != nil {
			return fmt.Errorf("send message: %w", err)
		}
		id, date := sentMessage(upd, req.RandomID)
		if id == 0 {
			return fmt.Errorf("the message was sent but Telegram returned no message id (%s)", upd.TypeName())
		}
		if date.IsZero() {
			date = time.Now()
		}
		out := sent{ID: id, Date: date.Local().Format(time.RFC3339), Chat: d.ID}
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetEscapeHTML(false)
			return enc.Encode(out)
		}
		fmt.Printf("sent message %d to %s at %s\n", out.ID, d.Title, date.Local().Format("2006-01-02 15:04:05"))
		return nil
	})
}

// randomID returns the random_id Telegram wants on every sent message.
func randomID() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.LittleEndian.Uint64(b[:]))
}

// sentMessage finds the id and date of the message that was just sent in the
// updates Telegram returns: a short form for private chats and basic groups,
// else a list that contains updateMessageID (matched on our random_id) and
// usually the new message itself.
func sentMessage(upd tg.UpdatesClass, randomID int64) (int, time.Time) {
	var (
		id   int
		date time.Time
		list []tg.UpdateClass
	)
	switch u := upd.(type) {
	case *tg.UpdateShortSentMessage:
		return u.ID, time.Unix(int64(u.Date), 0)
	case *tg.Updates:
		list = u.Updates
		date = time.Unix(int64(u.Date), 0)
	case *tg.UpdatesCombined:
		list = u.Updates
		date = time.Unix(int64(u.Date), 0)
	default:
		return 0, time.Time{}
	}
	for _, x := range list {
		if m, ok := x.(*tg.UpdateMessageID); ok && m.RandomID == randomID {
			id = m.ID
		}
	}
	for _, x := range list {
		var msg tg.MessageClass
		switch v := x.(type) {
		case *tg.UpdateNewMessage:
			msg = v.Message
		case *tg.UpdateNewChannelMessage:
			msg = v.Message
		}
		if m, ok := msg.(*tg.Message); ok && (id == 0 || m.ID == id) {
			if id == 0 {
				id = m.ID
			}
			date = time.Unix(int64(m.Date), 0)
		}
	}
	return id, date
}
