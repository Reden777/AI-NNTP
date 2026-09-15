package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const groupName = "ai.text"

type article struct {
	Number    int         `json:"number"`
	MessageID string      `json:"message_id"`
	Date      time.Time   `json:"date"`
	Header    mail.Header `json:"header"`
	Body      string      `json:"body"`
}

type store struct {
	mu       sync.RWMutex
	path     string
	articles []*article
	byID     map[string]*article
}

func openStore(path string) (*store, error) {
	s := &store{path: path, byID: make(map[string]*article)}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for sc.Scan() {
		var a article
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			return nil, fmt.Errorf("read article database: %w", err)
		}
		copy := a
		s.articles = append(s.articles, &copy)
		s.byID[a.MessageID] = &copy
	}
	return s, sc.Err()
}

func (s *store) add(a *article) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[a.MessageID]; exists {
		return errors.New("duplicate Message-ID")
	}
	a.Number = len(s.articles) + 1
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	b, err := json.Marshal(a)
	if err == nil {
		b = append(b, '\n')
		_, err = f.Write(b)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	s.articles = append(s.articles, a)
	s.byID[a.MessageID] = a
	return nil
}

func (s *store) snapshot() []*article {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*article, len(s.articles))
	copy(out, s.articles)
	return out
}

func (s *store) get(number int, id string) *article {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id != "" {
		return s.byID[id]
	}
	if number > 0 && number <= len(s.articles) {
		return s.articles[number-1]
	}
	return nil
}

type completionClient interface {
	Complete(context.Context, []chatMessage) (string, error)
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openRouter struct {
	key, model, baseURL string
	http                *http.Client
}

func (o *openRouter) Complete(ctx context.Context, messages []chatMessage) (string, error) {
	payload := struct {
		Model    string        `json:"model"`
		Messages []chatMessage `json:"messages"`
	}{o.model, messages}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.baseURL, "/")+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+o.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HTTP-Referer", "https://github.com/ai-nntp")
	req.Header.Set("X-Title", "AI-NNTP")
	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("OpenRouter returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var result struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return "", errors.New("OpenRouter returned no message")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

type server struct {
	store  *store
	ai     completionClient
	model  string
	logger *log.Logger
	jobMu  sync.RWMutex
	jobs   map[string]string
}

func (s *server) serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

type session struct {
	selected bool
	current  int
}

func (s *server) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	writeLine(w, "200 AI-NNTP ready - posting allowed")
	w.Flush()
	state := session{}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			writeLine(w, "500 command not recognized")
			w.Flush()
			continue
		}
		cmd := strings.ToUpper(fields[0])
		arg := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		switch cmd {
		case "CAPABILITIES":
			writeMultiline(w, "101 Capability list follows", []string{"VERSION 2", "READER", "POST", "OVER", "LIST ACTIVE", "IMPLEMENTATION AI-NNTP"})
		case "MODE":
			if strings.EqualFold(arg, "READER") {
				writeLine(w, "200 Reader mode - posting allowed")
			} else {
				writeLine(w, "501 unknown MODE variant")
			}
		case "LIST":
			articles := s.store.snapshot()
			high := len(articles)
			writeMultiline(w, "215 list of newsgroups follows", []string{fmt.Sprintf("%s %d 1 y", groupName, high)})
		case "GROUP":
			if !strings.EqualFold(arg, groupName) {
				writeLine(w, "411 no such news group")
				break
			}
			state.selected = true
			count := len(s.store.snapshot())
			if count > 0 {
				state.current = 1
			}
			writeLine(w, fmt.Sprintf("211 %d 1 %d %s", count, count, groupName))
		case "ARTICLE", "HEAD", "BODY", "STAT":
			s.articleCommand(w, &state, cmd, arg)
		case "NEXT", "LAST":
			s.move(w, &state, cmd == "NEXT")
		case "OVER", "XOVER":
			s.over(w, &state, arg)
		case "POST":
			writeLine(w, "340 send article; end with <CR-LF>.<CR-LF>")
			w.Flush()
			raw, err := readDot(r)
			if err != nil {
				return
			}
			if err := s.accept(raw); err != nil {
				writeLine(w, "441 posting failed: "+cleanStatus(err.Error()))
			} else {
				writeLine(w, "240 article received OK")
			}
		case "DATE":
			writeLine(w, "111 "+time.Now().UTC().Format("20060102150405"))
		case "HELP":
			writeMultiline(w, "100 help text follows", []string{"CAPABILITIES MODE READER LIST GROUP ARTICLE HEAD BODY STAT NEXT LAST OVER POST DATE QUIT"})
		case "QUIT":
			writeLine(w, "205 closing connection")
			w.Flush()
			return
		default:
			writeLine(w, "500 command not recognized")
		}
		w.Flush()
	}
}

