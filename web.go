package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type webArticle struct {
	Number     int       `json:"number"`
	MessageID  string    `json:"messageId"`
	ParentID   string    `json:"parentId,omitempty"`
	ThreadID   string    `json:"threadId"`
	Subject    string    `json:"subject"`
	From       string    `json:"from"`
	Body       string    `json:"body"`
	Date       time.Time `json:"date"`
	AI         bool      `json:"ai"`
	ReplyState string    `json:"replyState,omitempty"`
}

type webPost struct {
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	ReplyTo string `json:"replyTo"`
}

func (s *server) webHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/articles", s.listArticlesHTTP)
	mux.HandleFunc("POST /api/posts", s.postHTTP)
	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(assets)))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}

func (s *server) listArticlesHTTP(w http.ResponseWriter, _ *http.Request) {
	items := make([]webArticle, 0)
	for _, a := range s.store.snapshot() {
		refs := strings.Fields(a.Header.Get("References"))
		threadID := a.MessageID
		if len(refs) > 0 {
			threadID = refs[0]
		}
		items = append(items, webArticle{
			Number: a.Number, MessageID: a.MessageID,
			ParentID: a.Header.Get("In-Reply-To"), ThreadID: threadID,
			Subject: a.Header.Get("Subject"), From: a.Header.Get("From"),
			Body: a.Body, Date: a.Date, AI: roleFor(a) == "assistant",
			ReplyState: s.replyState(a.MessageID),
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *server) postHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	var input webPost
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid post")
		return
	}
	if err := ensureJSONEnd(dec); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid post")
		return
	}
	a, err := s.createWebPost(input)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"messageId": a.MessageID, "number": a.Number})
}

func ensureJSONEnd(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("extra JSON value")
	}
	return err
}

func (s *server) createWebPost(input webPost) (*article, error) {
	name := strings.TrimSpace(input.Name)
	subject := strings.TrimSpace(input.Subject)
	body := strings.TrimSpace(input.Body)
	if name == "" {
		name = "Anonymous"
	}
	if subject == "" || body == "" {
		return nil, errors.New("subject and message are required")
	}
	if len(name) > 100 || len(subject) > 200 || len(body) > 1<<20 {
		return nil, errors.New("post is too long")
	}
	if strings.ContainsAny(name+subject, "\r\n") {
		return nil, errors.New("headers cannot contain newlines")
	}

	from := (&mail.Address{Name: name, Address: "reader@ai-nntp.local"}).String()
	h := mail.Header{"From": {from}, "Subject": {subject}, "Newsgroups": {groupName}}
	if input.ReplyTo != "" {
		parent := s.store.get(0, input.ReplyTo)
		if parent == nil {
			return nil, errors.New("the article being replied to no longer exists")
		}
		refs := strings.TrimSpace(parent.Header.Get("References") + " " + parent.MessageID)
		headerSet(h, "References", refs)
		headerSet(h, "In-Reply-To", parent.MessageID)
	}
	now := time.Now().UTC()
	headerSet(h, "Date", now.Format(time.RFC1123Z))
	a := &article{MessageID: newMessageID(), Date: now, Header: h, Body: body}
	headerSet(h, "Message-ID", a.MessageID)
	if err := s.store.add(a); err != nil {
		return nil, err
	}
	s.queueReply(a)
	return a, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
