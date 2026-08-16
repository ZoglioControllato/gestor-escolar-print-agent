package events

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeEventsAPI é um Events API de mentira: fala o mesmo protocolo, não é a AWS.
//
// Nenhum teste desta fase toca a AWS — não há credencial no ambiente, e um teste que dependesse do
// serviço real seria verde por acidente ou vermelho por rede.
type fakeEventsAPI struct {
	srv *httptest.Server

	mu            sync.Mutex
	connections   []*handshake
	lastSubscribe map[string]any
	connected     chan *session
	ackTimeoutMs  int64

	// script decide o que o servidor faz depois do subscribe. Default: subscribe_success.
	onSubscribe func(s *session)
	// onConnectionInit permite responder erro em vez de ack.
	onConnectionInit func(s *session) bool
}

// handshake é o que o cliente ofereceu no upgrade — o que a matriz de autorização do servidor real
// leria.
type handshake struct {
	subprotocols []string
	authHost     string
	authToken    string
	authDecoded  bool
}

type session struct {
	conn *websocket.Conn
	t    *testing.T
}

func (s *session) send(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.conn.Write(ctx, websocket.MessageText, data)
}

func (s *session) recv(ctx context.Context) (map[string]any, error) {
	_, data, err := s.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func newFakeEventsAPI(t *testing.T) *fakeEventsAPI {
	t.Helper()
	f := &fakeEventsAPI{
		connected:    make(chan *session, 16),
		ackTimeoutMs: 300000,
	}
	f.onSubscribe = func(s *session) {
		_ = s.send(context.Background(), map[string]any{"type": "subscribe_success", "id": "sub"})
	}
	mux := http.NewServeMux()
	mux.HandleFunc(realtimePath, f.handle)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// host devolve o endereço no formato que o device-config publica (host, sem esquema) mas prefixado
// com `http://` para o teste alcançar o httptest — `realtimeURL` traduz para `ws://`.
func (f *fakeEventsAPI) host() string { return f.srv.URL }

// requestSubprotocols lê o `Sec-WebSocket-Protocol` oferecido pelo cliente. É o que o servidor real
// (e o authorizer atrás dele) enxerga do handshake.
func requestSubprotocols(r *http.Request) []string {
	var out []string
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(h, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func (f *fakeEventsAPI) handle(w http.ResponseWriter, r *http.Request) {
	hs := &handshake{subprotocols: requestSubprotocols(r)}
	for _, p := range hs.subprotocols {
		if !strings.HasPrefix(p, "header-") {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(p, "header-"))
		if err != nil {
			continue
		}
		var auth map[string]string
		if json.Unmarshal(raw, &auth) == nil {
			hs.authHost = auth["host"]
			hs.authToken = auth["Authorization"]
			hs.authDecoded = true
		}
	}
	f.mu.Lock()
	f.connections = append(f.connections, hs)
	f.mu.Unlock()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{wsSubprotocol},
	})
	if err != nil {
		return
	}
	s := &session{conn: conn}
	f.connected <- s

	ctx := r.Context()
	msg, err := s.recv(ctx)
	if err != nil {
		return
	}
	if msg["type"] != "connection_init" {
		_ = conn.Close(websocket.StatusProtocolError, "esperado connection_init")
		return
	}
	if f.onConnectionInit != nil && !f.onConnectionInit(s) {
		return
	}
	if err := s.send(ctx, map[string]any{"type": "connection_ack", "connectionTimeoutMs": f.ackTimeoutMs}); err != nil {
		return
	}

	sub, err := s.recv(ctx)
	if err != nil {
		return
	}
	if sub["type"] != "subscribe" {
		_ = conn.Close(websocket.StatusProtocolError, "esperado subscribe")
		return
	}
	f.mu.Lock()
	f.lastSubscribe = sub
	f.mu.Unlock()
	f.onSubscribe(s)

	// Mantém a conexão aberta até o cliente fechar ou o teste terminar.
	<-ctx.Done()
}

func (f *fakeEventsAPI) handshakes() []*handshake {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*handshake, len(f.connections))
	copy(out, f.connections)
	return out
}

func (f *fakeEventsAPI) subscribeMessage() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSubscribe
}

// waitConnection espera uma conexão nova, falhando (em vez de pendurar) se ela não vier.
func (f *fakeEventsAPI) waitConnection(t *testing.T, within time.Duration) *session {
	t.Helper()
	select {
	case s := <-f.connected:
		s.t = t
		return s
	case <-time.After(within):
		t.Fatalf("nenhuma conexão em %v", within)
		return nil
	}
}

func testCfg(f *fakeEventsAPI) Config {
	return Config{
		RealtimeEndpoint: f.host(),
		HTTPHost:         "events-dev.pedagogicoonline.com.br",
		Channel:          "/print/conta-1/device-1",
		Token:            "device-token-secreto",
	}
}
