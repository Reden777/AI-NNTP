package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeCompletion struct {
	calls chan []chatMessage
}

func (f *fakeCompletion) Complete(_ context.Context, messages []chatMessage) (string, error) {
	f.calls <- messages
	return "Hello from the model.", nil
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "articles.jsonl")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	h := make(map[string][]string)
	h["From"] = []string{"Alice <alice@example.test>"}
	a := &article{MessageID: "<one@example.test>", Date: time.Now().UTC(), Header: h, Body: "body"}
	if err := s.add(a); err != nil {
		t.Fatal(err)
	}
	loaded, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.get(1, "")
	if got == nil || got.MessageID != a.MessageID || got.Body != "body" {
		t.Fatalf("round trip = %#v", got)
	}
	if err := loaded.add(a); err == nil {
		t.Fatal("duplicate Message-ID was accepted")
	}
}

func TestNNTPPostCreatesAIReply(t *testing.T) {
	db, err := openStore(filepath.Join(t.TempDir(), "articles.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeCompletion{calls: make(chan []chatMessage, 1)}
	s := &server{store: db, ai: fake, model: "test/model", logger: log.New(io.Discard, "", 0)}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	go s.handle(serverConn)
	r := bufio.NewReader(clientConn)
	w := bufio.NewWriter(clientConn)
	readLine(t, r, "200 ")
	send(t, w, "GROUP ai.text\r\n")
	readLine(t, r, "211 0 ")
	send(t, w, "POST\r\n")
	readLine(t, r, "340 ")
	send(t, w, "From: Alice <alice@example.test>\r\nSubject: Greetings\r\nNewsgroups: ai.text\r\nMessage-ID: <user@example.test>\r\n\r\nHello AI.\r\n.\r\n")
	readLine(t, r, "240 ")
	select {
	case messages := <-fake.calls:
		if len(messages) != 2 || messages[1].Role != "user" || messages[1].Content != "Hello AI." {
			t.Fatalf("messages = %#v", messages)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("completion was not requested")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(db.snapshot()) != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(db.snapshot()) != 2 {
		t.Fatal("AI reply was not stored")
	}
	send(t, w, "ARTICLE 2\r\n")
	readLine(t, r, "220 2 ")
	articleText := readDotResponse(t, r)
	if !strings.Contains(articleText, "References: <user@example.test>") || !strings.Contains(articleText, "Hello from the model.") {
		t.Fatalf("article response:\n%s", articleText)
	}
	send(t, w, "QUIT\r\n")
	readLine(t, r, "205 ")
}

func TestDotReaderUnstuffs(t *testing.T) {
	b, err := readDot(bufio.NewReader(strings.NewReader("one\r\n..two\r\n.\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "one\r\n.two\r\n" {
		t.Fatalf("got %q", b)
	}
}

func TestIntegratedReader(t *testing.T) {
	db, err := openStore(filepath.Join(t.TempDir(), "articles.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeCompletion{calls: make(chan []chatMessage, 1)}
	s := &server{store: db, ai: fake, model: "test/model", logger: log.New(io.Discard, "", 0)}
	ts := httptest.NewServer(s.webHandler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	home, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(home), "AI·NNTP") {
		t.Fatalf("home: %d %q", resp.StatusCode, home)
	}

	payload := `{"name":"Alice","subject":"From the web","body":"Hello from the reader","replyTo":""}`
	resp, err = http.Post(ts.URL+"/api/posts", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created["messageId"] == "" {
		t.Fatalf("create: %d %#v", resp.StatusCode, created)
	}

	select {
	case messages := <-fake.calls:
		if messages[len(messages)-1].Content != "Hello from the reader" {
			t.Fatalf("messages = %#v", messages)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("completion was not requested")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(db.snapshot()) != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	resp, err = http.Get(ts.URL + "/api/articles")
	if err != nil {
		t.Fatal(err)
	}
	var articles []webArticle
	if err := json.NewDecoder(resp.Body).Decode(&articles); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(articles) != 2 || articles[0].ThreadID != articles[0].MessageID || articles[1].ThreadID != articles[0].MessageID || !articles[1].AI {
		t.Fatalf("articles = %#v", articles)
	}
}

func send(t *testing.T, w *bufio.Writer, text string) {
	t.Helper()
	if _, err := w.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
}

func readLine(t *testing.T, r *bufio.Reader, prefix string) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("response %q does not start with %q", line, prefix)
	}
	return line
}

func readDotResponse(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var out strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == ".\r\n" {
			return out.String()
		}
		out.WriteString(line)
	}
}