func (s *server) articleCommand(w *bufio.Writer, state *session, cmd, arg string) {
	number, id, ok := target(arg, state)
	if !ok {
		writeLine(w, "412 no newsgroup selected")
		return
	}
	a := s.store.get(number, id)
	if a == nil {
		writeLine(w, "430 no such article")
		return
	}
	if id == "" {
		state.current = a.Number
	}
	status := map[string]int{"ARTICLE": 220, "HEAD": 221, "BODY": 222, "STAT": 223}[cmd]
	writeLine(w, fmt.Sprintf("%d %d %s article", status, a.Number, a.MessageID))
	if cmd == "STAT" {
		return
	}
	lines := []string{}
	if cmd != "BODY" {
		lines = append(lines, headerLines(a)...)
	}
	if cmd == "ARTICLE" {
		lines = append(lines, "")
	}
	if cmd != "HEAD" {
		lines = append(lines, splitBody(a.Body)...)
	}
	writeDotLines(w, lines)
}

func target(arg string, state *session) (int, string, bool) {
	if arg == "" {
		return state.current, "", state.selected
	}
	if strings.HasPrefix(arg, "<") {
		return 0, arg, true
	}
	n, err := strconv.Atoi(arg)
	return n, "", err == nil && state.selected
}

func (s *server) move(w *bufio.Writer, state *session, next bool) {
	if !state.selected {
		writeLine(w, "412 no newsgroup selected")
		return
	}
	if next {
		state.current++
	} else {
		state.current--
	}
	a := s.store.get(state.current, "")
	if a == nil {
		if next {
			state.current--
		} else {
			state.current++
		}
		writeLine(w, "421 no next article in this group")
		return
	}
	writeLine(w, fmt.Sprintf("223 %d %s article", a.Number, a.MessageID))
}

func (s *server) over(w *bufio.Writer, state *session, arg string) {
	if !state.selected {
		writeLine(w, "412 no newsgroup selected")
		return
	}
	all := s.store.snapshot()
	lo, hi := state.current, state.current
	if arg != "" {
		parts := strings.SplitN(arg, "-", 2)
		lo, _ = strconv.Atoi(parts[0])
		hi = lo
		if len(parts) == 2 {
			if parts[1] == "" {
				hi = len(all)
			} else {
				hi, _ = strconv.Atoi(parts[1])
			}
		}
	}
	lines := []string{}
	for _, a := range all {
		if a.Number >= lo && a.Number <= hi {
			lines = append(lines, fmt.Sprintf("%d\t%s\t%s\t%s\t%s\t%s\t%d\t%d", a.Number, oneLine(a.Header.Get("Subject")), oneLine(a.Header.Get("From")), a.Date.Format(time.RFC1123Z), a.MessageID, oneLine(a.Header.Get("References")), len(a.Body), len(splitBody(a.Body))))
		}
	}
	writeMultiline(w, "224 Overview information follows", lines)
}

func (s *server) accept(raw []byte) error {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return errors.New("invalid article")
	}
	body, err := io.ReadAll(io.LimitReader(m.Body, 1<<20))
	if err != nil {
		return err
	}
	groups := strings.FieldsFunc(m.Header.Get("Newsgroups"), func(r rune) bool { return r == ',' || r == ' ' })
	if len(groups) != 1 || !strings.EqualFold(groups[0], groupName) {
		return errors.New("only ai.text is available")
	}
	if strings.TrimSpace(m.Header.Get("From")) == "" || strings.TrimSpace(m.Header.Get("Subject")) == "" {
		return errors.New("From and Subject headers are required")
	}
	id := strings.TrimSpace(m.Header.Get("Message-ID"))
	if id == "" {
		id = newMessageID()
		headerSet(m.Header, "Message-ID", id)
	}
	if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, ">") {
		return errors.New("invalid Message-ID")
	}
	now := time.Now().UTC()
	if m.Header.Get("Date") == "" {
		headerSet(m.Header, "Date", now.Format(time.RFC1123Z))
	}
	a := &article{MessageID: id, Date: now, Header: cloneHeader(m.Header), Body: strings.TrimRight(string(body), "\r\n")}
	if err := s.store.add(a); err != nil {
		return err
	}
	s.queueReply(a)
	return nil
}

func (s *server) queueReply(a *article) {
	s.jobMu.Lock()
	if s.jobs == nil {
		s.jobs = make(map[string]string)
	}
	s.jobs[a.MessageID] = "pending"
	s.jobMu.Unlock()
	go s.reply(a)
}

func (s *server) replyState(messageID string) string {
	s.jobMu.RLock()
	defer s.jobMu.RUnlock()
	return s.jobs[messageID]
}

func (s *server) finishReply(messageID, state string) {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	if state == "" {
		delete(s.jobs, messageID)
	} else {
		s.jobs[messageID] = state
	}
}

