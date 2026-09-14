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
