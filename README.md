# telegram-tools

Small command line tool that logs into Telegram as a **user account** over MTProto
(library: [gotd/td](https://github.com/gotd/td)) and dumps the history of a chat,
which the Bot API cannot do. It can also send a text message as your account with
the `send` command, and stream new messages in real time with the `watch` command.

## Setup

1. Get API credentials: open https://my.telegram.org, log in with your phone,
   choose **API development tools** (App configuration) and create an app.
   Note the `api_id` (a number) and `api_hash` (a hex string).
2. Copy `config.example.json` to `config.json` next to the exe and fill it in
   (`config.json` and `session.json` are git-ignored, they never leave your machine):

   ```json
   {
     "api_id": 123456,
     "api_hash": "0123456789abcdef0123456789abcdef",
     "phone": "+34600000000"
   }
   ```

   `phone` is the account's number in international format.
3. Log in once:

   ```
   telegram-tools login
   ```

   Telegram sends a login code (to the Telegram app if you are logged in elsewhere,
   otherwise by SMS). Type it at the prompt. If the account has a two-step
   verification (cloud) password you are asked for it too (typed in the clear).
   On success the user's name is printed.

The session is stored in `session.json` next to the exe. **That file is the key to
the account**: anyone who has it can act as you. Keep it on this machine and delete
it (and end the session in Telegram: Settings, Devices) when it is no longer needed.
Nothing else is stored; `config.json` holds only the API credentials and the phone.

## Commands

Every command accepts `--help`.

| Command | What it does |
|---|---|
| `telegram-tools login` | Interactive login, saves `session.json`. |
| `telegram-tools whoami` | Prints the logged in user. |
| `telegram-tools chats` | Lists all dialogs, one per line, tab separated: `kind  id  title`. Kind is `user`, `group` (basic group or supergroup) or `channel`. Your own Saved Messages are marked. |
| `telegram-tools history --chat "<title or id>" [flags]` | Dumps the messages of a chat, oldest first. |
| `telegram-tools search --chat "<title or id>" --query "text" [flags]` | Server-side text search in a chat (`messages.search`), same output as `history`. |
| `telegram-tools send --chat "<title or id>" --text "..." [flags]` | Sends a text message to a chat **as your account**. The only command that writes. |
| `telegram-tools watch [--chat "<title or id>"] ...` | Connects and streams new messages as JSON lines to stdout in real time. Stays connected until interrupted. |

Without a session every command prints `not logged in, run: telegram-tools login` and exits 1.

### Chat matching

`--chat` takes the exact numeric id (as shown by `chats`; the Bot API form
`-100…` is accepted too) or a case insensitive substring of the title. If several
chats match, they are listed and the command exits 1; use the id then.

### history flags

| Flag | Meaning |
|---|---|
| `--since 2026-10-04T20:00` | Only messages at or after this time. |
| `--until 2026-10-05` | Only messages at or before this time. |
| `--from <name or id>` | Only messages from this sender: exact user id, else case insensitive substring of the display name or username. |
| `--oldest` | Walk the chat from its first message forwards instead of from the newest backwards. |
| `--limit N` | Stop after N messages. Counted **after** the `--from` filter. `0` (default) means no limit. |
| `--out file` | Write to a file (UTF-8) instead of stdout. |
| `--json` | One JSON object per line instead of text. |

Times are interpreted in the local time zone of this machine and printed the same
way. Accepted forms: `2026-10-04T20:00`, `2026-10-04T20:00:00`, `2026-10-04`,
`2026-10-04 20:00`, RFC3339 with offset.

By default `history` fetches newest to oldest (in pages of 100) until `--since` or
`--limit` is reached and then prints everything oldest first. With `--oldest` it
starts at the beginning of the chat and walks forwards, which is what you want
for questions like "first message X ever sent":

```
telegram-tools history --chat "Agent Lab" --from funbot --oldest --limit 1
```

When `--from` is used in a supergroup or channel, the tool looks the sender up in
the member list and, if it finds exactly one match, lets the server filter by
sender (`messages.search` with `from_id`), which is much faster in big chats. In
any other case (basic groups, private chats, ambiguous names) it walks the
history and filters on the client; the result is the same, just slower.

### search flags

`--query` (required), plus `--from`, `--oldest`, `--limit`, `--out`, `--json` as
above. `search` has no `--since`/`--until`.

### send flags

| Flag | Meaning |
|---|---|
| `--text "..."` | The message. When empty the text is read from stdin, so multi line messages can be piped in. |
| `--reply-to <id>` | Reply to this message id (0, the default, means none). |
| `--json` | Print the result as one JSON object: `{"id": 123, "date": "...", "chat": <id>}`. |

Without `--json` it prints `sent message <id> to <title> at <time>`. The text is
sent as plain text (no markdown parsing), at most 4096 characters. On any failure
(unknown chat, empty text, Telegram error) nothing is printed on stdout, the error
goes to stderr and the exit code is 1:

```
echo "hello from the terminal" | telegram-tools send --chat "Saved Messages" --json
```

### watch flags

| Flag | Meaning |
|---|---|
| `--chat "<title or id>"` | Only stream messages from this chat. Repeatable; without it, all chats are watched. |

`watch` holds a persistent MTProto connection and outputs one JSON line per new or
edited message. Each line has the same fields as `history --json` plus `chat_id`
(the numeric id of the chat the message belongs to). Stop with Ctrl+C.

```
telegram-tools watch --chat "Agent Lab" --chat "Saved Messages"
```

## Output format

Text, one line per message:

```
YYYY-MM-DD HH:MM:SS  <id>  <sender name>: <text>
```

* Newlines inside a message become ` / `.
* Non-text media is shown as `[photo]`, `[video]`, `[audio]`, `[voice]`,
  `[sticker]`, `[gif]`, `[document: name]`, `[location]`, `[poll: question]`, …
  followed by the caption if any.
* Replies get the prefix `(reply to <id>)`, forwards `(fwd from <name>)`.
* Service messages (pins, joins, title changes) are shown as `[service] …`.
* Deleted messages are simply absent (ids have gaps).

With `--json`, each line is an object with `id`, `date` (RFC3339, local offset),
`sender_id`, `sender_name`, `text`, `media`, `reply_to` (0 when none) and
`fwd_from` ("" when none). Service messages have `"[service] …"` as text.

Progress and notes go to stderr, the messages to stdout (or `--out`), so piping
and redirecting work. In Windows PowerShell set
`[Console]::OutputEncoding = [System.Text.Encoding]::UTF8` or use `--out` to keep
emoji and accents intact.

## Rate limits

Telegram answers `FLOOD_WAIT_N` when a client asks too fast. The tool sleeps the
requested time (printed on stderr) and retries, up to 30 minutes per wait.

## Build

Go 1.25 or newer:

```
go build -o telegram-tools.exe .
```

Files: `main.go` (config, login, whoami), `chats.go` (dialogs, chat and sender
matching), `history.go` (history, search, formatting), `send.go` (send),
`watch.go` (real-time message streaming), `floodwait.go` (FLOOD_WAIT retry
middleware, flag helper).
