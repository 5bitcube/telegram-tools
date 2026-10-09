package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

const pageSize = 100

// record is one message in output form.
type record struct {
	ID         int    `json:"id"`
	Date       string `json:"date"`
	SenderID   int64  `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Text       string `json:"text"`
	Media      string `json:"media"`
	ReplyTo    int    `json:"reply_to"`
	FwdFrom    string `json:"fwd_from"`
	date       time.Time
	service    bool
}

func (r record) line() string {
	var b strings.Builder
	b.WriteString(r.date.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "  %d  %s: ", r.ID, r.SenderName)
	if r.ReplyTo != 0 {
		fmt.Fprintf(&b, "(reply to %d) ", r.ReplyTo)
	}
	if r.FwdFrom != "" {
		fmt.Fprintf(&b, "(fwd from %s) ", r.FwdFrom)
	}
	if r.service {
		b.WriteString("[service] ")
	} else if r.Media != "" {
		b.WriteString("[" + r.Media + "]")
		if r.Text != "" {
			b.WriteString(" ")
		}
	}
	b.WriteString(oneLine(r.Text))
	return b.String()
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", " / ")
}

// ---- flags shared by history and search ----

type fetchOpts struct {
	chat   string
	from   string
	since  string
	until  string
	limit  int
	oldest bool
	out    string
	json   bool

	sinceT, untilT time.Time
}

func addCommonFlags(fs *flag.FlagSet, o *fetchOpts, withTimes bool) {
	fs.StringVar(&o.chat, "chat", "", "chat title (case insensitive substring) or id (see: telegram-tools chats)")
	fs.StringVar(&o.from, "from", "", "only messages from this sender: exact id, else case insensitive substring of display name or username")
	if withTimes {
		fs.StringVar(&o.since, "since", "", "only messages at or after this time (local), e.g. 2026-10-04T20:00")
		fs.StringVar(&o.until, "until", "", "only messages at or before this time (local)")
	}
	fs.IntVar(&o.limit, "limit", 0, "stop after this many messages, counted after the --from filter (0 = no limit)")
	fs.BoolVar(&o.oldest, "oldest", false, "walk the chat from its beginning (oldest first) instead of from the end")
	fs.StringVar(&o.out, "out", "", "write to this file instead of stdout")
	fs.BoolVar(&o.json, "json", false, "one JSON object per line instead of text")
}

func (o *fetchOpts) parseTimes() error {
	var err error
	if o.sinceT, err = parseTime(o.since); err != nil {
		return err
	}
	if o.untilT, err = parseTime(o.until); err != nil {
		return err
	}
	if !o.sinceT.IsZero() && !o.untilT.IsZero() && o.untilT.Before(o.sinceT) {
		return fmt.Errorf("--until is before --since")
	}
	return nil
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q (use e.g. 2026-10-04T20:00 or 2026-10-04)", s)
}

// ---- commands ----

func cmdHistory(ctx context.Context, args []string) error {
	fs := newFlagSet("history", "telegram-tools history --chat \"<title or id>\" [--since T] [--until T] [--from <name or id>] [--oldest] [--limit N] [--out file] [--json]\n\n"+
		"Dumps the messages of a chat, printed oldest first.\n"+
		"By default it walks from the newest message backwards until --since or --limit is hit.\n"+
		"With --oldest it walks from the first message of the chat forwards instead, so\n"+
		"  history --chat \"Agent Lab\" --from funbot --oldest --limit 1\n"+
		"prints the first message funbot ever sent there.\n"+
		"Times are local machine time; accepted forms: 2026-10-04T20:00, 2026-10-04T20:00:00, 2026-10-04, RFC3339.")
	var o fetchOpts
	addCommonFlags(fs, &o, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := o.parseTimes(); err != nil {
		return err
	}
	return withClient(ctx, func(ctx context.Context, _ *telegram.Client, api *tg.Client) error {
		d, err := resolveChat(ctx, api, o.chat)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "chat: %s\n", d.String())

		var pager pageFunc
		if sender, ok := resolveSender(ctx, api, d, o.from); ok {
			// Server-side sender filter: messages.search with from_id and an empty query.
			pager = searchPager(api, d.Peer, "", sender, o)
		} else {
			pager = historyPager(api, d.Peer, o)
		}
		recs, err := fetchPages(ctx, pager, o)
		if err != nil {
			return err
		}
		return writeRecords(recs, o)
	})
}

func cmdSearch(ctx context.Context, args []string) error {
	fs := newFlagSet("search", "telegram-tools search --chat \"<title or id>\" --query \"text\" [--from <name or id>] [--oldest] [--limit N] [--out file] [--json]\n\n"+
		"Searches the messages of a chat (messages.search) and prints the matches oldest first, same format as history.\n"+
		"By default the newest matches are fetched first; with --oldest the search starts at the beginning of the chat.")
	var (
		query string
		o     fetchOpts
	)
	fs.StringVar(&query, "query", "", "text to search for")
	addCommonFlags(fs, &o, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("--query is required")
	}
	return withClient(ctx, func(ctx context.Context, _ *telegram.Client, api *tg.Client) error {
		d, err := resolveChat(ctx, api, o.chat)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "chat: %s\n", d.String())
		sender, _ := resolveSender(ctx, api, d, o.from)
		recs, err := fetchPages(ctx, searchPager(api, d.Peer, query, sender, o), o)
		if err != nil {
			return err
		}
		return writeRecords(recs, o)
	})
}

// ---- paging ----

// pageReq describes one page: offsetID/addOffset/limit as in messages.getHistory,
// first marks the first page (where a pager may pivot on a date instead).
type pageReq struct {
	offsetID  int
	addOffset int
	limit     int
	first     bool
}

type pageFunc func(ctx context.Context, p pageReq) (tg.MessagesMessagesClass, error)

// historyPager pages messages.getHistory. On the first page it pivots on a date:
// --until in default mode, --since in --oldest mode (offset_id 0, since offset_id
// takes priority over offset_date).
func historyPager(api *tg.Client, peer tg.InputPeerClass, o fetchOpts) pageFunc {
	return func(ctx context.Context, p pageReq) (tg.MessagesMessagesClass, error) {
		req := &tg.MessagesGetHistoryRequest{
			Peer:      peer,
			OffsetID:  p.offsetID,
			AddOffset: p.addOffset,
			Limit:     p.limit,
		}
		if p.first {
			if o.oldest && !o.sinceT.IsZero() {
				req.OffsetID = 0
				req.OffsetDate = int(o.sinceT.Unix()) - 1
			} else if !o.oldest && !o.untilT.IsZero() {
				req.OffsetDate = int(o.untilT.Unix()) + 1
			}
		}
		return api.MessagesGetHistory(ctx, req)
	}
}

// searchPager pages messages.search. It has no offset_date, so time bounds go
// through min_date/max_date and --oldest always starts at offset_id 1.
func searchPager(api *tg.Client, peer tg.InputPeerClass, query string, from tg.InputPeerClass, o fetchOpts) pageFunc {
	return func(ctx context.Context, p pageReq) (tg.MessagesMessagesClass, error) {
		req := &tg.MessagesSearchRequest{
			Peer:      peer,
			Q:         query,
			Filter:    &tg.InputMessagesFilterEmpty{},
			OffsetID:  p.offsetID,
			AddOffset: p.addOffset,
			Limit:     p.limit,
		}
		if from != nil {
			req.FromID = from
		}
		if !o.sinceT.IsZero() {
			req.MinDate = int(o.sinceT.Unix())
		}
		if !o.untilT.IsZero() {
			req.MaxDate = int(o.untilT.Unix())
		}
		return api.MessagesSearch(ctx, req)
	}
}

// fetchPages walks a paginated newest-first query and returns the records oldest first.
//
// Default mode: offset_id paging from the newest message backwards, stopping at --since.
// --oldest mode: offset_id = last seen id + 1 with add_offset = -page, which makes the
// server return the page of messages right after the pivot (the trick Telethon uses for
// reverse iteration), starting at offset_id 1 = the beginning of the chat.
func fetchPages(ctx context.Context, page pageFunc, o fetchOpts) ([]record, error) {
	ents := newEntities()
	seen := map[int]bool{}
	var recs []record
	offsetID := 0
	if o.oldest {
		offsetID = 1
	}
	first := true
	for {
		req := pageReq{offsetID: offsetID, limit: pageSize, first: first}
		if o.oldest {
			req.addOffset = -pageSize
		}
		first = false

		res, err := page(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("fetch messages: %w", err)
		}
		mod, ok := res.AsModified()
		if !ok {
			break
		}
		ents.add(mod.GetUsers(), mod.GetChats())
		msgs := mod.GetMessages()
		if len(msgs) == 0 {
			break
		}
		if o.oldest { // pages come newest first; walk this page oldest first
			for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
				msgs[i], msgs[j] = msgs[j], msgs[i]
			}
		}

		done := false
		fresh := 0
		next := offsetID
		for _, m := range msgs {
			nem, ok := m.(tg.NotEmptyMessage)
			if !ok || seen[nem.GetID()] {
				continue // deleted/empty message, or overlap with the previous page
			}
			seen[nem.GetID()] = true
			fresh++
			id := nem.GetID()
			if o.oldest {
				if id+1 > next {
					next = id + 1
				}
			} else if next == 0 || id < next {
				next = id
			}
			when := time.Unix(int64(nem.GetDate()), 0)
			if o.oldest {
				if !o.sinceT.IsZero() && when.Before(o.sinceT) {
					continue
				}
				if !o.untilT.IsZero() && when.After(o.untilT) {
					done = true
					break
				}
			} else {
				if !o.untilT.IsZero() && when.After(o.untilT) {
					continue
				}
				if !o.sinceT.IsZero() && when.Before(o.sinceT) {
					done = true
					break
				}
			}
			r := toRecord(nem, ents)
			if o.from != "" && !matchSender(r, ents, o.from) {
				continue
			}
			recs = append(recs, r)
			if o.limit > 0 && len(recs) >= o.limit {
				done = true
				break
			}
		}
		fmt.Fprintf(os.Stderr, "\rfetched %d messages...", len(recs))
		if done || fresh == 0 || len(msgs) < pageSize {
			break
		}
		offsetID = next
	}
	fmt.Fprintf(os.Stderr, "\rfetched %d messages.   \n", len(recs))
	if !o.oldest { // newest first -> oldest first
		for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
			recs[i], recs[j] = recs[j], recs[i]
		}
	}
	return recs, nil
}

// matchSender reports whether a record's sender matches --from: exact id,
// else case insensitive substring of the display name or the username.
func matchSender(r record, ents *entities, spec string) bool {
	spec = strings.TrimSpace(spec)
	if id, err := strconv.ParseInt(spec, 10, 64); err == nil {
		return r.SenderID == id
	}
	needle := strings.ToLower(strings.TrimPrefix(spec, "@"))
	if strings.Contains(strings.ToLower(r.SenderName), needle) {
		return true
	}
	if u, ok := ents.users[r.SenderID]; ok {
		for _, un := range userNames(u) {
			if strings.Contains(strings.ToLower(un), needle) {
				return true
			}
		}
	}
	return false
}

// ---- conversion ----

func toRecord(m tg.NotEmptyMessage, ents *entities) record {
	r := record{
		ID:   m.GetID(),
		date: time.Unix(int64(m.GetDate()), 0).Local(),
	}
	r.Date = r.date.Format(time.RFC3339)

	// Sender: from_id if present, else the peer itself (channel posts).
	from, ok := m.GetFromID()
	if !ok || from == nil {
		from = m.GetPeerID()
	}
	r.SenderID = peerID(from)
	r.SenderName = ents.name(from)

	if rt, ok := m.GetReplyTo(); ok {
		if h, ok := rt.(*tg.MessageReplyHeader); ok {
			r.ReplyTo = h.ReplyToMsgID
		}
	}

	switch msg := m.(type) {
	case *tg.Message:
		if msg.PostAuthor != "" && msg.FromID == nil {
			r.SenderName += " (" + msg.PostAuthor + ")"
		}
		if fwd, ok := msg.GetFwdFrom(); ok {
			r.FwdFrom = fwdName(fwd, ents)
		}
		r.Text = msg.Message
		if msg.Media != nil {
			r.Media = describeMedia(msg.Media)
		}
	case *tg.MessageService:
		r.service = true
		r.Text = describeAction(msg.Action, ents)
	}
	return r
}

func fwdName(fwd tg.MessageFwdHeader, ents *entities) string {
	if fwd.FromID != nil {
		name := ents.name(fwd.FromID)
		if fwd.PostAuthor != "" {
			name += " (" + fwd.PostAuthor + ")"
		}
		return name
	}
	if fwd.FromName != "" {
		return fwd.FromName
	}
	if fwd.PostAuthor != "" {
		return fwd.PostAuthor
	}
	return "unknown"
}

func describeMedia(media tg.MessageMediaClass) string {
	switch m := media.(type) {
	case *tg.MessageMediaEmpty:
		return ""
	case *tg.MessageMediaPhoto:
		return "photo"
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.AsNotEmpty()
		if !ok {
			return "document"
		}
		var filename string
		kind := ""
		for _, a := range doc.Attributes {
			switch attr := a.(type) {
			case *tg.DocumentAttributeSticker, *tg.DocumentAttributeCustomEmoji:
				return "sticker"
			case *tg.DocumentAttributeAudio:
				if attr.Voice {
					return "voice"
				}
				kind = "audio"
			case *tg.DocumentAttributeVideo:
				if kind == "" {
					if attr.RoundMessage {
						kind = "video note"
					} else {
						kind = "video"
					}
				}
			case *tg.DocumentAttributeAnimated:
				kind = "gif"
			case *tg.DocumentAttributeFilename:
				filename = attr.FileName
			}
		}
		if kind != "" {
			return kind
		}
		if filename != "" {
			return "document: " + filename
		}
		return "document"
	case *tg.MessageMediaWebPage:
		return "" // the link is already in the text
	case *tg.MessageMediaGeo, *tg.MessageMediaGeoLive, *tg.MessageMediaVenue:
		return "location"
	case *tg.MessageMediaContact:
		return "contact"
	case *tg.MessageMediaPoll:
		if m.Poll.Question.Text != "" {
			return "poll: " + oneLine(m.Poll.Question.Text)
		}
		return "poll"
	case *tg.MessageMediaDice:
		return fmt.Sprintf("dice %s %d", m.Emoticon, m.Value)
	case *tg.MessageMediaStory:
		return "story"
	case *tg.MessageMediaPaidMedia:
		return "paid media"
	case *tg.MessageMediaGame:
		return "game"
	case *tg.MessageMediaInvoice:
		return "invoice"
	}
	return "media: " + strings.TrimPrefix(media.TypeName(), "messageMedia")
}

func describeAction(action tg.MessageActionClass, ents *entities) string {
	users := func(ids []int64) string {
		names := make([]string, 0, len(ids))
		for _, id := range ids {
			names = append(names, ents.name(&tg.PeerUser{UserID: id}))
		}
		return strings.Join(names, ", ")
	}
	switch a := action.(type) {
	case *tg.MessageActionPinMessage:
		return "pinned a message"
	case *tg.MessageActionChatAddUser:
		return "added " + users(a.Users)
	case *tg.MessageActionChatDeleteUser:
		return "removed " + users([]int64{a.UserID})
	case *tg.MessageActionChatJoinedByLink:
		return "joined via invite link"
	case *tg.MessageActionChatJoinedByRequest:
		return "joined (request approved)"
	case *tg.MessageActionChatCreate:
		return "created group " + a.Title
	case *tg.MessageActionChannelCreate:
		return "created channel " + a.Title
	case *tg.MessageActionChatEditTitle:
		return "changed title to " + a.Title
	case *tg.MessageActionChatEditPhoto:
		return "changed chat photo"
	case *tg.MessageActionChatDeletePhoto:
		return "removed chat photo"
	case *tg.MessageActionChatMigrateTo:
		return fmt.Sprintf("group migrated to supergroup %d", a.ChannelID)
	case *tg.MessageActionChannelMigrateFrom:
		return fmt.Sprintf("supergroup created from group %d (%s)", a.ChatID, a.Title)
	case *tg.MessageActionTopicCreate:
		return "created topic " + a.Title
	case *tg.MessageActionHistoryClear:
		return "cleared history"
	case *tg.MessageActionPhoneCall:
		return "phone call"
	case *tg.MessageActionGroupCall:
		return "group call"
	case *tg.MessageActionContactSignUp:
		return "joined Telegram"
	case *tg.MessageActionSetMessagesTTL:
		return fmt.Sprintf("set auto-delete timer to %ds", a.Period)
	case *tg.MessageActionCustomAction:
		return a.Message
	}
	return strings.TrimPrefix(action.TypeName(), "messageAction")
}

// ---- output ----

func writeRecords(recs []record, o fetchOpts) error {
	var w io.Writer = os.Stdout
	if o.out != "" {
		f, err := os.Create(o.out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	if o.json {
		enc := json.NewEncoder(bw)
		enc.SetEscapeHTML(false)
		for _, r := range recs {
			if r.service {
				r.Text = "[service] " + r.Text
			}
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return nil
	}
	for _, r := range recs {
		if _, err := bw.WriteString(r.line() + "\n"); err != nil {
			return err
		}
	}
	return nil
}
