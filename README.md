# AI-NNTP

An NNTP Server that has OpenRouter integration. Sort of like a new kind of board-like communication interface over NNTP.

## Behavior

1. The user uses a newsreader to connect to the server.
2. The user posts a post to a newsgroup. (Available newsgroups are under the ai.* taxonomy, for now, just like an one-board imageboard, there only should be ai.text)
3. The server notices the post and sends an api request to openrouter.
4. An ai model (at least for now, Gemma 4 31B) replies.
5. The user replies to the ai.
6. The ai replies again, and so forth.

Eventually, the user will become capable of selecting the ai model he wants, and to add more ai models to his "conversation".

One single binary.

Language: Go

License: Tbd, please do not create one until later.

## Run

Go 1.22 or newer is required. Set an OpenRouter API key, then start the server:

```sh
export OPENROUTER_API_KEY='your-key'
go run .
```

Open the integrated newsreader at <http://127.0.0.1:8080>. It groups articles
into conversations, provides tailored new-post and reply forms, and refreshes
automatically while the model composes a response.

The NNTP server remains available on `127.0.0.1:1119` for external newsreaders,
and articles are persisted in `./data/articles.jsonl`. Both interfaces use the
same `ai.text` group and conversation history.

Configuration is available through flags or environment variables:

| Flag | Environment | Default |
| --- | --- | --- |
| `-addr` | `AI_NNTP_ADDR` | `127.0.0.1:1119` |
| `-web-addr` | `AI_NNTP_WEB_ADDR` | `127.0.0.1:8080` |
| `-data` | `AI_NNTP_DATA` | `./data/articles.jsonl` |
| `-model` | `OPENROUTER_MODEL` | `google/gemma-3-27b-it` |
| `-openrouter-url` | `OPENROUTER_URL` | `https://openrouter.ai/api/v1` |

For example, to allow LAN connections to both interfaces:

```sh
go run . -addr 0.0.0.0:1119 -web-addr 0.0.0.0:8080
```

There is currently no authentication or TLS, so do not expose the listener to
the public internet. Pass `-web-addr ''` to disable the integrated reader. Run
tests with `go test ./...` and build the single binary with
`go build -o ai-nntp .`; the web interface is embedded in that binary.
