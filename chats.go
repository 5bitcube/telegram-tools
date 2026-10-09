package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"
)

// entities caches users, chats and channels seen in API results, by id.
type entities struct {
	users    map[int64]*tg.User
	chats    map[int64]*tg.Chat
	channels map[int64]*tg.Channel
}

func newEntities() *entities {
	return &entities{
		users:    map[int64]*tg.User{},
		chats:    map[int64]*tg.Chat{},
		channels: map[int64]*tg.Channel{},
	}
}

func (e *entities) add(users []tg.UserClass, chats []tg.ChatClass) {
	for _, u := range users {
		if user, ok := u.(*tg.User); ok {
			e.users[user.ID] = user
		}
	}
	for _, c := range chats {
		switch chat := c.(type) {
		case *tg.Chat:
			e.chats[chat.ID] = chat
		case *tg.Channel:
			e.channels[chat.ID] = chat
		}
	}
}

// peerID returns the numeric id of a peer, 0 for nil.
func peerID(p tg.PeerClass) int64 {
	switch v := p.(type) {
	case *tg.PeerUser:
		return v.UserID
	case *tg.PeerChat:
		return v.ChatID
	case *tg.PeerChannel:
		return v.ChannelID
	}
	return 0
}

// name returns a display name for a peer, falling back to "<kind> <id>".
func (e *entities) name(p tg.PeerClass) string {
	switch v := p.(type) {
	case *tg.PeerUser:
		if u, ok := e.users[v.UserID]; ok {
			return userName(u)
		}
		return fmt.Sprintf("user %d", v.UserID)
	case *tg.PeerChat:
		if c, ok := e.chats[v.ChatID]; ok {
			return c.Title
		}
		return fmt.Sprintf("chat %d", v.ChatID)
	case *tg.PeerChannel:
		if c, ok := e.channels[v.ChannelID]; ok {
			return c.Title
		}
		return fmt.Sprintf("channel %d", v.ChannelID)
	}
	return ""
}

// dialog is one entry of the dialog list.
type dialog struct {
	Kind  string // user, group, channel
	ID    int64
	Title string
	Self  bool // the current user's own "Saved Messages"
	Peer  tg.InputPeerClass
}

func (d dialog) String() string {
	title := d.Title
	if d.Self {
		title += " (you, saved messages)"
	}
	return fmt.Sprintf("%s\t%d\t%s", d.Kind, d.ID, title)
}