func (s *server) reply(post *article) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	messages := []chatMessage{{Role: "system", Content: "You are participating in the ai.text NNTP newsgroup. Reply directly and helpfully to the user's post. Use plain text suitable for Usenet; do not add email headers."}}
	threadIDs := make(map[string]bool)
	for _, id := range strings.Fields(post.Header.Get("References")) {
		threadIDs[id] = true
	}
	threadIDs[post.MessageID] = true
	for _, a := range s.store.snapshot() {
		if a.Number > post.Number {
			break
		}
		if threadIDs[a.MessageID] {
			messages = append(messages, chatMessage{Role: roleFor(a), Content: a.Body})
		}
	}
	text, err := s.ai.Complete(ctx, messages)
	if err != nil {
		s.logger.Printf("AI reply to %s failed: %v", post.MessageID, err)
		s.finishReply(post.MessageID, "failed")
		return
	}
	subject := post.Header.Get("Subject")
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	refs := strings.TrimSpace(post.Header.Get("References") + " " + post.MessageID)
	h := mail.Header{"From": {s.model + " <ai@ai-nntp.local>"}, "Subject": {subject}, "Newsgroups": {groupName}, "References": {refs}, "In-Reply-To": {post.MessageID}}
	now := time.Now().UTC()
	headerSet(h, "Date", now.Format(time.RFC1123Z))
	a := &article{MessageID: newMessageID(), Date: now, Header: h, Body: text}
	headerSet(a.Header, "Message-ID", a.MessageID)
	if err := s.store.add(a); err != nil {
		s.logger.Printf("saving AI reply failed: %v", err)
		s.finishReply(post.MessageID, "failed")
		return
	}
	s.finishReply(post.MessageID, "")
}

func roleFor(a *article) string {
	if strings.Contains(a.Header.Get("From"), "<ai@ai-nntp.local>") {
		return "assistant"
	}
	return "user"
}

func headerLines(a *article) []string {
	keys := []string{"From", "Subject", "Newsgroups", "Date", "Message-ID", "References", "In-Reply-To"}
	out := []string{}
	for _, k := range keys {
		if v := a.Header.Get(k); v != "" {
			out = append(out, k+": "+v)
		}
	}
	return out
}
func splitBody(body string) []string {
	return strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
}
func writeLine(w *bufio.Writer, line string) { fmt.Fprint(w, line+"\r\n") }
func writeMultiline(w *bufio.Writer, status string, lines []string) {
	writeLine(w, status)
	writeDotLines(w, lines)
}
func writeDotLines(w *bufio.Writer, lines []string) {
	for _, line := range lines {
		if strings.HasPrefix(line, ".") {
			line = "." + line
		}
		writeLine(w, line)
	}
	writeLine(w, ".")
}
func readDot(r *bufio.Reader) ([]byte, error) {
	var b bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			return b.Bytes(), nil
		}
		if strings.HasPrefix(trimmed, "..") {
			trimmed = trimmed[1:]
		}
		b.WriteString(trimmed + "\r\n")
	}
}
func cloneHeader(h mail.Header) mail.Header {
	out := make(mail.Header, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}
func headerSet(h mail.Header, key, value string) {
	h[textproto.CanonicalMIMEHeaderKey(key)] = []string{value}
}
func newMessageID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "<" + hex.EncodeToString(b) + "@ai-nntp.local>"
}
func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", "\t", "\t", " ").Replace(s)
}
func cleanStatus(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	addr := flag.String("addr", env("AI_NNTP_ADDR", "127.0.0.1:1119"), "NNTP listen address")
	webAddr := flag.String("web-addr", env("AI_NNTP_WEB_ADDR", "127.0.0.1:8080"), "integrated web reader listen address (empty to disable)")
	data := flag.String("data", env("AI_NNTP_DATA", "./data/articles.jsonl"), "article database path")
	model := flag.String("model", env("OPENROUTER_MODEL", "google/gemma-3-27b-it"), "OpenRouter model")
	baseURL := flag.String("openrouter-url", env("OPENROUTER_URL", "https://openrouter.ai/api/v1"), "OpenRouter API base URL")
	flag.Parse()
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		log.Fatal("OPENROUTER_API_KEY is required")
	}
	db, err := openStore(*data)
	if err != nil {
		log.Fatal(err)
	}
	nntpListener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{store: db, model: *model, logger: log.Default(), ai: &openRouter{key: key, model: *model, baseURL: *baseURL, http: &http.Client{Timeout: 2 * time.Minute}}}
	log.Printf("AI-NNTP listening on %s; group=%s model=%s", nntpListener.Addr(), groupName, *model)
	if *webAddr == "" {
		if err := s.serve(nntpListener); err != nil {
			log.Fatal(err)
		}
		return
	}
	webListener, err := net.Listen("tcp", *webAddr)
	if err != nil {
		_ = nntpListener.Close()
		log.Fatal(err)
	}
	log.Printf("integrated newsreader available at http://%s", webListener.Addr())
	errors := make(chan error, 2)
	go func() { errors <- s.serve(nntpListener) }()
	webServer := &http.Server{Handler: s.webHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { errors <- webServer.Serve(webListener) }()
	log.Fatal(<-errors)
}