// listDialogs fetches all dialogs of the account.
func listDialogs(ctx context.Context, api *tg.Client) ([]dialog, error) {
	elems, err := dialogs.NewQueryBuilder(api).GetDialogs().BatchSize(100).Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("get dialogs: %w", err)
	}
	var out []dialog
	for _, el := range elems {
		dlg, ok := el.Dialog.(*tg.Dialog)
		if !ok {
			continue // dialogFolder (archive) entry, not a chat
		}
		d := dialog{Peer: el.Peer}
		switch p := dlg.Peer.(type) {
		case *tg.PeerUser:
			d.Kind, d.ID = "user", p.UserID
			if u, ok := el.Entities.User(p.UserID); ok {
				d.Title = userName(u)
				if u.Self {
					d.Self = true
					d.Title = "Saved Messages"
				}
			} else {
				d.Title = fmt.Sprintf("user %d", p.UserID)
			}
		case *tg.PeerChat:
			d.Kind, d.ID = "group", p.ChatID
			if c, ok := el.Entities.Chat(p.ChatID); ok {
				d.Title = c.Title
			} else {
				d.Title = fmt.Sprintf("chat %d", p.ChatID)
			}
		case *tg.PeerChannel:
			d.Kind, d.ID = "channel", p.ChannelID
			if c, ok := el.Entities.Channel(p.ChannelID); ok {
				d.Title = c.Title
				if c.Megagroup {
					d.Kind = "group"
				}
			} else {
				d.Title = fmt.Sprintf("channel %d", p.ChannelID)
			}
		default:
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// resolveChat finds one dialog by exact id (raw id or Bot API style -100... id),
// else by case insensitive substring match on the title. Several matches is an error.
func resolveChat(ctx context.Context, api *tg.Client, spec string) (dialog, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return dialog{}, fmt.Errorf("--chat is required (title or id; see: telegram-tools chats)")
	}
	all, err := listDialogs(ctx, api)
	if err != nil {
		return dialog{}, err
	}
	if id, err := strconv.ParseInt(spec, 10, 64); err == nil {
		for _, d := range all {
			if d.ID == id || botAPIID(d) == id {
				return d, nil
			}
		}
	}
	var matches []dialog
	needle := strings.ToLower(spec)
	for _, d := range all {
		if strings.ToLower(d.Title) == needle {
			return d, nil // exact title wins over substring matches
		}
		if strings.Contains(strings.ToLower(d.Title), needle) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 0:
		return dialog{}, fmt.Errorf("no chat matches %q (see: telegram-tools chats)", spec)
	case 1:
		return matches[0], nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d chats match %q, use the id:\n", len(matches), spec)
	for _, d := range matches {
		b.WriteString("  " + d.String() + "\n")
	}
	return dialog{}, fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
}

// userNames returns all usernames of a user (main one plus collectible ones), without "@".
func userNames(u *tg.User) []string {
	var out []string
	if u.Username != "" {
		out = append(out, u.Username)
	}
	for _, un := range u.Usernames {
		if un.Username != "" && un.Username != u.Username {
			out = append(out, un.Username)
		}
	}
	return out
}

// matchUser reports whether a user matches a --from spec: exact id, else case
// insensitive substring of the display name or of a username.
func matchUser(u *tg.User, spec string) bool {
	spec = strings.TrimSpace(spec)
	if id, err := strconv.ParseInt(spec, 10, 64); err == nil {
		return u.ID == id
	}
	needle := strings.ToLower(strings.TrimPrefix(spec, "@"))
	if strings.Contains(strings.ToLower(userName(u)), needle) {
		return true
	}
	for _, un := range userNames(u) {
		if strings.Contains(strings.ToLower(un), needle) {
			return true
		}
	}
	return false
}

// resolveSender tries to turn a --from spec into an input peer so the server can
// filter by sender (messages.search from_id). It only looks at supergroup/channel
// participants and gives up (returning false) unless exactly one member matches;
// the caller then falls back to filtering client side, which is always applied anyway.
func resolveSender(ctx context.Context, api *tg.Client, d dialog, spec string) (tg.InputPeerClass, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, false
	}
	ch, ok := d.Peer.(*tg.InputPeerChannel)
	if !ok {
		return nil, false
	}
	q := spec
	if _, err := strconv.ParseInt(spec, 10, 64); err == nil {
		q = "" // numeric id: list members and match on id
	}
	res, err := api.ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
		Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
		Filter:  &tg.ChannelParticipantsSearch{Q: strings.TrimPrefix(q, "@")},
		Limit:   200,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: participant lookup failed (%v); filtering by sender client side\n", err)
		return nil, false
	}
	parts, ok := res.AsModified()
	if !ok {
		return nil, false
	}
	var matches []*tg.User
	for _, uc := range parts.Users {
		if u, ok := uc.(*tg.User); ok && matchUser(u, spec) {
			matches = append(matches, u)
		}
	}
	if len(matches) != 1 {
		if len(matches) > 1 {
			fmt.Fprintf(os.Stderr, "note: %d members match --from %q; filtering by sender client side\n", len(matches), spec)
		}
		return nil, false
	}
	u := matches[0]
	fmt.Fprintf(os.Stderr, "sender: %s (id %d), filtered server side\n", userName(u), u.ID)
	return &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}, true
}

// botAPIID returns the id as the Bot API shows it (-<id> for groups, -100<id> for channels).
func botAPIID(d dialog) int64 {
	switch d.Peer.(type) {
	case *tg.InputPeerChat:
		return -d.ID
	case *tg.InputPeerChannel:
		return -1000000000000 - d.ID
	}
	return d.ID
}

func cmdChats(ctx context.Context, args []string) error {
	fs := newFlagSet("chats", "telegram-tools chats\n\nLists all dialogs, one per line, tab separated: kind (user/group/channel), id, title.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withClient(ctx, func(ctx context.Context, _ *telegram.Client, api *tg.Client) error {
		all, err := listDialogs(ctx, api)
		if err != nil {
			return err
		}
		for _, d := range all {
			fmt.Println(d.String())
		}
		return nil
	})
}
